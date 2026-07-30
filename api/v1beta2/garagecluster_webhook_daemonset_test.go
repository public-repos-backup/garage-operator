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
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const testDaemonSetClusterName = "ds-storage"

// validDaemonSetCluster is the canonical valid storage-DaemonSet cluster:
// hostPath volumes and a uniform layout capacity. Peer discovery is handled
// by the operator's own Admin-API bootstrap nudge, not Garage's native
// kubernetes_discovery, so Discovery is left unset here.
func validDaemonSetCluster() *GarageCluster {
	capacity := resource.MustParse("500Gi")
	return &GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testDaemonSetClusterName, Namespace: testNamespace},
		Spec: GarageClusterSpec{
			Storage: &StorageSpec{
				Workload: WorkloadTypeDaemonSet,
				Capacity: &capacity,
				Metadata: &VolumeConfig{Type: VolumeTypeHostPath, HostPath: "/var/lib/garage/meta"},
				Data:     &VolumeConfig{Type: VolumeTypeHostPath, HostPath: "/var/lib/garage/data"},
			},
			Replication: &ReplicationConfig{Factor: 1},
		},
	}
}

func TestGarageClusterValidator_AcceptsDaemonSetStorage(t *testing.T) {
	cluster := validDaemonSetCluster()
	if _, err := cluster.validateGarageCluster(); err != nil {
		t.Fatalf("valid DaemonSet storage cluster rejected: %v", err)
	}
}

func TestGarageClusterValidator_DaemonSetVolumeRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*GarageCluster)
		wantErr string
	}{
		{
			name: "PVC metadata volume rejected",
			mutate: func(c *GarageCluster) {
				c.Spec.Storage.Metadata = &VolumeConfig{Size: ptr.To(resource.MustParse("1Gi"))}
			},
			wantErr: string(VolumeTypeHostPath),
		},
		{
			name:    "PVC data volume rejected",
			mutate:  func(c *GarageCluster) { c.Spec.Storage.Data = &VolumeConfig{Size: ptr.To(resource.MustParse("10Gi"))} },
			wantErr: string(VolumeTypeHostPath),
		},
		{
			name:    "EmptyDir data volume rejected",
			mutate:  func(c *GarageCluster) { c.Spec.Storage.Data = &VolumeConfig{Type: VolumeTypeEmptyDir} },
			wantErr: string(VolumeTypeHostPath),
		},
		{
			name:    "HostPath type without a path rejected",
			mutate:  func(c *GarageCluster) { c.Spec.Storage.Data.HostPath = "" },
			wantErr: "hostPath",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := validDaemonSetCluster()
			tc.mutate(cluster)
			_, err := cluster.validateGarageCluster()
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestGarageClusterValidator_RejectsHostPathOutsideDaemonSet(t *testing.T) {
	cluster := validDaemonSetCluster()
	cluster.Spec.Storage.Workload = "" // default StatefulSet
	if _, err := cluster.validateGarageCluster(); err == nil {
		t.Fatal("expected rejection: HostPath volumes are only supported with workload DaemonSet")
	}
}

func TestGarageClusterValidator_RejectsDaemonSetWithoutCapacity(t *testing.T) {
	cluster := validDaemonSetCluster()
	cluster.Spec.Storage.Capacity = nil
	_, err := cluster.validateGarageCluster()
	if err == nil {
		t.Fatal("expected rejection: DaemonSet storage needs spec.storage.capacity for the layout")
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("error should mention capacity, got: %v", err)
	}
}

func TestGarageClusterDefaulter_DoesNotForceK8sDiscoveryForDaemonSet(t *testing.T) {
	// Garage's native kubernetes_discovery needs RBAC (CRD patch + namespaced
	// CR management) the operator does not grant workload pods, so the
	// defaulter must leave it alone — peer discovery for DaemonSet pods goes
	// through the operator's own Admin-API bootstrap nudge instead.
	d := &GarageClusterDefaulter{}
	cluster := validDaemonSetCluster()
	cluster.Spec.Discovery = nil
	if err := d.Default(context.Background(), cluster); err != nil {
		t.Fatalf("defaulter: %v", err)
	}
	if cluster.Spec.Discovery != nil {
		t.Fatalf("DaemonSet workload must not auto-enable kubernetes discovery, got: %+v", cluster.Spec.Discovery)
	}
}

func TestGarageClusterValidator_AllowsDaemonSetWithK8sDiscoveryDisabled(t *testing.T) {
	cluster := validDaemonSetCluster()
	cluster.Spec.Discovery = &DiscoveryConfig{
		Kubernetes: &KubernetesDiscoveryConfig{Enabled: ptr.To(false)},
	}
	if _, err := cluster.validateGarageCluster(); err != nil {
		t.Fatalf("discovery.kubernetes.enabled is no longer required for DaemonSet: %v", err)
	}
}

// validStatefulSetCluster is the canonical valid StatefulSet-workload
// storage cluster, used as the "other side" of workload-immutability
// transition tests.
func validStatefulSetCluster() *GarageCluster {
	return &GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testDaemonSetClusterName, Namespace: testNamespace},
		Spec: GarageClusterSpec{
			Storage: &StorageSpec{
				Workload: WorkloadTypeStatefulSet,
				Replicas: 1,
				Metadata: &VolumeConfig{},
				Data:     &VolumeConfig{Type: VolumeTypeEmptyDir},
			},
			Replication: &ReplicationConfig{Factor: 1},
		},
	}
}

