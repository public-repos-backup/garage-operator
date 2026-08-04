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
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

type legacyEnvironmentEntry struct {
	field string
	value corev1.EnvVar
}

type legacyEnvironmentInventory struct {
	rpcSecret  []legacyEnvironmentEntry
	config     []string
	credential []string
	envFrom    []string
}

func (i *legacyEnvironmentInventory) inspect(env []corev1.EnvVar, envFrom []corev1.EnvFromSource, field string) {
	for index := range env {
		if _, reserved := operatorReservedGarageEnv[env[index].Name]; !reserved {
			continue
		}
		entryField := fmt.Sprintf("%s.env[%d]", field, index)
		if env[index].Name == envGarageRPCSecret {
			i.rpcSecret = append(i.rpcSecret, legacyEnvironmentEntry{field: entryField, value: env[index]})
			continue
		}
		if env[index].Name == envGarageConfigFile {
			i.config = append(i.config, fmt.Sprintf("%s (%s)", entryField, env[index].Name))
		} else {
			i.credential = append(i.credential, fmt.Sprintf("%s (%s)", entryField, env[index].Name))
		}
	}
	for index := range envFrom {
		if garageEnvFromCanOverrideReserved(envFrom[index]) {
			i.envFrom = append(i.envFrom, fmt.Sprintf("%s.envFrom[%d]", field, index))
		}
	}
}

func (r *GarageClusterReconciler) legacyEnvironmentReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *GarageClusterReconciler) listGarageNodesByExactClusterReference(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) ([]garagev1beta1.GarageNode, error) {
	list := &garagev1beta1.GarageNodeList{}
	if err := r.legacyEnvironmentReader().List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing GarageNodes for released environment migration: %w", err)
	}
	nodes := make([]garagev1beta1.GarageNode, 0, len(list.Items))
	for index := range list.Items {
		if garageNodeReferencesCluster(&list.Items[index], cluster) {
			nodes = append(nodes, list.Items[index])
		}
	}
	return nodes, nil
}

func (r *GarageClusterReconciler) desiredLegacyEnvironmentInventory(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) (legacyEnvironmentInventory, []garagev1beta1.GarageNode, error) {
	var inventory legacyEnvironmentInventory
	if cluster.Spec.Storage != nil {
		inventory.inspect(cluster.Spec.Storage.Env, cluster.Spec.Storage.EnvFrom, "spec.storage")
		for index := range cluster.Spec.Storage.NodeLocalPools {
			pool := &cluster.Spec.Storage.NodeLocalPools[index]
			if pool.PodTemplate != nil {
				inventory.inspect(
					pool.PodTemplate.Env, pool.PodTemplate.EnvFrom,
					fmt.Sprintf("spec.storage.nodeLocalPools[%q].podTemplate", pool.Name),
				)
			}
		}
	}
	if cluster.Spec.Gateway != nil {
		inventory.inspect(cluster.Spec.Gateway.Env, cluster.Spec.Gateway.EnvFrom, "spec.gateway")
	}
	nodes, err := r.listGarageNodesByExactClusterReference(ctx, cluster)
	if err != nil {
		return inventory, nil, err
	}
	for index := range nodes {
		node := &nodes[index]
		if node.Spec.External != nil || isNodeLocalPoolBacked(node) {
			continue
		}
		inventory.inspect(node.Spec.Env, node.Spec.EnvFrom, fmt.Sprintf("GarageNode/%s.spec", node.Name))
	}
	return inventory, nodes, nil
}

func exactControllerOwner(object metav1.Object) *metav1.OwnerReference {
	if object == nil {
		return nil
	}
	return metav1.GetControllerOf(object)
}

func ownerRefMatches(owner *metav1.OwnerReference, apiVersion, kind, name string, uid types.UID) bool {
	return owner != nil && owner.APIVersion == apiVersion && owner.Kind == kind &&
		owner.Name == name && owner.UID == uid
}

