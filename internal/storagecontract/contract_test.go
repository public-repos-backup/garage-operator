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

package storagecontract

import (
	"strings"
	"testing"
	"time"
)

func validTerminalToken() (*TerminalToken, Actor, string) {
	nodeID := strings.Repeat("a", 64)
	actor := Actor{
		APIVersion: "garage.rajsingh.info/v1beta1",
		Kind:       "GarageNode",
		Namespace:  "garage",
		Name:       "storage-a",
		UID:        "node-uid",
	}
	started := time.Now().Add(-time.Minute)
	completed := time.Now()
	return &TerminalToken{
		Actor:                 actor,
		TransactionID:         "transaction",
		TargetHash:            TargetHash([]string{nodeID}, []string{nodeID}),
		StartedAt:             started,
		CompletedAt:           &completed,
		RoleRemovalNodeIDs:    []string{nodeID},
		RemovedStorageNodeIDs: []string{nodeID},
	}, actor, nodeID
}

func TestValidateTerminalAcceptsExactToken(t *testing.T) {
	token, actor, nodeID := validTerminalToken()
	if err := ValidateTerminal(token, actor, []string{nodeID}, []string{nodeID}); err != nil {
		t.Fatalf("valid terminal token rejected: %v", err)
	}
}

func TestValidateTerminalRejectsMalformedTargetShape(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TerminalToken)
	}{
		{name: "non hexadecimal ID", mutate: func(token *TerminalToken) {
			token.RoleRemovalNodeIDs = []string{"not-a-node-id"}
			token.RemovedStorageNodeIDs = []string{"not-a-node-id"}
			token.TargetHash = TargetHash(token.RoleRemovalNodeIDs, token.RemovedStorageNodeIDs)
		}},
		{name: "unsorted IDs", mutate: func(token *TerminalToken) {
			second := strings.Repeat("b", 64)
			token.RoleRemovalNodeIDs = []string{second, token.RoleRemovalNodeIDs[0]}
			token.TargetHash = TargetHash(token.RoleRemovalNodeIDs, token.RemovedStorageNodeIDs)
		}},
		{name: "storage outside role set", mutate: func(token *TerminalToken) {
			token.RemovedStorageNodeIDs = []string{strings.Repeat("b", 64)}
			token.TargetHash = TargetHash(token.RoleRemovalNodeIDs, token.RemovedStorageNodeIDs)
		}},
		{name: "unavailable outside storage set", mutate: func(token *TerminalToken) {
			token.UnavailableSourceNodeIDs = []string{strings.Repeat("b", 64)}
			token.TargetHash = TargetHash(token.RoleRemovalNodeIDs, token.RemovedStorageNodeIDs, token.UnavailableSourceNodeIDs)
		}},
		{name: "self inconsistent hash", mutate: func(token *TerminalToken) {
			token.TargetHash = "sha256:corrupt"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token, actor, nodeID := validTerminalToken()
			test.mutate(token)
			if err := ValidateTerminal(token, actor, []string{nodeID}, []string{nodeID}); err == nil {
				t.Fatal("malformed terminal token was accepted")
			}
		})
	}
}
