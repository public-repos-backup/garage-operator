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

package v1beta2

import (
	"bytes"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeLocalPoolWireContract(t *testing.T) {
	capacity := resource.MustParse("700Gi")
	cluster := &GarageCluster{Spec: GarageClusterSpec{Storage: &StorageSpec{
		NodeLocalPools: []NodeLocalPoolSpec{{
			Name:     testNodeLocalPoolName,
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"storage.example.com/garage-pool": testNodeLocalPoolName}},
			Capacity: &capacity,
			Metadata: &HostPathVolumeConfig{HostPath: testMetadataHostPath, HostPathType: corev1.HostPathDirectory},
			Data:     &HostPathVolumeConfig{HostPath: "/var/lib/garage/data"},
		}},
	}}}

	raw, err := json.Marshal(cluster)
	if err != nil {
		t.Fatalf("marshal GarageCluster: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"nodeLocalPools":[`)) {
		t.Fatalf("node-local pool API did not marshal as spec.storage.nodeLocalPools: %s", raw)
	}
	for _, obsolete := range [][]byte{[]byte(`"pools"`), []byte(`"nodePools"`), []byte(`"storagePools"`)} {
		if bytes.Contains(raw, obsolete) {
			t.Fatalf("obsolete node-local pool wire field %s was emitted: %s", obsolete, raw)
		}
	}
}

func TestUnreleasedNodeLocalPoolAliasesDoNotPopulateFinalAPI(t *testing.T) {
	for _, field := range []string{"pools", "nodePools", "storagePools"} {
		raw := []byte(`{"spec":{"storage":{"replicas":0,"` + field + `":[{"name":"local"}]}}}`)
		var cluster GarageCluster
		if err := json.Unmarshal(raw, &cluster); err != nil {
			t.Fatalf("unmarshal obsolete %s field: %v", field, err)
		}
		if cluster.Spec.Storage == nil {
			t.Fatalf("storage disappeared while decoding obsolete %s field", field)
		}
		if len(cluster.Spec.Storage.NodeLocalPools) != 0 {
			t.Fatalf("obsolete %s field populated nodeLocalPools: %#v", field, cluster.Spec.Storage.NodeLocalPools)
		}
	}

	raw := []byte(`{"spec":{"storage":{"replicas":0,"workload":"DaemonSet"}}}`)
	var cluster GarageCluster
	if err := json.Unmarshal(raw, &cluster); err != nil {
		t.Fatalf("unmarshal obsolete workload discriminator: %v", err)
	}
	if len(cluster.Spec.Storage.NodeLocalPools) != 0 {
		t.Fatalf("obsolete workload discriminator populated nodeLocalPools: %#v", cluster.Spec.Storage.NodeLocalPools)
	}
}

func TestNodeLocalPoolAndRolloutDeepCopyDoNotAlias(t *testing.T) {
	capacity := resource.MustParse("700Gi")
	pathCapacity := resource.MustParse("500Gi")
	original := &GarageCluster{
		Spec: GarageClusterSpec{Storage: &StorageSpec{
			NodeLocalPools: []NodeLocalPoolSpec{{
				Name:     testNodeLocalPoolName,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"storage.example.com/garage-pool": testNodeLocalPoolName}},
				Capacity: &capacity,
				Metadata: &HostPathVolumeConfig{HostPath: testMetadataHostPath, HostPathType: corev1.HostPathDirectory},
				DataPaths: []NodeLocalPoolDataPath{{
					Path: "/data/fast", HostPath: "/mnt/fast/garage", Capacity: &pathCapacity,
				}},
				PodTemplate: &NodeLocalPoolPodTemplate{PodLabels: map[string]string{"storage.example.com/media": "nvme"}},
			}},
		}},
		Status: GarageClusterStatus{StorageRollout: &StorageRolloutStatus{
			RetiredWorkloadUIDs: []string{"workload-a"},
			PersistentVolumeClaims: []StorageRolloutPersistentVolumeClaimStatus{{
				Name: "metadata-node-a-0", UID: "claim-a",
			}},
		}},
	}

	copy := original.DeepCopy()
	copy.Spec.Storage.NodeLocalPools[0].Selector.MatchLabels["storage.example.com/garage-pool"] = testChangedValue
	copy.Spec.Storage.NodeLocalPools[0].Capacity.Set(1)
	copy.Spec.Storage.NodeLocalPools[0].Metadata.HostPath = "/changed"
	copy.Spec.Storage.NodeLocalPools[0].DataPaths[0].Capacity.Set(2)
	copy.Spec.Storage.NodeLocalPools[0].PodTemplate.PodLabels["storage.example.com/media"] = testChangedValue
	copy.Status.StorageRollout.RetiredWorkloadUIDs[0] = "workload-b"
	copy.Status.StorageRollout.PersistentVolumeClaims[0].UID = "claim-b"

	pool := &original.Spec.Storage.NodeLocalPools[0]
	if got := pool.Selector.MatchLabels["storage.example.com/garage-pool"]; got != testNodeLocalPoolName {
		t.Fatalf("selector map aliased by DeepCopy: %q", got)
	}
	if got := pool.Capacity.String(); got != "700Gi" {
		t.Fatalf("pool capacity aliased by DeepCopy: %q", got)
	}
	if pool.Metadata.HostPath != testMetadataHostPath {
		t.Fatalf("metadata config aliased by DeepCopy: %q", pool.Metadata.HostPath)
	}
	if got := pool.DataPaths[0].Capacity.String(); got != "500Gi" {
		t.Fatalf("data-path capacity aliased by DeepCopy: %q", got)
	}
	if got := pool.PodTemplate.PodLabels["storage.example.com/media"]; got != "nvme" {
		t.Fatalf("pod labels aliased by DeepCopy: %q", got)
	}
	if got := original.Status.StorageRollout.RetiredWorkloadUIDs[0]; got != "workload-a" {
		t.Fatalf("retired workload identities aliased by DeepCopy: %q", got)
	}
	if got := original.Status.StorageRollout.PersistentVolumeClaims[0].UID; got != "claim-a" {
		t.Fatalf("PVC identities aliased by DeepCopy: %q", got)
	}
}
