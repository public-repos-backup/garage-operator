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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
	"github.com/rajsinghtech/garage-operator/internal/garage"
)

func nodeLocalPoolActivationClusterPrefix(cluster *garagev1beta2.GarageCluster) string {
	// Include the immutable UID so force-deleting and recreating a cluster with
	// the same namespace/name can never reactivate pods through orphaned labels
	// from the previous object.
	identity := cluster.Namespace + "/" + cluster.Name
	if cluster.UID != "" {
		identity += "/" + string(cluster.UID)
	}
	sum := sha256.Sum256([]byte(identity))
	return nodeLocalPoolActivationLabelDomain + nodeLocalPoolActivationLabelNamePrefix + fmt.Sprintf("%x", sum[:8]) + "-node-local-pool-"
}

func nodeLocalPoolActivationLabel(cluster *garagev1beta2.GarageCluster, nodeLocalPoolName string) string {
	sum := sha256.Sum256([]byte(nodeLocalPoolName))
	return nodeLocalPoolActivationClusterPrefix(cluster) + fmt.Sprintf("%x", sum[:8])
}

func nodeLocalPoolRetainedAnnotationClusterPrefix(cluster *garagev1beta2.GarageCluster) string {
	// Unlike the scheduling label, this identity record is deliberately stable
	// across recreation of a same-name GarageCluster. A clean finalization
	// removes it after the corresponding Garage role is gone; a force deletion
	// leaves it beside the retained HostPath so the replacement object can only
	// restart that disk under its previously committed Garage identity.
	identity := cluster.Namespace + "/" + cluster.Name
	sum := sha256.Sum256([]byte(identity))
	return nodeLocalPoolActivationLabelDomain + "gc-" + fmt.Sprintf("%x", sum[:8]) + "-node-local-"
}

func nodeLocalPoolRecoveryAnnotationClusterPrefix(cluster *garagev1beta2.GarageCluster) string {
	return nodeLocalPoolRetainedAnnotationClusterPrefix(cluster) + "pool-"
}

func nodeLocalPoolRecoveryNodeIDAnnotation(cluster *garagev1beta2.GarageCluster, nodeLocalPoolName string) string {
	sum := sha256.Sum256([]byte(nodeLocalPoolName))
	return nodeLocalPoolRecoveryAnnotationClusterPrefix(cluster) + fmt.Sprintf("%x", sum[:8]) + "-node-id"
}

func nodeLocalPoolHostPathClaimAnnotation(cluster *garagev1beta2.GarageCluster, nodeLocalPoolName string) string {
	// The qualified-name segment is limited to 63 bytes. Keep the private claim
	// key semantically node-local but shorter than the recovery-node-id prefix:
	// gc-<cluster hash>-node-local-<pool hash>-hostpath-claim is 62 bytes.
	sum := sha256.Sum256([]byte(nodeLocalPoolName))
	return nodeLocalPoolRetainedAnnotationClusterPrefix(cluster) +
		fmt.Sprintf("%x", sum[:8]) + nodeLocalPoolHostPathClaimSuffix
}

func newNodeLocalPoolHostPathClaim(
	cluster *garagev1beta2.GarageCluster,
	pool *garagev1beta2.NodeLocalPoolSpec,
	garageNodeID string,
) (nodeLocalPoolHostPathClaim, error) {
	if cluster == nil || pool == nil || cluster.Namespace == "" || cluster.Name == "" || pool.Name == "" {
		return nodeLocalPoolHostPathClaim{}, fmt.Errorf("node-local-pool HostPath claim requires an exact cluster and pool identity")
	}
	garageNodeID = canonicalGarageNodeID(garageNodeID)
	if garageNodeID != "" && !isValidGarageNodeID(garageNodeID) {
		return nodeLocalPoolHostPathClaim{}, fmt.Errorf("invalid Garage node ID %q in node-local-pool HostPath claim", garageNodeID)
	}
	paths := nodeLocalPoolHostPaths(pool)
	if len(paths) == 0 {
		return nodeLocalPoolHostPathClaim{}, fmt.Errorf("node-local pool %q has no HostPath to claim", pool.Name)
	}
	return nodeLocalPoolHostPathClaim{
		Version:           nodeLocalPoolHostPathClaimVersion,
		ClusterNamespace:  cluster.Namespace,
		ClusterName:       cluster.Name,
		NodeLocalPoolName: pool.Name,
		HostPaths:         paths,
		GarageNodeID:      garageNodeID,
	}, nil
}

