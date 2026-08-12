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
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
	"github.com/rajsinghtech/garage-operator/internal/garage"
)

func TestLayoutMutationCoordinatorSerializesPerGarageCluster(t *testing.T) {
	coordinator := NewLayoutMutationCoordinator()
	key := types.NamespacedName{Namespace: testGarageValue, Name: testSiteA}

	release, acquired := coordinator.TryAcquire(key)
	if !acquired {
		t.Fatal("first writer did not acquire its GarageCluster")
	}
	if _, acquired := coordinator.TryAcquire(key); acquired {
		t.Fatal("second writer acquired the same GarageCluster concurrently")
	}

	otherRelease, acquired := coordinator.TryAcquire(types.NamespacedName{Namespace: testGarageValue, Name: "site-b"})
	if !acquired {
		t.Fatal("an unrelated GarageCluster should not share the lock")
	}
	otherRelease()

	release()
	release() // release is deliberately idempotent
	releaseAgain, acquired := coordinator.TryAcquire(key)
	if !acquired {
		t.Fatal("GarageCluster lock was not released")
	}
	releaseAgain()
}

func TestLayoutMutationCoordinatorScopesRolloutMarkerToClusterUID(t *testing.T) {
	coordinator := NewLayoutMutationCoordinator()
	key := types.NamespacedName{Namespace: testGarageValue, Name: testSiteA}
	oldUID := types.UID("old-cluster-uid")
	newUID := types.UID("new-cluster-uid")

	coordinator.BeginNodeLocalPoolRollout(key, oldUID, key, oldUID)
	if !coordinator.NodeLocalPoolRolloutActive(key) {
		t.Fatal("old incarnation rollout marker was not recorded")
	}
	if err := coordinator.PruneStaleNodeLocalPoolRollout(context.Background(), nil, key, newUID); err != nil {
		t.Fatal(err)
	}
	if coordinator.NodeLocalPoolRolloutActive(key) {
		t.Fatal("same-name recreated GarageCluster inherited the old incarnation's rollout marker")
	}

	coordinator.BeginNodeLocalPoolRollout(key, newUID, key, newUID)
	coordinator.BeginNodeLocalPoolRollout(key, oldUID, key, oldUID)
	coordinator.EndNodeLocalPoolRollout(key, oldUID)
	if !coordinator.NodeLocalPoolRolloutActive(key) {
		t.Fatal("stale old-incarnation worker cleared or replaced the new rollout marker")
	}
	coordinator.EndNodeLocalPoolRollout(key, newUID)
	if coordinator.NodeLocalPoolRolloutActive(key) {
		t.Fatal("current incarnation could not clear its rollout marker")
	}
}

func TestCanonicalRolloutRehydrationDiscoversEdgeSourceAndRoutesOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	storage := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{
		Name: tierStorage, Namespace: testGarageValue, UID: testStorageClusterUID,
	}}
	edge := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-a", Namespace: testGarageValue, UID: "edge-a-uid"},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: storage.Name},
		}},
		Status: garagev1beta2.GarageClusterStatus{StorageRollout: &garagev1beta2.StorageRolloutStatus{}},
	}
	observer := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-b", Namespace: testGarageValue, UID: "edge-b-uid"},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: storage.Name},
		}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(storage, edge, observer).Build()
	coordinator := NewLayoutMutationCoordinator()
	if err := rehydrateNodeLocalPoolRolloutsForOwner(ctx, reader, coordinator, storage, false); err != nil {
		t.Fatal(err)
	}
	key := layoutOwnerKey(storage)
	owned, confirmed := coordinator.NodeLocalPoolRolloutSourceActive(key, edge.UID)
	if !owned || !confirmed {
		t.Fatalf("edge durable rollout was not rehydrated on canonical owner: owned=%v confirmed=%v", owned, confirmed)
	}
	if err := coordinator.PruneStaleNodeLocalPoolRollout(ctx, reader, key, storage.UID); err != nil {
		t.Fatal(err)
	}
	if !coordinator.NodeLocalPoolRolloutActive(key) {
		t.Fatal("canonical storage owner pruned a live edge-source rollout marker")
	}

	reconciler := &GarageClusterReconciler{
		Client: reader, APIReader: reader, LayoutMutations: coordinator,
	}
	blocked, err := reconciler.rehydrateLayoutOwnerRollout(ctx, edge)
	if err != nil || blocked {
		t.Fatalf("edge source must be allowed to recover its own transaction: blocked=%v err=%v", blocked, err)
	}
	blocked, err = reconciler.rehydrateLayoutOwnerRollout(ctx, observer)
	if err != nil || !blocked {
		t.Fatalf("different edge source must wait behind canonical transaction: blocked=%v err=%v", blocked, err)
	}
	coordinator.EndNodeLocalPoolRollout(key, storage.UID)
	if !coordinator.NodeLocalPoolRolloutActive(key) {
		t.Fatal("canonical owner source UID incorrectly cleared edge source marker")
	}
	coordinator.EndNodeLocalPoolRollout(key, edge.UID)
	if coordinator.NodeLocalPoolRolloutActive(key) {
		t.Fatal("edge source could not clear its own canonical marker")
	}
}

