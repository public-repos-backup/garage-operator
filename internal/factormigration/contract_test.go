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

package factormigration

import "testing"

func TestValidateUpdateRequiresMatchingAtomicRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		annotations map[string]string
		wantErr     bool
	}{
		{name: "missing", wantErr: true},
		{name: "mismatch", annotations: map[string]string{Annotation: "factor=3"}, wantErr: true},
		{name: "malformed", annotations: map[string]string{Annotation: "factor=2,factor=2"}, wantErr: true},
		{name: "matching", annotations: map[string]string{Annotation: "factor=2"}},
		{name: "matching forced", annotations: map[string]string{Annotation: "force, factor=2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateUpdate(1, 2, test.annotations)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateUpdate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