func encodeNodeLocalPoolHostPathClaim(claim nodeLocalPoolHostPathClaim) (string, error) {
	if err := validateNodeLocalPoolHostPathClaim(&claim); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(claim)
	if err != nil {
		return "", fmt.Errorf("encoding node-local-pool HostPath claim: %w", err)
	}
	return string(encoded), nil
}

func decodeNodeLocalPoolHostPathClaim(value string) (*nodeLocalPoolHostPathClaim, error) {
	claim := &nodeLocalPoolHostPathClaim{}
	if err := json.Unmarshal([]byte(value), claim); err != nil {
		return nil, fmt.Errorf("decoding node-local-pool HostPath claim: %w", err)
	}
	if err := validateNodeLocalPoolHostPathClaim(claim); err != nil {
		return nil, err
	}
	return claim, nil
}

func validateNodeLocalPoolHostPathClaim(claim *nodeLocalPoolHostPathClaim) error {
	if claim == nil || claim.Version != nodeLocalPoolHostPathClaimVersion {
		return fmt.Errorf("node-local-pool HostPath claim has unsupported version")
	}
	if claim.ClusterNamespace == "" || claim.ClusterName == "" || claim.NodeLocalPoolName == "" {
		return fmt.Errorf("node-local-pool HostPath claim has an incomplete owner")
	}
	if claim.GarageNodeID != "" {
		claim.GarageNodeID = canonicalGarageNodeID(claim.GarageNodeID)
		if !isValidGarageNodeID(claim.GarageNodeID) {
			return fmt.Errorf("node-local-pool HostPath claim has invalid Garage node ID %q", claim.GarageNodeID)
		}
	}
	normalizedPaths := make([]string, 0, len(claim.HostPaths))
	seen := make(map[string]struct{}, len(claim.HostPaths))
	for _, hostPath := range claim.HostPaths {
		hostPath = path.Clean(strings.TrimSpace(hostPath))
		if hostPath == "." || !strings.HasPrefix(hostPath, "/") {
			return fmt.Errorf("node-local-pool HostPath claim contains invalid path %q", hostPath)
		}
		if _, duplicate := seen[hostPath]; duplicate {
			return fmt.Errorf("node-local-pool HostPath claim repeats path %q", hostPath)
		}
		seen[hostPath] = struct{}{}
		normalizedPaths = append(normalizedPaths, hostPath)
	}
	if len(normalizedPaths) == 0 {
		return fmt.Errorf("node-local-pool HostPath claim contains no paths")
	}
	sort.Strings(normalizedPaths)
	claim.HostPaths = normalizedPaths
	return nil
}

func isNodeLocalPoolHostPathClaimAnnotation(key string) bool {
	return strings.HasPrefix(key, nodeLocalPoolActivationLabelDomain+"gc-") &&
		strings.HasSuffix(key, nodeLocalPoolHostPathClaimSuffix)
}

// nodeLocalPoolHostPathClaimCanTransition returns true only for the same durable
// owner and an append-only HostPath set. Admission separately preserves every
// existing container-path mapping; this controller-side check ensures an old
// ownership claim cannot be shrunk or reassigned while allowing a newly added
// disk to pass through the ordinary cross-cluster overlap preflight.
func nodeLocalPoolHostPathClaimCanTransition(
	claim *nodeLocalPoolHostPathClaim,
	cluster *garagev1beta2.GarageCluster,
	nodeLocalPoolName string,
	desiredPaths []string,
) bool {
	if claim == nil || cluster == nil ||
		claim.ClusterNamespace != cluster.Namespace ||
		claim.ClusterName != cluster.Name ||
		claim.NodeLocalPoolName != nodeLocalPoolName {
		return false
	}
	desired := make(map[string]struct{}, len(desiredPaths))
	for _, hostPath := range desiredPaths {
		desired[hostPath] = struct{}{}
	}
	for _, claimedPath := range claim.HostPaths {
		if _, retained := desired[claimedPath]; !retained {
			return false
		}
	}
	return true
}

