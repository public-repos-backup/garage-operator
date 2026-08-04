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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func managementHandleScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme,
		garagev1beta1.AddToScheme, garagev1beta2.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("AddToScheme: %v", err)
		}
	}
	return s
}

func newHandle(endpoint string) (*garagev1beta2.GarageCluster, *corev1.Secret) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: mhSecret, Namespace: mhNS},
		Data:       map[string][]byte{DefaultAdminTokenKey: []byte("prefix.secret")},
	}
	handle := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       mhName,
			Namespace:  mhNS,
			Finalizers: []string{garageClusterFinalizer},
		},
		Spec: garagev1beta2.GarageClusterSpec{
			ConnectTo: &garagev1beta2.ConnectToConfig{
				AdminAPIEndpoint:    endpoint,
				AdminTokenSecretRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: mhSecret}, Key: DefaultAdminTokenKey},
			},
		},
	}
	return handle, secret
}

// Reachable external Admin API → Phase Running, ManagementHandleReady True, and
// no managed workload created (no STS, ConfigMap, or Service).
func TestReconcileManagementHandle_ReachableSetsRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"layoutVersion":1,"nodes":[]}`))
	}))
	defer srv.Close()

	handle, secret := newHandle(srv.URL)
	s := managementHandleScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(handle, secret).
		WithStatusSubresource(&garagev1beta2.GarageCluster{}).
		Build()
	r := &GarageClusterReconciler{Client: fc, Scheme: s, ClusterDomain: testClusterDomain}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: mhName, Namespace: mhNS},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &garagev1beta2.GarageCluster{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: mhName, Namespace: mhNS}, got); err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	if got.Status.Phase != PhaseRunning {
		t.Errorf("Phase = %q, want %q", got.Status.Phase, PhaseRunning)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, garagev1beta1.ConditionManagementHandleReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ManagementHandleReady condition = %+v, want True", c)
	}

	// No managed workload should exist for a handle.
	sts := &appsv1.StatefulSet{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: mhName, Namespace: mhNS}, sts); err == nil {
		t.Error("a StatefulSet was created for a management handle, want none")
	}
	cm := &corev1.ConfigMap{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: mhName, Namespace: mhNS}, cm); err == nil {
		t.Error("a ConfigMap was created for a management handle, want none")
	}
	svc := &corev1.Service{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: mhName, Namespace: mhNS}, svc); err == nil {
		t.Error("a Service was created for a management handle, want none")
	}
	generatedRPC := &corev1.Secret{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: defaultRPCSecretName(handle), Namespace: mhNS}, generatedRPC); !apierrors.IsNotFound(err) {
		t.Errorf("generated RPC Secret lookup error = %v, want NotFound for an Admin-only handle", err)
	}
}

// Explicit RPC identity sources on a management handle are copied into the
// same immutable local snapshot contract used by managed workloads. This lets
// GarageKey derive deterministic credentials without following a mutable
// external Secret or provisioning any Garage workload for the handle.
func TestReconcileManagementHandle_PinsExplicitRPCIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"layoutVersion":1,"nodes":[]}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name      string
		configure func(*garagev1beta2.GarageCluster, *corev1.SecretKeySelector)
	}{
		{
			name: "network rpcSecretRef",
			configure: func(handle *garagev1beta2.GarageCluster, ref *corev1.SecretKeySelector) {
				handle.Spec.Network.RPCSecretRef = ref
			},
		},
		{
			name: "connectTo rpcSecretRef",
			configure: func(handle *garagev1beta2.GarageCluster, ref *corev1.SecretKeySelector) {
				handle.Spec.ConnectTo.RPCSecretRef = ref
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle, adminSecret := newHandle(srv.URL)
			handle.UID = types.UID("management-handle-uid")
			rpcSource := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "external-rpc", Namespace: mhNS},
				Data:       map[string][]byte{RPCSecretKey: []byte(strings.Repeat("a", 64))},
			}
			ref := &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: rpcSource.Name},
				Key:                  RPCSecretKey,
			}
			tc.configure(handle, ref)

			s := managementHandleScheme(t)
			fc := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(handle, adminSecret, rpcSource).
				WithStatusSubresource(&garagev1beta2.GarageCluster{}).
				Build()
			r := &GarageClusterReconciler{Client: fc, Scheme: s, ClusterDomain: testClusterDomain}

			if _, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: mhName, Namespace: mhNS},
			}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			gotHandle := &garagev1beta2.GarageCluster{}
			if err := fc.Get(context.Background(), types.NamespacedName{Name: mhName, Namespace: mhNS}, gotHandle); err != nil {
				t.Fatalf("get handle: %v", err)
			}
			if gotHandle.Status.Phase != PhaseRunning {
				t.Fatalf("Phase = %q, want %q", gotHandle.Status.Phase, PhaseRunning)
			}
			if gotHandle.Annotations[annotationRPCIdentitySHA256] == "" {
				t.Fatal("handle did not persist the pinned RPC identity fingerprint")
			}

			snapshot := &corev1.Secret{}
			if err := fc.Get(context.Background(), types.NamespacedName{Name: mhName + "-rpc-secret-snapshot", Namespace: mhNS}, snapshot); err != nil {
				t.Fatalf("get RPC identity snapshot: %v", err)
			}
			if snapshot.Immutable == nil || !*snapshot.Immutable {
				t.Fatal("RPC identity snapshot is not immutable")
			}
			if got, want := string(snapshot.Data[RPCSecretKey]), strings.Repeat("a", 64); got != want {
				t.Errorf("snapshot RPC identity = %q, want %q", got, want)
			}
			if !metav1.IsControlledBy(snapshot, gotHandle) {
				t.Errorf("snapshot ownerReferences = %+v, want handle controller owner", snapshot.OwnerReferences)
			}
		})
	}
}

// Unreachable external Admin API → Phase Pending, condition False.
func TestReconcileManagementHandle_UnreachableSetsPending(t *testing.T) {
	// Point at a closed server so the dial fails fast.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {}))
	url := srv.URL
	srv.Close()

	handle, secret := newHandle(url)
	s := managementHandleScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(handle, secret).
		WithStatusSubresource(&garagev1beta2.GarageCluster{}).
		Build()
	r := &GarageClusterReconciler{Client: fc, Scheme: s, ClusterDomain: testClusterDomain}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: mhName, Namespace: mhNS},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &garagev1beta2.GarageCluster{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: mhName, Namespace: mhNS}, got); err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	if got.Status.Phase != PhasePending {
		t.Errorf("Phase = %q, want %q", got.Status.Phase, PhasePending)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, garagev1beta1.ConditionManagementHandleReady); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ManagementHandleReady condition = %+v, want False", c)
	}
}

// A key's generated Secret on a management handle must expose the EXTERNAL S3
// endpoint (derived from connectTo.adminApiEndpoint host), not the nonexistent
// managed Service FQDN (#269).
func TestBuildSecretData_ManagementHandleEndpoint(t *testing.T) {
	handle := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: mhName, Namespace: mhNS},
		Spec: garagev1beta2.GarageClusterSpec{
			ConnectTo: &garagev1beta2.ConnectToConfig{AdminAPIEndpoint: mhEndpoint},
		},
	}
	key := &garagev1beta1.GarageKey{ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: mhNS}}
	key.Status.AccessKeyID = "GKtest"

	cfg := secretConfig{
		accessKeyIDKey:  "access_key_id",
		endpointKey:     "endpoint",
		hostKey:         "host",
		schemeKey:       "scheme",
		includeEndpoint: true,
	}

	s := managementHandleScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := &GarageKeyReconciler{Client: fc, Scheme: s, ClusterDomain: testClusterDomain}

	data := r.buildSecretData(context.Background(), cfg, key, handle, "sk")

	if got, want := string(data["endpoint"]), "http://garage.garage.svc:3900"; got != want {
		t.Errorf("endpoint = %q, want %q (external host, S3 port)", got, want)
	}
	if got, want := string(data["host"]), "garage.garage.svc:3900"; got != want {
		t.Errorf("host = %q, want %q", got, want)
	}
}
