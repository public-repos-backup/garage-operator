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
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

type retainedNodeLocalPoolIdentityEvidence struct {
	claimGarageNodeID     string
	recoveryGarageNodeID  string
	generatedGarageNodeID string
}

// retainedNodeLocalPoolIdentityIDs classifies every retained annotation for
// one Kubernetes Node before returning any Garage identity to the destructive
// Drain inventory. HostPath claims bind their own pool name through the exact
// derived key. A recovery pin must then bind to a pool known from the current
// spec, a surviving generated GarageNode, or an exact retained claim.
//
// Unknown keys and disagreeing claim/pin identities fail closed. In
// particular, an arbitrary annotation under the cluster's retained prefix is
// never interpreted as a raw Garage node ID.
func retainedNodeLocalPoolIdentityIDs(
	cluster *garagev1beta2.GarageCluster,
	kubernetesNode *corev1.Node,
	knownNodeLocalPoolNames map[string]struct{},
	generatedGarageNodeIDs map[string]string,
) ([]string, error) {
	if cluster == nil || kubernetesNode == nil {
		return nil, fmt.Errorf("retained node-local-pool identity inventory requires an exact cluster and Kubernetes Node")
	}

	retainedPrefix := nodeLocalPoolRetainedAnnotationClusterPrefix(cluster)
	retainedKeys := make([]string, 0)
	for key := range kubernetesNode.Annotations {
		if strings.HasPrefix(key, retainedPrefix) {
			retainedKeys = append(retainedKeys, key)
		}
	}
	if len(retainedKeys) == 0 && len(generatedGarageNodeIDs) == 0 {
		return nil, nil
	}
	sort.Strings(retainedKeys)

	poolNames := make(map[string]struct{}, len(knownNodeLocalPoolNames))
	for name := range knownNodeLocalPoolNames {
		if name != "" {
			poolNames[name] = struct{}{}
		}
	}
	for poolName, nodeID := range generatedGarageNodeIDs {
		if poolName == "" || !isValidGarageNodeID(nodeID) {
			return nil, fmt.Errorf(
				"kubernetes Node %s has incomplete generated GarageNode evidence for node-local pool %q",
				kubernetesNode.Name, poolName,
			)
		}
		poolNames[poolName] = struct{}{}
	}
	if cluster.Spec.Storage != nil {
		for i := range cluster.Spec.Storage.NodeLocalPools {
			if name := cluster.Spec.Storage.NodeLocalPools[i].Name; name != "" {
				poolNames[name] = struct{}{}
			}
		}
	}

	evidenceByPool := make(map[string]*retainedNodeLocalPoolIdentityEvidence)
	for poolName, nodeID := range generatedGarageNodeIDs {
		evidenceByPool[poolName] = &retainedNodeLocalPoolIdentityEvidence{
			generatedGarageNodeID: canonicalGarageNodeID(nodeID),
		}
	}
	nonClaimKeys := make([]string, 0, len(retainedKeys))
	for _, key := range retainedKeys {
		if !isNodeLocalPoolHostPathClaimAnnotation(key) {
			nonClaimKeys = append(nonClaimKeys, key)
			continue
		}
		claim, err := decodeNodeLocalPoolHostPathClaim(kubernetesNode.Annotations[key])
		if err != nil {
			return nil, fmt.Errorf(
				"kubernetes Node %s carries invalid retained HostPath claim in %s: %w",
				kubernetesNode.Name, key, err,
			)
		}
		if claim.ClusterNamespace != cluster.Namespace || claim.ClusterName != cluster.Name {
			return nil, fmt.Errorf(
				"kubernetes Node %s HostPath claim %s names unexpected owner %s/%s",
				kubernetesNode.Name, key, claim.ClusterNamespace, claim.ClusterName,
			)
		}
		expectedKey := nodeLocalPoolHostPathClaimAnnotation(cluster, claim.NodeLocalPoolName)
		if key != expectedKey {
			return nil, fmt.Errorf(
				"kubernetes Node %s HostPath claim key %s does not exactly bind decoded node-local pool %q (want %s)",
				kubernetesNode.Name, key, claim.NodeLocalPoolName, expectedKey,
			)
		}
		poolNames[claim.NodeLocalPoolName] = struct{}{}
		evidence := evidenceByPool[claim.NodeLocalPoolName]
		if evidence == nil {
			evidence = &retainedNodeLocalPoolIdentityEvidence{}
			evidenceByPool[claim.NodeLocalPoolName] = evidence
		}
		evidence.claimGarageNodeID = canonicalGarageNodeID(claim.GarageNodeID)
	}

	recoveryKeyToPool := make(map[string]string, len(poolNames))
	for poolName := range poolNames {
		key := nodeLocalPoolRecoveryNodeIDAnnotation(cluster, poolName)
		if previousPoolName, collision := recoveryKeyToPool[key]; collision && previousPoolName != poolName {
			return nil, fmt.Errorf(
				"node-local pools %q and %q derive the same retained recovery annotation key %s",
				previousPoolName, poolName, key,
			)
		}
		recoveryKeyToPool[key] = poolName
	}

	for _, key := range nonClaimKeys {
		poolName, known := recoveryKeyToPool[key]
		if !known {
			if isNodeLocalPoolRecoveryAnnotationKey(cluster, key) {
				return nil, fmt.Errorf(
					"kubernetes Node %s carries retained recovery annotation %s that cannot be bound to a declared pool, surviving GarageNode, or exact HostPath claim",
					kubernetesNode.Name, key,
				)
			}
			return nil, fmt.Errorf(
				"kubernetes Node %s carries unrecognized retained node-local-pool annotation %s",
				kubernetesNode.Name, key,
			)
		}
		nodeID := canonicalGarageNodeID(kubernetesNode.Annotations[key])
		if !isValidGarageNodeID(nodeID) {
			return nil, fmt.Errorf(
				"kubernetes Node %s carries invalid retained Garage identity %q in %s",
				kubernetesNode.Name, kubernetesNode.Annotations[key], key,
			)
		}
		evidence := evidenceByPool[poolName]
		if evidence == nil {
			evidence = &retainedNodeLocalPoolIdentityEvidence{}
			evidenceByPool[poolName] = evidence
		}
		evidence.recoveryGarageNodeID = nodeID
	}
	if len(evidenceByPool) > 1 {
		poolNames := make([]string, 0, len(evidenceByPool))
		for poolName := range evidenceByPool {
			poolNames = append(poolNames, poolName)
		}
		sort.Strings(poolNames)
		return nil, fmt.Errorf(
			"kubernetes Node %s carries node-local identity evidence for multiple pools %q; one Node may belong to at most one node-local pool",
			kubernetesNode.Name, poolNames,
		)
	}

	identities := make(map[string]struct{}, len(evidenceByPool))
	for poolName, evidence := range evidenceByPool {
		if evidence.claimGarageNodeID != "" && evidence.recoveryGarageNodeID != "" &&
			evidence.claimGarageNodeID != evidence.recoveryGarageNodeID {
			return nil, fmt.Errorf(
				"kubernetes Node %s node-local pool %q HostPath claim identity %s disagrees with recovery identity %s",
				kubernetesNode.Name, poolName, shortID(evidence.claimGarageNodeID), shortID(evidence.recoveryGarageNodeID),
			)
		}
		if evidence.generatedGarageNodeID != "" && evidence.claimGarageNodeID != "" &&
			evidence.generatedGarageNodeID != evidence.claimGarageNodeID {
			return nil, fmt.Errorf(
				"kubernetes Node %s node-local pool %q generated GarageNode identity %s disagrees with HostPath claim identity %s",
				kubernetesNode.Name, poolName, shortID(evidence.generatedGarageNodeID), shortID(evidence.claimGarageNodeID),
			)
		}
		if evidence.generatedGarageNodeID != "" && evidence.recoveryGarageNodeID != "" &&
			evidence.generatedGarageNodeID != evidence.recoveryGarageNodeID {
			return nil, fmt.Errorf(
				"kubernetes Node %s node-local pool %q generated GarageNode identity %s disagrees with recovery identity %s",
				kubernetesNode.Name, poolName, shortID(evidence.generatedGarageNodeID), shortID(evidence.recoveryGarageNodeID),
			)
		}
		if evidence.claimGarageNodeID != "" {
			identities[evidence.claimGarageNodeID] = struct{}{}
		}
		if evidence.recoveryGarageNodeID != "" {
			identities[evidence.recoveryGarageNodeID] = struct{}{}
		}
		if evidence.generatedGarageNodeID != "" {
			identities[evidence.generatedGarageNodeID] = struct{}{}
		}
	}

	result := make([]string, 0, len(identities))
	for nodeID := range identities {
		result = append(result, nodeID)
	}
	sort.Strings(result)
	return result, nil
}

func isNodeLocalPoolRecoveryAnnotationKey(
	cluster *garagev1beta2.GarageCluster,
	key string,
) bool {
	if cluster == nil || !strings.HasPrefix(key, nodeLocalPoolRecoveryAnnotationClusterPrefix(cluster)) ||
		!strings.HasSuffix(key, "-node-id") {
		return false
	}
	digest := strings.TrimSuffix(
		strings.TrimPrefix(key, nodeLocalPoolRecoveryAnnotationClusterPrefix(cluster)),
		"-node-id",
	)
	if len(digest) != 16 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
