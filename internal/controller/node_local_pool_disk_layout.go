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
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func storageDiskLayoutForPool(pool *garagev1beta2.NodeLocalPoolSpec) nodeLocalPoolDiskLayout {
	layout := nodeLocalPoolDiskLayout{
		Version:              storageDiskLayoutVersion,
		MetadataHostPath:     pool.Metadata.HostPath,
		MetadataHostPathType: effectivePoolHostPathType(pool.Metadata.HostPathType),
	}
	if pool.Data != nil {
		layout.DataPaths = []nodeLocalPoolDiskPath{{
			Path:         dataPath,
			HostPath:     pool.Data.HostPath,
			HostPathType: effectivePoolHostPathType(pool.Data.HostPathType),
		}}
	} else {
		layout.DataPaths = make([]nodeLocalPoolDiskPath, 0, len(pool.DataPaths))
		for i := range pool.DataPaths {
			entry := &pool.DataPaths[i]
			layout.DataPaths = append(layout.DataPaths, nodeLocalPoolDiskPath{
				Path:         entry.Path,
				HostPath:     entry.HostPath,
				HostPathType: effectivePoolHostPathType(entry.HostPathType),
				ReadOnly:     entry.ReadOnly,
			})
		}
	}
	sort.Slice(layout.DataPaths, func(i, j int) bool {
		return layout.DataPaths[i].Path < layout.DataPaths[j].Path
	})
	return layout
}

func marshalStorageDiskLayout(layout nodeLocalPoolDiskLayout) (string, error) {
	if err := validateStorageDiskLayoutRecord(&layout); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(layout)
	if err != nil {
		return "", fmt.Errorf("encoding storage disk layout: %w", err)
	}
	return string(encoded), nil
}

// storageDiskLayoutFromDaemonSet returns the last layout the operator rolled
// out, while cross-checking its durable mapping against the actual pod
// template. DaemonSets created before this annotation existed are handled
// conservatively by treating every discovered data path as writable. The next
// no-op reconcile records the current desired readOnly state.
func storageDiskLayoutFromDaemonSet(daemonSet *appsv1.DaemonSet) (nodeLocalPoolDiskLayout, error) {
	actual, err := extractStorageDiskLayoutFromPodSpec(&daemonSet.Spec.Template.Spec)
	if err != nil {
		return nodeLocalPoolDiskLayout{}, err
	}
	if daemonSet.Annotations == nil || daemonSet.Annotations[annotationStorageDiskLayout] == "" {
		return actual, nil
	}

	recorded, err := parseStorageDiskLayoutAnnotation(
		daemonSet.Annotations[annotationStorageDiskLayout],
	)
	if err != nil {
		return nodeLocalPoolDiskLayout{}, err
	}
	if recorded.MetadataHostPath != actual.MetadataHostPath ||
		recorded.MetadataHostPathType != actual.MetadataHostPathType ||
		!storageDiskMappingsEqual(recorded.DataPaths, actual.DataPaths) {
		return nodeLocalPoolDiskLayout{}, fmt.Errorf(
			"annotation %q does not match the DaemonSet pod template's HostPath mounts; refusing to infer disk identity from drifted state",
			annotationStorageDiskLayout,
		)
	}
	return recorded, nil
}

func parseStorageDiskLayoutAnnotation(raw string) (nodeLocalPoolDiskLayout, error) {
	recorded := nodeLocalPoolDiskLayout{}
	if err := json.Unmarshal([]byte(raw), &recorded); err != nil {
		return nodeLocalPoolDiskLayout{}, fmt.Errorf(
			"annotation %q is invalid JSON: %w",
			annotationStorageDiskLayout,
			err,
		)
	}
	if err := validateStorageDiskLayoutRecord(&recorded); err != nil {
		return nodeLocalPoolDiskLayout{}, fmt.Errorf(
			"annotation %q is invalid: %w",
			annotationStorageDiskLayout,
			err,
		)
	}
	return recorded, nil
}

