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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// UIDBoundReservationAlias returns a private 128-bit Garage alias bound to one
// exact Kubernetes object incarnation. Callers must compare the full result
// before using a persisted reservation alias as remote ownership evidence.
func UIDBoundReservationAlias(prefix, namespace, name string, uid types.UID) (string, error) {
	if uid == "" {
		return "", fmt.Errorf("object UID is required before reserving a remote identity")
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + name + "\x00" + string(uid)))
	return prefix + hex.EncodeToString(sum[:16]), nil
}
