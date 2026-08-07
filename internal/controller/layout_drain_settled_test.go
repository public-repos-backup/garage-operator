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
	"encoding/json"
	"strings"
	"testing"

	"github.com/rajsinghtech/garage-operator/internal/garage"
)

const testDrainNodeA = "aa11"

func drainingHistory(current uint64, trackers map[string]garage.NodeUpdateTrackers) *garage.LayoutHistoryResponse {
	return &garage.LayoutHistoryResponse{
		CurrentVersion: current,
		Versions: []garage.LayoutVersion{
			{Version: current, Status: garage.LayoutVersionStatusCurrent},
			{Version: current - 1, Status: garage.LayoutVersionStatusDraining},
		},
		UpdateTrackers: trackers,
	}
}

// The exact shape that wedged a single-node cluster: the node finished its full
// table sync at the current version, but Garage never retires the previous
// version because sync_ack_map only advances inside update_update_trackers, which
// runs at startup and on merging a peer advertisement. With no peers there is no
// trigger, so the version reports Draining until the process restarts. Blocking
// there means a plain zone relabel can never take a second layout change.
func TestSettledLayoutHistoryIgnoresBookkeepingOnlyDrain(t *testing.T) {
	history := drainingHistory(2, map[string]garage.NodeUpdateTrackers{
		testDrainNodeA: {Ack: 2, Sync: 2, SyncAck: 1},
	})
	if err := requireSettledLayoutHistoryResponse(history); err != nil {
		t.Fatalf("a drain waiting only on sync_ack bookkeeping must not block a mutation: %v", err)
	}
}

// Real data movement still blocks: sync trailing the current version means the
// node has not finished replicating what the current layout assigns it.
func TestSettledLayoutHistoryBlocksWhileDataStillMoving(t *testing.T) {
	history := drainingHistory(3, map[string]garage.NodeUpdateTrackers{
		testDrainNodeA: {Ack: 3, Sync: 3, SyncAck: 2},
		"bb22":         {Ack: 3, Sync: 1, SyncAck: 1},
	})
	err := requireSettledLayoutHistoryResponse(history)
	if err == nil {
		t.Fatal("a node whose sync tracker trails the current version must block")
	}
	// The message must name the laggard, so a drain waiting on a peer that is
	// never coming back is distinguishable from one still doing work.
	if !strings.Contains(err.Error(), "bb22") {
		t.Fatalf("want the lagging node named, got: %v", err)
	}
	if strings.Contains(err.Error(), testDrainNodeA) {
		t.Fatalf("a caught-up node must not be reported as waiting: %v", err)
	}
}

// A node that is gone never advances its sync tracker, so it still blocks — that
// is what the explicit cluster-wide skip-dead-nodes recovery is for, and the PR
// deliberately does not invoke it automatically.
func TestSettledLayoutHistoryStillBlocksOnADeadPeer(t *testing.T) {
	history := drainingHistory(4, map[string]garage.NodeUpdateTrackers{
		testDrainNodeA: {Ack: 4, Sync: 4, SyncAck: 3},
		"dead":         {Ack: 1, Sync: 1, SyncAck: 1},
	})
	if err := requireSettledLayoutHistoryResponse(history); err == nil {
		t.Fatal("a dead peer holding the drain back must still block")
	}
}

// Garage omits the trackers while only one version is active. If a version
// somehow reports Draining without them, we cannot prove the data moved.
func TestSettledLayoutHistoryStaysConservativeWithoutTrackers(t *testing.T) {
	if err := requireSettledLayoutHistoryResponse(drainingHistory(2, nil)); err == nil {
		t.Fatal("absent update trackers must not be read as a settled drain")
	}
}

func TestSettledLayoutHistoryPassesWithNoDrainingVersion(t *testing.T) {
	history := &garage.LayoutHistoryResponse{
		CurrentVersion: 5,
		Versions:       []garage.LayoutVersion{{Version: 5, Status: garage.LayoutVersionStatusCurrent}},
	}
	if err := requireSettledLayoutHistoryResponse(history); err != nil {
		t.Fatalf("a single-version history is settled: %v", err)
	}
}

// issue304LayoutHistory is the GetClusterLayoutHistory body reported in #304,
// verbatim: a completed node cycle whose layout has collapsed back to one live
// version, so Garage reports the previous version Historical and omits the
// per-node update trackers entirely.
const issue304LayoutHistory = `{
  "currentVersion": 2,
  "minAck": 2,
  "versions": [
    { "version": 2, "status": "Current",    "storageNodes": 4, "gatewayNodes": 0 },
    { "version": 1, "status": "Historical", "storageNodes": 3, "gatewayNodes": 0 }
  ],
  "updateTrackers": null
}`

// A graceful node cycle wedged permanently in Syncing (#304) because the
// Syncing → Draining gate asked whether the sibling's own update tracker had
// caught up. Garage emits updateTrackers only while more than one layout
// version is live (../garage src/api/admin/layout.rs), and the sibling
// finishing its sync is exactly the event that collapses the history back to
// one — so the evidence the gate read was destroyed by the event it existed to
// detect, and the lookup missed forever.
//
// The gate now asks about the history instead of about one node, which holds
// because the two are the same question upstream. `layout.versions` carries
// only live versions (retired ones move to `old_versions`), Draining means a
// live non-current version and Historical means version < min_stored, and the
// trackers are attached iff `versions.len() > 1`. Absent trackers therefore
// mean precisely "no version is Draining" — not "unknown" — which is why the
// conservative tracker-absence branches in DataMigrationSettled and
// waitForStorageRoleDrain are unreachable against a supported Garage rather
// than merely unlikely. The reporter's caveat, that a node which never joined
// also has no tracker, is answered before this gate: reconcileCycle requires
// the sibling's Status.InLayout against its exact current Pod UID.
func TestCycleSettledLayoutHistoryDoesNotWedgeOnCollapsedLayout(t *testing.T) {
	var history garage.LayoutHistoryResponse
	if err := json.Unmarshal([]byte(issue304LayoutHistory), &history); err != nil {
		t.Fatalf("decoding the reported payload: %v", err)
	}
	if history.UpdateTrackers != nil {
		t.Fatalf("a null updateTrackers must decode to a nil map, got %v", history.UpdateTrackers)
	}
	if draining := history.GetDrainingVersions(); len(draining) != 0 {
		t.Fatalf("a collapsed history reports no Draining version, got %v", draining)
	}
	if err := requireCycleSettledLayoutHistory(&history); err != nil {
		t.Fatalf("the cycle must leave Syncing once the layout collapses: %v", err)
	}
}