// exactManagedGaragePods inventories membership from immutable owner chains,
// never from user-mutable labels. It intentionally includes Manual GarageNodes
// that reference the cluster and cycle descendants whose controller is another
// exact GarageNode, because those processes share the same RPC identity.
func (r *GarageClusterReconciler) exactManagedGaragePods(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	nodes []garagev1beta1.GarageNode,
) ([]corev1.Pod, error) {
	reader := r.legacyEnvironmentReader()
	nodeUIDs := make(map[types.UID]garagev1beta1.GarageNode, len(nodes))
	for index := range nodes {
		nodeUIDs[nodes[index].UID] = nodes[index]
	}
	controlledByClusterOrNode := func(object metav1.Object) bool {
		owner := exactControllerOwner(object)
		if ownerRefMatches(owner, garagev1beta2.GroupVersion.String(), kindGarageCluster, cluster.Name, cluster.UID) {
			return true
		}
		if owner == nil || owner.APIVersion != garagev1beta1.GroupVersion.String() || owner.Kind != kindGarageNode {
			return false
		}
		node, found := nodeUIDs[owner.UID]
		return found && owner.Name == node.Name
	}

	workloadUIDs := make(map[types.UID]struct{})
	statefulSets := &appsv1.StatefulSetList{}
	if err := reader.List(ctx, statefulSets, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing StatefulSets for released environment migration: %w", err)
	}
	for index := range statefulSets.Items {
		if controlledByClusterOrNode(&statefulSets.Items[index]) {
			workloadUIDs[statefulSets.Items[index].UID] = struct{}{}
		}
	}
	daemonSets := &appsv1.DaemonSetList{}
	if err := reader.List(ctx, daemonSets, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing DaemonSets for released environment migration: %w", err)
	}
	for index := range daemonSets.Items {
		if controlledByClusterOrNode(&daemonSets.Items[index]) {
			workloadUIDs[daemonSets.Items[index].UID] = struct{}{}
		}
	}
	deployments := &appsv1.DeploymentList{}
	if err := reader.List(ctx, deployments, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing Deployments for released environment migration: %w", err)
	}
	deploymentUIDs := make(map[types.UID]string)
	for index := range deployments.Items {
		if controlledByClusterOrNode(&deployments.Items[index]) {
			workloadUIDs[deployments.Items[index].UID] = struct{}{}
			deploymentUIDs[deployments.Items[index].UID] = deployments.Items[index].Name
		}
	}
	replicaSets := &appsv1.ReplicaSetList{}
	if err := reader.List(ctx, replicaSets, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing ReplicaSets for released environment migration: %w", err)
	}
	for index := range replicaSets.Items {
		owner := exactControllerOwner(&replicaSets.Items[index])
		if owner == nil || owner.APIVersion != appsv1.SchemeGroupVersion.String() || owner.Kind != "Deployment" {
			continue
		}
		if name, found := deploymentUIDs[owner.UID]; found && name == owner.Name {
			workloadUIDs[replicaSets.Items[index].UID] = struct{}{}
		}
	}

	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing Pods for released environment migration: %w", err)
	}
	owned := make([]corev1.Pod, 0)
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		owner := exactControllerOwner(pod)
		if owner == nil {
			continue
		}
		if _, found := workloadUIDs[owner.UID]; found {
			owned = append(owned, *pod)
		}
	}
	sort.Slice(owned, func(left, right int) bool { return owned[left].Name < owned[right].Name })
	return owned, nil
}

func garageContainerForPod(pod *corev1.Pod) *corev1.Container {
	if pod == nil {
		return nil
	}
	for index := range pod.Spec.Containers {
		if pod.Spec.Containers[index].Name == defaultAppName {
			return &pod.Spec.Containers[index]
		}
	}
	return nil
}

func garageNodeNeedsLegacyEnvironmentEvaluation(
	ctx context.Context,
	reader client.Reader,
	node *garagev1beta1.GarageNode,
	cluster *garagev1beta2.GarageCluster,
) (bool, error) {
	if node == nil || cluster == nil || node.Spec.External != nil || isNodeLocalPoolBacked(node) {
		return false, nil
	}
	if cluster.Annotations[garagev1beta1.AnnotationMigrateLegacyRPCSecret] == annotationTrue ||
		cluster.Annotations[garagev1beta1.AnnotationAcknowledgeLegacyConfigMigration] == annotationTrue {
		return true, nil
	}
	statefulSet := &appsv1.StatefulSet{}
	if err := reader.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading GarageNode StatefulSet before released environment migration: %w", err)
	}
	owner := exactControllerOwner(statefulSet)
	if !ownerRefMatches(owner, garagev1beta1.GroupVersion.String(), kindGarageNode, node.Name, node.UID) {
		return false, fmt.Errorf("StatefulSet %s/%s is not controlled by exact GarageNode UID %s", node.Namespace, node.Name, node.UID)
	}
	pod := &corev1.Pod{Spec: statefulSet.Spec.Template.Spec}
	container := garageContainerForPod(pod)
	if container == nil {
		// This fast path only decides whether a legacy-environment audit is
		// needed. A malformed/partially seeded test or pre-controller workload
		// with no Garage container carries no legacy Garage environment to
		// migrate; ordinary StatefulSet reconciliation owns its repair.
		return false, nil
	}
	for index := range container.Env {
		env := container.Env[index]
		if _, reserved := operatorReservedGarageEnv[env.Name]; reserved &&
			!podReservedEnvIsOperatorManaged(pod, env) {
			return true, nil
		}
	}
	for index := range container.EnvFrom {
		if garageEnvFromCanOverrideReserved(container.EnvFrom[index]) {
			return true, nil
		}
	}
	return false, nil
}

