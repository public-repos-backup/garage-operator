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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

// Storage-DaemonSet workload (spec.storage.workload: DaemonSet): one Garage
// storage pod per matching Kubernetes node, with hostPath-backed metadata and
// data directories. Identity is durable per physical node — the hostPath
// metadata dir holds the node_key, so a rescheduled pod on the same node
// rejoins the layout with the same node_id and no data resync.
//
// Split of responsibility:
//   - The cluster-owned DaemonSet (<cr>-storage) owns the pods.
//   - One GarageNode per matching K8s node (DaemonSet-backed mode) owns that
//     node's layout role and drains it when the K8s Node is deleted.

// isDaemonSetStorage reports whether the cluster's storage tier runs as a
// DaemonSet workload.
func isDaemonSetStorage(cluster *garagev1beta2.GarageCluster) bool {
	return cluster.HasStorageTier() && cluster.Spec.Storage.EffectiveWorkload() == garagev1beta2.WorkloadTypeDaemonSet
}

// storageDaemonSetName returns the canonical name of the cluster-owned
// storage DaemonSet.
func storageDaemonSetName(cluster *garagev1beta2.GarageCluster) string {
	return cluster.Name + "-storage"
}

// reconcileStorageDaemonSet creates or updates the cluster-owned storage
// DaemonSet. Update gating mirrors reconcileGatewayStatefulSet: pods roll only
// when the config hash or pod-spec hash changes.
func (r *GarageClusterReconciler) reconcileStorageDaemonSet(ctx context.Context, cluster *garagev1beta2.GarageCluster, configHash string) error {
	log := logf.FromContext(ctx)
	st := cluster.Spec.Storage
	if st == nil {
		return nil
	}

	name := storageDaemonSetName(cluster)
	image := resolveGarageImage(cluster.Spec.Image, cluster.Spec.ImageRepository, r.DefaultImage)
	containerPorts := buildContainerPorts(cluster)
	volumes, volumeMounts := buildStorageDaemonSetVolumesAndMounts(cluster)

	podSpec := buildGaragePodSpec(PodSpecConfig{
		Image:                     image,
		ImagePullPolicy:           cluster.Spec.ImagePullPolicy,
		ImagePullSecrets:          cluster.Spec.ImagePullSecrets,
		Resources:                 st.Resources,
		NodeSelector:              st.NodeSelector,
		Tolerations:               st.Tolerations,
		Affinity:                  st.Affinity,
		PriorityClassName:         st.PriorityClassName,
		ServiceAccountName:        cluster.Spec.ServiceAccountName,
		SecurityContext:           st.SecurityContext,
		ContainerSecurityContext:  st.ContainerSecurityContext,
		TopologySpreadConstraints: st.TopologySpreadConstraints,
		Logging:                   cluster.Spec.Logging,
		Env:                       st.Env,
		EnvFrom:                   st.EnvFrom,
	}, volumes, volumeMounts, containerPorts)

	// Pod labels: tier selector labels plus labelCluster so the headless RPC
	// service, primary API service and the GarageNode controller's DaemonSet
	// pod discovery ({labelCluster, labelTier=storage}) all match these pods.
	podLabels := r.selectorLabelsForTier(cluster, tierStorage)
	podLabels[labelCluster] = cluster.Name
	for k, v := range st.PodLabels {
		podLabels[k] = v
	}

	podSpecHashStr := computePodSpecHash(podSpec, st.PodAnnotations, st.PodLabels)

	podAnnotations := make(map[string]string)
	for k, v := range st.PodAnnotations {
		podAnnotations[k] = v
	}
	podAnnotations["garage.rajsingh.info/config-hash"] = configHash
	podAnnotations["garage.rajsingh.info/pod-spec-hash"] = podSpecHashStr

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels:    r.labelsForTier(cluster, tierStorage),
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: r.selectorLabelsForTier(cluster, tierStorage)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: podAnnotations},
				Spec:       podSpec,
			},
		},
	}

	if err := controllerutil.SetControllerReference(cluster, ds, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.DaemonSet{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating storage DaemonSet", "name", name)
		return r.Create(ctx, ds)
	}
	if err != nil {
		return err
	}

	needsUpdate := !equality.Semantic.DeepEqual(existing.Labels, ds.Labels) || !metav1.IsControlledBy(existing, cluster)
	if existing.Spec.Template.Annotations["garage.rajsingh.info/config-hash"] != configHash ||
		existing.Spec.Template.Annotations["garage.rajsingh.info/pod-spec-hash"] != podSpecHashStr {
		needsUpdate = true
	}
	if !needsUpdate {
		return nil
	}
	existing.Spec.Template = ds.Spec.Template
	// Selector is immutable and intentionally left untouched.
	existing.Labels = ds.Labels
	existing.OwnerReferences = ds.OwnerReferences
	log.Info("Updating storage DaemonSet", "name", name)
	return r.Update(ctx, existing)
}

