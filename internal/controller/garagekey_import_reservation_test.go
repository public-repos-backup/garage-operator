/*
Copyright 2026 Raj Singh.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	"github.com/rajsinghtech/garage-operator/internal/garage"
)

func TestImportKeySecretRefSnapshotsExactMaterialBeforeRemoteWrite(t *testing.T) {
	const (
		namespace = "tenant"
		keyID     = "GKpinnedimport"
		keySecret = "pinned-secret-material"
	)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key := &garagev1beta1.GarageKey{
		ObjectMeta: metav1.ObjectMeta{Name: "imported", Namespace: namespace, UID: "key-uid"},
		Spec: garagev1beta1.GarageKeySpec{ImportKey: &garagev1beta1.ImportKeyConfig{
			SecretRef: &corev1.SecretReference{Name: "source"},
		}},
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: namespace},
		Data: map[string][]byte{
			defaultAccessKeyIDKey:     []byte(keyID),
			defaultSecretAccessKeyKey: []byte(keySecret),
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(key, source).Build()
	reconciler := &GarageKeyReconciler{Client: kubeClient, Scheme: scheme}

	importCalls := 0
	committed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v2/GetKeyInfo":
			if !committed {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(garage.Key{
				AccessKeyID: keyID, SecretAccessKey: keySecret, Name: key.Name,
			})
		case "/v2/ImportKey":
			importCalls++
			persisted := &garagev1beta1.GarageKey{}
			if err := kubeClient.Get(request.Context(), client.ObjectKeyFromObject(key), persisted); err != nil {
				t.Errorf("get GarageKey during ImportKey: %v", err)
			} else if got := persisted.Annotations[keyResolvedImportIDAnnotation]; got != keyID {
				t.Errorf("resolved identity at remote write = %q, want %q", got, keyID)
			}
			snapshot := &corev1.Secret{}
			if err := kubeClient.Get(request.Context(), client.ObjectKey{
				Name: importKeySnapshotName(key), Namespace: namespace,
			}, snapshot); err != nil {
				t.Errorf("get material snapshot during ImportKey: %v", err)
			} else {
				if snapshot.Immutable == nil || !*snapshot.Immutable {
					t.Errorf("material snapshot is not immutable")
				}
				if got := string(snapshot.Data[defaultAccessKeyIDKey]); got != keyID {
					t.Errorf("snapshot access key ID = %q, want %q", got, keyID)
				}
				if got := string(snapshot.Data[defaultSecretAccessKeyKey]); got != keySecret {
					t.Errorf("snapshot secret access key = %q, want original material", got)
				}
				if !metav1.IsControlledBy(snapshot, persisted) {
					t.Errorf("material snapshot is not controlled by GarageKey: %+v", snapshot.OwnerReferences)
				}
			}
			var body garage.ImportKeyRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode ImportKey: %v", err)
			}
			committed = true
			_ = json.NewEncoder(w).Encode(garage.Key{
				AccessKeyID: body.AccessKeyID, SecretAccessKey: body.SecretAccessKey,
				Name: body.Name, Buckets: []garage.KeyBucket{},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	garageClient := garage.NewClient(server.URL, "token")

	if _, _, err := reconciler.importKey(t.Context(), key, garageClient, key.Name); err != nil {
		t.Fatal(err)
	}
	if importCalls != 1 {
		t.Fatalf("ImportKey calls = %d, want 1", importCalls)
	}

	// Simulate a crash before status/output Secret persistence followed by a
	// source mutation that preserves the ID but replaces the credential. Recovery
	// must use the exact material that Garage committed, not the mutable source.
	freshSource := &corev1.Secret{}
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(source), freshSource); err != nil {
		t.Fatal(err)
	}
	freshSource.Data[defaultSecretAccessKeyKey] = []byte("changed-secret-material")
	if err := kubeClient.Update(t.Context(), freshSource); err != nil {
		t.Fatal(err)
	}
	freshKey := &garagev1beta1.GarageKey{}
	if err := kubeClient.Get(t.Context(), client.ObjectKeyFromObject(key), freshKey); err != nil {
		t.Fatal(err)
	}
	got, recoveredSecret, err := reconciler.importKey(t.Context(), freshKey, garageClient, key.Name)
	if err != nil {
		t.Fatalf("recover committed import after source mutation: %v", err)
	}
	if got.AccessKeyID != keyID || recoveredSecret != keySecret {
		t.Fatalf("recovered key = %+v, secret = %q; want original committed material", got, recoveredSecret)
	}
	if importCalls != 1 {
		t.Fatalf("ImportKey calls after changed source = %d, want 1", importCalls)
	}
	if freshKey.Status.AccessKeyID != "" {
		t.Fatalf("test unexpectedly persisted status access key ID %q", freshKey.Status.AccessKeyID)
	}
	output := &corev1.Secret{}
	if err := kubeClient.Get(t.Context(), client.ObjectKey{Name: key.Name, Namespace: namespace}, output); err == nil {
		t.Fatalf("test unexpectedly persisted output Secret")
	}

	// Finalization uses the durable snapshot even if both the mutable source and
	// the compatibility identity annotation disappear.
	if err := kubeClient.Delete(t.Context(), freshSource); err != nil {
		t.Fatal(err)
	}
	freshKey.Annotations = nil
	resolved, err := reconciler.garageKeyFinalizationID(t.Context(), freshKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != keyID {
		t.Fatalf("finalization access key ID = %q, want %q", resolved, keyID)
	}
}

func TestInlineImportRecoversCommittedKeyBeforeStatusWrite(t *testing.T) {
	const (
		keyID     = "GKinlinecommitted"
		keySecret = "inline-secret-material"
	)
	importCalls := 0
	getCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v2/GetKeyInfo":
			getCalls++
			if request.URL.Query().Get("id") != keyID || request.URL.Query().Get("showSecretKey") != "true" {
				t.Errorf("GetKeyInfo query = %s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(garage.Key{AccessKeyID: keyID, SecretAccessKey: keySecret, Name: "inline"})
		case "/v2/ImportKey":
			importCalls++
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	key := &garagev1beta1.GarageKey{
		ObjectMeta: metav1.ObjectMeta{Name: "inline", Namespace: "tenant"},
		Spec: garagev1beta1.GarageKeySpec{ImportKey: &garagev1beta1.ImportKeyConfig{
			AccessKeyID: keyID, SecretAccessKey: keySecret,
		}},
	}

	got, secret, err := (&GarageKeyReconciler{}).importKey(t.Context(), key, garage.NewClient(server.URL, "token"), key.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessKeyID != keyID || secret != keySecret || getCalls != 1 || importCalls != 0 {
		t.Fatalf("result=%+v secret=%q getCalls=%d importCalls=%d", got, secret, getCalls, importCalls)
	}
}

func TestInlineImportPinsIdentityBeforeRemoteWrite(t *testing.T) {
	const (
		keyID     = "GKinlinepinned"
		keySecret = "inline-pinned-secret"
	)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key := &garagev1beta1.GarageKey{
		ObjectMeta: metav1.ObjectMeta{Name: "inline-pinned", Namespace: "tenant", UID: "inline-key-uid"},
		Spec: garagev1beta1.GarageKeySpec{ImportKey: &garagev1beta1.ImportKeyConfig{
			AccessKeyID: keyID, SecretAccessKey: keySecret,
		}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(key).Build()
	reconciler := &GarageKeyReconciler{Client: kubeClient, Scheme: scheme}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v2/GetKeyInfo":
			w.WriteHeader(http.StatusNotFound)
		case "/v2/ImportKey":
			persisted := &garagev1beta1.GarageKey{}
			if err := kubeClient.Get(request.Context(), client.ObjectKeyFromObject(key), persisted); err != nil {
				t.Errorf("get GarageKey during ImportKey: %v", err)
			} else if got := persisted.Annotations[keyResolvedImportIDAnnotation]; got != keyID {
				t.Errorf("resolved identity at remote write = %q, want %q", got, keyID)
			}
			_ = json.NewEncoder(w).Encode(garage.Key{AccessKeyID: keyID, Name: key.Name})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	if _, _, err := reconciler.importKey(t.Context(), key, garage.NewClient(server.URL, "token"), key.Name); err != nil {
		t.Fatal(err)
	}
}