func isValidGarageNodeID(nodeID string) bool {
	nodeID = strings.TrimSpace(nodeID)
	if len(nodeID) != 64 {
		return false
	}
	_, err := hex.DecodeString(nodeID)
	return err == nil
}

func (r *GarageClusterReconciler) getNodeLocalPoolCommittedLayout(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) (*garage.ClusterLayout, error) {
	if r.nodeLocalPoolLayoutGetter != nil {
		return r.nodeLocalPoolLayoutGetter(ctx, cluster)
	}
	garageClient, err := GetGarageClient(ctx, r.Client, cluster, r.ClusterDomain)
	if err != nil {
		return nil, fmt.Errorf("creating Garage Admin API client for node-local-pool recovery: %w", err)
	}
	layout, err := garageClient.GetClusterLayout(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading committed Garage layout for node-local-pool recovery: %w", err)
	}
	return layout, nil
}

// nodeLocalPoolRecoveryRoleClaims returns exact committed positive-capacity role
// identities keyed by their operator-authored pool/Kubernetes-Node tags. An
// exact current cluster-uid tag wins. Same-name recreation can fall back to an
// older/legacy UID only when the role's layout zone matches that exact desired
// member; this prevents a federated site that reuses cluster/pool/Node names
// from lending its identity to another site.
func nodeLocalPoolRecoveryRoleClaims(
	cluster *garagev1beta2.GarageCluster,
	layout *garage.ClusterLayout,
	expectedZones map[string]string,
) (map[string]string, error) {
	if cluster == nil || layout == nil {
		return nil, fmt.Errorf("committed Garage layout is unavailable")
	}
	ownershipTag := fmt.Sprintf("cluster:%s/%s", cluster.Name, cluster.Namespace)
	expectedUIDTag := nodeClusterUIDTagPrefix + string(cluster.UID)
	exactClaims := make(map[string]string)
	fallbackCandidates := make(map[string][]string)
	addClaim := func(claims map[string]string, key, nodeID string) error {
		if previous := claims[key]; previous != "" && !strings.EqualFold(previous, nodeID) {
			nodeLocalPoolName, nodeName, _ := strings.Cut(key, "\x00")
			return fmt.Errorf(
				"garage roles %s and %s both claim node-local pool %q on Kubernetes Node %q",
				shortID(previous), shortID(nodeID), nodeLocalPoolName, nodeName,
			)
		}
		claims[key] = canonicalGarageNodeID(nodeID)
		return nil
	}
	for i := range layout.Roles {
		role := &layout.Roles[i]
		if role.Capacity == nil || *role.Capacity == 0 {
			continue
		}
		if !isValidGarageNodeID(role.ID) {
			return nil, fmt.Errorf("garage role %q has an invalid node ID", role.ID)
		}
		var (
			owned, storageTier, exactUID bool
			nodeLocalPoolName, nodeName  string
		)
		for _, tag := range role.Tags {
			switch {
			case tag == ownershipTag:
				owned = true
			case tag == "tier:"+tierStorage:
				storageTier = true
			case tag == expectedUIDTag && cluster.UID != "":
				exactUID = true
			case strings.HasPrefix(tag, nodeLocalPoolLayoutTagPrefix):
				value := strings.TrimPrefix(tag, nodeLocalPoolLayoutTagPrefix)
				if nodeLocalPoolName != "" && nodeLocalPoolName != value {
					return nil, fmt.Errorf("garage role %s claims multiple node-local pools", shortID(role.ID))
				}
				nodeLocalPoolName = value
			case strings.HasPrefix(tag, "kubernetes-node:"):
				value := strings.TrimPrefix(tag, "kubernetes-node:")
				if nodeName != "" && nodeName != value {
					return nil, fmt.Errorf("garage role %s claims multiple Kubernetes Nodes", shortID(role.ID))
				}
				nodeName = value
			}
		}
		if !storageTier || nodeLocalPoolName == "" || nodeName == "" {
			continue
		}
		key := nodeLocalPoolKey(nodeLocalPoolName, nodeName)
		if exactUID {
			if err := addClaim(exactClaims, key, role.ID); err != nil {
				return nil, err
			}
			continue
		}
		if !owned {
			continue
		}
		if expectedZone := strings.TrimSpace(expectedZones[key]); expectedZone != "" && role.Zone == expectedZone {
			candidate := canonicalGarageNodeID(role.ID)
			duplicate := false
			for _, previous := range fallbackCandidates[key] {
				if previous == candidate {
					duplicate = true
					break
				}
			}
			if !duplicate {
				fallbackCandidates[key] = append(fallbackCandidates[key], candidate)
			}
		}
	}
	claims := make(map[string]string, len(exactClaims)+len(fallbackCandidates))
	for key, nodeID := range exactClaims {
		claims[key] = nodeID
	}
	for key, candidates := range fallbackCandidates {
		if claims[key] != "" {
			continue
		}
		if len(candidates) > 1 {
			nodeLocalPoolName, nodeName, _ := strings.Cut(key, "\x00")
			return nil, fmt.Errorf(
				"garage roles %s and %s both claim node-local pool %q on Kubernetes Node %q in the expected zone",
				shortID(candidates[0]), shortID(candidates[1]), nodeLocalPoolName, nodeName,
			)
		}
		if len(candidates) == 1 {
			claims[key] = candidates[0]
		}
	}
	return claims, nil
}

