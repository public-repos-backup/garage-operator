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
	"testing"
)

func TestFindEnvTestBinaryDir(t *testing.T) {
	root := t.TempDir()
	complete := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, binary := range []string{"kube-apiserver", "etcd", "kubectl"} {
			if err := os.WriteFile(filepath.Join(dir, binary), nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	oldComplete := complete("1.34.0-linux-amd64")
	newComplete := complete("1.36.0-linux-amd64")
	incomplete := filepath.Join(root, "1.37.0-linux-amd64")
	if err := os.MkdirAll(incomplete, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomplete, "kube-apiserver"), nil, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := FindEnvTestBinaryDir(oldComplete, root); got != oldComplete {
		t.Fatalf("configured complete directory = %q, want %q", got, oldComplete)
	}
	if got := FindEnvTestBinaryDir(incomplete, root); got != newComplete {
		t.Fatalf("fallback directory = %q, want newest complete %q", got, newComplete)
	}
	if got := FindEnvTestBinaryDir("", filepath.Join(root, "missing")); got != "" {
		t.Fatalf("missing cache returned %q", got)
	}
}
