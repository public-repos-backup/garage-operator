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

package garageconfig

import (
	"fmt"
	"strings"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// ValidateNamespacedObjectReference validates the Kubernetes name and optional
// namespace carried by an object reference.
func ValidateNamespacedObjectReference(name, namespace, field string) error {
	if problems := utilvalidation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return fmt.Errorf("%s.name %q is invalid: %s", field, name, strings.Join(problems, "; "))
	}
	if namespace != "" {
		if problems := utilvalidation.IsDNS1123Label(namespace); len(problems) > 0 {
			return fmt.Errorf("%s.namespace %q is invalid: %s", field, namespace, strings.Join(problems, "; "))
		}
	}
	return nil
}
