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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const (
	mhName     = "existing"
	mhNS       = testGarageValue
	mhEndpoint = "http://garage.garage.svc:3903"
	mhSecret   = "ext-admin"
)

func connectToTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	if err := garagev1beta2.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme v1beta2: %v", err)
	}
	return s
}

// A management handle must dial the external adminApiEndpoint, not the managed
// Service FQDN, using the token from connectTo.adminTokenSecretRef (#269).
func TestGetGarageClient_ManagementHandle_AdminEndpoint(t *testing.T) {
	s := connectToTestScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: mhSecret, Namespace: mhNS},
		Data:       map[string][]byte{DefaultAdminTokenKey: []byte("prefix.secret")},
	}
	handle := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: mhName, Namespace: mhNS},
		Spec: garagev1beta2.GarageClusterSpec{
			ConnectTo: &garagev1beta2.ConnectToConfig{
				AdminAPIEndpoint:    mhEndpoint,
				AdminTokenSecretRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: mhSecret}, Key: DefaultAdminTokenKey},
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

	c, err := GetGarageClient(context.Background(), fc, handle, "cluster.local")
	if err != nil {
		t.Fatalf("GetGarageClient: %v", err)
	}
	if got := c.BaseURL(); got != mhEndpoint {
		t.Errorf("endpoint = %q, want external adminApiEndpoint (not svcFQDN)", got)
	}
}

// A handle whose token secret is missing must fail with a clear error rather
// than silently building a tokenless client.
func TestGetGarageClient_ManagementHandle_MissingSecret(t *testing.T) {
	s := connectToTestScheme(t)
	handle := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: mhName, Namespace: mhNS},
		Spec: garagev1beta2.GarageClusterSpec{
			ConnectTo: &garagev1beta2.ConnectToConfig{
				AdminAPIEndpoint:    mhEndpoint,
				AdminTokenSecretRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: DefaultAdminTokenKey},
			},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(s).Build()

	if _, err := GetGarageClient(context.Background(), fc, handle, "cluster.local"); err == nil {
		t.Fatal("GetGarageClient accepted a handle with a missing token secret, want error")
	}
}

func TestRecursiveClusterRefResolutionValidatesEveryHop(t *testing.T) {
	for _, test := range []struct {
		name       string
		ref        garagev1beta2.ClusterReference
		deleting   bool
		wantPhrase string
	}{
		{
			name: "remote kubeconfig",
			ref: garagev1beta2.ClusterReference{Name: "storage", KubeConfigSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "remote"},
			}},
			wantPhrase: "kubeConfigSecretRef is not supported",
		},
		{name: "malformed name", ref: garagev1beta2.ClusterReference{Name: "../storage"}, wantPhrase: "is invalid"},
		{name: "cross namespace", ref: garagev1beta2.ClusterReference{Name: "storage", Namespace: "other"}, wantPhrase: "must be empty or match"},
		{name: "deleting target", ref: garagev1beta2.ClusterReference{Name: "storage"}, deleting: true, wantPhrase: "is deleting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := connectToTestScheme(t)
			root := &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: mhNS},
				Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
					ClusterRef: &garagev1beta2.ClusterReference{Name: "nested"},
				}},
			}
			nested := &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "nested", Namespace: mhNS},
				Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
					ClusterRef: &test.ref,
				}},
			}
			target := &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: mhNS},
				Spec:       garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{}},
			}
			if test.deleting {
				now := metav1.Now()
				target.DeletionTimestamp = &now
				target.Finalizers = []string{"test"}
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nested, target).Build()

			if _, err := GetGarageClient(t.Context(), kubeClient, root, "cluster.local"); err == nil || !strings.Contains(err.Error(), test.wantPhrase) {
				t.Fatalf("GetGarageClient error = %v, want %q", err, test.wantPhrase)
			}
			if _, err := resolveGarageLayoutOwner(t.Context(), kubeClient, root); err == nil || !strings.Contains(err.Error(), test.wantPhrase) {
				t.Fatalf("resolveGarageLayoutOwner error = %v, want %q", err, test.wantPhrase)
			}
		})
	}
}

func TestCleanupClusterRefResolutionAllowsDeletingTargets(t *testing.T) {
	scheme := connectToTestScheme(t)
	now := metav1.Now()
	root := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deleting-root", Namespace: mhNS, DeletionTimestamp: &now,
			Finalizers: []string{"test"},
		},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			AdminAPIEndpoint: mhEndpoint,
			AdminTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: mhSecret},
				Key:                  DefaultAdminTokenKey,
			},
		}},
	}
	nested := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "nested", Namespace: mhNS},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: root.Name},
		}},
	}
	leaf := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "leaf", Namespace: mhNS},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: nested.Name},
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: mhSecret, Namespace: mhNS},
		Data:       map[string][]byte{DefaultAdminTokenKey: []byte("prefix.secret")},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, nested, secret).Build()

	if _, err := GetGarageClient(t.Context(), kubeClient, leaf, "cluster.local"); err == nil || !strings.Contains(err.Error(), "is deleting") {
		t.Fatalf("normal GetGarageClient error = %v, want deleting-target rejection", err)
	}
	got, err := GetGarageClientForCleanup(t.Context(), kubeClient, leaf, "cluster.local")
	if err != nil {
		t.Fatalf("cleanup GetGarageClient: %v", err)
	}
	if got.BaseURL() != mhEndpoint {
		t.Fatalf("cleanup endpoint = %q, want %q", got.BaseURL(), mhEndpoint)
	}
	if _, err := resolveGarageLayoutOwner(t.Context(), kubeClient, leaf); err == nil || !strings.Contains(err.Error(), "is deleting") {
		t.Fatalf("normal layout-owner resolution error = %v, want deleting-target rejection", err)
	}
	owner, err := resolveGarageLayoutOwnerForCleanup(t.Context(), kubeClient, leaf)
	if err != nil {
		t.Fatalf("cleanup layout-owner resolution: %v", err)
	}
	if owner.Name != root.Name || owner.UID != root.UID {
		t.Fatalf("cleanup layout owner = %s/%s UID %s, want root %s UID %s", owner.Namespace, owner.Name, owner.UID, root.Name, root.UID)
	}
}