func TestEndpointAliasesShareSyntheticRolloutOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	endpoint := "https://garage.example.test:3903"
	source := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-a", Namespace: testGarageValue, UID: "edge-a-uid"},
		Spec:       garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{AdminAPIEndpoint: endpoint}},
		Status:     garagev1beta2.GarageClusterStatus{StorageRollout: &garagev1beta2.StorageRolloutStatus{}},
	}
	alias := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-b", Namespace: testGarageValue, UID: "edge-b-uid"},
		Spec:       garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{AdminAPIEndpoint: endpoint + "/"}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, alias).Build()
	coordinator := NewLayoutMutationCoordinator()
	if layoutOwnerKey(source) != layoutOwnerKey(alias) || layoutRolloutOwnerID(source) != layoutRolloutOwnerID(alias) {
		t.Fatal("normalized endpoint aliases did not share a canonical key and synthetic owner identity")
	}
	if err := rehydrateNodeLocalPoolRolloutsForOwner(ctx, reader, coordinator, alias, false); err != nil {
		t.Fatal(err)
	}
	owned, confirmed := coordinator.NodeLocalPoolRolloutSourceActive(layoutOwnerKey(alias), source.UID)
	if !owned || !confirmed {
		t.Fatal("endpoint alias did not discover the other alias's durable rollout")
	}
	reconciler := &GarageClusterReconciler{
		Client: reader, APIReader: reader, LayoutMutations: coordinator,
	}
	blocked, err := reconciler.rehydrateLayoutOwnerRollout(ctx, source)
	if err != nil || blocked {
		t.Fatalf("endpoint source could not route its own transaction: blocked=%v err=%v", blocked, err)
	}
	blocked, err = reconciler.rehydrateLayoutOwnerRollout(ctx, alias)
	if err != nil || !blocked {
		t.Fatalf("different endpoint alias was not frozen behind active transaction: blocked=%v err=%v", blocked, err)
	}
}

type namespaceOnlyReader struct {
	client.Reader
	namespace string
}