func podSecretKeyForMount(pod *corev1.Pod, container *corev1.Container, mountPath string) (string, string, bool) {
	if pod == nil || container == nil {
		return "", "", false
	}
	volumeName := ""
	for index := range container.VolumeMounts {
		if container.VolumeMounts[index].MountPath == mountPath {
			volumeName = container.VolumeMounts[index].Name
			break
		}
	}
	if volumeName == "" {
		return "", "", false
	}
	for index := range pod.Spec.Volumes {
		volume := &pod.Spec.Volumes[index]
		if volume.Name != volumeName || volume.Secret == nil || volume.Secret.SecretName == "" {
			continue
		}
		key := RPCSecretKey
		for itemIndex := range volume.Secret.Items {
			if volume.Secret.Items[itemIndex].Path == RPCSecretKey {
				key = volume.Secret.Items[itemIndex].Key
				break
			}
		}
		return volume.Secret.SecretName, key, true
	}
	return "", "", false
}

func podReservedEnvIsOperatorManaged(pod *corev1.Pod, env corev1.EnvVar) bool {
	if env.Name == envGarageConfigFile && env.ValueFrom == nil && env.Value == garageConfigFileLocation {
		return true
	}
	if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
		return false
	}
	selector := env.ValueFrom.SecretKeyRef
	for index := range pod.Spec.Volumes {
		secret := pod.Spec.Volumes[index].Secret
		if secret == nil || secret.SecretName != selector.Name {
			continue
		}
		key := selector.Key
		if key == "" {
			key = env.Name
		}
		for itemIndex := range secret.Items {
			if secret.Items[itemIndex].Key == key {
				return true
			}
		}
		if len(secret.Items) == 0 {
			return true
		}
	}
	return false
}

func liveLegacyEnvironmentInventory(pods []corev1.Pod, verifyEveryPodRPC bool) (legacyEnvironmentInventory, error) {
	var inventory legacyEnvironmentInventory
	for podIndex := range pods {
		pod := &pods[podIndex]
		container := garageContainerForPod(pod)
		if container == nil {
			return inventory, fmt.Errorf("exact managed Pod %s/%s has no %q container", pod.Namespace, pod.Name, defaultAppName)
		}
		lastRPC := -1
		for index := range container.Env {
			env := container.Env[index]
			if env.Name == envGarageRPCSecret {
				lastRPC = index
				continue
			}
			if _, reserved := operatorReservedGarageEnv[env.Name]; reserved &&
				!podReservedEnvIsOperatorManaged(pod, env) {
				field := fmt.Sprintf("Pod/%s.spec.containers[%q].env[%d] (%s)", pod.Name, container.Name, index, env.Name)
				if env.Name == envGarageConfigFile {
					inventory.config = append(inventory.config, field)
				} else {
					inventory.credential = append(inventory.credential, field)
				}
			}
		}
		for index := range container.EnvFrom {
			if garageEnvFromCanOverrideReserved(container.EnvFrom[index]) {
				inventory.envFrom = append(inventory.envFrom,
					fmt.Sprintf("Pod/%s.spec.containers[%q].envFrom[%d]", pod.Name, container.Name, index))
			}
		}
		if lastRPC >= 0 {
			env := container.Env[lastRPC]
			if verifyEveryPodRPC || !podReservedEnvIsOperatorManaged(pod, env) {
				inventory.rpcSecret = append(inventory.rpcSecret, legacyEnvironmentEntry{
					field: fmt.Sprintf("Pod/%s.spec.containers[%q].env[%d]", pod.Name, container.Name, lastRPC),
					value: env,
				})
			}
			continue
		}
		if !verifyEveryPodRPC {
			continue
		}
		secretName, key, found := podSecretKeyForMount(pod, container, rpcSecretMountPath)
		if !found {
			return inventory, fmt.Errorf("exact managed Pod %s/%s has neither GARAGE_RPC_SECRET nor the operator RPC Secret mount", pod.Namespace, pod.Name)
		}
		inventory.rpcSecret = append(inventory.rpcSecret, legacyEnvironmentEntry{
			field: fmt.Sprintf("Pod/%s mounted RPC Secret", pod.Name),
			value: corev1.EnvVar{Name: envGarageRPCSecret, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: key,
				},
			}},
		})
	}
	return inventory, nil
}

