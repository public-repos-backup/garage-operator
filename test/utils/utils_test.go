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

package utils

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestPinCertManagerImages(t *testing.T) {
	var manifest strings.Builder
	for source := range certmanagerImagePins {
		if strings.Contains(source, "acmesolver") {
			_, _ = fmt.Fprintf(&manifest, "      - --acme-http01-solver-image=%s\n", source)
		} else {
			_, _ = fmt.Fprintf(&manifest, "      image: %q\n", source)
		}
	}

	pinned, err := pinCertManagerImages([]byte(manifest.String()))
	if err != nil {
		t.Fatalf("pin cert-manager images: %v", err)
	}
	for source, target := range certmanagerImagePins {
		if strings.Contains(string(pinned), `"`+source+`"`) {
			t.Errorf("unpinned image remains: %s", source)
		}
		if !strings.Contains(string(pinned), target) {
			t.Errorf("pinned image is absent: %s", target)
		}
	}
}

func TestPinCertManagerImagesFailsClosed(t *testing.T) {
	if _, err := pinCertManagerImages([]byte("image: example.invalid/new:latest\n")); err == nil {
		t.Fatal("expected an error when required cert-manager images are absent")
	}

	var manifest strings.Builder
	for source := range certmanagerImagePins {
		if strings.Contains(source, "acmesolver") {
			_, _ = fmt.Fprintf(&manifest, "- --acme-http01-solver-image=%s\n", source)
		} else {
			_, _ = fmt.Fprintf(&manifest, "image: %s\n", source)
		}
	}
	for _, unpinned := range []string{
		"image: example.invalid/new:latest\n",
		"- --new-helper-image=example.invalid/new:latest\n",
	} {
		if _, err := pinCertManagerImages([]byte(manifest.String() + unpinned)); err == nil ||
			!strings.Contains(err.Error(), "image without digest") {
			t.Errorf("expected unpinned transitive image error for %q, got %v", unpinned, err)
		}
	}
}

// An unbounded `kubectl delete` in a teardown hook blocks on Garage's
// fail-closed finalizers and burns the rest of the shard's `go test` budget, so
// the suite reports "test timed out" against an arbitrary cleanup line instead of
// the stuck finalizer. Deletes therefore carry a deadline by default — but only
// deletes, and only when the caller did not state one.
func TestBoundKubectlDelete(t *testing.T) {
	want := "--timeout=" + defaultKubectlDeleteTimeout
	for _, tc := range []struct {
		name  string
		args  []string
		bound bool
	}{
		{"bare delete", []string{kubectlBinary, kubectlDeleteVerb, "garagecluster", "c", "-n", "ns"}, true},
		{"delete with flags before the verb", []string{kubectlBinary, "--context=kind", kubectlDeleteVerb, "ns", "x"}, true},
		{"delete all", []string{kubectlBinary, kubectlDeleteVerb, "garagecluster", "--all", "-n", "ns", "--ignore-not-found"}, true},
		{"explicit timeout is preserved", []string{kubectlBinary, kubectlDeleteVerb, "ns", "x", "--timeout=90s"}, false},
		{"non-blocking delete needs no bound", []string{kubectlBinary, kubectlDeleteVerb, "-f", "u", "--wait=false"}, false},
		{"other verbs untouched", []string{kubectlBinary, "wait", "--for=delete", "pod/x"}, false},
		{"delete only as a value is not the verb", []string{kubectlBinary, "get", "deployments", "-l", "delete"}, false},
		{"not kubectl", []string{"make", kubectlDeleteVerb}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(tc.args[0], tc.args[1:]...) // #nosec G204 -- fixed test inputs
			before := strings.Join(cmd.Args, " ")
			boundKubectlDelete(cmd)
			after := strings.Join(cmd.Args, " ")

			bounded := after != before
			if bounded != tc.bound {
				t.Fatalf("bounded=%t want %t (%q -> %q)", bounded, tc.bound, before, after)
			}
			if tc.bound && !strings.HasSuffix(after, " "+want) {
				t.Fatalf("want %q appended, got %q", want, after)
			}
			if !tc.bound && strings.Count(after, "--timeout=") > strings.Count(before, "--timeout=") {
				t.Fatalf("must not add a deadline to %q, got %q", before, after)
			}
		})
	}
}

func TestBindKubectlContext(t *testing.T) {
	t.Setenv("KIND_CLUSTER", "garage-e2e")

	cmd := exec.Command(kubectlBinary, "get", "pods")
	if err := bindKubectlContext(cmd); err != nil {
		t.Fatal(err)
	}
	if got, want := cmd.Args, []string{kubectlBinary, "--context=kind-garage-e2e", "get", "pods"}; !slices.Equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}

	matching := exec.Command(kubectlBinary, "--context", "kind-garage-e2e", "get", "pods")
	if err := bindKubectlContext(matching); err != nil {
		t.Fatalf("matching context rejected: %v", err)
	}

	foreign := exec.Command(kubectlBinary, "--context=production", "get", "pods")
	if err := bindKubectlContext(foreign); err == nil {
		t.Fatal("foreign kubectl context was accepted during Kind E2E")
	}
}

// A second pass must not stack deadlines: Run bounds the command it is given, and
// a caller may hand the same *exec.Cmd to a retry.
func TestBoundKubectlDeleteIsIdempotent(t *testing.T) {
	cmd := exec.Command(kubectlBinary, kubectlDeleteVerb, "garagecluster", "c", "-n", "ns")
	boundKubectlDelete(cmd)
	once := strings.Join(cmd.Args, " ")
	boundKubectlDelete(cmd)
	if twice := strings.Join(cmd.Args, " "); twice != once {
		t.Fatalf("second pass changed the command: %q -> %q", once, twice)
	}
}
