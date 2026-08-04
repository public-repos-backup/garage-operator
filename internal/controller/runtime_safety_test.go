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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestGarageClusterRuntimeSafetyFailsBeforeManagedResourcePublication(t *testing.T) {
	tests := map[string]struct {
		mutate      func(*garagev1beta2.GarageCluster)
		wantMessage string
	}{
		"conflicting ownership": {
			mutate: func(cluster *garagev1beta2.GarageCluster) {
				cluster.Spec.ConnectTo = &garagev1beta2.ConnectToConfig{
					ClusterRef: &garagev1beta2.ClusterReference{Name: "legacy-remote"},
				}
			},
			wantMessage: "conflicting Garage ownership models",
		},
		"missing default volumes": {
			mutate: func(cluster *garagev1beta2.GarageCluster) {
				cluster.Spec.Storage.Replicas = 1
				cluster.Spec.Storage.Metadata = nil
			},
			wantMessage: "metadata and data are not both configured",
		},
		"released reserved environment": {
			mutate: func(cluster *garagev1beta2.GarageCluster) {
				cluster.Spec.Storage.Env = []corev1.EnvVar{{
					Name: envGarageRPCSecret, Value: strings.Repeat("a", 64),
				}}
			},
			wantMessage: "GARAGE_RPC_SECRET",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cluster := &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-runtime", Namespace: "runtime-safety", UID: types.UID("cluster-uid"),
					Finalizers: []string{garageClusterFinalizer},
				},
				Spec: garagev1beta2.GarageClusterSpec{
					Storage:     &garagev1beta2.StorageSpec{Replicas: 0},
					Replication: &garagev1beta2.ReplicationConfig{Factor: 1},
				},
			}
			tt.mutate(cluster)

			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := garagev1beta2.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := garagev1beta1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&garagev1beta2.GarageCluster{}).
				WithObjects(cluster).Build()
			reconciler := &GarageClusterReconciler{Client: kube, APIReader: kube, Scheme: scheme}

			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(cluster),
			}); err != nil {
				t.Fatalf("runtime fence returned an infrastructure error: %v", err)
			}

			stored := &garagev1beta2.GarageCluster{}
			if err := kube.Get(context.Background(), client.ObjectKeyFromObject(cluster), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.Phase != PhaseFailed {
				t.Fatalf("phase = %q, want %q", stored.Status.Phase, PhaseFailed)
			}
			ready := findStatusCondition(stored.Status.Conditions, PhaseReady)
			if ready == nil || !strings.Contains(ready.Message, tt.wantMessage) {
				t.Fatalf("Ready condition = %#v, want message containing %q", ready, tt.wantMessage)
			}

			assertEmpty := func(list client.ObjectList) {
				t.Helper()
				if err := kube.List(context.Background(), list, client.InNamespace(cluster.Namespace)); err != nil {
					t.Fatal(err)
				}
				switch objects := list.(type) {
				case *corev1.SecretList:
					if len(objects.Items) != 0 {
						t.Fatalf("runtime fence created Secrets: %+v", objects.Items)
					}
				case *corev1.ConfigMapList:
					if len(objects.Items) != 0 {
						t.Fatalf("runtime fence created ConfigMaps: %+v", objects.Items)
					}
				case *corev1.ServiceList:
					if len(objects.Items) != 0 {
						t.Fatalf("runtime fence created Services: %+v", objects.Items)
					}
				case *appsv1.StatefulSetList:
					if len(objects.Items) != 0 {
						t.Fatalf("runtime fence created StatefulSets: %+v", objects.Items)
					}
				case *appsv1.DaemonSetList:
					if len(objects.Items) != 0 {
						t.Fatalf("runtime fence created DaemonSets: %+v", objects.Items)
					}
				}
			}
			assertEmpty(&corev1.SecretList{})
			assertEmpty(&corev1.ConfigMapList{})
			assertEmpty(&corev1.ServiceList{})
			assertEmpty(&appsv1.StatefulSetList{})
			assertEmpty(&appsv1.DaemonSetList{})
		})
	}
}

func findStatusCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
