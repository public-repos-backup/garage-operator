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
	"os"
	"path/filepath"
	"testing"

	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestKubernetes125LegacyObjectsRemainFinalizerUpdatable(t *testing.T) {
	for _, root := range []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("..", "..", "charts", "garage-operator", "crd-bases"),
	} {
		t.Run(filepath.Base(filepath.Dir(root))+"/"+filepath.Base(root), func(t *testing.T) {
			bucketSpec := crdVersionSpecSchema(t, filepath.Join(root, "garage.rajsingh.info_garagebuckets.yaml"), "v1beta1")
			assertNoStringConstraints(t, bucketSpec.Properties["globalAlias"], "GarageBucket.spec.globalAlias")
			localAlias := requiredArrayItemSchema(t, bucketSpec.Properties["localAliases"], "GarageBucket.spec.localAliases")
			assertNoStringConstraints(t, localAlias.Properties["alias"], "GarageBucket.spec.localAliases[].alias")
			quotas := bucketSpec.Properties["quotas"]
			maxObjects := quotas.Properties["maxObjects"]
			if maxObjects.Minimum != nil {
				t.Fatalf("GarageBucket.spec.quotas.maxObjects retains minimum=%v; Kubernetes 1.25 would reject legacy finalizer updates", *maxObjects.Minimum)
			}

			keySpec := crdVersionSpecSchema(t, filepath.Join(root, "garage.rajsingh.info_garagekeys.yaml"), "v1beta1")
			permission := requiredArrayItemSchema(t, keySpec.Properties["bucketPermissions"], "GarageKey.spec.bucketPermissions")
			assertNoStringConstraints(t, permission.Properties["globalAlias"], "GarageKey.spec.bucketPermissions[].globalAlias")
		})
	}
}

func crdVersionSpecSchema(t *testing.T, path, versionName string) extensionsv1.JSONSchemaProps {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	crd := &extensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	for _, version := range crd.Spec.Versions {
		if version.Name == versionName {
			if version.Schema == nil {
				t.Fatalf("%s version %s has no schema", path, versionName)
			}
			return version.Schema.OpenAPIV3Schema.Properties["spec"]
		}
	}
	t.Fatalf("%s has no version %s", path, versionName)
	return extensionsv1.JSONSchemaProps{}
}

func requiredArrayItemSchema(t *testing.T, schema extensionsv1.JSONSchemaProps, field string) extensionsv1.JSONSchemaProps {
	t.Helper()
	if schema.Items == nil || schema.Items.Schema == nil {
		t.Fatalf("%s has no item schema", field)
	}
	return *schema.Items.Schema
}

func assertNoStringConstraints(t *testing.T, schema extensionsv1.JSONSchemaProps, field string) {
	t.Helper()
	if schema.MinLength != nil || schema.MaxLength != nil || schema.Pattern != "" {
		t.Fatalf("%s retains update-incompatible constraints minLength=%v maxLength=%v pattern=%q; Kubernetes 1.25 would reject legacy finalizer updates",
			field, schema.MinLength, schema.MaxLength, schema.Pattern)
	}
}