// validatePinnedNodeLocalPoolRecoveryRole validates one authoritative durable
// pin by exact Garage identity. Other sites may legitimately reuse the same
// cluster, pool, and Kubernetes Node names; they must not make an exact local
// pin ambiguous merely because their map key is identical.
func validatePinnedNodeLocalPoolRecoveryRole(
	cluster *garagev1beta2.GarageCluster,
	layout *garage.ClusterLayout,
	nodeID, nodeLocalPoolName, kubernetesNodeName, expectedZone string,
) error {
	if cluster == nil || layout == nil {
		return fmt.Errorf("committed Garage layout is unavailable")
	}
	nodeID = canonicalGarageNodeID(nodeID)
	ownershipTag := fmt.Sprintf("cluster:%s/%s", cluster.Name, cluster.Namespace)
	expectedUIDTag := nodeClusterUIDTagPrefix + string(cluster.UID)
	for i := range layout.Roles {
		role := &layout.Roles[i]
		if canonicalGarageNodeID(role.ID) != nodeID {
			continue
		}
		if role.Capacity == nil || *role.Capacity == 0 {
			return fmt.Errorf("garage identity %s has no positive-capacity committed role", shortID(nodeID))
		}
		var owned, storageTier, exactUID bool
		var taggedPool, taggedNode string
		for _, tag := range role.Tags {
			switch {
			case tag == ownershipTag:
				owned = true
			case tag == "tier:"+tierStorage:
				storageTier = true
			case tag == expectedUIDTag:
				exactUID = cluster.UID != ""
			case strings.HasPrefix(tag, nodeLocalPoolLayoutTagPrefix):
				value := strings.TrimPrefix(tag, nodeLocalPoolLayoutTagPrefix)
				if taggedPool != "" && taggedPool != value {
					return fmt.Errorf("garage identity %s claims multiple node-local pools", shortID(nodeID))
				}
				taggedPool = value
			case strings.HasPrefix(tag, "kubernetes-node:"):
				value := strings.TrimPrefix(tag, "kubernetes-node:")
				if taggedNode != "" && taggedNode != value {
					return fmt.Errorf("garage identity %s claims multiple Kubernetes Nodes", shortID(nodeID))
				}
				taggedNode = value
			}
		}
		if !storageTier || taggedPool != nodeLocalPoolName || taggedNode != kubernetesNodeName || (!exactUID && !owned) {
			return fmt.Errorf(
				"garage identity %s is not tagged for cluster %s/%s node-local pool %q on Kubernetes Node %q",
				shortID(nodeID), cluster.Namespace, cluster.Name, nodeLocalPoolName, kubernetesNodeName,
			)
		}
		if !exactUID && (strings.TrimSpace(expectedZone) == "" || role.Zone != expectedZone) {
			return fmt.Errorf(
				"garage identity %s belongs to layout zone %q, not expected site zone %q",
				shortID(nodeID), role.Zone, expectedZone,
			)
		}
		return nil
	}
	return fmt.Errorf("garage identity %s has no committed layout role", shortID(nodeID))
}