// validateNodeLocalPoolDiskLayoutBeforeConfigUpdate protects the same disk
// identity boundary before creating a new pool ConfigMap revision. The
// DaemonSet guard alone is too late: a rapid remove/re-add could otherwise
// create a garage.toml revision for an unsafe disk mapping before the
// DaemonSet transition is rejected.
//
// The record is duplicated on every config-resource revision so the guard survives
// an out-of-band DaemonSet deletion for as long as any pool GarageNode keeps a
// revision alive. Existing pre-record resources are bootstrapped from their
// DaemonSet; if that evidence is missing, the controller fails closed and asks
// an administrator to remove the stale resource explicitly.
func (r *GarageClusterReconciler) validateNodeLocalPoolDiskLayoutBeforeConfigUpdate(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	nodeLocalPoolName string,
	desired nodeLocalPoolDiskLayout,
) error {
	unrecordedResources := make([]string, 0)
	validateResource := func(kind string, object client.Object) error {
		if !isStorageDaemonSetConfigMapName(cluster, nodeLocalPoolName, object.GetName()) {
			return nil
		}
		if !metav1.IsControlledBy(object, cluster) {
			return fmt.Errorf(
				"existing %s %s is not controlled by GarageCluster %s/%s; refusing to adopt a colliding node-local-pool config",
				kind,
				object.GetName(),
				cluster.Namespace,
				cluster.Name,
			)
		}
		raw := object.GetAnnotations()[annotationStorageDiskLayout]
		if raw == "" {
			unrecordedResources = append(unrecordedResources, kind+"/"+object.GetName())
			return nil
		}
		recorded, err := parseStorageDiskLayoutAnnotation(raw)
		if err != nil {
			return fmt.Errorf("reading %s %s disk layout: %w", kind, object.GetName(), err)
		}
		return validateStorageDiskLayoutTransition(nodeLocalPoolName, recorded, desired)
	}
	configMaps := &corev1.ConfigMapList{}
	if err := r.nodeLocalPoolReader().List(ctx, configMaps, client.InNamespace(cluster.Namespace)); err != nil {
		return fmt.Errorf("listing pool %q ConfigMap revisions: %w", nodeLocalPoolName, err)
	}
	for i := range configMaps.Items {
		if err := validateResource("ConfigMap", &configMaps.Items[i]); err != nil {
			return err
		}
	}
	secrets := &corev1.SecretList{}
	if err := r.nodeLocalPoolReader().List(ctx, secrets, client.InNamespace(cluster.Namespace)); err != nil {
		return fmt.Errorf("listing pool %q Secret revisions: %w", nodeLocalPoolName, err)
	}
	for i := range secrets.Items {
		if err := validateResource("Secret", &secrets.Items[i]); err != nil {
			return err
		}
	}

	daemonSetKey := types.NamespacedName{
		Name:      storageDaemonSetName(cluster, nodeLocalPoolName),
		Namespace: cluster.Namespace,
	}
	daemonSet := &appsv1.DaemonSet{}
	daemonSetExists := false
	if err := r.nodeLocalPoolReader().Get(ctx, daemonSetKey, daemonSet); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("getting pool DaemonSet %s: %w", daemonSetKey.Name, err)
		}
	} else {
		daemonSetExists = true
		if !metav1.IsControlledBy(daemonSet, cluster) {
			return fmt.Errorf(
				"existing DaemonSet %s is not controlled by GarageCluster %s/%s; refusing to adopt a colliding workload",
				daemonSet.Name,
				cluster.Namespace,
				cluster.Name,
			)
		}
		recorded, err := storageDiskLayoutFromDaemonSet(daemonSet)
		if err != nil {
			return fmt.Errorf("reading existing DaemonSet %s disk layout: %w", daemonSet.Name, err)
		}
		if err := validateStorageDiskLayoutTransition(nodeLocalPoolName, recorded, desired); err != nil {
			return err
		}
	}

	if len(unrecordedResources) > 0 && !daemonSetExists {
		sort.Strings(unrecordedResources)
		return fmt.Errorf(
			"existing config resource(s) %s have no %q safety record and their pool DaemonSet is absent; "+
				"refusing to create garage.toml without disk-identity evidence (verify no pool GarageNodes remain, then delete the stale resources)",
			strings.Join(unrecordedResources, ", "),
			annotationStorageDiskLayout,
		)
	}
	return nil
}