// daemonSetGarageNodeName returns the name of the DaemonSet-backed GarageNode
// for a given K8s node. The "-ds-" infix keeps the namespace disjoint from
// auto-mode ordinal names (<cluster>-storage-<N>).
func daemonSetGarageNodeName(clusterName, k8sNodeName string) string {
	return clusterName + "-ds-" + k8sNodeName
}

// reconcileDaemonSetStorageNodes ensures one DaemonSet-backed GarageNode per
// Kubernetes node matching the storage tier's nodeSelector. GarageNodes are
// keyed by K8s node name (stable across pod churn): a pod reboot leaves the
// GarageNode — and therefore the layout role — untouched, while deleting the
// K8s Node removes the GarageNode and lets its finalizer drain the role.
func (r *GarageClusterReconciler) reconcileDaemonSetStorageNodes(ctx context.Context, cluster *garagev1beta2.GarageCluster) error {
	log := logf.FromContext(ctx)
	st := cluster.Spec.Storage
	if st == nil {
		return nil
	}

	k8sNodes := &corev1.NodeList{}
	var listOpts []client.ListOption
	if len(st.NodeSelector) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(st.NodeSelector))
	}
	if err := r.List(ctx, k8sNodes, listOpts...); err != nil {
		return fmt.Errorf("listing Kubernetes nodes: %w", err)
	}

	existing, err := r.listAutoModeStorageNodes(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing operator-owned storage GarageNodes: %w", err)
	}

	desiredByName := make(map[string]bool, len(k8sNodes.Items))
	for i := range k8sNodes.Items {
		k8sNode := &k8sNodes.Items[i]
		desired, err := r.buildDaemonSetStorageNode(cluster, k8sNode)
		if err != nil {
			return fmt.Errorf("building desired GarageNode for K8s node %s: %w", k8sNode.Name, err)
		}
		desiredByName[desired.Name] = true

		current, found := existing[desired.Name]
		if !found {
			log.Info("Creating DaemonSet-backed GarageNode", "name", desired.Name, "k8sNode", k8sNode.Name)
			if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("creating GarageNode %s: %w", desired.Name, err)
			}
			continue
		}

		// Update in place on drift — never delete+recreate, which would run the
		// finalizer and drain/resync the node's layout role.
		if daemonSetStorageNodeNeedsUpdate(current, desired) {
			log.Info("Updating DaemonSet-backed GarageNode (drift detected)", "name", desired.Name)
			current.Spec.Zone = desired.Spec.Zone
			current.Spec.Capacity = desired.Spec.Capacity
			current.Spec.Tags = desired.Spec.Tags
			current.Spec.Backing = desired.Spec.Backing
			current.Spec.KubernetesNodeName = desired.Spec.KubernetesNodeName
			if err := r.Update(ctx, current); err != nil {
				return fmt.Errorf("updating GarageNode %s: %w", current.Name, err)
			}
		}
	}

	// GarageNodes whose K8s node is gone fall out of the desired set;
	// deleting them triggers the per-node finalizer, which drains the layout
	// role. spec.storage.workload is immutable (webhook-enforced), so a
	// leftover ordinal (StatefulSet-shaped) GarageNode should never appear
	// here in normal operation — this also defensively cleans one up if it
	// does (e.g. a stray object predating the immutability rule).
	var toDelete []*garagev1beta1.GarageNode
	for name, n := range existing {
		if desiredByName[name] {
			continue
		}
		toDelete = append(toDelete, n)
	}

	// Refuse a scale-down that would drop the cluster below its replication
	// factor — same guard as reconcileAutoModeStorageNodes. Without it, draining
	// K8s Nodes (the documented way to scale DaemonSet storage down) could
	// delete GarageNode CRs faster than the per-node finalizer's
	// IsReplicationConstraint backstop can refuse the layout-role removal,
	// permanently orphaning the role. See federation caveat there: this only
	// applies to a standalone cluster, since a federated factor is satisfied
	// across all regions' storage nodes.
	if len(toDelete) > 0 && len(cluster.Spec.RemoteClusters) == 0 {
		factor := replicationFactorOf(cluster)
		// Unlike Auto-mode's per-ordinal PVC sizing, DaemonSet storage capacity
		// is required and applied uniformly to every node (webhook-enforced), so
		// every desired node counts as surviving unless it's already mid-deletion
		// in the pre-loop snapshot. This also correctly counts a node created
		// earlier in THIS reconcile (e.g. a K8s Node that just joined and was
		// created above in the same pass, so it doesn't exist yet in
		// `existing`) — using countLiveStorageNodes here would undercount it
		// and spuriously block what is really a scale-up, not a scale-down.
		surviving := 0
		for name := range desiredByName {
			if n, ok := existing[name]; ok && !n.DeletionTimestamp.IsZero() {
				continue
			}
			surviving++
		}
		if factor > 0 && surviving < factor {
			msg := fmt.Sprintf("refusing to scale storage down to %d GarageNode(s): %d live node(s) with positive capacity would remain, below replication.factor %d "+
				"(removing them would orphan their Garage layout roles). Lower spec.replication.factor, restore the K8s Node(s), or adjust storage.nodeSelector; the excess GarageNode(s) are kept.",
				len(desiredByName), surviving, factor)
			log.Info("DaemonSet storage scale-down blocked", "survivingLiveNodes", surviving, "replicationFactor", factor)
			return r.setScaleDownBlockedCondition(ctx, cluster, metav1.ConditionTrue, garagev1beta1.ReasonScaleDownWouldBreakQuorum, msg)
		}
	}

	for _, n := range toDelete {
		log.Info("Deleting DaemonSet-backed GarageNode (K8s node gone)", "name", n.Name)
		if err := r.Delete(ctx, n); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting GarageNode %s: %w", n.Name, err)
		}
	}

	return r.setScaleDownBlockedCondition(ctx, cluster, metav1.ConditionFalse, garagev1beta1.ReasonScaleDownSafe, "storage scale-down within replication factor")
}

