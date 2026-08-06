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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const (
	labelScaleTarget    = "garage.rajsingh.info/scale-target"
	scaleTargetDisabled = "disabled"
)

// garageClusterScaleObservation is the exact current workload projection
// exposed through Kubernetes' Scale subresource. It is intentionally separate
// from aggregate GarageCluster status: a selector-driven node-local pool,
// Manual GarageNode, or unified gateway must never make default-group Scale
// report more replicas than spec.storage.replicas controls.
type garageClusterScaleObservation struct {
	replicas int32
	selector string
}

// observeGarageClusterScale returns actual, non-terminating Pods selected by
// the one workload that the requested API's replica field can control.
//
// v1beta2 exposes only the default Auto storage group through /scale. The
// v1beta1 gateway-only projection historically maps spec.replicas to the edge
// gateway, so a gateway-only hub object retains that behavior. Manual storage
// has no scalable group and receives a stable no-match selector.
func (r *GarageClusterReconciler) observeGarageClusterScale(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) (garageClusterScaleObservation, error) {
	labels := map[string]string{
		labelCluster:     cluster.Name,
		labelScaleTarget: scaleTargetDisabled,
	}

	switch {
	case cluster.HasStorageTier() && cluster.EffectiveStorageLayoutPolicy() != LayoutPolicyManual:
		labels = map[string]string{
			labelCluster:      cluster.Name,
			labelTier:         tierStorage,
			labelStorageGroup: storageGroupDefault,
		}
	case !cluster.HasStorageTier() && cluster.HasGatewayTier() && cluster.Spec.LayoutPolicy != LayoutPolicyManual:
		labels = map[string]string{
			labelCluster: cluster.Name,
			labelTier:    tierGateway,
		}
	default:
		return garageClusterScaleObservation{
			selector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: labels}),
		}, nil
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(labels),
	); err != nil {
		return garageClusterScaleObservation{}, err
	}

	var replicas int32
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp.IsZero() {
			replicas++
		}
	}

	return garageClusterScaleObservation{
		replicas: replicas,
		selector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: labels}),
	}, nil
}
