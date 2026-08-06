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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
	"github.com/rajsinghtech/garage-operator/internal/garage"
)

const (
	annotationOperatorMetricsTokenReady  = "garage.rajsingh.info/operator-metrics-token-ready"
	annotationOperatorMetricsTokenName   = "garage.rajsingh.info/operator-metrics-token-name"
	annotationOperatorMetricsTokenPods   = "garage.rajsingh.info/operator-metrics-token-pod-set"
	annotationOperatorMetricsTokenIntent = "garage.rajsingh.info/operator-metrics-token-intent"
	labelOperatorMetricsToken            = "garage.rajsingh.info/operator-metrics-token"
	operatorMetricsScope                 = "Metrics"
)

func (r *GarageClusterReconciler) setOperatorMetricsTokenIntent(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
	present bool,
) error {
	expectedUID := cluster.UID
	expectedGeneration := cluster.Generation
	var updated *garagev1beta2.GarageCluster
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &garagev1beta2.GarageCluster{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), fresh); err != nil {
			return err
		}
		if fresh.UID != expectedUID || fresh.Generation != expectedGeneration {
			return fmt.Errorf("garageCluster UID or generation changed while updating operator metrics token intent")
		}
		if fresh.Annotations == nil {
			fresh.Annotations = make(map[string]string)
		}
		desired := ""
		if present {
			desired = operatorMetricsTokenName(fresh)
		}
		if fresh.Annotations[annotationOperatorMetricsTokenIntent] == desired {
			updated = fresh
			return nil
		}
		if desired == "" {
			delete(fresh.Annotations, annotationOperatorMetricsTokenIntent)
		} else {
			fresh.Annotations[annotationOperatorMetricsTokenIntent] = desired
		}
		if err := r.Update(ctx, fresh); err != nil {
			return err
		}
		updated = fresh
		return nil
	})
	if err != nil {
		return err
	}
	if updated != nil {
		adoptGarageClusterSnapshot(cluster, updated)
	}
	return nil
}

func operatorMetricsTokenSecretName(cluster *garagev1beta2.GarageCluster) string {
	return internalOperatorCredentialSecretName(cluster, "metrics")
}

func operatorMetricsTokenName(cluster *garagev1beta2.GarageCluster) string {
	return fmt.Sprintf("garage-operator-metrics:%s/%s:%s", cluster.Namespace, cluster.Name, cluster.UID)
}

func wantsOperatorMetricsToken(cluster *garagev1beta2.GarageCluster) bool {
	return cluster != nil && !cluster.IsManagementHandle() &&
		cluster.Spec.Admin != nil && cluster.Spec.Admin.AdminTokenSecretRef != nil &&
		(cluster.Spec.Admin.MetricsTokenSecretRef != nil || cluster.Spec.Admin.MetricsRequireToken) &&
		cluster.Spec.Monitoring != nil && cluster.Spec.Monitoring.Enabled != nil && *cluster.Spec.Monitoring.Enabled
}

func validateOperatorMetricsTokenSecret(
	secret *corev1.Secret,
	cluster *garagev1beta2.GarageCluster,
) (string, string, error) {
	key := types.NamespacedName{Name: operatorMetricsTokenSecretName(cluster), Namespace: cluster.Namespace}
	if secret == nil || secret.Name != key.Name || secret.Namespace != key.Namespace ||
		secret.Immutable == nil || !*secret.Immutable || !metav1.IsControlledBy(secret, cluster) ||
		secret.Labels[labelOperatorMetricsToken] != operatorAdminTokenReadyValue || len(secret.Data) != 2 ||
		secret.Annotations[annotationOperatorMetricsTokenName] != operatorMetricsTokenName(cluster) {
		return "", "", fmt.Errorf("operator metrics token Secret %s lost its exact immutable ownership/data/name contract", key)
	}
	id := string(secret.Data[operatorAdminTokenIDKey])
	token, err := canonicalDynamicAdminToken(secret.Data[metricsTokenKey], id)
	if err != nil {
		return "", "", fmt.Errorf("validating operator metrics token Secret %s: %w", key, err)
	}
	return id, token, nil
}