func (r namespaceOnlyReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	options := &client.ListOptions{}
	for _, option := range opts {
		option.ApplyToList(options)
	}
	if options.Namespace != r.namespace {
		return stderrors.New("cluster-wide List forbidden")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestNamespaceScopedRolloutRehydrationUsesNamespacedList(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{
		Name: testSiteA, Namespace: testGarageValue, UID: "site-a-uid",
	}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	restricted := namespaceOnlyReader{Reader: reader, namespace: owner.Namespace}
	if err := rehydrateNodeLocalPoolRolloutsForOwner(ctx, restricted, NewLayoutMutationCoordinator(), owner, false); err != nil {
		t.Fatalf("namespace-scoped PVC/SMB rehydration attempted a cluster-wide List: %v", err)
	}
	if err := rehydrateNodeLocalPoolRolloutsForOwner(ctx, restricted, NewLayoutMutationCoordinator(), owner, true); err == nil {
		t.Fatal("cluster-scoped discovery test seam did not attempt an all-namespace List")
	}
}

func TestCanonicalDrainRehydrationDiscoversMultiHopGatewaySource(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	storage := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tierStorage, Namespace: testGarageValue, UID: testStorageClusterUID},
		Spec:       garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{}},
	}
	handle := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "layout-handle", Namespace: testGarageValue, UID: "handle-uid"},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: storage.Name},
		}},
	}
	now := metav1.Now()
	edge := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-source", Namespace: testGarageValue, UID: testEdgeClusterUID},
		Spec: garagev1beta2.GarageClusterSpec{
			Gateway: &garagev1beta2.GatewaySpec{},
			ConnectTo: &garagev1beta2.ConnectToConfig{ClusterRef: &garagev1beta2.ClusterReference{
				Name: handle.Name,
			}},
		},
	}
	actor := storageDrainActorForCluster(edge)
	proof := &blockResyncProof{
		Actor: actor, TransactionID: "gateway-retirement", StartedAt: now,
		RoleRemovalNodeIDs: []string{testOwnedNodeID},
	}
	proof.TargetHash = storageDrainProofTargetHash(proof)
	edge.Status.StorageDrain = v1beta2StorageDrainStatus(proof)

	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(storage, handle, edge).Build()
	coordinator := NewLayoutMutationCoordinator()
	if err := rehydrateStorageDrainsForOwner(ctx, reader, coordinator, storage, false, true); err != nil {
		t.Fatal(err)
	}
	key := layoutOwnerKey(storage)
	ownerID := layoutRolloutOwnerID(storage)
	if !coordinator.StorageDrainActorActive(key, ownerID, actor.UID, proof.TransactionID, proof.TargetHash) {
		t.Fatal("owner-first restart did not rehydrate the edge source on its fully resolved canonical owner")
	}
	if coordinator.StorageDrainActorActive(key, layoutRolloutOwnerID(edge), actor.UID, proof.TransactionID, proof.TargetHash) {
		t.Fatal("multi-hop source UID was incorrectly used as the canonical marker owner")
	}
	coordinator.PruneStaleStorageDrain(key, storage.UID)
	if !coordinator.StorageDrainActive(key) {
		t.Fatal("canonical owner pruning deleted a live multi-hop gateway-retirement marker")
	}
}

func TestDrainRehydrationPrunesOnlyConfirmedStatusClearTail(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tierStorage, Namespace: testGarageValue, UID: testStorageClusterUID},
		Spec:       garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	key := layoutOwnerKey(owner)
	ownerID := layoutRolloutOwnerID(owner)
	actorUID := types.UID("drain-actor")

	provisional := NewLayoutMutationCoordinator()
	if !provisional.BeginStorageDrain(key, ownerID, actorUID, "transaction", "target") {
		t.Fatal("could not establish provisional drain marker")
	}
	if err := rehydrateStorageDrainsForOwner(ctx, reader, provisional, owner.DeepCopy(), false, true); err != nil {
		t.Fatal(err)
	}
	if !provisional.StorageDrainActive(key) {
		t.Fatal("authoritative nil status pruned a provisional marker whose status write may still be in flight")
	}

	confirmed := NewLayoutMutationCoordinator()
	if !confirmed.BeginStorageDrain(key, ownerID, actorUID, "transaction", "target") ||
		!confirmed.ConfirmStorageDrain(key, ownerID, actorUID, "transaction", "target") {
		t.Fatal("could not establish confirmed drain marker")
	}
	if err := rehydrateStorageDrainsForOwner(ctx, reader, confirmed, owner.DeepCopy(), false, true); err != nil {
		t.Fatal(err)
	}
	if confirmed.StorageDrainActive(key) {
		t.Fatal("confirmed status-clear crash tail was not pruned after an authoritative nil-status scan")
	}
}

func TestEndpointAliasDrainRehydrationUsesSyntheticCanonicalOwner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	endpoint := "https://garage.example.test:3903"
	source := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-source", Namespace: testGarageValue, UID: testEdgeClusterUID},
		Spec: garagev1beta2.GarageClusterSpec{
			Gateway:   &garagev1beta2.GatewaySpec{},
			ConnectTo: &garagev1beta2.ConnectToConfig{AdminAPIEndpoint: endpoint},
		},
	}
	alias := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint-alias", Namespace: testGarageValue, UID: "endpoint-alias-uid"},
		Spec:       garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{AdminAPIEndpoint: endpoint + "/"}},
	}
	actor := storageDrainActorForCluster(source)
	proof := &blockResyncProof{
		Actor: actor, TransactionID: "endpoint-retirement", StartedAt: metav1.Now(),
		RoleRemovalNodeIDs: []string{testOwnedNodeID},
	}
	proof.TargetHash = storageDrainProofTargetHash(proof)
	source.Status.StorageDrain = v1beta2StorageDrainStatus(proof)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, alias).Build()
	coordinator := NewLayoutMutationCoordinator()
	if err := rehydrateStorageDrainsForOwner(ctx, reader, coordinator, alias, false, true); err != nil {
		t.Fatal(err)
	}
	key := layoutOwnerKey(alias)
	ownerID := layoutRolloutOwnerID(alias)
	if ownerID == alias.UID || ownerID == source.UID {
		t.Fatalf("endpoint layout owner identity %q is not synthetic", ownerID)
	}
	if !coordinator.StorageDrainActorActive(key, ownerID, actor.UID, proof.TransactionID, proof.TargetHash) {
		t.Fatal("endpoint alias did not rehydrate the source drain under their shared synthetic owner")
	}
}

