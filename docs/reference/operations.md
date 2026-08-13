# Annotations and conditions

Annotations are imperative requests layered onto declarative resources. Most are consumed after success; failures retain the annotation for retry. Read `status.lastOperation`, conditions, and Events after every request.

## `GarageCluster` annotations

| Annotation | Value | Effect / risk |
| --- | --- | --- |
| `trigger-snapshot` | `true` | Snapshot metadata on all nodes; keeps the two newest snapshots |
| `trigger-repair` | `Tables`, `Blocks`, `Versions`, `MultipartUploads`, `BlockRefs`, `BlockRc`, `Rebalance`, `Aliases` | Start the named Garage repair |
| `scrub-command` | `start`, `pause`, `resume`, `cancel` | Control the block scrub worker; not `trigger-repair: Scrub` |
| `revert-layout` | `true` | Discard staged layout changes; does not undo an applied version |
| `retry-block-resync` | `true` or comma-separated hashes | Clear resync backoff for all or selected blocks |
| `purge-blocks` | comma-separated hashes | **Irreversible:** delete objects referencing selected blocks |
| `force-layout-apply` | `true` | Narrow initial/bootstrap override below factor; not a tombstone approval |
| `connect-nodes` | `nodeID@address:port,...` | One-shot external node bootstrap |
| `skip-dead-nodes` | `true` | Mark unresponsive nodes synced to unblock a draining layout |
| `allow-missing-data` | `true` | With `skip-dead-nodes`, permits missing-data recovery with data-loss risk |
| `retry-migration` | `true` | Retry legacy StatefulSet → per-`GarageNode` migration |
| `purge-cluster-layout` | `factor=N[,force]` | **Destructive:** coordinated replication-factor migration |
| `purge-cluster-layout-abort` | `true` | Abort factor migration; cannot undo an on-disk purge |
| `migrate-legacy-rpc-secret` | `true` | Stage exact migration from released RPC env override |
| `acknowledge-legacy-config-migration` | `true` | Attest equivalent rendered config after removing old file override |
| `drain` | `true` | Prepare explicit federated cluster/site drain |
| `recover-storage-rollout` | new nonce | Retry the exact persisted workload handoff after a workload-only failure |

## `GarageNode` annotations

| Annotation | Value | Effect |
| --- | --- | --- |
| `drain` | `true` | Prepare exact identity removal; wait for `DrainPrepared=True` before DELETE |
| `acknowledge-lost-source` | exact 64-hex Garage ID | Pair with `drain` only when the source/data is permanently lost |
| `cycle` | `true` | Add-before-remove replacement for eligible StatefulSet-backed storage |
| `maintenance.suspended` | use spec instead | The old pause annotation is not supported; use `spec.maintenance.suspended` |

## `GarageBucket` annotations

| Annotation | Value | Effect |
| --- | --- | --- |
| `cleanup-mpu` | `true` | Delete old incomplete multipart uploads |
| `cleanup-mpu-older-than` | duration such as `48h` | Threshold used with `cleanup-mpu`; invalid values default to `24h` |

## Important conditions

| Condition | Healthy meaning |
| --- | --- |
| `Ready` | Requested resource shape is reconciled |
| `ClusterHealthy` | Garage health checks are healthy |
| `LayoutApplied` / `LayoutStaged` | Layout state is applied or has pending staged changes |
| `NodesConnected` | Expected node processes are connected |
| `FederationReady` | Federation control-plane setup has converged |
| `GatewayConnected` | Edge/remote gateway connection is established; may report partial state |
| `GatewayLayoutDegraded` | False means every managed gateway has its capacity-less role |
| `GatewayTombstones` | False means no stale gateway roles await removal |
| `QuorumAtRisk` | False means all Garage partitions have write quorum |
| `PeerUnreachable` | False means no peer has sustained unreachability |
| `RemoteClustersHealthy` | True means remote sites are not stale |
| `FederationConfigured` | True means routable RPC advertisement exists |
| `ManagementHandleReady` | External Admin API is reachable for a handle |
| `StorageTopologyReady` | Auto storage membership and layout history are settled |
| `StorageScaleDownBlocked` | False means a requested scale-down preserves factor |
| `NodeLocalPoolsReady` | Desired pool membership is activated and retired safely |
| `StorageRolloutReady` | Identity-bearing workload templates are converged |
| `StorageDrainReady` | No active drain, or exact terminal evidence is complete |

For `GarageBucket`, inspect `BucketCreated`, `QuotaConfigured`, `WebsiteConfigured`, `LifecycleConfigured`, and `AliasesConfigured`. For `GarageKey`, inspect `KeyCreated`, `SecretCreated`, `PermissionsConfigured`, and `KeyExpired`. For `GarageNode`, inspect `NodeDiscovered`, `InLayout`, `NodeConnected`, `Draining`, `DrainPrepared`, and `Cycling`.

## Condition query

```bash
kubectl get garagecluster garage -n storage -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}'
```
