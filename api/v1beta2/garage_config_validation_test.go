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

package v1beta2

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestV1Beta2RejectsDuplicateEffectiveListenerPorts(t *testing.T) {
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
	cluster.Spec.ConnectTo = &ConnectToConfig{}
	cluster.Spec.Admin.BindAddress = "[::]:4901"
	if err := cluster.validateAPIs(); err != nil {
		t.Fatalf("connection-only management handle validated inactive listeners: %v", err)
	}
}

func TestV1Beta2GarageConfigValidation(t *testing.T) {
	compression := "0"
	positive := resource.MustParse("1Mi")
	cluster := &GarageCluster{Spec: GarageClusterSpec{
		Network:  NetworkConfig{RPCPingTimeout: &metav1.Duration{Duration: time.Millisecond}},
		Storage:  &StorageSpec{MetadataAutoSnapshotInterval: "600s"},
		Database: &DatabaseConfig{FjallBlockCacheSize: &positive},
		Blocks:   &BlockConfig{Size: &positive, CompressionLevel: &compression},
	}}
	if err := cluster.validateGarageConfigValues(); err != nil {
		t.Fatalf("valid Garage config values rejected: %v", err)
	}

	negative := resource.MustParse("-1Mi")
	cluster.Spec.Blocks.RAMBufferMax = &negative
	if err := cluster.validateGarageConfigValues(); err == nil {
		t.Fatal("negative Garage block quantity accepted")
	}
}

func TestV1Beta2AdminEndpointAndBindValidation(t *testing.T) {
	cluster := &GarageCluster{Spec: GarageClusterSpec{
		ConnectTo: &ConnectToConfig{AdminAPIEndpoint: "garage:3903"},
		Storage:   &StorageSpec{},
		Network:   NetworkConfig{RPCBindAddress: "unix:///run/garage/rpc.sock"},
	}}
	if err := cluster.validateConnectTo(); err == nil {
		t.Fatal("relative Admin API endpoint accepted")
	}
	if err := cluster.validateAPIs(); err == nil {
		t.Fatal("Unix RPC listener accepted for an operator-managed workload")
	}
}

func TestV1Beta2RejectsUnsupportedExternalIPPublicEndpoint(t *testing.T) {
	endpoint := &PublicEndpointConfig{
		Type:         "LoadBalancer",
		LoadBalancer: &LoadBalancerEndpointConfig{},
		ExternalIP:   &ExternalIPEndpointConfig{AddressTemplate: "garage-{{.Index}}.example.test"},
	}
	if err := validateSupportedPublicEndpoint(endpoint, "spec.publicEndpoint"); err == nil {
		t.Fatal("ignored ExternalIP public endpoint config accepted")
	}
}

func TestV1Beta2GarageClusterUpdateGrandfathersUnchangedStrictFields(t *testing.T) {
	ctx := context.Background()
	validator := &GarageClusterValidator{}
	now := metav1.Now()
	old := &GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "legacy-endpoint",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"example.test/cleanup"},
		},
		Spec: GarageClusterSpec{
			Storage: &StorageSpec{
				Replicas: 0,
				Metadata: &VolumeConfig{},
				Data:     &VolumeConfig{Type: VolumeTypeEmptyDir},
			},
			Replication: &ReplicationConfig{Factor: 1},
			PublicEndpoint: &PublicEndpointConfig{
				Type:       "ExternalIP",
				ExternalIP: &ExternalIPEndpointConfig{AddressTemplate: "garage-{{.Index}}.example.test"},
			},
		},
	}
	finalizerRemoval := old.DeepCopy()
	finalizerRemoval.Finalizers = nil
	if err := (&GarageClusterDefaulter{}).Default(ctx, finalizerRemoval); err != nil {
		t.Fatalf("default finalizer update: %v", err)
	}
	if _, err := validator.ValidateUpdate(ctx, old, finalizerRemoval); err != nil {
		t.Fatalf("finalizer removal with unchanged legacy endpoint was rejected: %v", err)
	}

	validOld := old.DeepCopy()
	validOld.Name = "new-endpoint"
	validOld.DeletionTimestamp = nil
	validOld.Finalizers = nil
	validOld.Spec.PublicEndpoint = nil
	if err := (&GarageClusterDefaulter{}).Default(ctx, validOld); err != nil {
		t.Fatalf("default old object: %v", err)
	}
	introduced := validOld.DeepCopy()
	introduced.Spec.PublicEndpoint = old.Spec.PublicEndpoint.DeepCopy()
	if _, err := validator.ValidateUpdate(ctx, validOld, introduced); err == nil || !strings.Contains(err.Error(), "externalIP") {
		t.Fatalf("new unsupported ExternalIP endpoint was accepted: %v", err)
	}
}

func TestV1Beta2ClusterReferencesRejectMalformedSyntax(t *testing.T) {
	for _, ref := range []ClusterReference{
		{Name: "Bad_Cluster"},
		{Name: "storage", Namespace: "Bad_Namespace"},
	} {
		cluster := &GarageCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: GarageClusterSpec{
			ConnectTo: &ConnectToConfig{ClusterRef: &ref},
		}}
		if err := cluster.validateConnectTo(); err == nil {
			t.Fatalf("malformed cluster reference %+v was accepted", ref)
		}
	}
}

func TestV1Beta2MalformedLegacyClusterReferenceIsCleanableButNotChangeable(t *testing.T) {
	ctx := t.Context()
	old := &GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-handle", Namespace: "default", Finalizers: []string{"example.test/cleanup"},
		},
		Spec: GarageClusterSpec{ConnectTo: &ConnectToConfig{ClusterRef: &ClusterReference{Name: "Bad_Cluster"}}},
	}
	cleanup := old.DeepCopy()
	cleanup.Finalizers = nil
	cleanup.Labels = map[string]string{"repair": "true"}
	if err := (&GarageClusterDefaulter{}).Default(ctx, cleanup); err != nil {
		t.Fatalf("default cleanup update: %v", err)
	}
	validator := &GarageClusterValidator{}
	if _, err := validator.ValidateUpdate(ctx, old, cleanup); err != nil {
		t.Fatalf("metadata/finalizer cleanup was rejected: %v", err)
	}
	changed := cleanup.DeepCopy()
	changed.Spec.ConnectTo.ClusterRef.Name = "Other_Bad_Cluster"
	if _, err := validator.ValidateUpdate(ctx, old, changed); err == nil {
		t.Fatal("changed malformed cluster reference was accepted")
	}
}

func TestV1Beta2RejectsUnsupportedRemoteDefaultCapacity(t *testing.T) {
	capacity := resource.MustParse("100Gi")
	cluster := &GarageCluster{Spec: GarageClusterSpec{RemoteClusters: []RemoteClusterConfig{{
		Name: "remote", Zone: "zone-b", DefaultCapacity: &capacity,
	}}}}
	if err := cluster.validateRemoteClusters(); err == nil {
		t.Fatal("unsupported remote defaultCapacity accepted")
	}
}

func TestV1Beta2RejectsServiceFieldsForWrongType(t *testing.T) {
	service := &ServiceConfig{Type: corev1.ServiceTypeClusterIP, LoadBalancerSourceRanges: []string{"198.51.100.0/24"}}
	if err := validateServiceConfig(service); err == nil {
		t.Fatal("LoadBalancer field accepted for ClusterIP Service")
	}
	service = &ServiceConfig{Type: corev1.ServiceTypeClusterIP, ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal}
	if err := validateServiceConfig(service); err == nil {
		t.Fatal("externalTrafficPolicy accepted for ClusterIP Service")
	}
}
