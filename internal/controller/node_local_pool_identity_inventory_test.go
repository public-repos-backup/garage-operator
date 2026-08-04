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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const (
	retainedIdentityA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	retainedIdentityB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestCollectGarageNodeIDsValidatesRetainedNodeLocalPoolEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name        string
		annotations func(*testing.T, string, string, string) map[string]string
		generatedID string
		wantIDs     []string
		wantError   string
	}{
		{
			name: "claim only identity is included",
			annotations: func(_ *testing.T, claimKey, _ string, claimValue string) map[string]string {
				return map[string]string{claimKey: claimValue}
			},
			wantIDs: []string{retainedIdentityA},
		},
		{
			name: "equal claim and recovery pin are included once",
			annotations: func(_ *testing.T, claimKey, recoveryKey, claimValue string) map[string]string {
				return map[string]string{claimKey: claimValue, recoveryKey: retainedIdentityA}
			},
			generatedID: retainedIdentityA,
			wantIDs:     []string{retainedIdentityA},
		},
		{
			name: "declared pool recovery pin is included without a claim",
			annotations: func(_ *testing.T, _ string, recoveryKey, _ string) map[string]string {
				return map[string]string{recoveryKey: retainedIdentityA}
			},
			wantIDs: []string{retainedIdentityA},
		},
		{
			name: "claim and recovery mismatch is rejected",
			annotations: func(_ *testing.T, claimKey, recoveryKey, claimValue string) map[string]string {
				return map[string]string{claimKey: claimValue, recoveryKey: retainedIdentityB}
			},
			wantError: "disagrees with recovery identity",
		},
		{
			name: "generated GarageNode and Node claim mismatch is rejected",
			annotations: func(_ *testing.T, claimKey, _ string, claimValue string) map[string]string {
				return map[string]string{claimKey: claimValue}
			},
			generatedID: retainedIdentityB,
			wantError:   "generated GarageNode identity",
		},
		{
			name: "malformed claim is rejected",
			annotations: func(_ *testing.T, claimKey, _ string, _ string) map[string]string {
				return map[string]string{claimKey: "{not-json"}
			},
			wantError: "invalid retained HostPath claim",
		},
		{
			name: "unknown retained key is rejected",
			annotations: func(_ *testing.T, claimKey, _ string, _ string) map[string]string {
				prefix := claimKey[:len(claimKey)-16-len(nodeLocalPoolHostPathClaimSuffix)]
				return map[string]string{prefix + "unknown-evidence": retainedIdentityA}
			},
			wantError: "unrecognized retained node-local-pool annotation",
		},
		{
			name: "unknown recovery key is rejected",
			annotations: func(_ *testing.T, claimKey, _ string, _ string) map[string]string {
				cluster := nodeLocalPoolActivationTestCluster("identity-inventory", "a")
				key := nodeLocalPoolRecoveryAnnotationClusterPrefix(cluster) + strings.Repeat("f", 16) + "-node-id"
				if key == claimKey {
					panic("test recovery key unexpectedly equals claim key")
				}
				return map[string]string{key: retainedIdentityA}
			},
			wantError: "cannot be bound",
		},
		{
			name: "claim key must exactly bind its decoded pool",
			annotations: func(_ *testing.T, claimKey, _ string, claimValue string) map[string]string {
				cluster := nodeLocalPoolActivationTestCluster("identity-inventory", "a")
				wrongKey := nodeLocalPoolRetainedAnnotationClusterPrefix(cluster) +
					strings.Repeat("f", 16) + nodeLocalPoolHostPathClaimSuffix
				if wrongKey == claimKey {
					wrongKey = nodeLocalPoolRetainedAnnotationClusterPrefix(cluster) +
						strings.Repeat("e", 16) + nodeLocalPoolHostPathClaimSuffix
				}
				return map[string]string{wrongKey: claimValue}
			},
			wantError: "does not exactly bind decoded node-local pool",
		},
		{
			name: "one Kubernetes Node cannot retain identities for two pools",
			annotations: func(t *testing.T, claimKey, _ string, claimValue string) map[string]string {
				t.Helper()
				cluster := nodeLocalPoolActivationTestCluster("identity-inventory", "a")
				secondPool := cluster.Spec.Storage.NodeLocalPools[0].DeepCopy()
				secondPool.Name = "second"
				secondClaim, err := newNodeLocalPoolHostPathClaim(cluster, secondPool, retainedIdentityB)
				if err != nil {
					t.Fatal(err)
				}
				secondValue, err := encodeNodeLocalPoolHostPathClaim(secondClaim)
				if err != nil {
					t.Fatal(err)
				}
				return map[string]string{
					claimKey: claimValue,
					nodeLocalPoolHostPathClaimAnnotation(cluster, secondPool.Name): secondValue,
				}
			},
			wantError: "identity evidence for multiple pools",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cluster := nodeLocalPoolActivationTestCluster("identity-inventory", "a")
			pool := &cluster.Spec.Storage.NodeLocalPools[0]
			claim, err := newNodeLocalPoolHostPathClaim(cluster, pool, retainedIdentityA)
			if err != nil {
				t.Fatal(err)
			}
			claimValue, err := encodeNodeLocalPoolHostPathClaim(claim)
			if err != nil {
				t.Fatal(err)
			}
			claimKey := nodeLocalPoolHostPathClaimAnnotation(cluster, pool.Name)
			recoveryKey := nodeLocalPoolRecoveryNodeIDAnnotation(cluster, pool.Name)
			kubernetesNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:        "identity-worker",
				Annotations: test.annotations(t, claimKey, recoveryKey, claimValue),
			}}
			scheme := deletionTestScheme(t)
			objects := []client.Object{kubernetesNode}
			if test.generatedID != "" {
				generated := &garagev1beta1.GarageNode{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "generated-identity-worker",
						Namespace: cluster.Namespace,
						OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
							cluster, garagev1beta2.GroupVersion.WithKind("GarageCluster"),
						)},
					},
					Spec: garagev1beta1.GarageNodeSpec{
						ClusterRef:         garagev1beta1.ClusterReference{Name: cluster.Name},
						Backing:            garagev1beta1.NodeBackingNodeLocalPool,
						KubernetesNodeName: kubernetesNode.Name,
						NodeLocalPoolName:  pool.Name,
					},
					Status: garagev1beta1.GarageNodeStatus{NodeID: test.generatedID},
				}
				objects = append(objects, generated)
			}
			reconciler, _ := deletionTestReconciler(scheme, objects...)

			got, err := reconciler.collectGarageNodeIDs(ctx, cluster)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("collectGarageNodeIDs() error = %v, want substring %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.wantIDs) {
				t.Fatalf("collectGarageNodeIDs() = %#v, want exactly %v", got, test.wantIDs)
			}
			for _, nodeID := range test.wantIDs {
				if !got[nodeID] {
					t.Fatalf("collectGarageNodeIDs() = %#v, missing %s", got, nodeID)
				}
			}
		})
	}
}