func TestExplicitDeadNodeRecoveryCrossesDrainButNotManagedPodRollout(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testSiteA, Namespace: testGarageValue, UID: testStorageClusterUID},
		Spec:       garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{}},
	}
	actor := storageDrainActorForCluster(owner)
	proof := &blockResyncProof{
		Actor: actor, TransactionID: "waiting-for-dead-peer", StartedAt: metav1.Now(),
		RoleRemovalNodeIDs: []string{testOwnedNodeID},
	}
	proof.TargetHash = storageDrainProofTargetHash(proof)
	owner.Status.StorageDrain = v1beta2StorageDrainStatus(proof)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	coordinator := NewLayoutMutationCoordinator()
	key := layoutOwnerKey(owner)
	ownerID := layoutRolloutOwnerID(owner)
	if !coordinator.BeginStorageDrain(key, ownerID, actor.UID, proof.TransactionID, proof.TargetHash) {
		t.Fatal("could not establish active drain marker")
	}

	mutations := 0
	mutate := func() error {
		mutations++
		return nil
	}
	if err := runResolvedExplicitDeadNodeRecoveryMutation(ctx, reader, coordinator, owner, mutate); err != nil {
		t.Fatalf("explicit dead-node recovery was blocked by the drain it is intended to recover: %v", err)
	}
	if mutations != 1 {
		t.Fatalf("explicit recovery mutations = %d, want 1", mutations)
	}

	if !coordinator.BeginNodeLocalPoolRollout(key, ownerID, client.ObjectKeyFromObject(owner), owner.UID) {
		t.Fatal("could not establish managed-Pod rollout exclusion")
	}
	err := runResolvedExplicitDeadNodeRecoveryMutation(ctx, reader, coordinator, owner, mutate)
	if !stderrors.Is(err, errLayoutMutationPending) {
		t.Fatalf("explicit dead-node recovery crossed an active managed-Pod rollout: %v", err)
	}
	if mutations != 1 {
		t.Fatalf("excluded explicit recovery ran mutate %d times, want 1", mutations)
	}
}

func TestRunLayoutMutationLosingWriterReturnsPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/GetClusterLayoutHistory" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"currentVersion":1,"versions":[{"version":1,"status":"current"}]}`))
	}))
	defer server.Close()

	coordinator := NewLayoutMutationCoordinator()
	cluster := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{Namespace: testGarageValue, Name: testSiteA}}
	client := garage.NewClient(server.URL, "token")
	entered := make(chan struct{})
	releaseMutation := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runLayoutMutation(context.Background(), coordinator, cluster, client, func() error {
			close(entered)
			<-releaseMutation
			return nil
		})
	}()
	<-entered

	err := runLayoutMutation(context.Background(), coordinator, cluster, client, func() error {
		t.Fatal("losing writer entered the critical section")
		return nil
	})
	if !stderrors.Is(err, errLayoutMutationPending) {
		t.Fatalf("losing writer error = %v, want errLayoutMutationPending", err)
	}
	close(releaseMutation)
	if err := <-done; err != nil {
		t.Fatalf("winning writer failed: %v", err)
	}
}

func TestRequireSettledLayoutHistoryResponseRejectsDrainingVersion(t *testing.T) {
	err := requireSettledLayoutHistoryResponse(&garage.LayoutHistoryResponse{
		CurrentVersion: 4,
		Versions: []garage.LayoutVersion{
			{Version: 3, Status: garage.LayoutVersionStatusDraining},
			{Version: 4, Status: garage.LayoutVersionStatusCurrent},
		},
	})
	if !stderrors.Is(err, errLayoutMutationPending) {
		t.Fatalf("draining history error = %v, want errLayoutMutationPending", err)
	}
}