type nodeLocalPoolRecoveryPins struct {
	nodeIDs   map[string]string
	layoutErr error
}

func nodeLocalPoolExpectedZone(cluster *garagev1beta2.GarageCluster, node *corev1.Node) string {
	if cluster == nil {
		return ""
	}
	zone := strings.TrimSpace(cluster.Spec.Zone)
	if zone == "" {
		zone = defaultZoneName
	}
	if cluster.Spec.ZoneFrom != nil && node != nil {
		if derived := strings.TrimSpace(node.Labels[cluster.Spec.ZoneFrom.NodeLabel]); derived != "" {
			zone = derived
		}
	}
	return zone
}

func mergeNodeLocalPoolRecoveryPin(
	pins, sources map[string]string,
	key, nodeID, source string,
) error {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil
	}
	if !isValidGarageNodeID(nodeID) {
		nodeLocalPoolName, nodeName, _ := strings.Cut(key, "\x00")
		return fmt.Errorf(
			"node-local pool %q on Kubernetes Node %q has invalid recovery node ID %q from %s",
			nodeLocalPoolName, nodeName, nodeID, source,
		)
	}
	if previous := pins[key]; previous != "" && !strings.EqualFold(previous, nodeID) {
		nodeLocalPoolName, nodeName, _ := strings.Cut(key, "\x00")
		return fmt.Errorf(
			"node-local pool %q on Kubernetes Node %q has conflicting recovery identities %s from %s and %s from %s",
			nodeLocalPoolName, nodeName, shortID(previous), sources[key], shortID(nodeID), source,
		)
	}
	if pins[key] == "" {
		pins[key] = nodeID
		sources[key] = source
	}
	return nil
}

