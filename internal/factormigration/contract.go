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

// Package factormigration owns the API-neutral request contract for a
// coordinated Garage replication-factor migration. Both served API versions
// and the controller use the same parser so admission cannot accept a request
// the reconciler interprets differently.
package factormigration

import (
	"fmt"
	"strconv"
	"strings"
)

const Annotation = "garage.rajsingh.info/purge-cluster-layout"

type Request struct {
	Factor int
	Force  bool
}

// Parse accepts the documented factor=N or factor=N,force request. Component
// order and surrounding whitespace are tolerated, but duplicate/unknown
// components are rejected so one annotation has exactly one meaning.
func Parse(value string) (Request, error) {
	request := Request{}
	seenFactor := false
	seenForce := false
	for _, raw := range strings.Split(value, ",") {
		part := strings.TrimSpace(raw)
		switch {
		case part == "force":
			if seenForce {
				return Request{}, fmt.Errorf("invalid purge annotation %q: duplicate force", value)
			}
			seenForce = true
			request.Force = true
		case strings.HasPrefix(part, "factor="):
			if seenFactor {
				return Request{}, fmt.Errorf("invalid purge annotation %q: duplicate factor", value)
			}
			factor, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(part, "factor=")))
			if err != nil || factor < 1 {
				return Request{}, fmt.Errorf("invalid purge annotation %q: factor must be a positive integer", value)
			}
			seenFactor = true
			request.Factor = factor
		default:
			return Request{}, fmt.Errorf("invalid purge annotation %q (expected \"factor=N[,force]\")", value)
		}
	}
	if !seenFactor {
		return Request{}, fmt.Errorf("invalid purge annotation %q: missing factor=N", value)
	}
	return request, nil
}

// ValidateUpdate requires the destructive request and the new factor to enter
// Kubernetes atomically. Without this boundary, the ordinary config rollout
// can restart one Garage process at the new factor before the purge state
// machine has quiesced every process.
func ValidateUpdate(oldFactor, newFactor int, annotations map[string]string) error {
	if oldFactor == 0 || oldFactor == newFactor {
		return nil
	}
	value := annotations[Annotation]
	if value == "" {
		return fmt.Errorf(
			"changing spec.replication.factor from %d to %d requires setting %s=%q in the same API update so the coordinated purge starts before any sequential pod rollout",
			oldFactor, newFactor, Annotation, fmt.Sprintf("factor=%d", newFactor),
		)
	}
	request, err := Parse(value)
	if err != nil {
		return err
	}
	if request.Factor != newFactor {
		return fmt.Errorf("%s requests factor=%d but spec.replication.factor is %d", Annotation, request.Factor, newFactor)
	}
	return nil
}
