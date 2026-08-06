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

package v1beta2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const garageClusterScaleValidationPath = "/validate-garage-rajsingh-info-garagecluster-scale"

// +kubebuilder:webhook:path=/validate-garage-rajsingh-info-garagecluster-scale,mutating=false,failurePolicy=fail,sideEffects=None,groups=garage.rajsingh.info,resources=garageclusters/scale,verbs=update,versions=v1beta1;v1beta2,name=vgarageclusterscale.kb.io,admissionReviewVersions=v1

// garageClusterScaleValidator applies the same full-object topology validation
// to /scale writes that GarageCluster.ValidateUpdate applies to ordinary
// updates. Kubernetes admission resource rules do not make `garageclusters`
// match `garageclusters/scale`, so this is a distinct fail-closed handler.
//
// Reader must be uncached. The Scale object's resourceVersion and this live
// read together ensure the safety decision is made against the object version
// the API server is about to update, including its current drain/rollout status.
type garageClusterScaleValidator struct {
	Reader client.Reader
}

var _ admission.Handler = &garageClusterScaleValidator{}

// Handle validates one autoscaling/v1 Scale update.
func (v *garageClusterScaleValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Update || req.SubResource != "scale" {
		return admission.Denied("GarageCluster scale webhook accepts only UPDATE requests to the scale subresource")
	}

	scale := &autoscalingv1.Scale{}
	if err := json.Unmarshal(req.Object.Raw, scale); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode GarageCluster Scale request: %w", err))
	}
	if v.Reader == nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("GarageCluster scale webhook has no API reader"))
	}

	current := &GarageCluster{}
	key := types.NamespacedName{Namespace: req.Namespace, Name: req.Name}
	if err := v.Reader.Get(ctx, key, current); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("read current GarageCluster for scale validation: %w", err))
	}
	if scale.ResourceVersion == "" || scale.ResourceVersion != current.ResourceVersion {
		return admission.Errored(http.StatusConflict, fmt.Errorf(
			"stale GarageCluster scale resourceVersion %q; current resourceVersion is %q",
			scale.ResourceVersion,
			current.ResourceVersion,
		))
	}

	requestedVersion := req.Resource.Version
	if req.RequestResource != nil && req.RequestResource.Version != "" {
		requestedVersion = req.RequestResource.Version
	}
	candidate, err := garageClusterWithScaleReplicas(current, requestedVersion, scale.Spec.Replicas)
	if err != nil {
		return admission.Denied(err.Error())
	}

	warnings, err := (&GarageClusterValidator{}).ValidateUpdate(ctx, current, candidate)
	if err != nil {
		return admission.Denied(err.Error()).WithWarnings(warnings...)
	}
	return admission.Allowed("GarageCluster scale update passed topology validation").WithWarnings(warnings...)
}

// garageClusterWithScaleReplicas projects the requested API version's replica
// field onto a v1beta2 hub copy. A v1beta1 unified or node-local view is
// storage-first, matching ConvertFrom; a v1beta1 edge-gateway view keeps its
// historical gateway scaling behavior. v1beta2 has no gateway scale path.
func garageClusterWithScaleReplicas(current *GarageCluster, requestedVersion string, replicas int32) (*GarageCluster, error) {
	if current == nil {
		return nil, fmt.Errorf("GarageCluster scale target is nil")
	}
	candidate := current.DeepCopy()

	switch requestedVersion {
	case GroupVersion.Version:
		if current.Spec.Storage == nil {
			return nil, fmt.Errorf("v1beta2 Scale is available only for the Auto default storage group; gateway-only clusters must change spec.gateway.replicas directly")
		}
		if current.EffectiveStorageLayoutPolicy() == layoutPolicyManual {
			return nil, fmt.Errorf("scale is unsupported when spec.storage.layoutPolicy is Manual because ordinary GarageNodes are individually owned")
		}
		candidate.Spec.Storage.Replicas = replicas
	case "v1beta1":
		switch {
		case current.Spec.Storage != nil:
			if current.EffectiveStorageLayoutPolicy() == layoutPolicyManual {
				return nil, fmt.Errorf("scale is unsupported for Manual storage because ordinary GarageNodes are individually owned")
			}
			candidate.Spec.Storage.Replicas = replicas
		case current.Spec.Gateway != nil:
			if current.Spec.LayoutPolicy == layoutPolicyManual {
				return nil, fmt.Errorf("scale is unsupported for Manual gateways because ordinary GarageNodes are individually owned")
			}
			candidate.Spec.Gateway.Replicas = replicas
		default:
			return nil, fmt.Errorf("scale is unavailable for a management-handle GarageCluster")
		}
	default:
		return nil, fmt.Errorf("unsupported GarageCluster scale API version %q", requestedVersion)
	}

	return candidate, nil
}
