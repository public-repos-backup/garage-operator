# Day-2 operations

Use status and conditions as the operator's control plane. A resource can be in a valid Kubernetes state while Garage is still joining nodes, draining a layout, waiting for a repair, or losing quorum.

## Inspect before changing

```bash
kubectl get garagecluster garage -n storage -o yaml
kubectl get garagecluster garage -n storage \
  -o custom-columns=PHASE:.status.phase,READY:.status.readyReplicas,DESIRED:.status.replicas,LAYOUT:.status.layoutVersion,DIAGNOSIS:.status.layoutDiagnosis
kubectl get garagenode -n storage -o wide
kubectl get events -n storage --sort-by=.lastTimestamp | tail -50
```

Before a topology change, confirm:

- all expected nodes are `Connected=True` and `InLayout=True`;
- no prior layout version is `Draining`;
- `QuorumAtRisk` is not active;
- `StorageRolloutReady=True` and `StorageDrainReady=True` where relevant;
- no gateway tombstones or pending factor migration block the operation.

## Scale the Auto storage group

```bash
kubectl scale garagecluster garage -n storage --replicas=5
kubectl get garagecluster garage -n storage -w
```

This changes only `spec.storage.replicas` for the default Auto-managed storage group. It does not change node-local pool membership, Manual `GarageNode`s, or gateway replicas. The operator adds members before removing any and serializes layout changes.

Scale-down is refused when it would leave fewer positive-capacity roles than `replication.factor`. Do not remove that guard with a raw Kubernetes deletion.

## Scale gateways

Edit the tier directly:

```bash
kubectl patch garagecluster garage -n storage --type=merge \
  -p '{"spec":{"gateway":{"replicas":4}}}'
```

For unified Auto gateways, the operator creates or drains one gateway `GarageNode` at a time. For edge gateways, it manages the cluster-level gateway StatefulSet and its capacity-less roles. See [gateway tombstones](maintenance-and-recovery.md#gateway-tombstones).

## Change Garage configuration

Use the typed fields in `GarageCluster.spec` or `GarageNode.spec`: image, database, blocks, worker tuning, logging, endpoint ports, and environment variables. The operator renders the effective TOML and coordinates identity-bearing Pod replacement.

Operator-reserved variables are rejected because they can override mesh identity or the credentials and timeout settings used by safety proofs:

- `GARAGE_CONFIG_FILE`;
- `GARAGE_RPC_SECRET` and `GARAGE_RPC_SECRET_FILE`;
- `GARAGE_ADMIN_TOKEN` and `GARAGE_ADMIN_TOKEN_FILE`;
- `GARAGE_METRICS_TOKEN` and `GARAGE_METRICS_TOKEN_FILE`.

Use typed Secret references instead. Existing releases with reserved overrides require the [migration procedure](upgrades.md#reserved-environment-migrations).

## Change replication factor

This is a destructive, disruptive layout migration. It scales storage to zero, purges each node's on-disk `cluster_layout`, rebuilds at the new factor, and triggers re-replication. It is Auto-only and refused for federated or node-local-pool layouts.

The factor annotation and spec edit must be one API update:

```bash
kubectl patch garagecluster garage -n storage --type=merge -p '{
  "metadata":{"annotations":{"garage.rajsingh.info/purge-cluster-layout":"factor=5"}},
  "spec":{"replication":{"factor":5}}
}'
```

Monitor the durable state machine:

```bash
kubectl get garagecluster garage -n storage \
  -o jsonpath='{.status.factorMigration}{"\n"}'
```

Abort with `garage.rajsingh.info/purge-cluster-layout-abort=true` only when necessary. An abort restores workloads but cannot undo a purge already written to disk.

## Trigger a repair or scrub

```bash
kubectl annotate garagecluster garage -n storage \
  garage.rajsingh.info/trigger-repair=Tables
kubectl annotate garagecluster garage -n storage \
  garage.rajsingh.info/scrub-command=start
kubectl get garagecluster garage -n storage \
  -o jsonpath='{.status.lastOperation}{"\n"}'
```

Supported repair types and recovery annotations are listed in the [operations reference](../reference/operations.md). `trigger-repair: Scrub` is not valid; use `scrub-command`.

## Pause reconciliation

```yaml
spec:
  maintenance:
    suspended: true
```

Maintenance suspension is version-controlled and requeues periodically without changing the managed workloads. The old `pause-reconcile` annotation is not honored.
