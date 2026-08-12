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
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rajsinghtech/garage-operator/internal/garageconfig"
)

func TestV1Beta1RejectsDuplicateEffectiveListenerPorts(t *testing.T) {
	cluster := &GarageCluster{Spec: GarageClusterSpec{
		S3API: &S3APIConfig{BindAddress: "0.0.0.0:4900"},
		Admin: &AdminConfig{BindAddress: "[::]:4900"},
	}}
	if err := cluster.validateAPIs(); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("duplicate effective listener ports error = %v", err)
	}
	cluster = &GarageCluster{Spec: GarageClusterSpec{Network: NetworkConfig{RPCBindPort: 3900}}}
	if err := cluster.validateAPIs(); err == nil || !strings.Contains(err.Error(), "spec.s3Api") {
		t.Fatalf("default enabled S3 listener collision error = %v", err)
	}
	disabled := false
	cluster = &GarageCluster{Spec: GarageClusterSpec{
		Network: NetworkConfig{RPCBindAddress: "[::]:4901"},
		S3API:   &S3APIConfig{BindAddress: "[::]:4900"},
		WebAPI:  &WebAPIConfig{Enabled: &disabled, BindAddress: "[::]:4903"},
		Admin:   &AdminConfig{BindAddress: "[::]:4903"},
	}}
	if err := cluster.validateAPIs(); err != nil {
		t.Fatalf("explicitly disabled Web listener still participated in collision validation: %v", err)
	}
	cluster.Spec.Replicas = 0
	cluster.Spec.Storage = StorageConfig{}
	cluster.Spec.ConnectTo = &ConnectToConfig{}
	cluster.Spec.Admin.BindAddress = "[::]:4901"
	if err := cluster.validateAPIs(); err != nil {
		t.Fatalf("connection-only management handle validated inactive listeners: %v", err)
	}
}

