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

package controller

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rajsinghtech/garage-operator/internal/garage"
)

const (
	statusConditionExampleLimit          = 5
	statusConditionMessageLimit          = 4096
	maximumReportedLayoutHistoryVersions = 64
)

// summarizeNodeLocalPoolItems keeps per-member detail on generated GarageNode
// resources instead of duplicating an unbounded inventory in parent status.
func summarizeNodeLocalPoolItems(prefix string, items []string) string {
	if len(items) == 0 {
		return prefix
	}
	examples := append([]string(nil), items...)
	sort.Strings(examples)
	total := len(examples)
	if len(examples) > statusConditionExampleLimit {
		examples = examples[:statusConditionExampleLimit]
	}
	message := fmt.Sprintf("%s (%d total; examples: %s", prefix, total, strings.Join(examples, ", "))
	if remaining := total - len(examples); remaining > 0 {
		message += fmt.Sprintf("; %d more", remaining)
	}
	message += ")"
	return limitStatusConditionMessage(message)
}

// summarizeConditionItems keeps a full inventory in its dedicated status
// field while placing only a count and bounded examples in a condition. Small
// sets keep the previous human-readable sentence shape.
func summarizeConditionItems(prefix string, items []string, suffix string) string {
	if len(items) <= statusConditionExampleLimit {
		return limitStatusConditionMessage(prefix + strings.Join(items, ", ") + suffix)
	}
	examples := append([]string(nil), items...)
	sort.Strings(examples)
	examples = examples[:statusConditionExampleLimit]
	message := fmt.Sprintf(
		"%s%d total (examples: %s; %d more)%s",
		prefix, len(items), strings.Join(examples, ", "),
		len(items)-len(examples), suffix,
	)
	return limitStatusConditionMessage(message)
}

func limitStatusConditionMessage(message string) string {
	if len(message) <= statusConditionMessageLimit {
		return message
	}
	message = message[:statusConditionMessageLimit-3]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message + "..."
}

// reportedLayoutHistoryVersions bounds an informational parent-status
// projection. Safety state machines always consume Garage's complete live
// response. Garage v2.3 returns newest versions first, but retain every
// non-Historical entry ahead of historical detail so an upstream ordering
// change cannot hide a current/draining version merely because old history is
// large.
func reportedLayoutHistoryVersions(versions []garage.LayoutVersion) []garage.LayoutVersion {
	if len(versions) <= maximumReportedLayoutHistoryVersions {
		return versions
	}
	reported := make([]garage.LayoutVersion, 0, maximumReportedLayoutHistoryVersions)
	for _, version := range versions {
		if version.Status == garage.LayoutVersionStatusHistorical {
			continue
		}
		reported = append(reported, version)
		if len(reported) == maximumReportedLayoutHistoryVersions {
			return reported
		}
	}
	for _, version := range versions {
		if version.Status != garage.LayoutVersionStatusHistorical {
			continue
		}
		reported = append(reported, version)
		if len(reported) == maximumReportedLayoutHistoryVersions {
			break
		}
	}
	return reported
}
