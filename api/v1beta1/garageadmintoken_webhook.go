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

package v1beta1

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const garageAdminTokenDefaultKey = "admin-token"

var garageadmintokenlog = logf.Log.WithName("garageadmintoken-resource")

// SetupWebhookWithManager sets up the webhook with the Manager.
func (r *GarageAdminToken) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, r).
		WithValidator(&GarageAdminTokenValidator{Client: mgr.GetClient()}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-garage-rajsingh-info-v1beta1-garageadmintoken,mutating=false,failurePolicy=fail,sideEffects=None,groups=garage.rajsingh.info,resources=garageadmintokens,verbs=create;update;delete,versions=v1beta1,name=vgarageadmintoken.kb.io,admissionReviewVersions=v1

var _ admission.Validator[*GarageAdminToken] = &GarageAdminTokenValidator{}

// +kubebuilder:object:generate=false

// GarageAdminTokenValidator handles validation for GarageAdminToken.
type GarageAdminTokenValidator struct {
	Client client.Client
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type.
func (v *GarageAdminTokenValidator) ValidateCreate(ctx context.Context, obj *GarageAdminToken) (admission.Warnings, error) {
	garageadmintokenlog.Info("validate create", "name", obj.Name)
	return v.validateGarageAdminToken(ctx, obj)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type.
func (v *GarageAdminTokenValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *GarageAdminToken) (admission.Warnings, error) {
	garageadmintokenlog.Info("validate update", "name", newObj.Name)
	warnings, err := v.validateGarageAdminTokenWithOptions(ctx, newObj, equality.Semantic.DeepEqual(oldObj.Spec, newObj.Spec))
	if err != nil {
		return warnings, err
	}
	oldTemplate, newTemplate := oldObj.Spec.SecretTemplate, newObj.Spec.SecretTemplate
	identityChanged := !equality.Semantic.DeepEqual(oldObj.Spec.ClusterRef, newObj.Spec.ClusterRef)
	if oldTemplate == nil || newTemplate == nil {
		identityChanged = identityChanged || (oldTemplate == nil) != (newTemplate == nil)
	} else {
		identityChanged = identityChanged || oldTemplate.Name != newTemplate.Name ||
			oldTemplate.TokenKey != newTemplate.TokenKey || oldTemplate.EndpointKey != newTemplate.EndpointKey ||
			!equality.Semantic.DeepEqual(oldTemplate.IncludeEndpoint, newTemplate.IncludeEndpoint)
	}
	if identityChanged {
		return warnings, fmt.Errorf("clusterRef and secretTemplate name/tokenKey/endpointKey are immutable; create a new GarageAdminToken instead of orphaning static bootstrap material")
	}
	return warnings, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type.
func (v *GarageAdminTokenValidator) ValidateDelete(ctx context.Context, obj *GarageAdminToken) (admission.Warnings, error) {
	garageadmintokenlog.Info("validate delete", "name", obj.Name)
	if v.Client == nil {
		return nil, fmt.Errorf("cannot prove the generated static bootstrap Secret is no longer referenced: webhook client is unavailable")
	}
	clusterNamespace := obj.Namespace
	if obj.Spec.ClusterRef.Namespace != "" {
		clusterNamespace = obj.Spec.ClusterRef.Namespace
	}
	cluster := &garagev1beta2.GarageCluster{}
	err := v.Client.Get(ctx, types.NamespacedName{Name: obj.Spec.ClusterRef.Name, Namespace: clusterNamespace}, cluster)
	if apierrors.IsNotFound(err) {
		return admission.Warnings{"deleting the Kubernetes Secret cannot revoke static bearer bytes already loaded by a surviving Garage process"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot prove the static bootstrap Secret is unused: %w", err)
	}
	secretName, tokenKey := garageAdminTokenEffectiveSecretIdentity(obj)
	if cluster.Namespace == obj.Namespace && cluster.Spec.Admin != nil && cluster.Spec.Admin.AdminTokenSecretRef != nil {
		ref := cluster.Spec.Admin.AdminTokenSecretRef
		refKey := ref.Key
		if refKey == "" {
			refKey = garageAdminTokenDefaultKey
		}
		if ref.Name == secretName && refKey == tokenKey {
			return nil, fmt.Errorf("garageCluster %s/%s still references generated Secret %s/%s:%s; rotate or remove spec.admin.adminTokenSecretRef before deletion", cluster.Namespace, cluster.Name, obj.Namespace, secretName, tokenKey)
		}
	}
	return admission.Warnings{"deleting the Kubernetes Secret cannot revoke static bearer bytes already loaded by a Garage process; complete credential rotation before deletion"}, nil
}

func (v *GarageAdminTokenValidator) validateGarageAdminToken(_ context.Context, obj *GarageAdminToken) (admission.Warnings, error) {
	return v.validateGarageAdminTokenWithOptions(context.Background(), obj, false)
}

func (v *GarageAdminTokenValidator) validateGarageAdminTokenWithOptions(_ context.Context, obj *GarageAdminToken, allowUnchangedLegacy bool) (admission.Warnings, error) {
	var warnings admission.Warnings

	if obj.Spec.ClusterRef.Name == "" {
		return warnings, fmt.Errorf("clusterRef.name is required")
	}
	if !allowUnchangedLegacy {
		if err := ValidateClusterReference(obj.Spec.ClusterRef, "clusterRef"); err != nil {
			return warnings, err
		}
	}

	// Cross-namespace cluster reference requires a GarageReferenceGrant.
	targetNS := obj.Spec.ClusterRef.Namespace
	if targetNS == "" {
		targetNS = obj.Namespace
	}
	if targetNS != obj.Namespace {
		return warnings, fmt.Errorf("clusterRef.namespace must match metadata.namespace: GarageAdminToken provisions a local static bootstrap Secret, and GarageCluster credential refs cannot consume it across namespaces")
	}

	if obj.Spec.ExpiresAt != nil && obj.Spec.NeverExpires {
		return warnings, fmt.Errorf("expiresAt and neverExpires are mutually exclusive")
	}
	if obj.Spec.ExpiresAt != nil {
		return warnings, fmt.Errorf("expiresAt is unsupported for static bootstrap material; Garage does not assign or revoke this token")
	}
	if obj.Spec.Name != "" {
		return warnings, fmt.Errorf("spec.name is unsupported for static bootstrap material because no Garage Admin-token row is created")
	}
	if !allowUnchangedLegacy {
		if err := ValidateGarageAdminTokenMaterialSpec(obj); err != nil {
			return warnings, err
		}
	}
	warnings = admission.Warnings{"GarageAdminToken creates immutable static bootstrap material only; deletion is refused while the referenced GarageCluster consumes the Secret, and removing the source does not revoke bearer bytes already loaded by Garage"}

	return warnings, nil
}

// ValidateGarageAdminTokenMaterialSpec validates every value copied into the
// generated Secret. It is shared by admission and reconciliation so directly
// persisted objects fail with the same actionable error.
func ValidateGarageAdminTokenMaterialSpec(obj *GarageAdminToken) error {
	if obj == nil || obj.Spec.SecretTemplate == nil {
		return nil
	}
	template := obj.Spec.SecretTemplate
	if template.Name != "" {
		if problems := utilvalidation.IsDNS1123Subdomain(template.Name); len(problems) > 0 {
			return fmt.Errorf("secretTemplate.name %q is invalid: %s", template.Name, strings.Join(problems, "; "))
		}
	}
	_, tokenKey := garageAdminTokenEffectiveSecretIdentity(obj)
	endpointKey := "admin-endpoint"
	if template.EndpointKey != "" {
		endpointKey = template.EndpointKey
	}
	if tokenKey == endpointKey {
		return fmt.Errorf("secretTemplate.tokenKey and endpointKey must be different")
	}
	for field, key := range map[string]string{"tokenKey": tokenKey, "endpointKey": endpointKey} {
		if problems := utilvalidation.IsConfigMapKey(key); len(problems) > 0 {
			return fmt.Errorf("secretTemplate.%s %q is not a valid Secret data key: %s", field, key, strings.Join(problems, "; "))
		}
	}
	for key, value := range template.Labels {
		if problems := utilvalidation.IsQualifiedName(key); len(problems) > 0 {
			return fmt.Errorf("secretTemplate.labels key %q is invalid: %s", key, strings.Join(problems, "; "))
		}
		if problems := utilvalidation.IsValidLabelValue(value); len(problems) > 0 {
			return fmt.Errorf("secretTemplate.labels[%q] value %q is invalid: %s", key, value, strings.Join(problems, "; "))
		}
	}
	for key := range template.Annotations {
		if problems := utilvalidation.IsQualifiedName(key); len(problems) > 0 {
			return fmt.Errorf("secretTemplate.annotations key %q is invalid: %s", key, strings.Join(problems, "; "))
		}
	}
	return nil
}

func garageAdminTokenEffectiveSecretIdentity(obj *GarageAdminToken) (string, string) {
	name := obj.Name
	key := garageAdminTokenDefaultKey
	if obj.Spec.SecretTemplate != nil {
		if obj.Spec.SecretTemplate.Name != "" {
			name = obj.Spec.SecretTemplate.Name
		}
		if obj.Spec.SecretTemplate.TokenKey != "" {
			key = obj.Spec.SecretTemplate.TokenKey
		}
	}
	return name, key
}
