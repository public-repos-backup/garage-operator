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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestObserveGarageClusterScaleKeepsAggregateMembersOutOfScale(t *testing.T) {
	const (
		namespace   = "scale-status"
		clusterName = "garage"
	)
	defaultLabels := map[string]string{
		labelCluster:      clusterName,
		labelTier:         tierStorage,
		labelStorageGroup: storageGroupDefault,
	}
	poolLabels := map[string]string{
		labelCluster:       clusterName,
		labelTier:          tierStorage,
		labelStorageGroup:  storageGroupNodeLocal,
		labelNodeLocalPool: testTagLocal,
	}
	gatewayLabels := map[string]string{
		labelCluster: clusterName,
		labelTier:    tierGateway,
	}

	newPod := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
	}
	terminating := newPod("default-terminating", defaultLabels)
	now := metav1.NewTime(time.Now())
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test.example/finalizer"}

	tests := []struct {
		name         string
		cluster      *garagev1beta2.GarageCluster
		pods         []client.Object
		wantReplicas int32
		wantSelector string
	}{
		{
			name: "default group excludes pool gateway and terminating pods",
			cluster: &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
				Spec: garagev1beta2.GarageClusterSpec{
					LayoutPolicy: LayoutPolicyAuto,
					Storage:      &garagev1beta2.StorageSpec{Replicas: 3},
					Gateway:      &garagev1beta2.GatewaySpec{Replicas: 1},
				},
			},
			pods: []client.Object{
				newPod("default-0", defaultLabels), newPod("default-1", defaultLabels), terminating,
				newPod("pool-0", poolLabels), newPod("gateway-0", gatewayLabels),
			},
			wantReplicas: 2,
			wantSelector: "garage.rajsingh.info/cluster=garage,garage.rajsingh.info/storage-group=default,garage.rajsingh.info/tier=storage",
		},
		{
			name: "pool only exposes an empty default group",
			cluster: &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
				Spec: garagev1beta2.GarageClusterSpec{
					LayoutPolicy: LayoutPolicyAuto,
					Storage: &garagev1beta2.StorageSpec{Replicas: 0, NodeLocalPools: []garagev1beta2.NodeLocalPoolSpec{{
						Name: testTagLocal,
					}}},
				},
			},
			pods:         []client.Object{newPod("pool-0", poolLabels), newPod("pool-1", poolLabels)},
			wantReplicas: 0,
			wantSelector: "garage.rajsingh.info/cluster=garage,garage.rajsingh.info/storage-group=default,garage.rajsingh.info/tier=storage",
		},
		{
			name: "manual mixed storage is explicitly not scalable",
			cluster: &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
				Spec: garagev1beta2.GarageClusterSpec{
					LayoutPolicy: LayoutPolicyAuto,
					Storage: &garagev1beta2.StorageSpec{
						Replicas:       0,
						LayoutPolicy:   LayoutPolicyManual,
						NodeLocalPools: []garagev1beta2.NodeLocalPoolSpec{{Name: testTagLocal}},
					},
				},
			},
			pods: []client.Object{
				newPod("manual-smb", defaultLabels), newPod("pool-0", poolLabels),
			},
			wantReplicas: 0,
			wantSelector: "garage.rajsingh.info/cluster=garage,garage.rajsingh.info/scale-target=disabled",
		},
		{
			name: "gateway only preserves v1beta1 gateway scale observation",
			cluster: &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
				Spec: garagev1beta2.GarageClusterSpec{
					LayoutPolicy: LayoutPolicyAuto,
					Gateway:      &garagev1beta2.GatewaySpec{Replicas: 2},
				},
			},
			pods:         []client.Object{newPod("gateway-0", gatewayLabels), newPod("gateway-1", gatewayLabels), newPod("pool-0", poolLabels)},
			wantReplicas: 2,
			wantSelector: "garage.rajsingh.info/cluster=garage,garage.rajsingh.info/tier=gateway",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			objects := make([]client.Object, len(tc.pods))
			copy(objects, tc.pods)
			reconciler := &GarageClusterReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
			}
			got, err := reconciler.observeGarageClusterScale(context.Background(), tc.cluster)
			if err != nil {
				t.Fatal(err)
			}
			if got.replicas != tc.wantReplicas || got.selector != tc.wantSelector {
				t.Fatalf("scale observation = %d %q, want %d %q", got.replicas, got.selector, tc.wantReplicas, tc.wantSelector)
			}
		})
	}
}
