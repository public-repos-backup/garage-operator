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

package v1beta1

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestGarageNodeNodeLocalPoolWireContract(t *testing.T) {
	node := &GarageNode{Spec: GarageNodeSpec{
		Backing:           NodeBackingNodeLocalPool,
		NodeLocalPoolName: "local-700",
	}}

	raw, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal GarageNode: %v", err)
	}
	for _, required := range [][]byte{[]byte(`"backing":"NodeLocalPool"`), []byte(`"nodeLocalPoolName":"local-700"`)} {
		if !bytes.Contains(raw, required) {
			t.Fatalf("GarageNode node-local pool contract missing %s: %s", required, raw)
		}
	}
	for _, obsolete := range [][]byte{
		[]byte(`"backing":"StoragePool"`),
		[]byte(`"backing":"NodePool"`),
		[]byte(`"poolName"`),
		[]byte(`"nodePoolName"`),
		[]byte(`"storagePoolName"`),
	} {
		if bytes.Contains(raw, obsolete) {
			t.Fatalf("obsolete GarageNode wire contract %s was emitted: %s", obsolete, raw)
		}
	}
	if ConditionNodeLocalPoolsReady != "NodeLocalPoolsReady" {
		t.Fatalf("node-local pool condition wire value = %q", ConditionNodeLocalPoolsReady)
	}
}

func TestUnreleasedGarageNodePoolAliasesDoNotPopulateFinalAPI(t *testing.T) {
	for _, field := range []string{"poolName", "nodePoolName", "storagePoolName"} {
		raw := []byte(`{"spec":{"backing":"StoragePool","` + field + `":"local"}}`)
		var node GarageNode
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatalf("unmarshal obsolete %s field: %v", field, err)
		}
		if node.Spec.NodeLocalPoolName != "" {
			t.Fatalf("obsolete %s populated nodeLocalPoolName: %q", field, node.Spec.NodeLocalPoolName)
		}
		if node.Spec.Backing == NodeBackingNodeLocalPool {
			t.Fatalf("obsolete StoragePool backing aliased to NodeLocalPool")
		}
		if _, err := node.validateGarageNode(); err == nil || !bytes.Contains([]byte(err.Error()), []byte("backing must be")) {
			t.Fatalf("obsolete StoragePool backing was not rejected explicitly: %v", err)
		}
	}
}
