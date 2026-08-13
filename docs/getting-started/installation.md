# Installation

This page installs the operator and its CRDs. It does not create a Garage cluster; use the [quickstart](quickstart.md) after the operator is ready.

## Requirements

- Kubernetes `1.25+` for ordinary workloads; `1.27+` for [node-local pools](../node-local-pools.md).
- Helm `3.8+`.
- Garage `v2.0.0+`; the default image is the tested Garage `v2.3.0` digest.
- cert-manager for the admission and conversion webhooks. The chart enables webhooks by default.

The operator must be allowed to watch the namespaces in which its `GarageCluster`, `GarageNode`, bucket, key, and token resources live. A cluster-scoped installation is required for `zoneFrom` and node-local pools.

## Install the latest published chart

```bash
helm install garage-operator \
  oci://ghcr.io/rajsinghtech/charts/garage-operator \
  --namespace garage-operator-system \
  --create-namespace
```

Pin a version in production:

```bash
helm install garage-operator \
  oci://ghcr.io/rajsinghtech/charts/garage-operator \
  --version 0.7.4 \
  --namespace garage-operator-system \
  --create-namespace
```

The chart installs CRDs and keeps them on uninstall by default. It enables:

- conversion and validating webhooks;
- leader election;
- an authenticated HTTPS metrics endpoint and metrics Service;
- restrictive operator pod security defaults.

!!! danger "Do not disable webhooks for production storage"
    `webhooks.enabled=false` removes conversion and admission safety boundaries. It is limited to local development or simple v1beta2-only `EmptyDir` experiments. It is not supported for node-local pools, controller-managed persistent claims, PVC-backed rollout/recovery, or prepared storage deletion.

## Verify the installation

```bash
kubectl -n garage-operator-system rollout status \
  deployment/garage-operator-controller-manager --timeout=180s
kubectl get crd \
  garageclusters.garage.rajsingh.info \
  garagebuckets.garage.rajsingh.info \
  garagekeys.garage.rajsingh.info \
  garagenodes.garage.rajsingh.info
```

Check the chart's rendered configuration before upgrading an existing release:

```bash
helm get values garage-operator \
  --namespace garage-operator-system --all
helm status garage-operator --namespace garage-operator-system
```

## Namespace-scoped operation

Set `watchNamespaces` when the operator should reconcile only selected namespaces. The release namespace is always included. CRDs remain cluster-scoped and must still be installed by a cluster administrator.

```bash
helm upgrade --install garage-operator \
  oci://ghcr.io/rajsinghtech/charts/garage-operator \
  --namespace garage-operator-system \
  --create-namespace \
  --set 'watchNamespaces={storage,team-a,team-b}'
```

Use the chart's `cosi.namespace` when enabling COSI in a separate namespace; authorize that namespace with a `GarageReferenceGrant` in each target cluster namespace.

## Private registries and immutable images

Use `imagePullSecrets` for the operator image and `defaultGarageImage` for Garage pods that omit `spec.image`. For supply-chain policy, use `image.digest` for the operator and pin `spec.image` or `defaultGarageImage` to a digest.

Released images and charts have keyless cosign signatures and provenance. See the repository README's [artifact verification section](https://github.com/rajsinghtech/garage-operator#verifying-release-artifacts) for the verification commands.

## Uninstall

```bash
helm uninstall garage-operator --namespace garage-operator-system
```

CRDs and their custom resources are retained by default. Deleting a CRD deletes its custom resources, so inspect and back up them before removing CRDs:

```bash
kubectl get garageclusters,garagenodes,garagebuckets,garagekeys,garageadmintokens,garagereferencegrants -A -o yaml > garage-operator-resources.yaml
```

The operator does not automatically delete Garage data when its Helm release is removed. Follow the resource-specific [deletion and drain procedure](../operations/maintenance-and-recovery.md) before removing storage.
