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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const convergedReason = "Converged"

// scaleFreezeCluster is a plain storage cluster whose StorageRolloutReady
// condition is unconverged — the ordinary state while a node is still coming up.
func scaleFreezeCluster(replicas int32) *GarageCluster {
	return &GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-cluster", Namespace: "ns", Generation: 2},
		Spec: GarageClusterSpec{
			Zone:        "us-test",
			Replication: &ReplicationConfig{Factor: 1},
			Storage: &StorageSpec{
				Replicas: replicas,
				Metadata: &VolumeConfig{Type: VolumeTypeEmptyDir},
				Data:     &VolumeConfig{Type: VolumeTypeEmptyDir},
			},
		},
		Status: GarageClusterStatus{
			Conditions: []metav1.Condition{{
				Type:               "StorageRolloutReady",
				Status:             metav1.ConditionFalse,
				Reason:             "Waiting",
				ObservedGeneration: 2,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// The spec freeze must key on a real replacement transaction, not on the
// StorageRolloutReady convergence signal.
//
// That condition reads False/Waiting while a brand-new node is still discovering
// its identity, and reads stale whenever its observedGeneration merely trails a
// spec edit — with status.storageRollout nil throughout. Nothing owns an actor in
// either state. Freezing on them denies safe edits, and breaks GitOps outright:
// Argo/Flux re-apply continuously, so every settling period becomes a sync error
// the user cannot act on.
//
// Note this is only about the freeze. Genuinely unsafe topology changes keep
// their own guards — removing a member that holds positive capacity is a drain,
// and that separately requires StorageRolloutReady=True first.
func TestRolloutFreezeIgnoresTheConvergenceCondition(t *testing.T) {
	cluster := scaleFreezeCluster(3)
	if garageClusterNodeLocalPoolRolloutActive(cluster) {
		t.Fatal("an unconverged StorageRolloutReady condition must not read as an in-flight replacement")
	}

	cluster.Status.Conditions[0].Status = metav1.ConditionTrue
	cluster.Status.Conditions[0].Reason = convergedReason
	cluster.Status.Conditions[0].ObservedGeneration = 1 // trails Generation 2
	if garageClusterNodeLocalPoolRolloutActive(cluster) {
		t.Fatal("a condition whose observedGeneration merely trails must not freeze the spec")
	}

	cluster.Status.StorageRollout = &StorageRolloutStatus{GarageNodeName: "auto-cluster-storage-1"}
	if !garageClusterNodeLocalPoolRolloutActive(cluster) {
		t.Fatal("a real replacement transaction must freeze the spec")
	}
}

// Adding a member while the cluster is still settling is safe — no actor is being
// replaced and nothing is drained — so admission must accept it.
func TestValidateUpdate_ScaleUpAllowedWhileRolloutConditionUnconverged(t *testing.T) {
	validator := &GarageClusterValidator{}
	old := scaleFreezeCluster(2)
	newer := old.DeepCopy()
	newer.Spec.Storage.Replicas = 3
	newer.Generation = 3

	if _, err := validator.ValidateUpdate(context.Background(), old, newer); err != nil {
		t.Fatalf("scale-up must be accepted when no replacement transaction owns an actor: %v", err)
	}
}

// When a transaction really is in flight, the freeze still applies — and names
// the actor it owns rather than a recovery annotation nobody set.
func TestValidateUpdate_ScaleDownFrozenDuringRealRolloutTransaction(t *testing.T) {
	validator := &GarageClusterValidator{}
	old := scaleFreezeCluster(3)
	old.Status.StorageRollout = &StorageRolloutStatus{
		GarageNodeName: "auto-cluster-storage-1",
		GarageNodeUID:  "node-uid",
		GarageNodeID:   "abcdef0123456789",
		WorkloadUID:    "sts-uid",
		PreviousPodUID: "pod-uid",
	}
	newer := old.DeepCopy()
	newer.Spec.Storage.Replicas = 2
	newer.Generation = 3

	_, err := validator.ValidateUpdate(context.Background(), old, newer)
	if err == nil {
		t.Fatal("membership must stay frozen while a replacement transaction is in flight")
	}
	if !strings.Contains(err.Error(), "auto-cluster-storage-1") {
		t.Fatalf("the denial must name the actor that froze the spec, got: %v", err)
	}
	if strings.Contains(err.Error(), "recover-storage-rollout") {
		t.Fatalf("the denial must not blame an annotation that is not set: %v", err)
	}
}