func (r *GarageClusterReconciler) resolveLegacyRPCEnvironment(
	ctx context.Context,
	namespace string,
	entry legacyEnvironmentEntry,
) ([]byte, string, error) {
	if entry.value.ValueFrom == nil {
		canonical, fingerprint, err := canonicalRPCIdentity([]byte(entry.value.Value))
		if err != nil {
			return nil, "", fmt.Errorf("%s contains an invalid Garage RPC credential: %w", entry.field, err)
		}
		return canonical, fingerprint, nil
	}
	if entry.value.ValueFrom.SecretKeyRef == nil {
		return nil, "", fmt.Errorf("%s uses a non-Secret valueFrom source; automatic RPC credential migration requires a literal or SecretKeyRef", entry.field)
	}
	selector := entry.value.ValueFrom.SecretKeyRef
	secret := &corev1.Secret{}
	if err := r.legacyEnvironmentReader().Get(ctx, types.NamespacedName{
		Name: selector.Name, Namespace: namespace,
	}, secret); err != nil {
		return nil, "", fmt.Errorf("reading %s Secret %s/%s: %w", entry.field, namespace, selector.Name, err)
	}
	raw, found := secret.Data[selector.Key]
	if !found {
		return nil, "", fmt.Errorf("%s Secret %s/%s has no key %q", entry.field, namespace, selector.Name, selector.Key)
	}
	canonical, fingerprint, err := canonicalRPCIdentity(raw)
	if err != nil {
		return nil, "", fmt.Errorf("%s Secret %s/%s key %q is not a Garage RPC credential: %w", entry.field, namespace, selector.Name, selector.Key, err)
	}
	return canonical, fingerprint, nil
}

func (r *GarageClusterReconciler) directNetworkRPCIdentity(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) ([]byte, string, string, error) {
	ref := cluster.Spec.Network.RPCSecretRef
	if ref == nil {
		return nil, "", "", fmt.Errorf("spec.network.rpcSecretRef is required for explicit legacy GARAGE_RPC_SECRET migration")
	}
	key := ref.Key
	if key == "" {
		key = RPCSecretKey
	}
	secret := &corev1.Secret{}
	if err := r.legacyEnvironmentReader().Get(ctx, types.NamespacedName{
		Name: ref.Name, Namespace: cluster.Namespace,
	}, secret); err != nil {
		return nil, "", "", fmt.Errorf("reading spec.network.rpcSecretRef %s/%s: %w", cluster.Namespace, ref.Name, err)
	}
	raw, found := secret.Data[key]
	if !found {
		return nil, "", "", fmt.Errorf("spec.network.rpcSecretRef Secret %s/%s has no key %q", cluster.Namespace, ref.Name, key)
	}
	canonical, fingerprint, err := canonicalRPCIdentity(raw)
	if err != nil {
		return nil, "", "", fmt.Errorf("validating spec.network.rpcSecretRef %s/%s key %q: %w", cluster.Namespace, ref.Name, key, err)
	}
	return canonical, fingerprint, cluster.Namespace + "/" + ref.Name + ":" + key, nil
}