// daemonSetStorageNodeNeedsUpdate reports drift on the fields the operator
// owns for DaemonSet-backed GarageNodes.
func daemonSetStorageNodeNeedsUpdate(current, desired *garagev1beta1.GarageNode) bool {
	if current.Spec.Zone != desired.Spec.Zone {
		return true
	}
	if (current.Spec.Capacity == nil) != (desired.Spec.Capacity == nil) {
		return true
	}
	if current.Spec.Capacity != nil && desired.Spec.Capacity != nil && current.Spec.Capacity.Cmp(*desired.Spec.Capacity) != 0 {
		return true
	}
	if !tagSetEqual(current.Spec.Tags, desired.Spec.Tags) {
		return true
	}
	if current.Spec.Backing != desired.Spec.Backing || current.Spec.KubernetesNodeName != desired.Spec.KubernetesNodeName {
		return true
	}
	return false
}

// buildDaemonSetStorageNode constructs the desired DaemonSet-backed GarageNode
// for a Kubernetes node. Zone comes from the node's topology label with the
// cluster spec.zone as fallback; capacity is the uniform spec.storage.capacity.
// No Spec.Storage is set: the GarageNode owns no StatefulSet or PVCs — the
// cluster-owned DaemonSet provides the pod and its hostPath volumes.
func (r *GarageClusterReconciler) buildDaemonSetStorageNode(cluster *garagev1beta2.GarageCluster, k8sNode *corev1.Node) (*garagev1beta1.GarageNode, error) {
	name := daemonSetGarageNodeName(cluster.Name, k8sNode.Name)

	zone := k8sNode.Labels[corev1.LabelTopologyZone]
	if zone == "" {
		zone = cluster.Spec.Zone
	}
	if zone == "" {
		zone = defaultZoneName
	}

	var capacity *resource.Quantity
	if cluster.Spec.Storage.Capacity != nil {
		c := cluster.Spec.Storage.Capacity.DeepCopy()
		capacity = &c
	}

	node := &garagev1beta1.GarageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				labelCluster:      cluster.Name,
				labelTier:         tierStorage,
				labelAppManagedBy: managedByOperatorValue,
			},
		},
		Spec: garagev1beta1.GarageNodeSpec{
			ClusterRef:         garagev1beta1.ClusterReference{Name: cluster.Name},
			Backing:            garagev1beta1.NodeBackingDaemonSet,
			KubernetesNodeName: k8sNode.Name,
			Zone:               zone,
			Capacity:           capacity,
			Tags:               buildNodeTags(cluster.Name, cluster.Namespace, tierStorage, cluster.Spec.DefaultNodeTags, name),
		},
	}

	if err := controllerutil.SetControllerReference(cluster, node, r.Scheme); err != nil {
		return nil, err
	}
	return node, nil
}

