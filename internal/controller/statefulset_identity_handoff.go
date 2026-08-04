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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
)

// prepareStatefulSetPVCIdentityHandoff makes every volumeClaimTemplate PVC
// independent from the exact StatefulSet and Pod that currently use it. The
// transition is intentionally multi-reconcile:
//
//  1. persist Retain/Retain and return;
//  2. wait until the StatefulSet controller observes that generation;
//  3. remove only the exact old StatefulSet/Pod owner references and return;
//  4. re-read the claims and report ready only when none can be garbage
//     collected with either identity.
//
// Waiting for observedGeneration prevents an already-running StatefulSet
// controller sync based on Delete retention from reattaching a destructive
// owner reference after this function removes it.
func prepareStatefulSetPVCIdentityHandoff(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	statefulSet *appsv1.StatefulSet,
	pod *corev1.Pod,
) (bool, error) {
	if writer == nil || reader == nil || statefulSet == nil {
		return false, fmt.Errorf("StatefulSet PVC identity handoff requires an exact writer, reader, and StatefulSet")
	}
	if statefulSet.UID == "" || !statefulSet.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("StatefulSet %s/%s is absent or already deleting before PVC identity handoff", statefulSet.Namespace, statefulSet.Name)
	}
	if !statefulSetRetentionIsRetain(statefulSet) {
		statefulSet.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		}
		if err := writer.Update(ctx, statefulSet); err != nil {
			return false, fmt.Errorf("persisting Retain/Retain on StatefulSet %s/%s: %w", statefulSet.Namespace, statefulSet.Name, err)
		}
		return false, nil
	}
	if statefulSet.Status.ObservedGeneration < statefulSet.Generation {
		return false, nil
	}
	if len(statefulSet.Spec.VolumeClaimTemplates) == 0 {
		return true, nil
	}
	if pod == nil || pod.UID == "" || !pod.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("StatefulSet %s/%s has identity-bearing PVCs but no exact live Pod", statefulSet.Namespace, statefulSet.Name)
	}
	podOwner := metav1.GetControllerOf(pod)
	if pod.Namespace != statefulSet.Namespace || podOwner == nil ||
		podOwner.APIVersion != appsv1.SchemeGroupVersion.String() ||
		podOwner.Kind != kindStatefulSet || podOwner.Name != statefulSet.Name || podOwner.UID != statefulSet.UID {
		return false, fmt.Errorf("pod %s/%s is not controlled by exact StatefulSet UID %s", pod.Namespace, pod.Name, statefulSet.UID)
	}

	changed := false
	for i := range statefulSet.Spec.VolumeClaimTemplates {
		claimName := fmt.Sprintf("%s-%s-0", statefulSet.Spec.VolumeClaimTemplates[i].Name, statefulSet.Name)
		claim := &corev1.PersistentVolumeClaim{}
		if err := reader.Get(ctx, types.NamespacedName{Name: claimName, Namespace: statefulSet.Namespace}, claim); err != nil {
			return false, fmt.Errorf("reading identity-bearing PVC %s/%s before StatefulSet handoff: %w", statefulSet.Namespace, claimName, err)
		}
		if claim.UID == "" || !claim.DeletionTimestamp.IsZero() {
			return false, fmt.Errorf("identity-bearing PVC %s/%s is absent or already deleting", claim.Namespace, claim.Name)
		}
		kept := make([]metav1.OwnerReference, 0, len(claim.OwnerReferences))
		claimChanged := false
		for j := range claim.OwnerReferences {
			ref := claim.OwnerReferences[j]
			switch {
			case exactOwnerReference(ref, appsv1.SchemeGroupVersion.String(), kindStatefulSet, statefulSet.Name, statefulSet.UID):
				claimChanged = true
			case exactOwnerReference(ref, corev1.SchemeGroupVersion.String(), "Pod", pod.Name, pod.UID):
				claimChanged = true
			default:
				return false, fmt.Errorf(
					"identity-bearing PVC %s/%s has unexpected owner %s %s (%s) UID %s; refusing automatic handoff",
					claim.Namespace, claim.Name, ref.Kind, ref.Name, ref.APIVersion, ref.UID,
				)
			}
		}
		if !claimChanged {
			continue
		}
		claim.OwnerReferences = kept
		if err := writer.Update(ctx, claim); err != nil {
			return false, fmt.Errorf("detaching exact old workload owners from PVC %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		changed = true
	}
	if changed {
		return false, nil
	}
	return true, nil
}

func statefulSetRetentionIsRetain(statefulSet *appsv1.StatefulSet) bool {
	if statefulSet == nil || statefulSet.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		// Kubernetes defaults both dimensions to Retain.
		return true
	}
	policy := statefulSet.Spec.PersistentVolumeClaimRetentionPolicy
	return policy.WhenDeleted == appsv1.RetainPersistentVolumeClaimRetentionPolicyType &&
		policy.WhenScaled == appsv1.RetainPersistentVolumeClaimRetentionPolicyType
}

func exactOwnerReference(ref metav1.OwnerReference, apiVersion, kind, name string, uid types.UID) bool {
	return ref.APIVersion == apiVersion && ref.Kind == kind && ref.Name == name && ref.UID == uid
}

func (r *GarageNodeReconciler) prepareCycleSourcePVCIdentityHandoff(
	ctx context.Context,
	node *garagev1beta1.GarageNode,
) (bool, error) {
	statefulSet := &appsv1.StatefulSet{}
	if err := r.nodeLocalPoolReader().Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, statefulSet); err != nil {
		return false, fmt.Errorf("reading cycle source StatefulSet: %w", err)
	}
	owner := metav1.GetControllerOf(statefulSet)
	if node.UID == "" || owner == nil ||
		owner.APIVersion != garagev1beta1.GroupVersion.String() ||
		owner.Kind != kindGarageNode || owner.Name != node.Name || owner.UID != node.UID {
		return false, fmt.Errorf("cycle source StatefulSet %s/%s is not controlled by GarageNode UID %s", statefulSet.Namespace, statefulSet.Name, node.UID)
	}
	var pod *corev1.Pod
	if len(statefulSet.Spec.VolumeClaimTemplates) > 0 {
		currentPod, err := r.statefulSetPodForNode(ctx, node)
		if err != nil {
			return false, err
		}
		pod = currentPod
	}
	return prepareStatefulSetPVCIdentityHandoff(ctx, r.Client, r.nodeLocalPoolReader(), statefulSet, pod)
}
