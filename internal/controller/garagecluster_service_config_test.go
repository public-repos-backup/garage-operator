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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestReconcileAPIServiceAppliesAndUpdatesServiceConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "garage", Namespace: "storage", UID: "cluster-uid"},
		Spec: garagev1beta2.GarageClusterSpec{Network: garagev1beta2.NetworkConfig{
			Service: &garagev1beta2.ServiceConfig{
				Type:                     corev1.ServiceTypeLoadBalancer,
				LoadBalancerIP:           "192.0.2.10",
				LoadBalancerSourceRanges: []string{"198.51.100.0/24"},
				ExternalTrafficPolicy:    corev1.ServiceExternalTrafficPolicyLocal,
			},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &GarageClusterReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	assertService := func(ip, sourceRange string, policy corev1.ServiceExternalTrafficPolicy) {
		t.Helper()
		service := &corev1.Service{}
		if err := c.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, service); err != nil {
			t.Fatal(err)
		}
		if service.Spec.LoadBalancerIP != ip || len(service.Spec.LoadBalancerSourceRanges) != 1 ||
			service.Spec.LoadBalancerSourceRanges[0] != sourceRange || service.Spec.ExternalTrafficPolicy != policy {
			t.Fatalf("managed Service config = %+v", service.Spec)
		}
	}

	if err := r.reconcileAPIService(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	assertService("192.0.2.10", "198.51.100.0/24", corev1.ServiceExternalTrafficPolicyLocal)

	cluster.Spec.Network.Service.LoadBalancerIP = "192.0.2.11"
	cluster.Spec.Network.Service.LoadBalancerSourceRanges = []string{"203.0.113.0/24"}
	cluster.Spec.Network.Service.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	if err := r.reconcileAPIService(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	assertService("192.0.2.11", "203.0.113.0/24", corev1.ServiceExternalTrafficPolicyCluster)
}
