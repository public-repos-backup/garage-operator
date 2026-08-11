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
	"context"
	"encoding/hex"
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
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
	"github.com/rajsinghtech/garage-operator/internal/garage"
)

func TestCreateOrAdoptDeterministic_TombstoneUsesDurableReplacementIdentity(t *testing.T) {
	const (
		namespace = "tenant"
		keyName   = "backup"
	)
	rpcSecret := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	decodedRPCSecret, err := hex.DecodeString(string(rpcSecret))
	if err != nil {
		t.Fatal(err)
	}
	baseID, _ := deriveKeyMaterial(decodedRPCSecret, namespace, keyName)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{Name: "garage", Namespace: namespace}}
	key := &garagev1beta1.GarageKey{ObjectMeta: metav1.ObjectMeta{Name: keyName, Namespace: namespace}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: managedRPCSecretName(cluster), Namespace: namespace},
		Data:       map[string][]byte{RPCSecretKey: rpcSecret},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(key, secret).Build()
	reconciler := &GarageKeyReconciler{Client: kubeClient, Scheme: scheme}

	var importIDs []string
	var replacementID string
	var replacementSecret string
	replacementImported := false
	createCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v2/ImportKey":
			var body garage.ImportKeyRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Errorf("decode ImportKey request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			importIDs = append(importIDs, body.AccessKeyID)
			if body.AccessKeyID == baseID {
				w.WriteHeader(http.StatusConflict)
				return
			}
			if replacementID == "" {
				replacementID = body.AccessKeyID
				replacementSecret = body.SecretAccessKey
			}
			if body.AccessKeyID != replacementID {
				t.Errorf("replacement identity changed: got %q, want %q", body.AccessKeyID, replacementID)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if replacementImported {
				w.WriteHeader(http.StatusConflict)
				return
			}
			replacementImported = true
			_ = json.NewEncoder(w).Encode(garage.Key{
				AccessKeyID: body.AccessKeyID, SecretAccessKey: body.SecretAccessKey,
				Name: body.Name, Buckets: []garage.KeyBucket{},
			})
		case "/v2/GetKeyInfo":
			id := req.URL.Query().Get("id")
			if id == baseID || id != replacementID || !replacementImported {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(garage.Key{
				AccessKeyID: replacementID, SecretAccessKey: replacementSecret, Name: keyName, Buckets: []garage.KeyBucket{},
			})
		case "/v2/CreateKey":
			createCalls++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	garageClient := garage.NewClient(server.URL, "token")

	// The tombstone detection may only persist a new identity. It must not make
	// a random CreateKey call in the same reconciliation.
	if _, _, err := reconciler.createOrAdoptDeterministic(context.Background(), key, cluster, garageClient, keyName); err == nil {
		t.Fatalf("tombstoned base identity unexpectedly succeeded: imports=%v base=%q", importIDs, baseID)
	}
	if createCalls != 0 {
		t.Fatalf("CreateKey calls = %d, want 0", createCalls)
	}
	persisted := &garagev1beta1.GarageKey{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(key), persisted); err != nil {
		t.Fatal(err)
	}
	nonce := persisted.Annotations[keyReplacementNonceAnnotation]
	if nonce == "" {
		t.Fatal("replacement nonce was not persisted before retry")
	}
	wantReplacementID, _ := deriveKeyMaterial(decodedRPCSecret, namespace, keyName+keyReplacementIdentitySeparator+nonce)

	// Simulate a crash after the exact replacement is imported but before status
	// is written. A retry from Kubernetes state must import/fetch the same ID.
	created, _, err := reconciler.createOrAdoptDeterministic(context.Background(), persisted, cluster, garageClient, keyName)
	if err != nil {
		t.Fatal(err)
	}
	if created.AccessKeyID != wantReplacementID {
		t.Fatalf("created access key ID = %q, want %q", created.AccessKeyID, wantReplacementID)
	}
	reloaded := &garagev1beta1.GarageKey{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(key), reloaded); err != nil {
		t.Fatal(err)
	}
	adopted, _, err := reconciler.createOrAdoptDeterministic(context.Background(), reloaded, cluster, garageClient, keyName)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.AccessKeyID != wantReplacementID {
		t.Fatalf("adopted access key ID = %q, want %q", adopted.AccessKeyID, wantReplacementID)
	}
	if len(importIDs) != 3 || importIDs[0] != baseID || importIDs[1] != wantReplacementID || importIDs[2] != wantReplacementID {
		t.Fatalf("ImportKey IDs = %v, want base then exact replacement twice", importIDs)
	}
	if createCalls != 0 {
		t.Fatalf("CreateKey calls = %d, want 0", createCalls)
	}
}

func TestCreateOrAdoptDeterministic_NameChangeAfterRemoteWriteKeepsIdentity(t *testing.T) {
	const (
		namespace  = "tenant"
		objectName = "backup-key"
	)
	rpcSecret := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	decodedRPCSecret, err := hex.DecodeString(string(rpcSecret))
	if err != nil {
		t.Fatal(err)
	}
	wantID, wantSecret := deriveKeyMaterial(decodedRPCSecret, namespace, objectName)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{Name: "garage", Namespace: namespace}}
	key := &garagev1beta1.GarageKey{
		ObjectMeta: metav1.ObjectMeta{Name: objectName, Namespace: namespace},
		Spec:       garagev1beta1.GarageKeySpec{Name: "first display name"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: managedRPCSecretName(cluster), Namespace: namespace},
		Data:       map[string][]byte{RPCSecretKey: rpcSecret},
	}
	reconciler := &GarageKeyReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Scheme: scheme}

	var importIDs []string
	var updatedName string
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v2/ImportKey":
			var body garage.ImportKeyRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Errorf("decode ImportKey: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			importIDs = append(importIDs, body.AccessKeyID)
			if created {
				w.WriteHeader(http.StatusConflict)
				return
			}
			created = true
			_ = json.NewEncoder(w).Encode(garage.Key{AccessKeyID: body.AccessKeyID, SecretAccessKey: body.SecretAccessKey, Name: body.Name})
		case "/v2/GetKeyInfo":
			_ = json.NewEncoder(w).Encode(garage.Key{AccessKeyID: wantID, SecretAccessKey: wantSecret, Name: "first display name"})
		case "/v2/UpdateKey":
			var body garage.UpdateKeyRequestBody
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Errorf("decode UpdateKey: %v", err)
			}
			updatedName = body.Name
			_ = json.NewEncoder(w).Encode(garage.Key{AccessKeyID: wantID, SecretAccessKey: wantSecret, Name: body.Name})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	garageClient := garage.NewClient(server.URL, "token")

	first, _, err := reconciler.createOrAdoptDeterministic(t.Context(), key, cluster, garageClient, key.Spec.Name)
	if err != nil || first.AccessKeyID != wantID {
		t.Fatalf("first create = %+v, err=%v, want ID %q", first, err, wantID)
	}
	// Simulate a crash before status.accessKeyId is persisted, followed by a
	// display-name edit. The retry must adopt the first exact credential.
	key.Spec.Name = "second display name"
	second, _, err := reconciler.createOrAdoptDeterministic(t.Context(), key, cluster, garageClient, key.Spec.Name)
	if err != nil || second.AccessKeyID != wantID {
		t.Fatalf("retry = %+v, err=%v, want ID %q", second, err, wantID)
	}
	if len(importIDs) != 2 || importIDs[0] != wantID || importIDs[1] != wantID {
		t.Fatalf("ImportKey IDs = %v, want stable ID %q", importIDs, wantID)
	}
	if updatedName != key.Spec.Name {
		t.Fatalf("updated display name = %q, want %q", updatedName, key.Spec.Name)
	}
}

