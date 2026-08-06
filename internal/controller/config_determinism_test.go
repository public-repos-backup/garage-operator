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
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const (
	testFastDataPath    = "/data/fast"
	testDefaultDataPath = "/data/default-pool"
	testStaleDataPath   = "/data/stale"
	testLegacyDataPath  = "/data/legacy"
	testFastValue       = "fast"
	testDiskTypeLabel   = "disk.example.com/type"
	testDegradedMode    = "degraded"
	testRetentionRetain = "Retain"
)

// TestGenerateGarageConfigDeterministic guards against config-hash thrash: the
// rendered garage.toml must be byte-identical across calls for a fixed spec.
// Before the fix, the [consul_discovery.meta] block iterated a Go map directly,
// so field order varied per call, the config hash changed every reconcile, and
// the per-node StatefulSets rolled in an endless loop. A multi-key Meta map
// makes the old non-determinism overwhelmingly likely to surface across N runs.
func TestGenerateGarageConfigDeterministic(t *testing.T) {
	enabled := true
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "det-cluster", Namespace: "det-ns"},
		Spec: garagev1beta2.GarageClusterSpec{
			Storage: &garagev1beta2.StorageSpec{Replicas: 3},
			Discovery: &garagev1beta2.DiscoveryConfig{
				Consul: &garagev1beta2.ConsulDiscoveryConfig{
					Enabled:  &enabled,
					HTTPAddr: "http://consul.service.consul:8500",
					Meta: map[string]string{
						"zone":    "us-east-1",
						"rack":    "r1",
						"tier":    tierStorage,
						"datacen": "dc1",
						"env":     "prod",
						"role":    "garagesvc",
						"owner":   "platform-team",
					},
				},
			},
		},
	}

	first := generateGarageConfig(cluster, &configContext{})
	for i := 0; i < 100; i++ {
		got := generateGarageConfig(cluster, &configContext{})
		if got != first {
			t.Fatalf("generateGarageConfig is non-deterministic at iteration %d\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

func TestGenerateGarageConfigConsulRoundTripsTOMLTableScopeAndQuoting(t *testing.T) {
	enabled := true
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "consul-roundtrip", Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{
			Storage: &garagev1beta2.StorageSpec{Replicas: 1},
			Discovery: &garagev1beta2.DiscoveryConfig{Consul: &garagev1beta2.ConsulDiscoveryConfig{
				Enabled:     &enabled,
				API:         consulAPICatalog,
				HTTPAddr:    `https://consul.example/v1/"quoted"\path`,
				ServiceName: `garage-"storage"\service`,
				Tags:        []string{`tier="hot"`, `path\tag`},
				Datacenters: []string{`dc-"east"`, `dc\west`},
				Meta: map[string]string{
					`rack"\name`: "row\n1",
					"unicode":    "café",
				},
			}},
		},
	}
	token := `tok"\en`
	rendered := generateGarageConfig(cluster, &configContext{ConsulToken: token})
	var decoded struct {
		Consul struct {
			API         string            `toml:"api"`
			HTTPAddr    string            `toml:"consul_http_addr"`
			ServiceName string            `toml:"service_name"`
			Token       string            `toml:"token"`
			Tags        []string          `toml:"tags"`
			Datacenters []string          `toml:"datacenters"`
			Meta        map[string]string `toml:"meta"`
		} `toml:"consul_discovery"`
	}
	if _, err := toml.Decode(rendered, &decoded); err != nil {
		t.Fatalf("generated garage.toml is not parseable TOML: %v\n%s", err, rendered)
	}
	if decoded.Consul.API != consulAPICatalog || decoded.Consul.HTTPAddr != cluster.Spec.Discovery.Consul.HTTPAddr ||
		decoded.Consul.ServiceName != cluster.Spec.Discovery.Consul.ServiceName || decoded.Consul.Token != token {
		t.Fatalf("Consul scalar values did not round-trip: %+v", decoded.Consul)
	}
	if !reflect.DeepEqual(decoded.Consul.Tags, cluster.Spec.Discovery.Consul.Tags) ||
		!reflect.DeepEqual(decoded.Consul.Datacenters, cluster.Spec.Discovery.Consul.Datacenters) ||
		!reflect.DeepEqual(decoded.Consul.Meta, cluster.Spec.Discovery.Consul.Meta) {
		t.Fatalf("Consul collection values or table scope did not round-trip: %+v", decoded.Consul)
	}
}

func TestNodeLocalPoolDataPathMapOrderIsDeterministic(t *testing.T) {
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "ordered-pool", Namespace: "pool-config-test"},
	}
	fastCapacity := resource.MustParse("500Gi")
	archiveCapacity := resource.MustParse("1Ti")
	pool := &garagev1beta2.NodeLocalPoolSpec{
		Name:     testTagLocal,
		Metadata: &garagev1beta2.HostPathVolumeConfig{HostPath: "/var/lib/garage/meta"},
		DataPaths: []garagev1beta2.NodeLocalPoolDataPath{
			{Path: "/data/z-archive", HostPath: "/mnt/archive", Capacity: &archiveCapacity},
			{Path: "/data/a-fast", HostPath: "/mnt/fast", Capacity: &fastCapacity},
		},
	}
	reversed := pool.DeepCopy()
	reversed.DataPaths[0], reversed.DataPaths[1] = reversed.DataPaths[1], reversed.DataPaths[0]

	firstConfig := generateGarageConfig(cluster, nodeLocalPoolConfigContext(&configContext{}, pool))
	secondConfig := generateGarageConfig(cluster, nodeLocalPoolConfigContext(&configContext{}, reversed))
	if firstConfig != secondConfig {
		t.Fatalf("map-list YAML order changed Garage's ordered data_dir:\n--- first ---\n%s\n--- reversed ---\n%s", firstConfig, secondConfig)
	}
	firstVolumes, firstMounts := buildStorageDaemonSetVolumesAndMounts(cluster, pool, "config-hash")
	secondVolumes, secondMounts := buildStorageDaemonSetVolumesAndMounts(cluster, reversed, "config-hash")
	if !reflect.DeepEqual(firstVolumes, secondVolumes) || !reflect.DeepEqual(firstMounts, secondMounts) {
		t.Fatal("map-list YAML order changed node-local-pool volume or mount identity")
	}
}

