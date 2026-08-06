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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestGatewayConfigMountAlwaysNamesPublishedSharedRevision(t *testing.T) {
	for _, sensitive := range []bool{false, true} {
		t.Run(map[bool]string{false: "ConfigMap", true: "Secret"}[sensitive], func(t *testing.T) {
			cluster := &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: testSiteName, Namespace: testGarageValue},
				Spec: garagev1beta2.GarageClusterSpec{
					Storage: &garagev1beta2.StorageSpec{Replicas: 1},
					Gateway: &garagev1beta2.GatewaySpec{Replicas: 1, RPCPublicAddr: "gateway.example:3901"},
				},
			}
			if sensitive {
				cluster.Spec.Discovery = &garagev1beta2.DiscoveryConfig{Consul: &garagev1beta2.ConsulDiscoveryConfig{
					Enabled: ptr.To(true), TokenSecretRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "consul"}, Key: remoteAdminTokenKey,
					},
				}}
			}

			volumes, _ := buildGatewayVolumesAndMounts(cluster, strings.Repeat("a", 64))
			if len(volumes) == 0 || volumes[0].Name != configVolumeName {
				t.Fatalf("gateway config volume missing: %+v", volumes)
			}
			want := garageConfigRevisionName("site-config", strings.Repeat("a", 64))
			if sensitive {
				if volumes[0].Secret == nil || volumes[0].Secret.SecretName != want {
					t.Fatalf("sensitive gateway mounted %+v, want shared Secret %q", volumes[0].VolumeSource, want)
				}
			} else if volumes[0].ConfigMap == nil || volumes[0].ConfigMap.Name != want {
				t.Fatalf("gateway mounted %+v, want shared ConfigMap %q", volumes[0].VolumeSource, want)
			}
			if strings.Contains(want, "gateway-config") {
				t.Fatalf("test expected a shared config revision, got %q", want)
			}
		})
	}
}