func TestCreateOrAdoptDeterministic_AdoptsExactLegacyDisplayNameIdentity(t *testing.T) {
	const (
		namespace  = "tenant"
		objectName = "backup-key"
		legacyName = "legacy display name"
	)
	rpcSecret := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	decodedRPCSecret, err := hex.DecodeString(string(rpcSecret))
	if err != nil {
		t.Fatal(err)
	}
	legacyID, legacySecret := deriveKeyMaterial(decodedRPCSecret, namespace, legacyName)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{Name: "garage", Namespace: namespace}}
	key := &garagev1beta1.GarageKey{
		ObjectMeta: metav1.ObjectMeta{Name: objectName, Namespace: namespace},
		Spec:       garagev1beta1.GarageKeySpec{Name: legacyName},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: managedRPCSecretName(cluster), Namespace: namespace},
		Data:       map[string][]byte{RPCSecretKey: rpcSecret},
	}
	reconciler := &GarageKeyReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Scheme: scheme}
	var imports int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v2/GetKeyInfo":
			if req.URL.Query().Get("id") != legacyID {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(garage.Key{AccessKeyID: legacyID, SecretAccessKey: legacySecret, Name: legacyName})
		case "/v2/ImportKey":
			imports++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adopted, secretKey, err := reconciler.createOrAdoptDeterministic(t.Context(), key, cluster, garage.NewClient(server.URL, "token"), legacyName)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.AccessKeyID != legacyID || secretKey != legacySecret {
		t.Fatalf("adopted=%+v secret=%q, want exact legacy identity %q", adopted, secretKey, legacyID)
	}
	if imports != 0 {
		t.Fatalf("canonical ImportKey calls = %d, want zero after exact legacy adoption", imports)
	}
}
