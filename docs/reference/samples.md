# Samples and generated schemas

The repository keeps runnable examples under `config/samples/`. They are reviewed with the CRD schemas and are the best starting point for fields not covered in the conceptual guides.

## Main samples

| Sample | Covers |
| --- | --- |
| [`garage_v1beta2_garagecluster.yaml`](https://github.com/rajsinghtech/garage-operator/blob/main/config/samples/garage_v1beta2_garagecluster.yaml) | Persistent storage, gateways, federation, endpoint exposure |
| [`garage_v1beta2_garagecluster_gateway.yaml`](https://github.com/rajsinghtech/garage-operator/blob/main/config/samples/garage_v1beta2_garagecluster_gateway.yaml) | Unified, local edge, and remote edge patterns |
| [`garage_v1beta2_garagecluster_node_local_pools.yaml`](https://github.com/rajsinghtech/garage-operator/blob/main/config/samples/garage_v1beta2_garagecluster_node_local_pools.yaml) | Mixed PVC/SMB and node-local HostPath storage |
| [`garage_v1beta2_garagecluster_management_handle.yaml`](https://github.com/rajsinghtech/garage-operator/blob/main/config/samples/garage_v1beta2_garagecluster_management_handle.yaml) | Existing Garage management handle |
| [`garage_v1beta2_garagecluster_zone_from.yaml`](https://github.com/rajsinghtech/garage-operator/blob/main/config/samples/garage_v1beta2_garagecluster_zone_from.yaml) | Node-label-derived failure domains |
| [`garage_v1beta1_garagenode.yaml`](https://github.com/rajsinghtech/garage-operator/blob/main/config/samples/garage_v1beta1_garagenode.yaml) | Manual, gateway, external, and per-node endpoint resources |
| [`config/samples/cosi/`](https://github.com/rajsinghtech/garage-operator/tree/main/config/samples/cosi) | COSI BucketClass, BucketAccessClass, BucketClaim, and BucketAccess |

## Validate examples locally

```bash
make schemas
make validate-manifests
```

`make validate-manifests` requires `kubeconform`. It validates the sample API versions against `schemas/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json` using Kubernetes `1.25.0` as the baseline. Node-local pool runtime prerequisites are tested separately; schema validation alone does not prove scheduling-gate support or HostPath safety.

## Generated schemas

The schemas are generated from the Go API types and CRDs. Do not hand-edit them. Refresh them with `make manifests` or `make schemas`, then run the relevant tests and inspect the diff.

The [schema README](https://github.com/rajsinghtech/garage-operator/blob/main/schemas/README.md) documents editor integration. The generated CRD files under [`config/crd/bases/`](https://github.com/rajsinghtech/garage-operator/tree/main/config/crd/bases) are what Kubernetes applies.

## Chart examples

The Helm chart's [README](https://github.com/rajsinghtech/garage-operator/blob/main/charts/garage-operator/README.md) is retained as a short package-level reference. This site is the canonical cross-topic guide; use the chart README when you are inspecting the chart directory or packaging it for a registry.
