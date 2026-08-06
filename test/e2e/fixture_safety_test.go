//go:build e2e
// +build e2e

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

package e2e

import "testing"

func TestFixturePathsOverlap(t *testing.T) {
	t.Parallel()

	const fixtureRoot = "/tmp/garage-e2e-ds"
	tests := []struct {
		name      string
		candidate string
		want      bool
	}{
		{name: "equal", candidate: fixtureRoot, want: true},
		{name: "descendant", candidate: fixtureRoot + "/metadata", want: true},
		{name: "ancestor", candidate: "/tmp", want: true},
		{name: "filesystem root", candidate: "/", want: true},
		{name: "cleaned descendant", candidate: fixtureRoot + "/data/../metadata", want: true},
		{name: "textual prefix sibling", candidate: fixtureRoot + "-other", want: false},
		{name: "sibling", candidate: "/tmp/garage-e2e-other", want: false},
		{name: "unrelated", candidate: "/var/lib/garage", want: false},
		{name: "empty", candidate: "", want: false},
		{name: "relative", candidate: "tmp/garage-e2e-ds", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := fixturePathsOverlap(test.candidate, fixtureRoot); got != test.want {
				t.Fatalf("fixturePathsOverlap(%q, %q) = %t, want %t",
					test.candidate, fixtureRoot, got, test.want)
			}
		})
	}
}