func extractStorageDiskLayoutFromPodSpec(podSpec *corev1.PodSpec) (nodeLocalPoolDiskLayout, error) {
	layout := nodeLocalPoolDiskLayout{Version: storageDiskLayoutVersion}
	type mountedHostPath struct {
		path     string
		pathType corev1.HostPathType
	}
	hostPathByVolume := make(map[string]mountedHostPath)
	for i := range podSpec.Volumes {
		volume := &podSpec.Volumes[i]
		if volume.HostPath != nil {
			pathType := corev1.HostPathType("")
			if volume.HostPath.Type != nil {
				pathType = *volume.HostPath.Type
			}
			hostPathByVolume[volume.Name] = mountedHostPath{
				path:     volume.HostPath.Path,
				pathType: effectivePoolHostPathType(pathType),
			}
		}
	}

	var garageContainer *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == defaultAppName {
			garageContainer = &podSpec.Containers[i]
			break
		}
	}
	if garageContainer == nil {
		return nodeLocalPoolDiskLayout{}, fmt.Errorf("DaemonSet pod template has no %q container", defaultAppName)
	}
	for i := range garageContainer.VolumeMounts {
		mount := &garageContainer.VolumeMounts[i]
		// Production Directory mounts carry a second File-typed HostPath for
		// .garage-volume-id. It is a kubelet preflight guard, not a Garage data
		// directory, and therefore is not part of the persisted disk layout.
		if strings.HasPrefix(mount.Name, storageVolumeMarkerNamePrefix) {
			continue
		}
		hostPath, isHostPath := hostPathByVolume[mount.Name]
		if !isHostPath {
			continue
		}
		if mount.MountPath == metadataPath {
			if layout.MetadataHostPath != "" {
				return nodeLocalPoolDiskLayout{}, fmt.Errorf("DaemonSet pod template mounts multiple HostPaths at %s", metadataPath)
			}
			layout.MetadataHostPath = hostPath.path
			layout.MetadataHostPathType = hostPath.pathType
			continue
		}
		layout.DataPaths = append(layout.DataPaths, nodeLocalPoolDiskPath{
			Path:         mount.MountPath,
			HostPath:     hostPath.path,
			HostPathType: hostPath.pathType,
			// The Kubernetes mount remains writable when Garage's data_dir is
			// read_only because Garage still writes its marker file. An
			// unannotated legacy DaemonSet must therefore be treated as
			// writable until the operator records the Garage-level state.
			ReadOnly: false,
		})
	}
	sort.Slice(layout.DataPaths, func(i, j int) bool {
		return layout.DataPaths[i].Path < layout.DataPaths[j].Path
	})
	if err := validateStorageDiskLayoutRecord(&layout); err != nil {
		return nodeLocalPoolDiskLayout{}, fmt.Errorf("invalid HostPath layout in DaemonSet pod template: %w", err)
	}
	return layout, nil
}

func validateStorageDiskLayoutRecord(layout *nodeLocalPoolDiskLayout) error {
	if layout.Version != storageDiskLayoutVersion {
		return fmt.Errorf("unsupported version %d", layout.Version)
	}
	if layout.MetadataHostPath == "" {
		return fmt.Errorf("metadataHostPath is empty")
	}
	layout.MetadataHostPathType = effectivePoolHostPathType(layout.MetadataHostPathType)
	if layout.MetadataHostPathType != corev1.HostPathDirectory &&
		layout.MetadataHostPathType != corev1.HostPathDirectoryOrCreate {
		return fmt.Errorf("metadataHostPathType %q is unsupported", layout.MetadataHostPathType)
	}
	if len(layout.DataPaths) == 0 {
		return fmt.Errorf("dataPaths is empty")
	}
	seenPaths := make(map[string]struct{}, len(layout.DataPaths))
	for i := range layout.DataPaths {
		entry := &layout.DataPaths[i]
		if entry.Path == "" || entry.HostPath == "" {
			return fmt.Errorf("dataPaths[%d] has an empty path or hostPath", i)
		}
		entry.HostPathType = effectivePoolHostPathType(entry.HostPathType)
		if entry.HostPathType != corev1.HostPathDirectory &&
			entry.HostPathType != corev1.HostPathDirectoryOrCreate {
			return fmt.Errorf("dataPaths[%d].hostPathType %q is unsupported", i, entry.HostPathType)
		}
		if _, duplicate := seenPaths[entry.Path]; duplicate {
			return fmt.Errorf("data path %q is duplicated", entry.Path)
		}
		seenPaths[entry.Path] = struct{}{}
	}
	sort.Slice(layout.DataPaths, func(i, j int) bool {
		return layout.DataPaths[i].Path < layout.DataPaths[j].Path
	})
	return nil
}

