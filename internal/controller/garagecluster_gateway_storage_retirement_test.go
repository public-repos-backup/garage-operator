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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEdgeGatewayStatefulSetStorageRetiredWaitsForOldPodsAndClaims(t *testing.T) {
	const namespace = "gateway-retirement"
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "garage-gateway", Namespace: namespace, UID: types.UID("old-sts-uid")},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](0),
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: metadataVolName},
			}},
		},
	}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "garage-gateway-0", Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.SchemeGroupVersion.String(), Kind: kindStatefulSet,
			Name: statefulSet.Name, UID: statefulSet.UID, Controller: ptr.To(true),
		}},
	}}
	oldClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: metadataVolName + "-" + statefulSet.Name + "-0", Namespace: namespace,
	}}
	unrelatedClaim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: metadataVolName + "-another-gateway-0", Namespace: namespace,
	}}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oldPod, oldClaim, unrelatedClaim).Build()
	reconciler := &GarageClusterReconciler{Client: client}

	clear, reason, err := reconciler.edgeGatewayStatefulSetStorageRetired(context.Background(), statefulSet)
	if err != nil || clear || !strings.Contains(reason, oldPod.Name) {
		t.Fatalf("old Pod was not a hard recreation fence: clear=%v reason=%q err=%v", clear, reason, err)
	}
	if err := client.Delete(context.Background(), oldPod); err != nil {
		t.Fatal(err)
	}
	clear, reason, err = reconciler.edgeGatewayStatefulSetStorageRetired(context.Background(), statefulSet)
	if err != nil || clear || !strings.Contains(reason, oldClaim.Name) {
		t.Fatalf("old PVC was not a hard recreation fence: clear=%v reason=%q err=%v", clear, reason, err)
	}
	if err := client.Delete(context.Background(), oldClaim); err != nil {
		t.Fatal(err)
	}
	clear, reason, err = reconciler.edgeGatewayStatefulSetStorageRetired(context.Background(), statefulSet)
	if err != nil || !clear || reason != "" {
		t.Fatalf("retired old storage did not release recreation: clear=%v reason=%q err=%v", clear, reason, err)
	}
}
