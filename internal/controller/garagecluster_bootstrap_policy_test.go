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
	"testing"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestClusterControllerBootstrapCoversEveryLocalStorageTopology(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cluster *garagev1beta2.GarageCluster
		want    bool
	}{
		{name: "nil cluster", want: false},
		{
			name: "auto storage",
			cluster: &garagev1beta2.GarageCluster{Spec: garagev1beta2.GarageClusterSpec{
				Storage: &garagev1beta2.StorageSpec{},
			}},
			want: true,
		},
		{
			name: "manual SMB storage",
			cluster: &garagev1beta2.GarageCluster{Spec: garagev1beta2.GarageClusterSpec{
				Storage: &garagev1beta2.StorageSpec{LayoutPolicy: LayoutPolicyManual},
			}},
			want: true,
		},
		{
			name: "manual edge gateway",
			cluster: &garagev1beta2.GarageCluster{Spec: garagev1beta2.GarageClusterSpec{
				Gateway: &garagev1beta2.GatewaySpec{}, LayoutPolicy: LayoutPolicyManual,
			}},
			want: false,
		},
		{
			name: "auto edge gateway",
			cluster: &garagev1beta2.GarageCluster{Spec: garagev1beta2.GarageClusterSpec{
				Gateway: &garagev1beta2.GatewaySpec{},
			}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clusterControllerOwnsBootstrap(tt.cluster); got != tt.want {
				t.Fatalf("clusterControllerOwnsBootstrap() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestExactOperatorTokenBridgeCoversEveryLocalGarageNodeTopology(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		node *garagev1beta1.GarageNode
		want bool
	}{
		{name: "absent node", want: false},
		{name: "local storage", node: &garagev1beta1.GarageNode{}, want: true},
		{
			name: "manual edge gateway",
			node: &garagev1beta1.GarageNode{Spec: garagev1beta1.GarageNodeSpec{Gateway: true}},
			want: true,
		},
		{
			name: "external role",
			node: &garagev1beta1.GarageNode{Spec: garagev1beta1.GarageNodeSpec{
				External: &garagev1beta1.ExternalNodeConfig{},
			}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := garageNodeCanUseExactOperatorTokenBridge(tt.node); got != tt.want {
				t.Fatalf("garageNodeCanUseExactOperatorTokenBridge() = %t, want %t", got, tt.want)
			}
		})
	}
}
