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

// Package storagecontract owns the API-neutral authorization boundary for a
// completed storage drain. Both admission versions and the reconcilers use this
// package so DELETE cannot be admitted with a token that finalization would
// later reject (or vice versa).
package storagecontract

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Actor identifies the exact Kubernetes object incarnation authorized to
// advance and consume a storage-drain transaction.
type Actor struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        string
}

// TerminalToken contains the immutable portion of a completed storage-drain
// proof. Observation details such as worker IDs remain controller state; this
// subset is the authorization contract shared at the DELETE/finalizer handoff.
type TerminalToken struct {
	Actor                    Actor
	TransactionID            string
	TargetHash               string
	StartedAt                time.Time
	CompletedAt              *time.Time
	RoleRemovalNodeIDs       []string
	RemovedStorageNodeIDs    []string
	UnavailableSourceNodeIDs []string
}

// NormalizeNodeIDs returns the canonical, sorted set representation used by
// the drain target hash.
func NormalizeNodeIDs(nodeIDs []string) []string {
	seen := make(map[string]struct{}, len(nodeIDs))
	result := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		nodeID = strings.ToLower(strings.TrimSpace(nodeID))
		if nodeID == "" {
			continue
		}
		if _, exists := seen[nodeID]; exists {
			continue
		}
		seen[nodeID] = struct{}{}
		result = append(result, nodeID)
	}
	if len(result) == 0 {
		return nil
	}
	sort.Strings(result)
	return result
}

// TargetHash fingerprints the complete removal intent. The unavailable set is
// omitted when empty for compatibility with transactions created before
// lost-source recovery was selected.
func TargetHash(roleRemovalNodeIDs, removedStorageNodeIDs []string, unavailableSourceNodeIDs ...[]string) string {
	payload := "roles\x00" + strings.Join(NormalizeNodeIDs(roleRemovalNodeIDs), "\x00") +
		"\x00storage\x00" + strings.Join(NormalizeNodeIDs(removedStorageNodeIDs), "\x00")
	if len(unavailableSourceNodeIDs) > 0 {
		unavailable := NormalizeNodeIDs(unavailableSourceNodeIDs[0])
		if len(unavailable) > 0 {
			payload += "\x00unavailable\x00" + strings.Join(unavailable, "\x00")
		}
	}
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("sha256:%x", sum[:])
}

// ValidateTerminal verifies that token is a complete, internally consistent
// authorization issued to expectedActor and includes every required target.
func ValidateTerminal(
	token *TerminalToken,
	expectedActor Actor,
	requiredRoleRemovalNodeIDs []string,
	requiredRemovedStorageNodeIDs []string,
) error {
	if token == nil {
		return fmt.Errorf("terminal storage-drain token is absent")
	}
	if err := validateActor(token.Actor, expectedActor); err != nil {
		return err
	}
	if strings.TrimSpace(token.TransactionID) == "" {
		return fmt.Errorf("terminal storage-drain transaction ID is empty")
	}
	if token.StartedAt.IsZero() || token.CompletedAt == nil || token.CompletedAt.IsZero() ||
		token.CompletedAt.Before(token.StartedAt) {
		return fmt.Errorf("terminal storage-drain timestamps are invalid")
	}

	roles, err := validateCanonicalNodeIDSet("role-removal", token.RoleRemovalNodeIDs)
	if err != nil {
		return err
	}
	removedStorage, err := validateCanonicalNodeIDSet("removed-storage", token.RemovedStorageNodeIDs)
	if err != nil {
		return err
	}
	unavailable, err := validateCanonicalNodeIDSet("unavailable-source", token.UnavailableSourceNodeIDs)
	if err != nil {
		return err
	}

	roleSet := makeSet(roles)
	removedStorageSet := makeSet(removedStorage)
	for _, nodeID := range removedStorage {
		if _, exists := roleSet[nodeID]; !exists {
			return fmt.Errorf("removed storage node %s is absent from the role-removal target set", nodeID)
		}
	}
	for _, nodeID := range unavailable {
		if _, exists := removedStorageSet[nodeID]; !exists {
			return fmt.Errorf("unavailable source node %s is absent from the removed-storage target set", nodeID)
		}
	}

	expectedHash := TargetHash(roles, removedStorage, unavailable)
	if token.TargetHash != expectedHash {
		return fmt.Errorf("terminal storage-drain target hash does not match its target sets")
	}
	if err := requireNodeIDs("role-removal", roleSet, requiredRoleRemovalNodeIDs); err != nil {
		return err
	}
	if err := requireNodeIDs("removed-storage", removedStorageSet, requiredRemovedStorageNodeIDs); err != nil {
		return err
	}
	return nil
}

func validateActor(actual, expected Actor) error {
	if strings.TrimSpace(expected.UID) == "" {
		return fmt.Errorf("expected storage-drain actor UID is empty")
	}
	if actual != expected {
		return fmt.Errorf("terminal storage-drain actor does not match the expected Kubernetes object incarnation")
	}
	if strings.TrimSpace(actual.APIVersion) == "" || strings.TrimSpace(actual.Kind) == "" ||
		strings.TrimSpace(actual.Namespace) == "" || strings.TrimSpace(actual.Name) == "" {
		return fmt.Errorf("terminal storage-drain actor identity is incomplete")
	}
	return nil
}

func validateCanonicalNodeIDSet(label string, nodeIDs []string) ([]string, error) {
	normalized := NormalizeNodeIDs(nodeIDs)
	if len(normalized) != len(nodeIDs) {
		return nil, fmt.Errorf("terminal storage-drain %s target set is not canonical", label)
	}
	for i := range normalized {
		if normalized[i] != nodeIDs[i] {
			return nil, fmt.Errorf("terminal storage-drain %s target set is not canonical", label)
		}
		if !validNodeID(normalized[i]) {
			return nil, fmt.Errorf("terminal storage-drain %s target %q is not a 64-character hexadecimal Garage node ID", label, normalized[i])
		}
	}
	return normalized, nil
}

func requireNodeIDs(label string, targetSet map[string]struct{}, required []string) error {
	for _, nodeID := range NormalizeNodeIDs(required) {
		if _, exists := targetSet[nodeID]; !exists {
			return fmt.Errorf("required node %s is absent from the %s target set", nodeID, label)
		}
	}
	return nil
}

func makeSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func validNodeID(nodeID string) bool {
	if len(nodeID) != 64 {
		return false
	}
	for _, char := range nodeID {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
