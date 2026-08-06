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

package workloadidentity

import (
	"strings"
	"testing"
)

func TestPodLabelContract(t *testing.T) {
	t.Parallel()
	reserved := "garage.rajsingh.info/storage-group"
	invalid := "not a label"
	oldLabels := map[string]string{
		reserved:            "hostile",
		invalid:             "legacy",
		"example.com/media": "nvme",
	}

	if err := ValidatePodLabels(oldLabels, "spec.podLabels"); err == nil {
		t.Fatal("strict create validation accepted reserved/invalid labels")
	}
	warnings, err := ValidatePodLabelUpdate(oldLabels, oldLabels, "spec.podLabels")
	if err != nil {
		t.Fatalf("unchanged legacy labels stranded an update: %v", err)
	}
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, " "), "operator-managed") ||
		!strings.Contains(strings.Join(warnings, " "), "invalid") {
		t.Fatalf("expected explicit reserved and invalid grandfather warnings, got %v", warnings)
	}
	filtered := UserPodLabels(oldLabels)
	if len(filtered) != 1 || filtered["example.com/media"] != "nvme" {
		t.Fatalf("renderer filter retained an unsafe label: %#v", filtered)
	}

	changedReserved := map[string]string{reserved: "different"}
	if _, err := ValidatePodLabelUpdate(oldLabels, changedReserved, "spec.podLabels"); err == nil {
		t.Fatal("reserved label mutation was accepted")
	}
	changedInvalid := map[string]string{invalid: "different"}
	if _, err := ValidatePodLabelUpdate(oldLabels, changedInvalid, "spec.podLabels"); err == nil {
		t.Fatal("invalid label mutation was accepted")
	}
	if warnings, err := ValidatePodLabelUpdate(oldLabels, map[string]string{"example.com/media": "hdd"}, "spec.podLabels"); err != nil || len(warnings) != 0 {
		t.Fatalf("cleanup plus ordinary user-label update was rejected: warnings=%v err=%v", warnings, err)
	}
}