// resolveNodeLocalPoolRecoveryPins combines the durable Kubernetes-side identity
// records with exact operator-tagged roles in Garage's committed layout. A
// missing layout is non-fatal: already pinned processes can make the Admin API
// reachable again, while genuinely new identities remain under the ordinary
// layout-mutation barrier. Any disagreement is fatal and surfaced as an
// identity collision before a HostPath workload is started or updated.
func (r *GarageClusterReconciler) resolveNodeLocalPoolRecoveryPins(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	states map[string]*nodeLocalPoolState,
	existingByPair map[string]*garagev1beta1.GarageNode,
) (*nodeLocalPoolRecoveryPins, error) {
	result := &nodeLocalPoolRecoveryPins{nodeIDs: make(map[string]string)}
	sources := make(map[string]string)
	validateUniqueIdentities := func() error {
		keysByNodeID := make(map[string]string)
		for key, nodeID := range result.nodeIDs {
			nodeID = canonicalGarageNodeID(nodeID)
			if nodeID == "" {
				continue
			}
			if previousKey := keysByNodeID[nodeID]; previousKey != "" && previousKey != key {
				previousPool, previousNode, _ := strings.Cut(previousKey, "\x00")
				nodeLocalPoolName, nodeName, _ := strings.Cut(key, "\x00")
				return fmt.Errorf(
					"garage identity %s is claimed by node-local pool %q on Kubernetes Node %q from %s and pool %q on Node %q from %s",
					shortID(nodeID), previousPool, previousNode, sources[previousKey], nodeLocalPoolName, nodeName, sources[key],
				)
			}
			keysByNodeID[nodeID] = key
		}
		return nil
	}
	needsLayout := false
	for nodeLocalPoolName, state := range states {
		for nodeName, k8sNode := range state.desiredNodes {
			key := nodeLocalPoolKey(nodeLocalPoolName, nodeName)
			annotationKey := nodeLocalPoolRecoveryNodeIDAnnotation(cluster, nodeLocalPoolName)
			if err := mergeNodeLocalPoolRecoveryPin(
				result.nodeIDs, sources, key,
				k8sNode.Annotations[annotationKey],
				"kubernetes node annotation "+annotationKey,
			); err != nil {
				return nil, err
			}
			claimKey := nodeLocalPoolHostPathClaimAnnotation(cluster, nodeLocalPoolName)
			if claimValue := k8sNode.Annotations[claimKey]; claimValue != "" {
				claim, err := decodeNodeLocalPoolHostPathClaim(claimValue)
				if err != nil || !nodeLocalPoolHostPathClaimCanTransition(
					claim, cluster, nodeLocalPoolName, nodeLocalPoolHostPaths(state.pool),
				) {
					return nil, fmt.Errorf("kubernetes node %q carries an invalid HostPath claim for node-local pool %q", nodeName, nodeLocalPoolName)
				}
				if err := mergeNodeLocalPoolRecoveryPin(
					result.nodeIDs, sources, key, claim.GarageNodeID,
					"kubernetes node HostPath claim "+claimKey,
				); err != nil {
					return nil, err
				}
			}
			if current := existingByPair[key]; current != nil {
				if err := mergeNodeLocalPoolRecoveryPin(
					result.nodeIDs, sources, key,
					current.Annotations[garagev1beta1.AnnotationNodeLocalPoolRecoveryNodeID],
					"GarageNode "+current.Name+" identity pin",
				); err != nil {
					return nil, err
				}
				if current.Status.InLayout {
					if err := mergeNodeLocalPoolRecoveryPin(
						result.nodeIDs, sources, key,
						current.Status.NodeID,
						"GarageNode "+current.Name+" committed status",
					); err != nil {
						return nil, err
					}
				}
			}
			if result.nodeIDs[key] == "" {
				needsLayout = true
			}
		}
	}
	if !needsLayout {
		return result, validateUniqueIdentities()
	}

	layout, err := r.getNodeLocalPoolCommittedLayout(ctx, cluster)
	if err != nil {
		result.layoutErr = err
		return result, validateUniqueIdentities()
	}
	expectedZones := make(map[string]string)
	for nodeLocalPoolName, state := range states {
		for nodeName, kubernetesNode := range state.desiredNodes {
			expectedZones[nodeLocalPoolKey(nodeLocalPoolName, nodeName)] = nodeLocalPoolExpectedZone(cluster, kubernetesNode)
		}
	}
	claims, err := nodeLocalPoolRecoveryRoleClaims(cluster, layout, expectedZones)
	if err != nil {
		return nil, err
	}
	for nodeLocalPoolName, state := range states {
		for nodeName := range state.desiredNodes {
			key := nodeLocalPoolKey(nodeLocalPoolName, nodeName)
			if err := mergeNodeLocalPoolRecoveryPin(
				result.nodeIDs, sources, key, claims[key], "committed Garage layout",
			); err != nil {
				return nil, err
			}
		}
	}
	return result, validateUniqueIdentities()
}

func nodeLocalPoolActivationValueForDaemonSet(daemonSet *appsv1.DaemonSet) string {
	if daemonSet != nil {
		if value := strings.TrimSpace(daemonSet.Annotations[annotationNodeLocalPoolActivationValue]); value != "" {
			return value
		}
	}
	return nodeLocalPoolActivationLabelValue
}

func nodeLocalPoolActivationValueForWorkloadUID(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return "workload-" + fmt.Sprintf("%x", sum[:16])
}

// nodeLocalPoolActivationValueIsActive reports whether a Node-label value can
// satisfy an unfenced node-local-pool workload selector. The initial rollout used
// the legacy value "true"; adopted/recreated DaemonSets use a UID-derived token.
// Treat every other nonempty value conservatively as active while excluding the
// two explicit scheduling fences.
func nodeLocalPoolActivationValueIsActive(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != nodeLocalPoolActivationFenceValue && value != nodeLocalPoolActivationQuarantineValue
}
