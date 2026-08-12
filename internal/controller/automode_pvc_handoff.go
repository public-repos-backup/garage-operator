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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func retainedAutoModePVCHandoff(
	cluster *garagev1beta2.GarageCluster,
	node *garagev1beta1.GarageNode,
	pvc *corev1.PersistentVolumeClaim,
) (*garagev1beta2.AutoModePVCHandoffStatus, error) {
	if cluster == nil || node == nil || pvc == nil || node.UID == "" || pvc.UID == "" {
		return nil, nil
	}
	slot := node.Labels[labelAutoNodeSlot]
	tier := node.Labels[labelTier]
	if slot == "" || node.Labels[labelAppManagedBy] != managedByOperatorValue ||
		(tier != tierGateway && tier != tierStorage) || node.Spec.ClusterRef.Name != cluster.Name ||
		!metav1.IsControlledBy(node, cluster) {
		return nil, nil
	}

	var match *garagev1beta2.AutoModePVCHandoffStatus
	for i := range cluster.Status.AutoModePVCHandoffs {
		handoff := &cluster.Status.AutoModePVCHandoffs[i]
		if handoff.PVCName != pvc.Name {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("GarageCluster status has duplicate Auto-mode PVC handoffs for %q", pvc.Name)
		}
		match = handoff
	}
	if match == nil {
		return nil, nil
	}
	if match.SlotName != slot || match.PVCUID != string(pvc.UID) ||
		match.ReplacementGarageNodeUID != string(node.UID) {
		return nil, fmt.Errorf("refusing retained Auto-mode PVC %s/%s because its exact status handoff does not authorize GarageNode UID %s", pvc.Namespace, pvc.Name, node.UID)
	}
	if !managedNodePVCNonceMatchesNode(node, match.ReplacementReservationHash) {
		return nil, fmt.Errorf("refusing retained Auto-mode PVC %s/%s because the replacement GarageNode nonce does not match its status commitment", pvc.Namespace, pvc.Name)
	}
	pinnedUID := pvc.Annotations[managedPVCNodeUIDAnnotation]
	if pinnedUID != match.PreviousGarageNodeUID && pinnedUID != match.ReplacementGarageNodeUID {
		return nil, fmt.Errorf("refusing retained Auto-mode PVC %s/%s because its previous GarageNode UID correlation %q does not match the exact handoff", pvc.Namespace, pvc.Name, pinnedUID)
	}
	return match, nil
}

func managedNodePVCNonceMatchesNode(node *garagev1beta1.GarageNode, hash string) bool {
	if node == nil {
		return false
	}
	nonce := node.Annotations[autoModePVCHandoffNonceAnnotation]
	return nonce != "" && hash != "" && managedNodePVCNonceDigest(nonce) == hash
}

func managedNodePVCNonceDigest(nonce string) string {
	digest := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(digest[:])
}

func (r *GarageClusterReconciler) updateAutoModePVCHandoffs(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	mutate func([]garagev1beta2.AutoModePVCHandoffStatus) ([]garagev1beta2.AutoModePVCHandoffStatus, error),
) error {
	key := client.ObjectKeyFromObject(cluster)
	originalUID := cluster.UID
	for attempt := 0; attempt < StatusUpdateMaxRetries; attempt++ {
		fresh := &garagev1beta2.GarageCluster{}
		if err := r.safetyReader().Get(ctx, key, fresh); err != nil {
			return fmt.Errorf("reading GarageCluster before updating Auto-mode PVC handoffs: %w", err)
		}
		if originalUID != "" && fresh.UID != originalUID {
			return fmt.Errorf("refusing to update Auto-mode PVC handoffs across GarageCluster recreation: expected UID %s, got %s", originalUID, fresh.UID)
		}
		next, err := mutate(slices.Clone(fresh.Status.AutoModePVCHandoffs))
		if err != nil {
			return err
		}
		fresh.Status.AutoModePVCHandoffs = next
		if err := r.Status().Update(ctx, fresh); err != nil {
			if errors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("updating Auto-mode PVC handoffs: %w", err)
		}
		*cluster = *fresh.DeepCopy()
		return nil
	}
	return fmt.Errorf("updating Auto-mode PVC handoffs exhausted conflict retries")
}

func autoModePVCHandoffsForSlot(
	handoffs []garagev1beta2.AutoModePVCHandoffStatus, slot string,
) []garagev1beta2.AutoModePVCHandoffStatus {
	var out []garagev1beta2.AutoModePVCHandoffStatus
	for i := range handoffs {
		if handoffs[i].SlotName == slot {
			out = append(out, handoffs[i])
		}
	}
	return out
}

func authoritativeObjectAbsent(ctx context.Context, reader client.Reader, key types.NamespacedName, obj client.Object) (bool, error) {
	if err := reader.Get(ctx, key, obj); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