// deleteStorageDaemonSet removes the cluster-owned storage DaemonSet. Called
// whenever the storage tier isn't DaemonSet-workload (no storage tier, or
// StatefulSet workload) — a no-op in the common case, since
// spec.storage.workload is immutable and a StatefulSet-workload cluster never
// had one; defensive cleanup if a DaemonSet exists anyway.
func (r *GarageClusterReconciler) deleteStorageDaemonSet(ctx context.Context, cluster *garagev1beta2.GarageCluster) error {
	log := logf.FromContext(ctx)
	existing := &appsv1.DaemonSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: storageDaemonSetName(cluster), Namespace: cluster.Namespace}, existing); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	log.Info("Removing storage DaemonSet", "name", existing.Name)
	if err := r.Delete(ctx, existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// buildStorageDaemonSetVolumesAndMounts returns the volumes and mounts for a
// storage-DaemonSet pod: shared ConfigMap, RPC secret, and hostPath-backed
// metadata/data directories (DirectoryOrCreate so first boot on a fresh node
// works without manual provisioning).
func buildStorageDaemonSetVolumesAndMounts(cluster *garagev1beta2.GarageCluster) ([]corev1.Volume, []corev1.VolumeMount) {
	st := cluster.Spec.Storage

	mounts := []corev1.VolumeMount{
		{Name: configVolumeName, MountPath: configMountPath, ReadOnly: true},
		{Name: RPCSecretKey, MountPath: rpcSecretMountPath, ReadOnly: true},
		{Name: metadataVolName, MountPath: metadataPath},
		{Name: dataVolName, MountPath: dataPath},
	}

	rpcSecretName := cluster.Name + "-rpc-secret"
	rpcSecretKey := RPCSecretKey
	if cluster.Spec.Network.RPCSecretRef != nil {
		rpcSecretName = cluster.Spec.Network.RPCSecretRef.Name
		if cluster.Spec.Network.RPCSecretRef.Key != "" {
			rpcSecretKey = cluster.Spec.Network.RPCSecretRef.Key
		}
	}

	hostPathType := corev1.HostPathDirectoryOrCreate
	var metaHostPath, dataHostPath string
	if st.Metadata != nil {
		metaHostPath = st.Metadata.HostPath
	}
	if st.Data != nil {
		dataHostPath = st.Data.HostPath
	}

	volumes := []corev1.Volume{
		{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cluster.Name + "-config"},
				},
			},
		},
		{
			Name: RPCSecretKey,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  rpcSecretName,
					DefaultMode: ptr.To[int32](0600),
					Items:       []corev1.KeyToPath{{Key: rpcSecretKey, Path: RPCSecretKey}},
				},
			},
		},
		{
			Name: metadataVolName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: metaHostPath, Type: &hostPathType},
			},
		},
		{
			Name: dataVolName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: dataHostPath, Type: &hostPathType},
			},
		},
	}

	if cluster.Spec.Admin != nil && cluster.Spec.Admin.AdminTokenSecretRef != nil {
		adminTokenKey := DefaultAdminTokenKey
		if cluster.Spec.Admin.AdminTokenSecretRef.Key != "" {
			adminTokenKey = cluster.Spec.Admin.AdminTokenSecretRef.Key
		}
		volumes = append(volumes, corev1.Volume{
			Name: DefaultAdminTokenKey,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  cluster.Spec.Admin.AdminTokenSecretRef.Name,
					DefaultMode: ptr.To[int32](0600),
					Items:       []corev1.KeyToPath{{Key: adminTokenKey, Path: DefaultAdminTokenKey}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      DefaultAdminTokenKey,
			MountPath: adminSecretMountPath,
			ReadOnly:  true,
		})
	}

	return volumes, mounts
}
