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

package testutil

import (
	"os"
	"path/filepath"
)

// FindEnvTestBinaryDir returns a complete explicitly configured envtest binary
// directory, or the newest complete cached version below cacheRoot.
func FindEnvTestBinaryDir(configured, cacheRoot string) string {
	if envTestBinaryDirComplete(configured) {
		return configured
	}
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return ""
	}
	// os.ReadDir sorts by filename. Kubernetes release directory names sort in
	// version order for the supported 1.x versions, so walk backwards.
	for i := len(entries) - 1; i >= 0; i-- {
		if !entries[i].IsDir() {
			continue
		}
		candidate := filepath.Join(cacheRoot, entries[i].Name())
		if envTestBinaryDirComplete(candidate) {
			return candidate
		}
	}
	return ""
}

func envTestBinaryDirComplete(directory string) bool {
	if directory == "" {
		return false
	}
	for _, binary := range []string{"kube-apiserver", "etcd", "kubectl"} {
		info, err := os.Stat(filepath.Join(directory, binary))
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return false
		}
	}
	return true
}