func storageDiskMappingsEqual(a, b []nodeLocalPoolDiskPath) bool {
	if len(a) != len(b) {
		return false
	}
	aByPath := make(map[string]nodeLocalPoolDiskPath, len(a))
	for i := range a {
		aByPath[a[i].Path] = a[i]
	}
	for i := range b {
		entry, found := aByPath[b[i].Path]
		if !found || entry.HostPath != b[i].HostPath || entry.HostPathType != b[i].HostPathType {
			return false
		}
	}
	return true
}

func validateStorageDiskLayoutTransition(
	nodeLocalPoolName string,
	oldLayout, newLayout nodeLocalPoolDiskLayout,
) error {
	if oldLayout.MetadataHostPath != newLayout.MetadataHostPath {
		return fmt.Errorf(
			"existing DaemonSet for pool %q mounts metadata hostPath %q, not %q; its node identities must fully drain and the stale DaemonSet must disappear before this pool name can use another metadata directory",
			nodeLocalPoolName,
			oldLayout.MetadataHostPath,
			newLayout.MetadataHostPath,
		)
	}
	if storageHostPathTypeLoosens(oldLayout.MetadataHostPathType, newLayout.MetadataHostPathType) {
		return fmt.Errorf(
			"existing DaemonSet for pool %q cannot loosen metadata hostPathType from Directory to DirectoryOrCreate; a missing metadata disk must fail closed instead of creating an empty node identity",
			nodeLocalPoolName,
		)
	}

	oldByPath := make(map[string]nodeLocalPoolDiskPath, len(oldLayout.DataPaths))
	for i := range oldLayout.DataPaths {
		oldByPath[oldLayout.DataPaths[i].Path] = oldLayout.DataPaths[i]
	}
	newByPath := make(map[string]nodeLocalPoolDiskPath, len(newLayout.DataPaths))
	newPathByHost := make(map[string]string, len(newLayout.DataPaths))
	for i := range newLayout.DataPaths {
		entry := newLayout.DataPaths[i]
		newByPath[entry.Path] = entry
		newPathByHost[entry.HostPath] = entry.Path
	}
	for containerPath, oldEntry := range oldByPath {
		if newEntry, retained := newByPath[containerPath]; retained {
			if newEntry.HostPath != oldEntry.HostPath {
				return fmt.Errorf(
					"existing DaemonSet for pool %q cannot remap data path %q from hostPath %q to %q; retain the old mapping, or remove and fully drain the whole pool before recreating it with another disk layout",
					nodeLocalPoolName,
					containerPath,
					oldEntry.HostPath,
					newEntry.HostPath,
				)
			}
			if storageHostPathTypeLoosens(oldEntry.HostPathType, newEntry.HostPathType) {
				return fmt.Errorf(
					"existing DaemonSet for pool %q cannot loosen data path %q hostPathType from Directory to DirectoryOrCreate; a missing data disk must fail closed",
					nodeLocalPoolName,
					containerPath,
				)
			}
			continue
		}
		if movedTo := newPathByHost[oldEntry.HostPath]; movedTo != "" {
			return fmt.Errorf(
				"existing DaemonSet for pool %q cannot move hostPath %q from data path %q to %q because Garage persists drive identity by data_dir path",
				nodeLocalPoolName,
				oldEntry.HostPath,
				containerPath,
				movedTo,
			)
		}
		return fmt.Errorf(
			"existing DaemonSet for pool %q cannot remove data path %q (%s) in place: Garage layout and rebalance completion do not prove the HostPath is empty; retain it readOnly or drain and recreate the whole pool after independently verifying the disk is empty",
			nodeLocalPoolName,
			containerPath,
			oldEntry.HostPath,
		)
	}
	return nil
}

