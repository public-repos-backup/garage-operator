# Custom resources

The generated CRDs under `config/crd/bases/` and JSON schemas under `schemas/` are authoritative for validation. This page is an operator-oriented map of the current API surface.

## Resource map

| Kind | API version | Scope | Purpose |
| --- | --- | --- | --- |
| `GarageCluster` | `garage.rajsingh.info/v1beta2` (preferred), `v1beta1` (deprecated) | Namespaced | Garage workload topology, configuration, layout, federation, health, and operations |
| `GarageBucket` | `garage.rajsingh.info/v1beta1` | Namespaced | Bucket, alias, quota, website, lifecycle, and key grants |
| `GarageKey` | `garage.rajsingh.info/v1beta1` | Namespaced | S3 key import/generation and permissions |
| `GarageNode` | `garage.rajsingh.info/v1beta1` | Namespaced | One managed, gateway, external, or node-local Garage identity |
| `GarageAdminToken` | `garage.rajsingh.info/v1beta1` | Namespaced | Static Admin token Secret template |
| `GarageReferenceGrant` | `garage.rajsingh.info/v1beta1` | Namespaced | Allow listed namespaces/kinds to make cross-namespace references |

Short names include `gc`, `gb`, `gk`, `gn`, `gat`, and `grg` as published in the CRDs. Confirm them on the target cluster with `kubectl api-resources`.

## GarageCluster v1beta2

### Topology fields

| Field | Meaning |
| --- | --- |
| `storage` | Default Auto storage group and optional node-local pools |
| `gateway` | Unified or edge gateway tier |
| `connectTo` | Edge gateway target or management-handle target |
| `layoutPolicy` | `Auto` or one-way handoff to `Manual` |
| `deletionPolicy` | `Destroy` for whole-store teardown or `Drain` for federated site retirement |
| `zone`, `zoneFrom` | Static or Kubernetes Node-derived layout zone |
| `remoteClusters` | Additive federation imports |

### Service and Garage configuration fields

`network` configures RPC, the API Service, and the shared RPC Secret. `s3Api`, `k2vApi`, `webApi`, and `admin` configure listeners. `database`, `blocks`, `discovery`, `security`, `logging`, and `workers` map to supported Garage configuration. `publicEndpoint` is for remote RPC reachability; it is not a replacement for the S3 Service.

### Storage fields

`storage.replicas` controls only the default Auto PVC group. `storage.metadata` stores identity/database data; `storage.data` stores blocks; `storage.data.paths` supports multi-HDD striping. `storage.dataPaths` belongs to ordinary `GarageNode` resources. `pvcRetentionPolicy`, `capacityReservePercent`, `podDisruptionBudget`, `env`, and `envFrom` affect the default group. `nodeLocalPools` adds selector-driven HostPath identities and requires the separate guide.

### Status fields worth watching

| Field / condition | Why it matters |
| --- | --- |
| `status.phase`, `Ready` | Overall reconciliation state |
| `status.layoutVersion`, `status.layoutHistory` | Current and recent Garage layout state |
| `status.layoutDiagnosis` | One-line actionable health summary |
| `status.endpoints` | Rendered S3, Admin, RPC, K2V, and website endpoints |
| `status.nodes` | Per-node observations and identity health |
| `status.storageRollout` | Exact identity/workload handoff evidence |
| `status.storageDrain` | Exact actor and block-resync proof for removal |
| `status.factorMigration` | Replication-factor migration state |
| `status.pendingGatewayTombstones` | Capacity-less gateway roles awaiting cleanup |
| `status.gatewayNodesNotInLayout` | Gateway identities that lost local auth-table replication |

## GarageBucket

Core fields are `clusterRef`, `bucketId`, `globalAlias`, `localAliases`, `quotas`, `website`, `lifecycle`, and `keyPermissions`.

- Use `bucketId` to pin an existing Garage bucket and prevent replacement.
- Use `globalAlias` for stable S3 bucket identity; omit it to default from metadata name.
- `localAliases` create per-key aliases.
- `quotas` supports size and object limits.
- `website` supports `indexDocument` and `errorDocument`; advanced website features use S3 APIs.
- `lifecycle` supports Garage's subset of S3 expiration and incomplete-upload rules.

## GarageKey

Core fields are `clusterRef`, `name`, `importKey`, `secretTemplate`, `bucketPermissions`, `allBuckets`, `permissions`, `expiresAt`, and `neverExpires`.

`allBuckets` is intentionally cluster-wide and includes buckets created outside Kubernetes. Per-bucket permissions layer on top. `expiresAt` and `neverExpires` are mutually exclusive; expiration marks the resource but does not rotate or delete credentials automatically.

## GarageNode

Core fields are `clusterRef`, `nodeId`, `zone`, `zoneFrom`, `capacity`, `gateway`, `external`, `backing`, `kubernetesNodeName`, `nodeLocalPoolName`, `storage`, `network`, `publicEndpoint`, `maintenance`, and the inherited Pod template fields.

`nodeId` is authoritative for external nodes and an expected pin for managed nodes. `gateway` and `external` change the workload/identity semantics and should be treated as immutable. `backing: NodeLocalPool` is controller-owned and cannot be used to create an arbitrary pool member.

## GarageAdminToken

This resource creates static bootstrap Secret material. `clusterRef` identifies the cluster and `secretTemplate` controls the generated Secret. `name`, `expiresAt`, and `neverExpires` are retained for API compatibility but do not turn it into a revocable Garage token row.

## GarageReferenceGrant

The grant lives in the destination namespace and lists the source kind/namespace in `spec.from`. `spec.to` narrows the destination kind and optional name. `GarageNode` does not support cross-namespace cluster references.

```yaml
apiVersion: garage.rajsingh.info/v1beta1
kind: GarageReferenceGrant
metadata:
  name: allow-team-a
  namespace: storage
spec:
  from:
    - kind: GarageBucket
      namespace: team-a
    - kind: GarageKey
      namespace: team-a
  to:
    - kind: GarageCluster
      name: garage
    - kind: GarageBucket
```

Deleting a grant makes dependent resources fail closed on reconciliation; it does not itself revoke already-issued Garage permissions.

## Validation and compatibility-only fields

Some fields remain in schemas to support conversion or old manifests but are rejected, warned, or ignored. Examples include `security.tls` (Garage removed `rpc_tls`), `publicEndpoint.externalIP`, `remoteClusters[].defaultCapacity`, arbitrary `volumeClaimTemplateSpec`, and `connectTo.clusterRef.kubeConfigSecretRef`. Read admission warnings and errors as the current contract.

For complete field-level descriptions, use the versioned schemas:

- [`garagecluster_v1beta2.json`](https://github.com/rajsinghtech/garage-operator/blob/main/schemas/garagecluster_v1beta2.json)
- [`garagecluster_v1beta1.json`](https://github.com/rajsinghtech/garage-operator/blob/main/schemas/garagecluster_v1beta1.json)
- [`garagebucket_v1beta1.json`](https://github.com/rajsinghtech/garage-operator/blob/main/schemas/garagebucket_v1beta1.json)
- [`garagekey_v1beta1.json`](https://github.com/rajsinghtech/garage-operator/blob/main/schemas/garagekey_v1beta1.json)
- [`garagenode_v1beta1.json`](https://github.com/rajsinghtech/garage-operator/blob/main/schemas/garagenode_v1beta1.json)