func TestRequireExclusiveStagedLayoutChangesRejectsForeignState(t *testing.T) {
	capacity := uint64(1024)
	intended := garage.NodeRoleChange{ID: testOwnedNodeID, Zone: testSiteA, Capacity: &capacity}
	foreign := garage.NodeRoleChange{ID: testForeignValue, Remove: true}
	parameters := &garage.LayoutParameters{}

	tests := []struct {
		name   string
		layout *garage.ClusterLayout
		roles  []garage.NodeRoleChange
		params *garage.LayoutParameters
		all    bool
		want   bool
	}{
		{
			name:   "foreign role",
			layout: &garage.ClusterLayout{StagedRoleChanges: []garage.NodeRoleChange{foreign}},
			roles:  []garage.NodeRoleChange{intended},
			want:   true,
		},
		{
			name:   "mismatched owned role",
			layout: &garage.ClusterLayout{StagedRoleChanges: []garage.NodeRoleChange{{ID: intended.ID, Remove: true}}},
			roles:  []garage.NodeRoleChange{intended},
			want:   true,
		},
		{
			name:   "foreign parameters",
			layout: &garage.ClusterLayout{StagedParameters: parameters},
			want:   true,
		},
		{
			name:   "missing intended role after stage",
			layout: &garage.ClusterLayout{},
			roles:  []garage.NodeRoleChange{intended},
			all:    true,
			want:   true,
		},
		{
			name:   "exact interrupted residue",
			layout: &garage.ClusterLayout{StagedRoleChanges: []garage.NodeRoleChange{intended}, StagedParameters: parameters},
			roles:  []garage.NodeRoleChange{intended},
			params: parameters,
			all:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireExclusiveStagedLayoutChanges(tt.layout, tt.roles, tt.params, tt.all)
			if got := stderrors.Is(err, errLayoutMutationPending); got != tt.want {
				t.Fatalf("pending=%v error=%v, want pending=%v", got, err, tt.want)
			}
		})
	}
}

func TestStageAndApplyExclusiveLayoutResumesOnlyExactResidue(t *testing.T) {
	owned := garage.NodeRoleChange{ID: testOwnedNodeID, Remove: true}
	staged := []garage.NodeRoleChange{owned}
	applyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case pathGetClusterLayout:
			_ = json.NewEncoder(w).Encode(garage.ClusterLayout{Version: 3, StagedRoleChanges: staged})
		case pathApplyLayout:
			applyCalls++
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	client := garage.NewClient(server.URL, "token")
	initial := &garage.ClusterLayout{Version: 3, StagedRoleChanges: []garage.NodeRoleChange{owned}}
	if _, err := stageAndApplyExclusiveLayout(context.Background(), client, initial, []garage.NodeRoleChange{owned}, nil, nil); err != nil {
		t.Fatalf("exact interrupted residue was not resumed: %v", err)
	}
	if applyCalls != 1 {
		t.Fatalf("Apply calls = %d, want 1", applyCalls)
	}
}

func TestStageAndApplyExclusiveLayoutRejectsPostStageForeignChange(t *testing.T) {
	owned := garage.NodeRoleChange{ID: testOwnedNodeID, Remove: true}
	foreign := garage.NodeRoleChange{ID: testForeignValue, Remove: true}
	var staged []garage.NodeRoleChange
	applyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case pathGetClusterLayout:
			_ = json.NewEncoder(w).Encode(garage.ClusterLayout{Version: 3, StagedRoleChanges: staged})
		case pathUpdateLayout:
			var update garage.UpdateClusterLayoutRequest
			_ = json.NewDecoder(request.Body).Decode(&update)
			staged = append(append([]garage.NodeRoleChange(nil), update.Roles...), foreign)
		case pathApplyLayout:
			applyCalls++
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	client := garage.NewClient(server.URL, "token")
	_, err := stageAndApplyExclusiveLayout(
		context.Background(),
		client,
		&garage.ClusterLayout{Version: 3},
		[]garage.NodeRoleChange{owned},
		nil,
		func() error { return client.UpdateClusterLayout(context.Background(), []garage.NodeRoleChange{owned}) },
	)
	if !stderrors.Is(err, errLayoutMutationPending) {
		t.Fatalf("post-stage foreign change error = %v, want pending", err)
	}
	if applyCalls != 0 {
		t.Fatalf("foreign staging state was applied %d time(s)", applyCalls)
	}
}

