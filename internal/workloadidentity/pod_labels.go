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

// Package workloadidentity owns the Kubernetes labels that bind Garage
// workloads to immutable selectors, Services, scale targets, and durable member
// identities. API admission and renderers share this list so a user label can
// never silently change workload ownership.
package workloadidentity

import (
	"fmt"
	"sort"
	"strings"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

var reservedPodLabels = map[string]struct{}{
	"app.kubernetes.io/name":               {},
	"app.kubernetes.io/instance":           {},
	"app.kubernetes.io/component":          {},
	"app.kubernetes.io/managed-by":         {},
	"garage.rajsingh.info/cluster":         {},
	"garage.rajsingh.info/tier":            {},
	"garage.rajsingh.info/node":            {},
	"garage.rajsingh.info/storage-group":   {},
	"garage.rajsingh.info/scale-target":    {},
	"garage.rajsingh.info/node-local-pool": {},
	"garage.rajsingh.info/kubernetes-node": {},
}

// IsReservedPodLabel reports whether the operator owns a label used for
// workload identity, selection, routing, or Scale projection.
func IsReservedPodLabel(key string) bool {
	_, reserved := reservedPodLabels[key]
	return reserved
}

// ValidatePodLabels validates Kubernetes syntax and rejects operator-owned
// labels. It is the strict create-time contract.
func ValidatePodLabels(labels map[string]string, field string) error {
	if err := validatePodLabelSyntax(labels, field); err != nil {
		return err
	}
	for key := range labels {
		if IsReservedPodLabel(key) {
			return fmt.Errorf("%s: %q is operator-managed and cannot be overridden", field, key)
		}
	}
	return nil
}

// ValidatePodLabelUpdate permits an unchanged reserved label admitted by an
// older release so metadata/finalizer cleanup remains possible. Introduction or
// mutation is rejected; removal is always allowed. Renderers ignore every such
// grandfathered value and restore the operator's canonical label.
func ValidatePodLabelUpdate(oldLabels, newLabels map[string]string, field string) ([]string, error) {
	var grandfatheredReserved, grandfatheredInvalid []string
	for key, newValue := range newLabels {
		oldValue, existed := oldLabels[key]
		if syntaxError := validatePodLabel(key, newValue, field); syntaxError != nil {
			if !existed || oldValue != newValue {
				return nil, syntaxError
			}
			grandfatheredInvalid = append(grandfatheredInvalid, key)
			continue
		}
		if !IsReservedPodLabel(key) {
			continue
		}
		if !existed || oldValue != newValue {
			return nil, fmt.Errorf("%s: operator-managed label %q cannot be introduced or changed", field, key)
		}
		grandfatheredReserved = append(grandfatheredReserved, key)
	}
	var warnings []string
	if len(grandfatheredReserved) > 0 {
		sort.Strings(grandfatheredReserved)
		warnings = append(warnings, fmt.Sprintf(
			"%s contains unchanged legacy operator-managed labels (%s); the values are ignored by rendered workloads and should be removed",
			field, strings.Join(grandfatheredReserved, ", "),
		))
	}
	if len(grandfatheredInvalid) > 0 {
		sort.Strings(grandfatheredInvalid)
		warnings = append(warnings, fmt.Sprintf(
			"%s contains unchanged legacy invalid labels (%s); the values are ignored by rendered workloads and should be removed",
			field, strings.Join(grandfatheredInvalid, ", "),
		))
	}
	return warnings, nil
}

// UserPodLabels returns a copy containing only labels a user is allowed to
// control. Renderers overlay canonical operator labels after this filtering.
func UserPodLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		if !IsReservedPodLabel(key) && validatePodLabel(key, value, "podLabels") == nil {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func validatePodLabelSyntax(labels map[string]string, field string) error {
	for key, value := range labels {
		if err := validatePodLabel(key, value, field); err != nil {
			return err
		}
	}
	return nil
}

func validatePodLabel(key, value, field string) error {
	if errs := utilvalidation.IsQualifiedName(key); len(errs) > 0 {
		return fmt.Errorf("%s: invalid key %q: %s", field, key, strings.Join(errs, "; "))
	}
	if errs := utilvalidation.IsValidLabelValue(value); len(errs) > 0 {
		return fmt.Errorf("%s: invalid value %q for %q: %s", field, value, key, strings.Join(errs, "; "))
	}
	return nil
}