func storageHostPathTypeLoosens(oldType, newType corev1.HostPathType) bool {
	return effectivePoolHostPathType(oldType) == corev1.HostPathDirectory &&
		effectivePoolHostPathType(newType) != corev1.HostPathDirectory
}

// nodeLocalPoolHostPathConflicts prevents two independent GarageClusters from
// mounting overlapping host directories on the same Kubernetes Node. Running
// multiple Garage clusters on one Node is valid when their paths are disjoint,
// so the check is deliberately scoped to the intersection of live selector
// matches. Desired foreign pools and retained/retiring foreign DaemonSets are
// both considered: the latter closes the drain window after a pool disappears
// from another cluster's spec but its Garage identity is still online.
func (r *GarageClusterReconciler) nodeLocalPoolHostPathConflicts(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	states map[string]*nodeLocalPoolState,
) ([]string, error) {
	if len(states) == 0 {
		return nil, nil
	}
	nodeLocalObjectSelector, err := labels.Parse(labelNodeLocalPool)
	if err != nil {
		return nil, fmt.Errorf("building node-local-pool object selector: %w", err)
	}
	nodeLocalObjects := client.MatchingLabelsSelector{Selector: nodeLocalObjectSelector}

	conflicts := make(map[string]struct{})
	// Claims are the serialized ownership boundary. They remain readable when
	// every workload object or private scheduling label from the old owner is
	// gone, so inspect them before relying on desired foreign selectors.
	for nodeLocalPoolName, state := range states {
		expectedClaimKey := nodeLocalPoolHostPathClaimAnnotation(cluster, nodeLocalPoolName)
		expectedPaths := nodeLocalPoolHostPaths(state.pool)
		for nodeName, node := range state.desiredNodes {
			for key, value := range node.Annotations {
				if !isNodeLocalPoolHostPathClaimAnnotation(key) {
					continue
				}
				claim, err := decodeNodeLocalPoolHostPathClaim(value)
				if err != nil {
					conflicts[fmt.Sprintf("node %s: malformed durable HostPath claim %s: %v", nodeName, key, err)] = struct{}{}
					continue
				}
				if key == expectedClaimKey {
					if !nodeLocalPoolHostPathClaimCanTransition(claim, cluster, nodeLocalPoolName, expectedPaths) {
						conflicts[fmt.Sprintf(
							"node %s: pool %s conflicts with its existing durable HostPath claim owned by %s/%s pool %s",
							nodeName, nodeLocalPoolName, claim.ClusterNamespace, claim.ClusterName, claim.NodeLocalPoolName,
						)] = struct{}{}
					}
					continue
				}
				for _, pair := range overlappingNodeLocalPoolHostPaths(expectedPaths, claim.HostPaths) {
					conflicts[fmt.Sprintf(
						"node %s: pool %s path %s overlaps durable claim %s/%s pool %s path %s",
						nodeName, nodeLocalPoolName, pair[0], claim.ClusterNamespace, claim.ClusterName, claim.NodeLocalPoolName, pair[1],
					)] = struct{}{}
				}
			}
		}
	}
	otherClusters := &garagev1beta2.GarageClusterList{}
	if err := r.nodeLocalPoolReader().List(ctx, otherClusters); err != nil {
		return nil, fmt.Errorf("listing GarageClusters: %w", err)
	}
	for i := range otherClusters.Items {
		other := &otherClusters.Items[i]
		if other.Namespace == cluster.Namespace && other.Name == cluster.Name {
			continue
		}
		if other.Spec.Storage == nil {
			continue
		}
		for j := range other.Spec.Storage.NodeLocalPools {
			otherPool := &other.Spec.Storage.NodeLocalPools[j]
			selector, err := metav1.LabelSelectorAsSelector(&otherPool.Selector)
			if err != nil {
				// A malformed foreign object cannot be activated by this
				// controller. Admission normally makes this unreachable; do not
				// let it deny service to otherwise unrelated clusters.
				continue
			}
			otherPaths := nodeLocalPoolHostPaths(otherPool)
			for nodeLocalPoolName, state := range states {
				pathPairs := overlappingNodeLocalPoolHostPaths(nodeLocalPoolHostPaths(state.pool), otherPaths)
				if len(pathPairs) == 0 {
					continue
				}
				for nodeName, node := range state.desiredNodes {
					foreignActivated := nodeLocalPoolActivationValueIsActive(
						node.Labels[nodeLocalPoolActivationLabel(other, otherPool.Name)],
					)
					foreignPinned := canonicalGarageNodeID(
						node.Annotations[nodeLocalPoolRecoveryNodeIDAnnotation(other, otherPool.Name)],
					) != ""
					if !selector.Matches(labels.Set(node.Labels)) && !foreignActivated && !foreignPinned {
						continue
					}
					for _, pair := range pathPairs {
						conflicts[fmt.Sprintf(
							"node %s: pool %s path %s overlaps %s/%s pool %s path %s",
							nodeName, nodeLocalPoolName, pair[0], other.Namespace, other.Name, otherPool.Name, pair[1],
						)] = struct{}{}
					}
				}
			}
		}
	}

	foreignDaemonSets := &appsv1.DaemonSetList{}
	if err := r.nodeLocalPoolReader().List(ctx, foreignDaemonSets, nodeLocalObjects); err != nil {
		return nil, fmt.Errorf("listing node-local-pool DaemonSets: %w", err)
	}
	for i := range foreignDaemonSets.Items {
		daemonSet := &foreignDaemonSets.Items[i]
		owner := metav1.GetControllerOf(daemonSet)
		if owner == nil || owner.Kind != kindGarageCluster ||
			!strings.HasPrefix(owner.APIVersion, garagev1beta2.GroupVersion.Group+"/") {
			continue
		}
		if daemonSet.Namespace == cluster.Namespace && owner.UID == cluster.UID {
			continue
		}
		nodeLocalPoolName := daemonSet.Labels[labelNodeLocalPool]
		activationLabel := daemonSet.Annotations[annotationNodeLocalPoolActivationLabel]
		if nodeLocalPoolName == "" || activationLabel == "" {
			continue
		}
		sharesDesiredNode := false
		for _, state := range states {
			for _, node := range state.desiredNodes {
				if nodeLocalPoolActivationValueIsActive(node.Labels[activationLabel]) {
					sharesDesiredNode = true
					break
				}
			}
			if sharesDesiredNode {
				break
			}
		}
		if !sharesDesiredNode {
			continue
		}
		layout, err := storageDiskLayoutFromDaemonSet(daemonSet)
		if err != nil {
			return nil, fmt.Errorf("reading foreign node-local-pool workload %s/%s disk layout: %w", daemonSet.Namespace, daemonSet.Name, err)
		}
		foreignPaths := storageDiskLayoutHostPaths(layout)
		for ownNodeLocalPoolName, state := range states {
			pathPairs := overlappingNodeLocalPoolHostPaths(nodeLocalPoolHostPaths(state.pool), foreignPaths)
			if len(pathPairs) == 0 {
				continue
			}
			for nodeName, node := range state.desiredNodes {
				if !nodeLocalPoolActivationValueIsActive(node.Labels[activationLabel]) {
					continue
				}
				for _, pair := range pathPairs {
					conflicts[fmt.Sprintf(
						"node %s: pool %s path %s overlaps retained workload %s/%s pool %s path %s",
						nodeName, ownNodeLocalPoolName, pair[0], daemonSet.Namespace, daemonSet.Name, nodeLocalPoolName, pair[1],
					)] = struct{}{}
				}
			}
		}
	}

	// A node-local-pool GarageNode is durable ownership evidence even if its
	// private activation label was removed for lost-source recovery. Resolve its
	// parent pool or retained DaemonSet layout and keep overlapping paths fenced.
	foreignGarageNodes := &garagev1beta1.GarageNodeList{}
	if err := r.nodeLocalPoolReader().List(ctx, foreignGarageNodes, nodeLocalObjects); err != nil {
		return nil, fmt.Errorf("listing node-local-pool GarageNodes for HostPath ownership: %w", err)
	}
	for i := range foreignGarageNodes.Items {
		garageNode := &foreignGarageNodes.Items[i]
		if garageNode.Spec.Backing != garagev1beta1.NodeBackingNodeLocalPool ||
			garageNode.Spec.KubernetesNodeName == "" || garageNode.Spec.NodeLocalPoolName == "" {
			continue
		}
		currentClusterNode := garageNode.Namespace == cluster.Namespace &&
			garageNode.Spec.ClusterRef.Name == cluster.Name && metav1.IsControlledBy(garageNode, cluster)
		if currentClusterNode {
			continue
		}

		var foreignPaths []string
		for j := range otherClusters.Items {
			other := &otherClusters.Items[j]
			if other.Namespace != garageNode.Namespace || other.Name != garageNode.Spec.ClusterRef.Name || other.Spec.Storage == nil {
				continue
			}
			for k := range other.Spec.Storage.NodeLocalPools {
				if other.Spec.Storage.NodeLocalPools[k].Name == garageNode.Spec.NodeLocalPoolName {
					foreignPaths = nodeLocalPoolHostPaths(&other.Spec.Storage.NodeLocalPools[k])
					break
				}
			}
			break
		}
		if len(foreignPaths) == 0 {
			for j := range foreignDaemonSets.Items {
				daemonSet := &foreignDaemonSets.Items[j]
				if daemonSet.Namespace != garageNode.Namespace ||
					daemonSet.Labels[labelCluster] != garageNode.Spec.ClusterRef.Name ||
					daemonSet.Labels[labelNodeLocalPool] != garageNode.Spec.NodeLocalPoolName {
					continue
				}
				layout, err := storageDiskLayoutFromDaemonSet(daemonSet)
				if err != nil {
					return nil, fmt.Errorf("reading retained workload %s/%s for GarageNode %s/%s: %w",
						daemonSet.Namespace, daemonSet.Name, garageNode.Namespace, garageNode.Name, err)
				}
				foreignPaths = storageDiskLayoutHostPaths(layout)
				break
			}
		}
		for ownNodeLocalPoolName, state := range states {
			if state.desiredNodes[garageNode.Spec.KubernetesNodeName] == nil {
				continue
			}
			if len(foreignPaths) == 0 {
				conflicts[fmt.Sprintf(
					"node %s: pool %s cannot prove disjoint HostPaths from retained GarageNode %s/%s cluster %s pool %s",
					garageNode.Spec.KubernetesNodeName, ownNodeLocalPoolName, garageNode.Namespace, garageNode.Name,
					garageNode.Spec.ClusterRef.Name, garageNode.Spec.NodeLocalPoolName,
				)] = struct{}{}
				continue
			}
			for _, pair := range overlappingNodeLocalPoolHostPaths(nodeLocalPoolHostPaths(state.pool), foreignPaths) {
				conflicts[fmt.Sprintf(
					"node %s: pool %s path %s overlaps retained GarageNode %s/%s cluster %s pool %s path %s",
					garageNode.Spec.KubernetesNodeName, ownNodeLocalPoolName, pair[0], garageNode.Namespace, garageNode.Name,
					garageNode.Spec.ClusterRef.Name, garageNode.Spec.NodeLocalPoolName, pair[1],
				)] = struct{}{}
			}
		}
	}

	// Pod state is the final source of truth for a mounted HostPath. An
	// activation label and even the DaemonSet can disappear while a slow or
	// terminating pod still has the directory mounted. Inspect the actual pod
	// volumes so a second GarageCluster cannot claim an overlapping parent or
	// child path until the foreign pod is truly gone.
	foreignPods := &corev1.PodList{}
	if err := r.nodeLocalPoolReader().List(ctx, foreignPods, nodeLocalObjects); err != nil {
		return nil, fmt.Errorf("listing live node-local-pool pods for HostPath ownership: %w", err)
	}
	currentDaemonSetUIDs, err := r.ownedNodeLocalPoolDaemonSetUIDs(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("listing current node-local-pool workloads for HostPath ownership: %w", err)
	}
	for i := range foreignPods.Items {
		pod := &foreignPods.Items[i]
		if pod.Spec.NodeName == "" || pod.Labels[labelNodeLocalPool] == "" ||
			pod.Labels[labelTier] != tierStorage ||
			pod.Labels[labelStorageGroup] != storageGroupNodeLocal ||
			pod.Labels[labelAppManagedBy] != operatorName {
			continue
		}
		nodeLocalPoolName := pod.Labels[labelNodeLocalPool]
		// Names and labels are reusable. Only a pod controlled by the exact live
		// DaemonSet UID owned by this GarageCluster incarnation may be excluded
		// from the foreign-mount fence. A force-deleted/recreated same-name
		// GarageCluster must continue to see a lingering old pod as foreign until
		// that process and its HostPath mounts are actually gone.
		currentClusterPod := pod.Namespace == cluster.Namespace &&
			pod.Labels[labelCluster] == cluster.Name &&
			isStorageDaemonSetPodForPoolUID(cluster, nodeLocalPoolName, currentDaemonSetUIDs[nodeLocalPoolName], pod)
		foreignPaths := nodeLocalPoolPodHostPaths(pod)
		if len(foreignPaths) == 0 {
			continue
		}
		for ownNodeLocalPoolName, state := range states {
			if state.desiredNodes[pod.Spec.NodeName] == nil {
				continue
			}
			if currentClusterPod && pod.Labels[labelNodeLocalPool] == ownNodeLocalPoolName {
				continue
			}
			pathPairs := overlappingNodeLocalPoolHostPaths(nodeLocalPoolHostPaths(state.pool), foreignPaths)
			for _, pair := range pathPairs {
				conflicts[fmt.Sprintf(
					"node %s: pool %s path %s overlaps mounted pod %s/%s cluster %s pool %s path %s",
					pod.Spec.NodeName, ownNodeLocalPoolName, pair[0], pod.Namespace, pod.Name,
					pod.Labels[labelCluster], pod.Labels[labelNodeLocalPool], pair[1],
				)] = struct{}{}
			}
			if currentClusterPod && len(pathPairs) == 0 {
				conflicts[fmt.Sprintf(
					"node %s: pool %s cannot activate while previous pool pod %s/%s from pool %s still exists",
					pod.Spec.NodeName, ownNodeLocalPoolName, pod.Namespace, pod.Name, pod.Labels[labelNodeLocalPool],
				)] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(conflicts))
	for conflict := range conflicts {
		out = append(out, conflict)
	}
	sort.Strings(out)
	return out, nil
}

func nodeLocalPoolPodHostPaths(pod *corev1.Pod) []string {
	if pod == nil {
		return nil
	}
	var paths []string
	for i := range pod.Spec.Volumes {
		volume := &pod.Spec.Volumes[i]
		if volume.HostPath == nil ||
			(volume.Name != metadataVolName && volume.Name != dataVolName && !strings.HasPrefix(volume.Name, dataVolName+"-")) {
			continue
		}
		paths = append(paths, path.Clean(volume.HostPath.Path))
	}
	sort.Strings(paths)
	return paths
}

func nodeLocalPoolHostPaths(pool *garagev1beta2.NodeLocalPoolSpec) []string {
	if pool == nil {
		return nil
	}
	paths := make([]string, 0, 1+len(pool.DataPaths))
	if pool.Metadata != nil && pool.Metadata.HostPath != "" {
		paths = append(paths, path.Clean(pool.Metadata.HostPath))
	}
	if pool.Data != nil && pool.Data.HostPath != "" {
		paths = append(paths, path.Clean(pool.Data.HostPath))
	}
	for i := range pool.DataPaths {
		if pool.DataPaths[i].HostPath != "" {
			paths = append(paths, path.Clean(pool.DataPaths[i].HostPath))
		}
	}
	sort.Strings(paths)
	return paths
}

func storageDiskLayoutHostPaths(layout nodeLocalPoolDiskLayout) []string {
	paths := make([]string, 0, 1+len(layout.DataPaths))
	paths = append(paths, path.Clean(layout.MetadataHostPath))
	for i := range layout.DataPaths {
		paths = append(paths, path.Clean(layout.DataPaths[i].HostPath))
	}
	sort.Strings(paths)
	return paths
}

func overlappingNodeLocalPoolHostPaths(left, right []string) [][2]string {
	var overlaps [][2]string
	for _, leftPath := range left {
		for _, rightPath := range right {
			if nodeLocalPoolHostPathsOverlap(leftPath, rightPath) {
				overlaps = append(overlaps, [2]string{leftPath, rightPath})
			}
		}
	}
	return overlaps
}

func nodeLocalPoolHostPathsOverlap(left, right string) bool {
	left = path.Clean(left)
	right = path.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}