func TestGarageKeyRejectsImportSnapshotOutputCollision(t *testing.T) {
	key := &GarageKey{
		ObjectMeta: metav1.ObjectMeta{Name: "imported", Namespace: "tenant"},
		Spec: GarageKeySpec{
			ClusterRef:     ClusterReference{Name: "garage"},
			ImportKey:      &ImportKeyConfig{SecretRef: &corev1.SecretReference{Name: "source"}},
			SecretTemplate: &SecretTemplate{Name: garageconfig.GarageKeyImportSnapshotName("imported")},
		},
	}
	if err := ValidateGarageKeyMaterialSpec(key); err == nil || !strings.Contains(err.Error(), "immutable import material snapshot") {
		t.Fatalf("snapshot/output collision error = %v", err)
	}

	validator := &GarageKeyValidator{}
	updated := key.DeepCopy()
	updated.Labels = map[string]string{"repair": "true"}
	warnings, err := validator.ValidateUpdate(context.Background(), key, updated)
	if err != nil {
		t.Fatalf("unchanged legacy collision blocked metadata repair: %v", err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "temporarily tolerated") {
		t.Fatalf("legacy collision warning = %v", warnings)
	}

	updated.Spec.SecretTemplate.Name = "generated-output"
	if _, err := validator.ValidateUpdate(context.Background(), key, updated); err != nil {
		t.Fatalf("safe output Secret rename rejected: %v", err)
	}
}

func TestV1Beta1GarageConfigValidation(t *testing.T) {
	badCompression := "23"
	zero := resource.MustParse("0")
	cluster := &GarageCluster{Spec: GarageClusterSpec{
		Network:  NetworkConfig{RPCPingTimeout: &metav1.Duration{Duration: time.Microsecond}},
		Database: &DatabaseConfig{LMDBMapSize: &zero},
		Blocks:   &BlockConfig{CompressionLevel: &badCompression},
	}}
	if err := cluster.validateGarageConfigValues(); err == nil {
		t.Fatal("invalid Garage config values accepted")
	}

	cluster.Spec.Network.RPCPingTimeout = &metav1.Duration{Duration: time.Millisecond}
	cluster.Spec.Database = nil
	validCompression := "0"
	cluster.Spec.Blocks.CompressionLevel = &validCompression
	cluster.Spec.Storage.MetadataAutoSnapshotInterval = "10m"
	if err := cluster.validateGarageConfigValues(); err != nil {
		t.Fatalf("valid Garage config values rejected: %v", err)
	}
}

func TestGarageNodeRejectsShortMetadataSnapshotInterval(t *testing.T) {
	node := &GarageNode{Spec: GarageNodeSpec{
		Storage: &NodeStorageConfig{
			MetadataAutoSnapshotInterval: "5m",
			Data: &NodeVolumeConfig{
				Size: ptrQuantity(resource.MustParse("1Gi")),
			},
		},
	}}
	if err := node.validateStorage(); err == nil {
		t.Fatal("GarageNode metadata snapshot interval below 600s accepted")
	}
}

func TestV1Beta1RejectsUnsupportedExternalIPPublicEndpoint(t *testing.T) {
	endpoint := &PublicEndpointConfig{
		Type:       publicEndpointTypeExternalIP,
		ExternalIP: &ExternalIPEndpointConfig{Addresses: map[string]string{"garage-0": "192.0.2.10"}},
	}
	if err := validateSupportedPublicEndpoint(endpoint, "spec.publicEndpoint"); err == nil {
		t.Fatal("unsupported ExternalIP public endpoint accepted")
	}
}

func TestV1Beta1StrictFieldsAreGrandfatheredOnlyWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	t.Run("GarageKey", func(t *testing.T) {
		validator := &GarageKeyValidator{}
		old := &GarageKey{
			ObjectMeta: metav1.ObjectMeta{
				Name: "legacy-key", Namespace: "tenant", DeletionTimestamp: &now,
				Finalizers: []string{"example.test/cleanup"},
			},
			Spec: GarageKeySpec{
				ClusterRef: ClusterReference{Name: "garage"},
				SecretTemplate: &SecretTemplate{
					AccessKeyIDKey: "same", SecretAccessKeyKey: "same",
				},
			},
		}
		cleanup := old.DeepCopy()
		cleanup.Finalizers = nil
		if err := (&GarageKeyDefaulter{}).Default(ctx, cleanup); err != nil {
			t.Fatalf("default cleanup: %v", err)
		}
		if _, err := validator.ValidateUpdate(ctx, old, cleanup); err != nil {
			t.Fatalf("unchanged invalid material blocked finalizer removal: %v", err)
		}

		validOld := old.DeepCopy()
		validOld.DeletionTimestamp = nil
		validOld.Finalizers = nil
		validOld.Spec.SecretTemplate.AccessKeyIDKey = "access-key-id"
		validOld.Spec.SecretTemplate.SecretAccessKeyKey = "secret-access-key"
		if err := (&GarageKeyDefaulter{}).Default(ctx, validOld); err != nil {
			t.Fatalf("default valid old object: %v", err)
		}
		introduced := validOld.DeepCopy()
		introduced.Spec.SecretTemplate.SecretAccessKeyKey = introduced.Spec.SecretTemplate.AccessKeyIDKey
		if _, err := validator.ValidateUpdate(ctx, validOld, introduced); err == nil || !strings.Contains(err.Error(), "both use") {
			t.Fatalf("new material collision was accepted: %v", err)
		}
	})

	t.Run("GarageBucket", func(t *testing.T) {
		negative := int64(-1)
		old := &GarageBucket{
			ObjectMeta: metav1.ObjectMeta{
				Name: "legacy-bucket", Namespace: "tenant", DeletionTimestamp: &now,
				Finalizers: []string{"example.test/cleanup"},
			},
			Spec: GarageBucketSpec{
				ClusterRef:  ClusterReference{Name: "garage"},
				GlobalAlias: "legacy-bucket",
				Quotas:      &BucketQuotas{MaxObjects: &negative},
			},
		}
		validator := &GarageBucketValidator{}
		cleanup := old.DeepCopy()
		cleanup.Finalizers = nil
		if _, err := validator.ValidateUpdate(ctx, old, cleanup); err != nil {
			t.Fatalf("unchanged negative quota blocked finalizer removal: %v", err)
		}

		zero := int64(0)
		validOld := old.DeepCopy()
		validOld.DeletionTimestamp = nil
		validOld.Finalizers = nil
		validOld.Spec.Quotas.MaxObjects = &zero
		introduced := validOld.DeepCopy()
		introduced.Spec.Quotas.MaxObjects = &negative
		if _, err := validator.ValidateUpdate(ctx, validOld, introduced); err == nil || !strings.Contains(err.Error(), "must be >= 0") {
			t.Fatalf("new negative quota was accepted: %v", err)
		}
	})

	t.Run("GarageAdminToken", func(t *testing.T) {
		old := &GarageAdminToken{
			ObjectMeta: metav1.ObjectMeta{
				Name: "legacy-token", Namespace: "tenant", DeletionTimestamp: &now,
				Finalizers: []string{"example.test/cleanup"},
			},
			Spec: GarageAdminTokenSpec{
				ClusterRef:     ClusterReference{Name: "garage"},
				SecretTemplate: &AdminTokenSecretTemplate{Name: "Invalid Secret Name"},
			},
		}
		validator := &GarageAdminTokenValidator{}
		cleanup := old.DeepCopy()
		cleanup.Finalizers = nil
		if _, err := validator.ValidateUpdate(ctx, old, cleanup); err != nil {
			t.Fatalf("unchanged invalid Secret name blocked finalizer removal: %v", err)
		}

		validOld := old.DeepCopy()
		validOld.DeletionTimestamp = nil
		validOld.Finalizers = nil
		validOld.Spec.SecretTemplate.Name = "valid-secret"
		introduced := validOld.DeepCopy()
		introduced.Spec.SecretTemplate.Name = "Invalid Secret Name"
		if _, err := validator.ValidateUpdate(ctx, validOld, introduced); err == nil || !strings.Contains(err.Error(), "is invalid") {
			t.Fatalf("new invalid Secret name was accepted: %v", err)
		}
	})

	t.Run("GarageCluster", func(t *testing.T) {
		old := &GarageCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "legacy-cluster", Namespace: "tenant", DeletionTimestamp: &now,
				Finalizers: []string{"example.test/cleanup"},
			},
			Spec: GarageClusterSpec{
				Replicas: 1,
				Storage:  StorageConfig{Data: &VolumeConfig{Type: VolumeTypeEmptyDir}},
				PublicEndpoint: &PublicEndpointConfig{
					Type:       publicEndpointTypeExternalIP,
					ExternalIP: &ExternalIPEndpointConfig{Addresses: map[string]string{"garage-0": "192.0.2.10"}},
				},
			},
		}
		validator := &GarageClusterValidator{}
		cleanup := old.DeepCopy()
		cleanup.Finalizers = nil
		if err := (&GarageClusterDefaulter{}).Default(ctx, cleanup); err != nil {
			t.Fatalf("default cleanup: %v", err)
		}
		if _, err := validator.ValidateUpdate(ctx, old, cleanup); err != nil {
			t.Fatalf("unchanged legacy endpoint blocked finalizer removal: %v", err)
		}

		validOld := old.DeepCopy()
		validOld.DeletionTimestamp = nil
		validOld.Finalizers = nil
		validOld.Spec.PublicEndpoint = nil
		if err := (&GarageClusterDefaulter{}).Default(ctx, validOld); err != nil {
			t.Fatalf("default valid old object: %v", err)
		}
		introduced := validOld.DeepCopy()
		introduced.Spec.PublicEndpoint = old.Spec.PublicEndpoint.DeepCopy()
		if _, err := validator.ValidateUpdate(ctx, validOld, introduced); err == nil || !strings.Contains(err.Error(), "externalIP") {
			t.Fatalf("new unsupported ExternalIP endpoint was accepted: %v", err)
		}
	})
}

func TestV1Beta1RejectsUnsupportedRemoteDefaultCapacity(t *testing.T) {
	capacity := resource.MustParse("100Gi")
	cluster := &GarageCluster{Spec: GarageClusterSpec{RemoteClusters: []RemoteClusterConfig{{
		Name: "remote", Zone: "zone-b", DefaultCapacity: &capacity,
	}}}}
	if err := cluster.validateRemoteClusters(); err == nil {
		t.Fatal("unsupported remote defaultCapacity accepted")
	}
}

func TestV1Beta1RejectsServiceFieldsForWrongType(t *testing.T) {
	service := &ServiceConfig{Type: corev1.ServiceTypeClusterIP, LoadBalancerIP: "192.0.2.10"}
	if err := validateServiceConfig(service); err == nil {
		t.Fatal("LoadBalancer field accepted for ClusterIP Service")
	}
	service = &ServiceConfig{Type: corev1.ServiceTypeNodePort, ExternalTrafficPolicy: "invalid"}
	if err := validateServiceConfig(service); err == nil {
		t.Fatal("invalid externalTrafficPolicy accepted")
	}
}
