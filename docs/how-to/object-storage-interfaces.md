# Use CSI-S3 and COSI

The operator supports native S3 clients directly through `GarageKey` Secrets. CSI-S3 and COSI are optional integrations for workloads that need a Kubernetes storage/provisioning abstraction.

## CSI-S3

[k8s-csi-s3](https://github.com/yandex-cloud/k8s-csi-s3) mounts an S3 bucket through FUSE. It is useful for filesystem-oriented workloads, but it is not a block-storage replacement: expect higher latency, no true random writes, and no `fsync` semantics.

Create a bucket and key with the data keys expected by the CSI driver:

```yaml
apiVersion: garage.rajsingh.info/v1beta1
kind: GarageKey
metadata:
  name: csi-s3-key
  namespace: storage
spec:
  clusterRef:
    name: garage
  secretTemplate:
    name: csi-s3-secret
    accessKeyIdKey: accessKeyID
    secretAccessKeyKey: secretAccessKey
    additionalData:
      endpoint: http://garage.garage.svc:3900
      region: garage
  bucketPermissions:
    - bucketRef:
        name: csi-s3
      read: true
      write: true
```

Install CSI-S3 separately and configure its StorageClass to use the existing Secret. The CSI-S3 namespace needs a privileged Pod Security Admission exception because its node plugin uses FUSE.

## COSI

COSI is a separate Kubernetes API and controller. The operator is a COSI driver implementation; it does not replace the cluster-wide COSI controller and does not use a per-driver sidecar.

Enable it in Helm:

```bash
helm upgrade --install garage-operator \
  oci://ghcr.io/rajsinghtech/charts/garage-operator \
  --namespace garage-operator-system \
  --set cosi.enabled=true
```

Install the COSI CRDs and cluster-wide controller from the pinned revision documented in the repository README, then create a `BucketClass` and `BucketAccessClass` with `driverName: garage.rajsingh.info`.

```yaml
apiVersion: objectstorage.k8s.io/v1alpha2
kind: BucketClass
metadata:
  name: garage-standard
spec:
  driverName: garage.rajsingh.info
  deletionPolicy: Delete
  parameters:
    clusterRef: garage
    clusterNamespace: storage
---
apiVersion: objectstorage.k8s.io/v1alpha2
kind: BucketAccessClass
metadata:
  name: garage-readwrite
spec:
  driverName: garage.rajsingh.info
  authenticationType: Key
  parameters:
    clusterRef: garage
    clusterNamespace: storage
```

Only S3 and Key authentication are supported. `BucketClaim` creates a Garage bucket and `BucketAccess` creates a key/credential Secret through the operator's shadow resources. `Delete` waits for the bucket to be empty; a non-empty bucket is not silently destroyed.

If `cosi.namespace` differs from the target `GarageCluster` namespace, create a `GarageReferenceGrant` in that target namespace for the COSI shadow `GarageBucket` and `GarageKey` kinds.