func TestGarageDataDirCapacityUsesExactKubernetesBytes(t *testing.T) {
	tests := []struct {
		name     string
		quantity string
		want     string
	}{
		{name: "fractional byte rounds by Kubernetes semantics", quantity: "500m", want: "1"},
		{name: "decimal exponent", quantity: "1e3", want: "1000"},
		{name: "binary SI", quantity: "1Gi", want: "1073741824"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quantity := resource.MustParse(test.quantity)
			if got := garageBytesize(&quantity); got != test.want {
				t.Fatalf("garageBytesize(%q) = %q, want %q", test.quantity, got, test.want)
			}
		})
	}

	milli := resource.MustParse("500m")
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "exact-bytes", Namespace: "capacity-test"},
		Spec: garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{
			Data: &garagev1beta2.VolumeConfig{Paths: []garagev1beta2.DataPath{{
				Path: "/data/default", Capacity: &milli,
			}}},
		}},
	}
	defaultConfig := generateGarageConfig(cluster, &configContext{})
	if !strings.Contains(defaultConfig, `capacity = "1"`) || strings.Contains(defaultConfig, `capacity = "500m"`) {
		t.Fatalf("default multi-disk config did not render exact bytes:\n%s", defaultConfig)
	}
	pool := &garagev1beta2.NodeLocalPoolSpec{DataPaths: []garagev1beta2.NodeLocalPoolDataPath{{
		Path: "/data/pool", HostPath: "/var/lib/garage/pool", Capacity: &milli,
	}}}
	poolConfig := generateGarageConfig(cluster, nodeLocalPoolConfigContext(&configContext{}, pool))
	if !strings.Contains(poolConfig, `capacity = "1"`) || strings.Contains(poolConfig, `capacity = "500m"`) {
		t.Fatalf("node-local-pool multi-disk config did not render exact bytes:\n%s", poolConfig)
	}
}

