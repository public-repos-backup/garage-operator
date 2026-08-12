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
	"fmt"

	"github.com/rajsinghtech/garage-operator/internal/garageconfig"
)

// ValidateSupportedClusterReference rejects remote-kubeconfig references until
// the operator has a real remote Kubernetes client implementation. Controllers
// call this as well as admission so persisted objects cannot silently fall back
// to the local client when webhooks are unavailable.
func ValidateSupportedClusterReference(ref ClusterReference, field string) error {
	if ref.KubeConfigSecretRef != nil {
		return fmt.Errorf("%s.kubeConfigSecretRef is not supported; the operator can reference GarageClusters only through its configured Kubernetes client", field)
	}
	return nil
}

// ValidateClusterReference validates every cluster-reference field that is
// safe for the operator to resolve through its configured Kubernetes client.
func ValidateClusterReference(ref ClusterReference, field string) error {
	if err := ValidateSupportedClusterReference(ref, field); err != nil {
		return err
	}
	return validateNamespacedObjectReference(ref.Name, ref.Namespace, field)
}

func validateNamespacedObjectReference(name, namespace, field string) error {
	return garageconfig.ValidateNamespacedObjectReference(name, namespace, field)
}

func effectiveClusterReference(ref ClusterReference, objectNamespace string) (string, string) {
	namespace := ref.Namespace
	if namespace == "" {
		namespace = objectNamespace
	}
	return ref.Name, namespace
}

func clusterReferenceChanged(oldRef, newRef ClusterReference, objectNamespace string) bool {
	oldName, oldNamespace := effectiveClusterReference(oldRef, objectNamespace)
	newName, newNamespace := effectiveClusterReference(newRef, objectNamespace)
	return oldName != newName || oldNamespace != newNamespace
}