func (r *GarageClusterReconciler) managedRPCSnapshotMatches(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	canonical []byte,
	fingerprint string,
	requireImmutable bool,
) error {
	targetName := managedRPCSecretName(cluster)
	target := &corev1.Secret{}
	err := r.legacyEnvironmentReader().Get(ctx, types.NamespacedName{
		Name: targetName, Namespace: cluster.Namespace,
	}, target)
	if apierrors.IsNotFound(err) {
		if requireImmutable {
			return fmt.Errorf("managed RPC snapshot %s/%s does not exist yet; waiting for the GarageCluster controller to create and pin it", cluster.Namespace, targetName)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading managed RPC snapshot %s/%s: %w", cluster.Namespace, targetName, err)
	}
	if !hasExactGarageClusterControllerReference(target, cluster) {
		return fmt.Errorf("managed RPC snapshot %s/%s is not controlled by exact GarageCluster UID %s", cluster.Namespace, targetName, cluster.UID)
	}
	raw, found := target.Data[RPCSecretKey]
	if !found {
		return fmt.Errorf("managed RPC snapshot %s/%s has no key %q", cluster.Namespace, targetName, RPCSecretKey)
	}
	targetCanonical, targetFingerprint, err := canonicalRPCIdentity(raw)
	if err != nil {
		return fmt.Errorf("validating managed RPC snapshot %s/%s: %w", cluster.Namespace, targetName, err)
	}
	if targetFingerprint != fingerprint || string(targetCanonical) != string(canonical) {
		return fmt.Errorf("managed RPC snapshot %s/%s has fingerprint %s, but the verified active legacy credential has %s; while every old Pod still uses GARAGE_RPC_SECRET, update this retained mutable Secret to the exact active value—the operator will never delete or overwrite mismatched identity bytes", cluster.Namespace, targetName, targetFingerprint, fingerprint)
	}
	if requireImmutable && (target.Immutable == nil || !*target.Immutable) {
		return fmt.Errorf("managed RPC snapshot %s/%s matches the active credential but is not immutable yet; waiting for the GarageCluster controller to pin it before a GarageNode rolls", cluster.Namespace, targetName)
	}
	return nil
}

type legacyEnvironmentMigrationState struct {
	blocked             bool
	message             string
	completeRPCSnapshot bool
}

func (r *GarageClusterReconciler) legacyEnvironmentMigrationNeeded(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	includeUnobservedGeneration bool,
) (bool, error) {
	if cluster == nil {
		return false, nil
	}
	if cluster.Annotations[garagev1beta1.AnnotationMigrateLegacyRPCSecret] == annotationTrue ||
		cluster.Annotations[garagev1beta1.AnnotationAcknowledgeLegacyConfigMigration] == annotationTrue ||
		(includeUnobservedGeneration && cluster.Status.ObservedGeneration < cluster.Generation) {
		return true, nil
	}
	desired, _, err := r.desiredLegacyEnvironmentInventory(ctx, cluster)
	if err != nil {
		return false, err
	}
	return len(desired.rpcSecret) > 0 || len(desired.config) > 0 ||
		len(desired.credential) > 0 || len(desired.envFrom) > 0, nil
}

func (r *GarageClusterReconciler) completeLegacyRPCEnvironmentMigration(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) error {
	key := client.ObjectKeyFromObject(cluster)
	var updated *garagev1beta2.GarageCluster
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &garagev1beta2.GarageCluster{}
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		if fresh.UID != cluster.UID {
			return fmt.Errorf("GarageCluster UID changed while completing released RPC environment migration")
		}
		if fresh.Annotations[garagev1beta1.AnnotationMigrateLegacyRPCSecret] != annotationTrue {
			updated = fresh
			return nil
		}
		delete(fresh.Annotations, garagev1beta1.AnnotationMigrateLegacyRPCSecret)
		if err := r.Update(ctx, fresh); err != nil {
			return err
		}
		updated = fresh
		return nil
	}); err != nil {
		return fmt.Errorf("removing completed %s annotation: %w", garagev1beta1.AnnotationMigrateLegacyRPCSecret, err)
	}
	if updated != nil {
		adoptGarageClusterSnapshot(cluster, updated)
	}
	return nil
}