// TestGenerateGarageConfigGatewayDataDir guards the unified-cluster interaction
// where storage uses multi-disk data_dir entries but a gateway pod mounts only
// /data/data. A gateway-specific ConfigMap must not inherit paths that are not
// present in that pod.
func TestGenerateGarageConfigGatewayDataDir(t *testing.T) {
	diskCapacity := resource.MustParse("10Gi")
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "unified", Namespace: "gateway-config-test"},
		Spec: garagev1beta2.GarageClusterSpec{
			Storage: &garagev1beta2.StorageSpec{
				Data: &garagev1beta2.VolumeConfig{
					Paths: []garagev1beta2.DataPath{{
						Path:     testFastDataPath,
						Capacity: &diskCapacity,
					}},
				},
			},
			Gateway: &garagev1beta2.GatewaySpec{Replicas: 1},
		},
	}

	got := generateGarageConfig(cluster, &configContext{ForceSingleDataDir: true})
	if !strings.Contains(got, `data_dir = "/data/data"`) {
		t.Fatalf("gateway config does not use its mounted data directory:\n%s", got)
	}
	if strings.Contains(got, testFastDataPath) {
		t.Fatalf("gateway config inherited an unmounted storage data path:\n%s", got)
	}
}

func TestNodeLocalPoolConfigContextIsolatesDataPathsAndSharedRPCAddress(t *testing.T) {
	defaultCapacity := resource.MustParse("10Gi")
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed", Namespace: "pool-config-test"},
		Spec: garagev1beta2.GarageClusterSpec{
			Storage: &garagev1beta2.StorageSpec{
				Data: &garagev1beta2.VolumeConfig{Paths: []garagev1beta2.DataPath{{
					Path: testDefaultDataPath, Capacity: &defaultCapacity,
				}}},
			},
			Network: garagev1beta2.NetworkConfig{RPCPublicAddr: testSharedRPCPublicAddr},
		},
	}
	base := &configContext{
		RPCPublicAddr:             "load-balancer.example.net:3901",
		NodeRPCPublicAddr:         "default-node.example.net:3901",
		TierRPCPublicAddrOverride: "tier.example.net:3901",
		NodeDataDirPaths:          []NodeDataDirPath{{Path: testStaleDataPath, Capacity: "1Gi"}},
	}

	t.Run("single path", func(t *testing.T) {
		pool := &garagev1beta2.NodeLocalPoolSpec{
			Data: &garagev1beta2.HostPathVolumeConfig{HostPath: "/mnt/local"},
		}
		got := generateGarageConfig(cluster, nodeLocalPoolConfigContext(base, pool))
		if !strings.Contains(got, `data_dir = "/data/data"`) {
			t.Fatalf("single-disk pool did not use its mounted path:\n%s", got)
		}
		for _, leaked := range []string{testDefaultDataPath, testStaleDataPath, "rpc_public_addr ="} {
			if strings.Contains(got, leaked) {
				t.Fatalf("single-disk pool config leaked %q from the default/shared context:\n%s", leaked, got)
			}
		}
	})

	t.Run("multiple paths", func(t *testing.T) {
		fastCapacity := resource.MustParse("500Gi")
		pool := &garagev1beta2.NodeLocalPoolSpec{
			DataPaths: []garagev1beta2.NodeLocalPoolDataPath{
				{Path: testFastDataPath, HostPath: "/mnt/fast", Capacity: &fastCapacity},
				{Path: testLegacyDataPath, HostPath: "/mnt/legacy", ReadOnly: true},
			},
		}
		got := generateGarageConfig(cluster, nodeLocalPoolConfigContext(base, pool))
		for _, wanted := range []string{
			`{ path = "` + testFastDataPath + `", capacity = "536870912000" }`,
			`{ path = "` + testLegacyDataPath + `", read_only = true }`,
		} {
			if !strings.Contains(got, wanted) {
				t.Fatalf("multi-disk pool config missing %q:\n%s", wanted, got)
			}
		}
		for _, leaked := range []string{testDefaultDataPath, testStaleDataPath, "rpc_public_addr ="} {
			if strings.Contains(got, leaked) {
				t.Fatalf("multi-disk pool config leaked %q from the default/shared context:\n%s", leaked, got)
			}
		}
	})
}
