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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNodesForKubernetesNodeMapsBothManagedStorageWorkloads(t *testing.T) {
	const (
		worker    = "worker-a"
		namespace = "garage"
		cluster   = "store"
		pool      = "local"
	)
	managedLabels := map[string]string{labelAppManagedBy: operatorName}
	statefulLabels := map[string]string{
		labelAppManagedBy: operatorName,
		labelGarageNode:   "store-storage-0",
	}
	daemonSetLabels := map[string]string{
		labelAppManagedBy:  operatorName,
		labelCluster:       cluster,
		labelNodeLocalPool: pool,
		labelStorageGroup:  storageGroupNodeLocal,
		labelTier:          tierStorage,
	}
	pod := func(name, nodeName string, labels map[string]string, ownerKind string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace, Labels: labels,
				OwnerReferences: []metav1.OwnerReference{{Kind: ownerKind, Controller: ptr.To(true)}},
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		pod("stateful", worker, statefulLabels, kindStatefulSet),
		pod("pool", worker, daemonSetLabels, daemonSetKind),
		pod("other-worker", "worker-b", statefulLabels, kindStatefulSet),
		pod("wrong-owner", worker, managedLabels, kindStatefulSet),
	).Build()
	reconciler := &GarageNodeReconciler{Client: client}

	requests := reconciler.nodesForKubernetesNode(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: worker},
	})
	got := make(map[types.NamespacedName]struct{}, len(requests))
	for _, request := range requests {
		got[request.NamespacedName] = struct{}{}
	}
	want := []types.NamespacedName{
		{Name: "store-storage-0", Namespace: namespace},
		{Name: nodeLocalPoolGarageNodeName(cluster, pool, worker), Namespace: namespace},
	}
	if len(got) != len(want) {
		t.Fatalf("mapped requests = %+v, want exactly %+v", requests, want)
	}
	for _, key := range want {
		if _, found := got[key]; !found {
			t.Errorf("mapped requests = %+v, missing %s", requests, key)
		}
	}
}