func getReadyOperatorMetricsToken(
	ctx context.Context,
	c client.Client,
	cluster *garagev1beta2.GarageCluster,
) (string, bool, error) {
	if !wantsOperatorMetricsToken(cluster) {
		return "", false, nil
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: operatorMetricsTokenSecretName(cluster), Namespace: cluster.Namespace}
	if err := c.Get(ctx, key, secret); err != nil {
		if errors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	_, token, err := validateOperatorMetricsTokenSecret(secret, cluster)
	if err != nil {
		return "", false, err
	}
	if secret.Annotations[annotationOperatorMetricsTokenReady] != operatorAdminTokenReadyValue {
		return "", false, nil
	}
	set, err := getOperatorAdminPodSet(ctx, c, cluster)
	if err != nil {
		return "", false, fmt.Errorf("operator metrics token is authoritative but the complete managed Pod set is not ready: %w", err)
	}
	if secret.Annotations[annotationOperatorMetricsTokenPods] != set.Hash {
		return "", false, fmt.Errorf("operator metrics token has not been directly verified on the current managed Pod incarnation set")
	}
	return token, true, nil
}

func (r *GarageClusterReconciler) operatorMetricsTokenRotationBridgeReady(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) (bool, error) {
	token, ready, err := getReadyOperatorMetricsToken(ctx, r.Client, cluster)
	if err != nil || !ready {
		return ready, err
	}
	set, err := getOperatorAdminPodSet(ctx, r.safetyReader(), cluster)
	if err != nil {
		return false, err
	}
	if err := verifyDynamicMetricsTokenOnPods(ctx, set.Pods, getAdminPort(cluster), token); err != nil {
		return false, err
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: operatorMetricsTokenSecretName(cluster), Namespace: cluster.Namespace}
	if err := r.Get(ctx, key, secret); err != nil {
		return false, err
	}
	id := string(secret.Data[operatorAdminTokenIDKey])
	bootstrap, err := r.staticGarageClientForPod(ctx, &set.Pods[0], getAdminPort(cluster))
	if err != nil {
		return false, err
	}
	info, err := bootstrap.GetAdminTokenInfo(ctx, id, "")
	if err != nil {
		return false, err
	}
	if err := validateManagedAdminTokenInfo(info, id, operatorMetricsTokenName(cluster), []string{operatorMetricsScope}); err != nil {
		return false, err
	}
	return true, nil
}

func verifyDynamicMetricsTokenOnPods(
	ctx context.Context,
	pods []corev1.Pod,
	adminPort int32,
	token string,
) error {
	for i := range pods {
		probeCtx, cancel := context.WithTimeout(ctx, operatorAdminTokenVerificationTimout)
		err := garage.NewClient(adminEndpoint(pods[i].Status.PodIP, adminPort), token).CheckMetrics(probeCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("dynamic metrics token has not reached Pod %s/%s: %w", pods[i].Namespace, pods[i].Name, err)
		}
	}
	return nil
}

func (r *GarageClusterReconciler) deleteOperatorMetricsTokensByName(
	ctx context.Context,
	bootstrap *garage.Client,
	cluster *garagev1beta2.GarageCluster,
) (bool, error) {
	tokens, err := bootstrap.ListAdminTokens(ctx)
	if err != nil {
		return false, err
	}
	removed := false
	for i := range tokens {
		if tokens[i].ID == nil || tokens[i].Name != operatorMetricsTokenName(cluster) {
			continue
		}
		if err := bootstrap.DeleteAdminToken(ctx, *tokens[i].ID); err != nil && !garage.IsNotFound(err) {
			return removed, err
		}
		removed = true
	}
	return removed, nil
}

func (r *GarageClusterReconciler) reconcileOperatorMetricsToken(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) error {
	if cluster == nil || cluster.IsManagementHandle() {
		return nil
	}
	key := types.NamespacedName{Name: operatorMetricsTokenSecretName(cluster), Namespace: cluster.Namespace}
	secret := &corev1.Secret{}
	secretErr := r.Get(ctx, key, secret)
	if secretErr != nil && !errors.IsNotFound(secretErr) {
		return secretErr
	}
	if !wantsOperatorMetricsToken(cluster) {
		if errors.IsNotFound(secretErr) && cluster.Annotations[annotationOperatorMetricsTokenIntent] == "" {
			return nil
		}
		return r.revokeOperatorMetricsToken(ctx, cluster)
	}

	podSet, err := getOperatorAdminPodSet(ctx, r.safetyReader(), cluster)
	if err != nil {
		return fmt.Errorf("proving complete managed process set for dynamic metrics token: %w", err)
	}
	bootstrap, err := r.staticGarageClientForPod(ctx, &podSet.Pods[0], getAdminPort(cluster))
	if err != nil {
		return err
	}

	if secretErr == nil {
		id, token, err := validateOperatorMetricsTokenSecret(secret, cluster)
		if err != nil {
			return err
		}
		if secret.Annotations[annotationOperatorMetricsTokenReady] != operatorAdminTokenReadyValue {
			if err := r.requireOperatorTokenBootstrapLayout(ctx, cluster, podSet.Pods, bootstrap); err != nil {
				return err
			}
		}
		info, err := bootstrap.GetAdminTokenInfo(ctx, id, "")
		if garage.IsNotFound(err) {
			if err := r.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
				return err
			}
			return fmt.Errorf("removed a missing/unreplicated metrics token Secret; waiting to recreate after committed layout")
		}
		if err != nil {
			return err
		}
		if err := validateManagedAdminTokenInfo(info, id, operatorMetricsTokenName(cluster), []string{operatorMetricsScope}); err != nil {
			return err
		}
		if err := verifyDynamicMetricsTokenOnPods(ctx, podSet.Pods, getAdminPort(cluster), token); err != nil {
			return err
		}
		if secret.Annotations[annotationOperatorMetricsTokenReady] != operatorAdminTokenReadyValue ||
			secret.Annotations[annotationOperatorMetricsTokenPods] != podSet.Hash {
			before := secret.DeepCopy()
			if secret.Annotations == nil {
				secret.Annotations = make(map[string]string)
			}
			secret.Annotations[annotationOperatorMetricsTokenReady] = operatorAdminTokenReadyValue
			secret.Annotations[annotationOperatorMetricsTokenPods] = podSet.Hash
			if err := r.Patch(ctx, secret, client.MergeFrom(before)); err != nil {
				return fmt.Errorf("activating dynamic metrics token after exact process-set verification: %w", err)
			}
		}
		if cluster.Annotations[annotationOperatorMetricsTokenIntent] != "" {
			if err := r.setOperatorMetricsTokenIntent(ctx, cluster, false); err != nil {
				return fmt.Errorf("clearing persisted metrics-token creation intent: %w", err)
			}
		}
		return nil
	}

	if err := r.setOperatorMetricsTokenIntent(ctx, cluster, true); err != nil {
		return fmt.Errorf("persisting metrics-token creation intent before one-time Garage response: %w", err)
	}
	if err := r.verifyMountedStaticAdminTokensOnPods(ctx, podSet.Pods, getAdminPort(cluster)); err != nil {
		return fmt.Errorf("cannot clean metrics token crash residue safely: %w", err)
	}
	removed, err := r.deleteOperatorMetricsTokensByName(ctx, bootstrap, cluster)
	if err != nil {
		return fmt.Errorf("cleaning operator metrics token residue: %w", err)
	}
	if removed {
		return fmt.Errorf("removed operator metrics token whose one-time Secret was not persisted; waiting to recreate")
	}
	if err := r.requireOperatorTokenBootstrapLayout(ctx, cluster, podSet.Pods, bootstrap); err != nil {
		return err
	}
	friendlyName := operatorMetricsTokenName(cluster)
	scope := []string{operatorMetricsScope}
	created, err := bootstrap.CreateAdminToken(ctx, garage.AdminTokenUpdate{
		Name: &friendlyName, NeverExpires: true, Scope: &scope,
	})
	if err != nil {
		return fmt.Errorf("creating table-backed metrics token: %w", err)
	}
	if created.ID == nil || *created.ID == "" {
		return fmt.Errorf("garage returned a table-backed metrics token without an ID")
	}
	id := *created.ID
	if _, err := canonicalDynamicAdminToken([]byte(created.SecretToken), id); err != nil {
		return err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels: mergeLabels(r.labelsForCluster(cluster), map[string]string{labelOperatorMetricsToken: operatorAdminTokenReadyValue}),
			Annotations: map[string]string{
				annotationOperatorMetricsTokenReady: "false",
				annotationOperatorMetricsTokenName:  friendlyName,
			},
		},
		Type: corev1.SecretTypeOpaque, Immutable: ptr.To(true),
		Data: map[string][]byte{
			metricsTokenKey:         []byte(created.SecretToken),
			operatorAdminTokenIDKey: []byte(id),
		},
	}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("persisting one-time table-backed metrics token Secret %s: %w", key, err)
	}
	return r.setOperatorMetricsTokenIntent(ctx, cluster, false)
}