func TestGarageClusterValidator_RejectsWorkloadSwitch(t *testing.T) {
	v := &GarageClusterValidator{}

	t.Run("StatefulSet to DaemonSet", func(t *testing.T) {
		old := validStatefulSetCluster()
		newer := validDaemonSetCluster()
		newer.ObjectMeta = old.ObjectMeta

		_, err := v.ValidateUpdate(context.Background(), old, newer)
		if err == nil {
			t.Fatal("ValidateUpdate accepted StatefulSet->DaemonSet workload switch, want error")
		}
		if !strings.Contains(err.Error(), "spec.storage.workload is immutable") {
			t.Fatalf("error should mention workload immutability, got: %v", err)
		}
	})

	t.Run("DaemonSet to StatefulSet", func(t *testing.T) {
		old := validDaemonSetCluster()
		newer := validStatefulSetCluster()
		newer.ObjectMeta = old.ObjectMeta

		_, err := v.ValidateUpdate(context.Background(), old, newer)
		if err == nil {
			t.Fatal("ValidateUpdate accepted DaemonSet->StatefulSet workload switch, want error")
		}
		if !strings.Contains(err.Error(), "spec.storage.workload is immutable") {
			t.Fatalf("error should mention workload immutability, got: %v", err)
		}
	})

	t.Run("unset defaults to StatefulSet, not a transition", func(t *testing.T) {
		old := validStatefulSetCluster()
		old.Spec.Storage.Workload = "" // predates the field, or left at zero value

		same := old.DeepCopy()
		same.Spec.Storage.Workload = WorkloadTypeStatefulSet // explicit now

		if _, err := v.ValidateUpdate(context.Background(), old, same); err != nil {
			t.Fatalf("ValidateUpdate rejected ''->StatefulSet (not a real transition): %v", err)
		}
	})

	// Removing spec.storage entirely must also be rejected once it was a
	// DaemonSet — otherwise a two-step edit (nil out storage, then re-add it
	// with a different workload) would land oldObj.HasStorageTier() == false
	// on the second update and sail through the immutability check above.
	t.Run("removing storage entirely (DaemonSet) is rejected", func(t *testing.T) {
		old := validDaemonSetCluster()
		newer := old.DeepCopy()
		newer.Spec.Storage = nil
		newer.Spec.ConnectTo = &ConnectToConfig{ClusterRef: &ClusterReference{Name: storeClusterRefName}}

		_, err := v.ValidateUpdate(context.Background(), old, newer)
		if err == nil {
			t.Fatal("ValidateUpdate accepted removing spec.storage from a DaemonSet cluster, want error")
		}
		if !strings.Contains(err.Error(), "spec.storage cannot be removed") {
			t.Fatalf("error should mention storage removal, got: %v", err)
		}
	})

	// A StatefulSet-workload storage tier has no re-add-with-different-workload
	// bypass concern in the same way — this is its existing, supported teardown
	// path (e.g. converting to a management handle), so it stays allowed.
	t.Run("removing storage entirely (StatefulSet) is allowed", func(t *testing.T) {
		old := validStatefulSetCluster()
		newer := old.DeepCopy()
		newer.Spec.Storage = nil
		newer.Spec.ConnectTo = &ConnectToConfig{ClusterRef: &ClusterReference{Name: storeClusterRefName}}

		if _, err := v.ValidateUpdate(context.Background(), old, newer); err != nil {
			t.Fatalf("ValidateUpdate rejected removing spec.storage from a StatefulSet cluster: %v", err)
		}
	})

	t.Run("adding storage tier for the first time is not a removal", func(t *testing.T) {
		old := &GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: testDaemonSetClusterName, Namespace: testNamespace},
			Spec: GarageClusterSpec{
				ConnectTo:   &ConnectToConfig{ClusterRef: &ClusterReference{Name: storeClusterRefName}},
				Replication: &ReplicationConfig{Factor: 1},
			},
		}
		newer := validStatefulSetCluster()
		newer.ObjectMeta = old.ObjectMeta

		if _, err := v.ValidateUpdate(context.Background(), old, newer); err != nil {
			t.Fatalf("ValidateUpdate rejected adding a storage tier where none existed: %v", err)
		}
	})
}

func TestGarageClusterValidator_RejectsDaemonSetWithManualLayout(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*GarageCluster)
	}{
		{"storage.layoutPolicy Manual", func(c *GarageCluster) { c.Spec.Storage.LayoutPolicy = layoutPolicyManual }},
		{"cluster layoutPolicy Manual (effective)", func(c *GarageCluster) { c.Spec.LayoutPolicy = layoutPolicyManual }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster := validDaemonSetCluster()
			tc.mutate(cluster)
			_, err := cluster.validateGarageCluster()
			if err == nil {
				t.Fatal("expected rejection: DaemonSet workload requires operator-managed layout")
			}
			if !strings.Contains(err.Error(), "Manual") {
				t.Fatalf("error should mention the Manual layout conflict, got: %v", err)
			}
		})
	}
}
