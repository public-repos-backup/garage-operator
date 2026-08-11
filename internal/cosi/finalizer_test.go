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

package cosi

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cosiv1alpha2 "sigs.k8s.io/container-object-storage-interface/client/apis/objectstorage/v1alpha2"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestBucketAccessCleanupHandsOffSharedFinalizerToUpstreamController(t *testing.T) {
	const (
		driver          = "garage.example.test"
		shadowNamespace = "garage-system"
		unrelated       = "example.test/unrelated"
	)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, cosiv1alpha2.AddToScheme,
		garagev1beta1.AddToScheme, garagev1beta2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	now := metav1.Now()
	access := &cosiv1alpha2.BucketAccess{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pending-access", Namespace: "tenant", UID: "pending-access-uid", DeletionTimestamp: &now,
			Finalizers:  []string{cosiv1alpha2.ProtectionFinalizer, GarageProtectionFinalizer, unrelated},
			Annotations: map[string]string{"example.test/preserve": "yes"},
		},
		Status: cosiv1alpha2.BucketAccessStatus{DriverName: driver},
	}
	kubeClient := newCOSIClientBuilder().WithScheme(scheme).WithObjects(access).Build()
	provisioner := NewProvisionerWithFactory(kubeClient, shadowNamespace,
		func(context.Context, client.Client, *garagev1beta2.GarageCluster) (GarageClient, error) {
			t.Fatal("pending cancellation must not construct a Garage client")
			return nil, nil
		})
	reconciler := &BucketAccessReconciler{
		Client: kubeClient, Scheme: scheme, DriverName: driver,
		Namespace: shadowNamespace, Provisioner: provisioner,
	}
	objectKey := types.NamespacedName{Name: access.Name, Namespace: access.Namespace}
	if _, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: objectKey}); err != nil {
		t.Fatal(err)
	}

	got := &cosiv1alpha2.BucketAccess{}
	if err := kubeClient.Get(t.Context(), objectKey, got); err != nil {
		t.Fatalf("BucketAccess disappeared before upstream deletion bookkeeping: %v", err)
	}
	if containsString(got.Finalizers, GarageProtectionFinalizer) {
		t.Fatalf("Garage finalizer remains after cleanup: %v", got.Finalizers)
	}
	if !containsString(got.Finalizers, cosiv1alpha2.ProtectionFinalizer) || !containsString(got.Finalizers, unrelated) {
		t.Fatalf("cleanup removed a finalizer owned by another controller: %v", got.Finalizers)
	}
	if _, ok := got.Annotations[cosiv1alpha2.SidecarCleanupFinishedAnnotation]; !ok {
		t.Fatalf("cleanup did not hand deletion back to the upstream COSI controller: %v", got.Annotations)
	}
	if got.Annotations["example.test/preserve"] != "yes" {
		t.Fatalf("cleanup overwrote unrelated annotations: %v", got.Annotations)
	}
}