// evaluateLegacyEnvironmentMigration never mutates workloads. It proves the
// active credential from exact owner-derived Pods and desired GarageNode specs,
// and distinguishes a verifiable RPC migration from a semantic config
// attestation. The parent controller may adopt the matching retained Secret;
// GarageNode controllers require that snapshot to be immutable first.
func (r *GarageClusterReconciler) evaluateLegacyEnvironmentMigration(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	requireImmutableSnapshot bool,
) (legacyEnvironmentMigrationState, error) {
	state := legacyEnvironmentMigrationState{}
	desired, nodes, err := r.desiredLegacyEnvironmentInventory(ctx, cluster)
	if err != nil {
		return state, err
	}
	pods, err := r.exactManagedGaragePods(ctx, cluster, nodes)
	if err != nil {
		return state, err
	}
	migrationRequested := cluster.Annotations[garagev1beta1.AnnotationMigrateLegacyRPCSecret] == annotationTrue
	live, err := liveLegacyEnvironmentInventory(pods, migrationRequested || len(desired.rpcSecret) > 0)
	if err != nil {
		return state, err
	}

	if len(desired.envFrom) > 0 || len(live.envFrom) > 0 {
		unsafe := append(append([]string(nil), desired.envFrom...), live.envFrom...)
		sort.Strings(unsafe)
		return state, fmt.Errorf("released envFrom sources capable of injecting Garage config or credentials remain desired or active (%s); Kubernetes does not record the resolved startup bytes, so an acknowledgement cannot prove RPC identity. Remove or convert these sources under the previous operator before upgrading, or keep workloads frozen for an explicit manual migration", strings.Join(unsafe, ", "))
	}
	if len(desired.credential) > 0 || len(live.credential) > 0 {
		unsafe := append(append([]string(nil), desired.credential...), live.credential...)
		sort.Strings(unsafe)
		return state, fmt.Errorf("released non-RPC credential overrides remain desired or active (%s); a config acknowledgement cannot authorize credential replacement. Migrate each value to its typed Secret reference with exact byte equality before allowing workloads to roll", strings.Join(unsafe, ", "))
	}
	if len(desired.config) > 0 {
		sort.Strings(desired.config)
		return state, fmt.Errorf("released reserved config overrides remain in the desired API (%s); remove them before reconciliation. If exact old Pods still use the override afterward, set %s=true only after auditing semantic equivalence with the operator-rendered config", strings.Join(desired.config, ", "), garagev1beta1.AnnotationAcknowledgeLegacyConfigMigration)
	}
	if len(live.config) > 0 && cluster.Annotations[garagev1beta1.AnnotationAcknowledgeLegacyConfigMigration] != annotationTrue {
		sort.Strings(live.config)
		return state, fmt.Errorf("exact existing Pods still use released config overrides (%s); audit their effective Garage configuration, then set %s=true to attest semantic equivalence before the coordinated rollout", strings.Join(live.config, ", "), garagev1beta1.AnnotationAcknowledgeLegacyConfigMigration)
	}

	rpcEntries := append(append([]legacyEnvironmentEntry(nil), desired.rpcSecret...), live.rpcSecret...)
	if len(rpcEntries) == 0 && !migrationRequested {
		return state, nil
	}
	if !migrationRequested && len(desired.rpcSecret) > 0 {
		return state, fmt.Errorf("released GARAGE_RPC_SECRET overrides remain; stage spec.network.rpcSecretRef with the exact same credential and set %s=true before removing the overrides", garagev1beta1.AnnotationMigrateLegacyRPCSecret)
	}

	canonical, fingerprint, descriptor, err := r.directNetworkRPCIdentity(ctx, cluster)
	if err != nil {
		return state, err
	}
	for index := range rpcEntries {
		actual, actualFingerprint, err := r.resolveLegacyRPCEnvironment(ctx, cluster.Namespace, rpcEntries[index])
		if err != nil {
			return state, err
		}
		if actualFingerprint != fingerprint || string(actual) != string(canonical) {
			return state, fmt.Errorf("%s has Garage RPC fingerprint %s, but spec.network.rpcSecretRef %s has %s; refusing a credential rotation or split mesh", rpcEntries[index].field, actualFingerprint, descriptor, fingerprint)
		}
	}
	if err := r.managedRPCSnapshotMatches(ctx, cluster, canonical, fingerprint, requireImmutableSnapshot); err != nil {
		return state, err
	}
	if len(desired.rpcSecret) > 0 {
		state.blocked = true
		state.message = fmt.Sprintf("verified every exact managed Garage RPC environment against %s (fingerprint %s) and the retained managed snapshot; remove the released GARAGE_RPC_SECRET entries now, leaving %s=true until the controller pins the snapshot", descriptor, fingerprint, garagev1beta1.AnnotationMigrateLegacyRPCSecret)
		return state, nil
	}
	state.completeRPCSnapshot = migrationRequested
	return state, nil
}