// revokeOperatorMetricsToken uses an exact Pod's static startup bearer. A
// response lost after the tombstone commits is therefore retryable: the next
// static-authenticated Delete observes NotFound instead of failing auth with
// the token that just deleted itself.
func (r *GarageClusterReconciler) revokeOperatorMetricsToken(
	ctx context.Context,
	cluster *garagev1beta2.GarageCluster,
) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: operatorMetricsTokenSecretName(cluster), Namespace: cluster.Namespace}
	secretErr := r.Get(ctx, key, secret)
	if errors.IsNotFound(secretErr) && !wantsOperatorMetricsToken(cluster) &&
		cluster.Annotations[annotationOperatorMetricsTokenIntent] == "" {
		return nil
	}
	if secretErr != nil && !errors.IsNotFound(secretErr) {
		return secretErr
	}
	bootstrap, err := r.operatorTokenRevocationClient(ctx, cluster)
	if err != nil {
		return err
	}
	if _, err := r.deleteOperatorMetricsTokensByName(ctx, bootstrap, cluster); err != nil {
		return fmt.Errorf("revoking operator metrics token: %w", err)
	}
	if secretErr == nil {
		if err := r.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	if cluster.DeletionTimestamp.IsZero() && cluster.Annotations[annotationOperatorMetricsTokenIntent] != "" {
		if err := r.setOperatorMetricsTokenIntent(ctx, cluster, false); err != nil {
			return err
		}
	}
	return nil
}
