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
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const (
	// Garage encodes positive-capacity roles in a u8 and accepts at most 256.
	// The operator supports 255 desired node-local identities. When the live
	// layout already contains 255 roles, a new activation is eligible only when
	// this control plane can prove a retiring generated identity. This does not
	// reserve Garage's final role across independently operated federated sites.
	maxNodeLocalPoolMembers = 255
)

var errNodeLocalPoolGarageRoleLimit = errors.New("garage positive-capacity role limit reached")

type nodeLocalPoolMembership struct {
	desiredNodesByPool map[string]map[string]*corev1.Node
	poolByNode         map[string]string
	emptyPools         []string
	selectorConflicts  []string
	selectedMembers    int
}

type compiledNodeLocalPoolSelector struct {
	name     string
	selector labels.Selector
}

func (r *GarageClusterReconciler) readNodeLocalPoolMembership(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) (*nodeLocalPoolMembership, error) {
	nodes := &corev1.NodeList{}
	if err := r.nodeLocalPoolReader().List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("listing Kubernetes Nodes once for all node-local pool selectors: %w", err)
	}
	return evaluateNodeLocalPoolMembership(cluster.Spec.Storage.NodeLocalPools, nodes.Items)
}

func evaluateNodeLocalPoolMembership(
	pools []garagev1beta2.NodeLocalPoolSpec,
	nodes []corev1.Node,
) (*nodeLocalPoolMembership, error) {
	result := &nodeLocalPoolMembership{
		desiredNodesByPool: make(map[string]map[string]*corev1.Node, len(pools)),
		poolByNode:         make(map[string]string),
	}
	selectors := make([]compiledNodeLocalPoolSelector, 0, len(pools))
	for i := range pools {
		pool := &pools[i]
		selector, err := metav1.LabelSelectorAsSelector(&pool.Selector)
		if err != nil {
			return nil, fmt.Errorf("parsing selector for node-local pool %q: %w", pool.Name, err)
		}
		selectors = append(selectors, compiledNodeLocalPoolSelector{name: pool.Name, selector: selector})
		result.desiredNodesByPool[pool.Name] = make(map[string]*corev1.Node)
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	for i := range nodes {
		node := &nodes[i]
		for _, compiled := range selectors {
			if !compiled.selector.Matches(labels.Set(node.Labels)) {
				continue
			}
			poolName := compiled.name
			if previousPool := result.poolByNode[node.Name]; previousPool != "" && previousPool != poolName {
				result.selectorConflicts = append(result.selectorConflicts,
					fmt.Sprintf("%s=%s/%s", node.Name, previousPool, poolName))
				continue
			}
			result.poolByNode[node.Name] = poolName
			result.desiredNodesByPool[poolName][node.Name] = node
		}
	}
	result.selectedMembers = len(result.poolByNode)
	for i := range pools {
		if len(result.desiredNodesByPool[pools[i].Name]) == 0 {
			result.emptyPools = append(result.emptyPools, pools[i].Name)
		}
	}
	sort.Strings(result.emptyPools)
	sort.Strings(result.selectorConflicts)
	return result, nil
}

// requireNodeLocalPoolActivationHeadroom runs under the canonical layout
// coordinator immediately before the Kubernetes Node CAS. It closes races with
// Manual/PVC/federated writers that share Garage's global 256-role limit.
func (r *GarageClusterReconciler) requireNodeLocalPoolActivationHeadroom(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	poolName, kubernetesNodeName string,
) error {
	layout, err := r.getNodeLocalPoolCommittedLayout(ctx, cluster)
	if err != nil {
		bootstrap, bootstrapErr := r.nodeLocalPoolBootstrapMayInferHeadroom(ctx, cluster)
		if bootstrapErr != nil {
			return bootstrapErr
		}
		if bootstrap {
			return nil
		}
		return fmt.Errorf("%w: cannot read the shared Garage layout before activating %s/%s: %v",
			errNodeLocalPoolGarageRoleLimit, poolName, kubernetesNodeName, err)
	}
	// Staged additions consume headroom immediately, while staged removals do
	// not restore it until they are committed. This is deliberately the
	// worst-case set: a crash or explicit revert may discard the staged removal
	// after this activation decision.
	projectedStorageRoles := projectedGarageStorageRoleIDs(layout, nil)
	for i := range layout.StagedRoleChanges {
		change := &layout.StagedRoleChanges[i]
		if change.Remove || change.Capacity == nil {
			continue
		}
		projectedStorageRoles[canonicalGarageNodeID(change.ID)] = struct{}{}
	}

	existing, _, err := r.listNodeLocalPoolStorageNodes(ctx, cluster)
	if err != nil {
		return fmt.Errorf("checking node-local role headroom: %w", err)
	}
	for _, node := range existing {
		if node.Spec.NodeLocalPoolName != poolName || node.Spec.KubernetesNodeName != kubernetesNodeName {
			continue
		}
		if _, alreadyAssigned := projectedStorageRoles[canonicalGarageNodeID(node.Status.NodeID)]; alreadyAssigned {
			return nil
		}
	}

	if len(projectedStorageRoles) < maxNodeLocalPoolMembers {
		return nil
	}
	if len(projectedStorageRoles) >= garageMaximumStorageRoles {
		return fmt.Errorf("%w: Garage's committed layout plus uncommitted staged additions already contains %d positive-capacity roles (hard maximum %d); commit a removal before activating %s/%s",
			errNodeLocalPoolGarageRoleLimit, len(projectedStorageRoles), garageMaximumStorageRoles,
			poolName, kubernetesNodeName)
	}

	membership, err := r.readNodeLocalPoolMembership(ctx, cluster)
	if err != nil {
		return err
	}
	for _, node := range existing {
		if _, current := projectedStorageRoles[canonicalGarageNodeID(node.Status.NodeID)]; !current {
			continue
		}
		desiredNodes, poolDeclared := membership.desiredNodesByPool[node.Spec.NodeLocalPoolName]
		if !node.DeletionTimestamp.IsZero() || !poolDeclared ||
			(len(desiredNodes) > 0 && desiredNodes[node.Spec.KubernetesNodeName] == nil) {
			return nil // locally proven transient add-before-remove eligibility
		}
	}
	return fmt.Errorf("%w: Garage has %d positive-capacity roles and no locally proven retiring node-local role; activating %s/%s would consume Garage's final hard-limit slot without a safe transient replacement transition (node-local selected-member maximum %d)",
		errNodeLocalPoolGarageRoleLimit, len(projectedStorageRoles),
		poolName, kubernetesNodeName, maxNodeLocalPoolMembers)
}

func (r *GarageClusterReconciler) nodeLocalPoolBootstrapMayInferHeadroom(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) (bool, error) {
	if cluster == nil || cluster.Spec.Storage == nil || cluster.Spec.Storage.Replicas != 0 ||
		len(cluster.Spec.RemoteClusters) > 0 || cluster.Spec.ConnectTo != nil {
		return false, nil
	}
	nodes := &garagev1beta1.GarageNodeList{}
	if err := r.nodeLocalPoolReader().List(ctx, nodes, client.InNamespace(cluster.Namespace)); err != nil {
		return false, fmt.Errorf("listing GarageNodes before node-local bootstrap headroom inference: %w", err)
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		namespace := node.Spec.ClusterRef.Namespace
		if namespace == "" {
			namespace = node.Namespace
		}
		if namespace == cluster.Namespace && node.Spec.ClusterRef.Name == cluster.Name {
			return false, nil
		}
	}
	return true, nil
}

func (r *GarageClusterReconciler) clustersForKubernetesNode(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	node, ok := object.(*corev1.Node)
	if !ok {
		return nil
	}
	clusters := &garagev1beta2.GarageClusterList{}
	if err := r.List(ctx, clusters); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		matches := cluster.Spec.ZoneFrom != nil
		if !matches && cluster.Spec.Storage != nil {
			for j := range cluster.Spec.Storage.NodeLocalPools {
				selector, err := metav1.LabelSelectorAsSelector(&cluster.Spec.Storage.NodeLocalPools[j].Selector)
				if err != nil || selector.Matches(labels.Set(node.Labels)) {
					matches = true
					break
				}
			}
		}
		if matches {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		}
	}
	return requests
}
