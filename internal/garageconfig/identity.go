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
	"crypto/sha256"
	"encoding/hex"
	"strings"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// GarageKeyImportSnapshotName returns the controller-private Secret name used
// to pin mutable Secret-backed import material before any remote write.
func GarageKeyImportSnapshotName(name string) string {
	identity := name + "-import-snapshot"
	if len(identity) <= 63 && len(utilvalidation.IsDNS1123Label(identity)) == 0 {
		return identity
	}
	sum := sha256.Sum256([]byte(identity))
	suffix := "-" + hex.EncodeToString(sum[:8])
	prefix := strings.Trim(strings.ReplaceAll(strings.ToLower(identity), ".", "-"), "-")
	if len(prefix) > 63-len(suffix) {
		prefix = strings.TrimRight(prefix[:63-len(suffix)], "-")
	}
	if prefix == "" {
		prefix = "garage"
	}
	return prefix + suffix
}

// COSIShadowResourceName returns the deterministic Kubernetes name used for a
// COSI shadow resource.
func COSIShadowResourceName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "cosi-" + hex.EncodeToString(sum[:8])
}