func TestLayoutOwnerKeySharesReferencedAndExternalLayouts(t *testing.T) {
	storage := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{Name: tierStorage, Namespace: testGarageValue}}
	gateway := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tierGateway, Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: tierStorage},
		}},
	}
	if layoutOwnerKey(storage) != layoutOwnerKey(gateway) {
		t.Fatalf("clusterRef gateway key %v != storage key %v", layoutOwnerKey(gateway), layoutOwnerKey(storage))
	}

	external := func(name, endpoint string) *garagev1beta2.GarageCluster {
		return &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testGarageValue},
			Spec:       garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{AdminAPIEndpoint: endpoint}},
		}
	}
	left := layoutOwnerKey(external("left", "HTTPS://GARAGE.EXAMPLE:3903/"))
	right := layoutOwnerKey(external("right", "https://garage.example:3903"))
	if left != right {
		t.Fatalf("equivalent external endpoints produced different keys: %v != %v", left, right)
	}
	if left == layoutOwnerKey(external("other", "https://other.example:3903")) {
		t.Fatal("different external layouts produced the same key")
	}
}

func TestResolveGarageLayoutOwnerFollowsCanonicalChains(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	storage := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tierStorage, Namespace: testGarageValue, UID: testStorageClusterUID},
		Spec:       garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{}},
	}
	handleB := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "handle-b", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: storage.Name},
		}},
	}
	handleA := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "handle-a", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: handleB.Name, Namespace: handleB.Namespace},
		}},
	}
	edge := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testEdgeValue, Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{
			Gateway:   &garagev1beta2.GatewaySpec{},
			ConnectTo: &garagev1beta2.ConnectToConfig{ClusterRef: &garagev1beta2.ClusterReference{Name: handleA.Name}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(storage, handleB, handleA, edge).Build()
	owner, err := resolveGarageLayoutOwner(context.Background(), c, edge)
	if err != nil {
		t.Fatal(err)
	}
	if owner.UID != storage.UID || layoutOwnerKey(owner) != (types.NamespacedName{Namespace: storage.Namespace, Name: storage.Name}) {
		t.Fatalf("resolved owner = %s/%s uid=%s, want storage", owner.Namespace, owner.Name, owner.UID)
	}
}

func TestResolveGarageLayoutOwnerCollapsesHandleAndDirectEndpointAliases(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	handle := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "external-store", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			AdminAPIEndpoint: "HTTPS://GARAGE.EXAMPLE:3903/",
		}},
	}
	edgeViaHandle := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-handle", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{
			Gateway:   &garagev1beta2.GatewaySpec{},
			ConnectTo: &garagev1beta2.ConnectToConfig{ClusterRef: &garagev1beta2.ClusterReference{Name: handle.Name}},
		},
	}
	direct := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-direct", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{
			Gateway:   &garagev1beta2.GatewaySpec{},
			ConnectTo: &garagev1beta2.ConnectToConfig{AdminAPIEndpoint: "https://garage.example:3903"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(handle, edgeViaHandle, direct).Build()
	owner, err := resolveGarageLayoutOwner(context.Background(), c, edgeViaHandle)
	if err != nil {
		t.Fatal(err)
	}
	if layoutOwnerKey(owner) != layoutOwnerKey(direct) {
		t.Fatalf("edge via handle key %v != direct endpoint key %v", layoutOwnerKey(owner), layoutOwnerKey(direct))
	}
}

func TestResolveGarageLayoutOwnerRejectsReferenceCycle(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	a := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: "b"},
		}},
	}
	b := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{ConnectTo: &garagev1beta2.ConnectToConfig{
			ClusterRef: &garagev1beta2.ClusterReference{Name: "a"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b).Build()
	_, err := resolveGarageLayoutOwner(context.Background(), c, a)
	if err == nil || !strings.Contains(err.Error(), "cyclic GarageCluster connectTo.clusterRef chain") {
		t.Fatalf("reference-cycle error = %v", err)
	}
}
