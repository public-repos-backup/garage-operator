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
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const managedPVCTestControllerUsername = "system:serviceaccount:operator-system:garage-operator"

func managedPVCUpdateRequest(t *testing.T, oldClaim, newClaim *corev1.PersistentVolumeClaim, username string) admission.Request {
	t.Helper()
	oldRaw, err := json.Marshal(oldClaim)
	if err != nil {
		t.Fatal(err)
	}
	newRaw, err := json.Marshal(newClaim)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: persistentVolumeClaimResource},
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
		UserInfo:  authenticationv1.UserInfo{Username: username},
	}}
}

func TestManagedPVCFinalizerWebhookRejectsNamespaceUserRemoval(t *testing.T) {
	oldClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{managedPVCFinalizer}}}
	newClaim := oldClaim.DeepCopy()
	newClaim.Finalizers = nil
	response := (&managedPVCFinalizerValidator{controllerUsername: managedPVCTestControllerUsername}).Handle(
		t.Context(), managedPVCUpdateRequest(t, oldClaim, newClaim, "tenant-user"),
	)
	if response.Allowed {
		t.Fatal("namespace user removed the managed PVC replacement barrier")
	}
}

func TestManagedPVCFinalizerWebhookRejectsStorageRolloutBarrierRemoval(t *testing.T) {
	rolloutFinalizer := nodeLocalPoolActivationLabelDomain + storageRolloutPVCFinalizerPrefix + "0123456789abcdef"
	oldClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{rolloutFinalizer}}}
	newClaim := oldClaim.DeepCopy()
	newClaim.Finalizers = nil
	response := (&managedPVCFinalizerValidator{controllerUsername: managedPVCTestControllerUsername}).Handle(
		t.Context(), managedPVCUpdateRequest(t, oldClaim, newClaim, "tenant-user"),
	)
	if response.Allowed {
		t.Fatal("namespace user removed the storage-rollout PVC identity barrier")
	}
}

func TestManagedPVCFinalizerWebhookAllowsControllerRemoval(t *testing.T) {
	oldClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{managedPVCFinalizer}}}
	newClaim := oldClaim.DeepCopy()
	newClaim.Finalizers = nil
	response := (&managedPVCFinalizerValidator{controllerUsername: managedPVCTestControllerUsername}).Handle(
		t.Context(), managedPVCUpdateRequest(t, oldClaim, newClaim, managedPVCTestControllerUsername),
	)
	if !response.Allowed {
		t.Fatalf("controller service account could not remove replacement barrier: %#v", response.Result)
	}
}

func TestManagedPVCFinalizerWebhookAllowsUnrelatedPVCUpdate(t *testing.T) {
	oldClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{managedPVCFinalizer}}}
	newClaim := oldClaim.DeepCopy()
	newClaim.Labels = map[string]string{"updated": "true"}
	response := (&managedPVCFinalizerValidator{controllerUsername: managedPVCTestControllerUsername}).Handle(
		t.Context(), managedPVCUpdateRequest(t, oldClaim, newClaim, "tenant-user"),
	)
	if !response.Allowed {
		t.Fatalf("unrelated PVC update was rejected: %#v", response.Result)
	}
}
