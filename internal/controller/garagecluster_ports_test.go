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
	"strings"
	"testing"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestCustomBindAddressesDriveManagedPorts(t *testing.T) {
	cluster := &garagev1beta2.GarageCluster{}
	cluster.Spec.Network.RPCBindPort = 3901
	cluster.Spec.Network.RPCBindAddress = "[::]:4901"
	cluster.Spec.S3API = &garagev1beta2.S3APIConfig{BindPort: 3900, BindAddress: "0.0.0.0:4900"}
	cluster.Spec.K2VAPI = &garagev1beta2.K2VAPIConfig{BindPort: 3904, BindAddress: "[::]:4904"}
	cluster.Spec.WebAPI = &garagev1beta2.WebAPIConfig{BindPort: 3902, BindAddress: "0.0.0.0:4902"}
	cluster.Spec.Admin = &garagev1beta2.AdminConfig{BindPort: 3903, BindAddress: "[::]:4903"}

	if got := getRPCPort(cluster); got != 4901 {
		t.Fatalf("getRPCPort() = %d, want 4901", got)
	}
	if got := getS3Port(cluster); got != 4900 {
		t.Fatalf("getS3Port() = %d, want 4900", got)
	}
	if got := getK2VPort(cluster); got != 4904 {
		t.Fatalf("getK2VPort() = %d, want 4904", got)
	}
	if got := getWebPort(cluster); got != 4902 {
		t.Fatalf("getWebPort() = %d, want 4902", got)
	}
	if got := getAdminPort(cluster); got != 4903 {
		t.Fatalf("getAdminPort() = %d, want 4903", got)
	}
	if err := validateManagedListenerPorts(cluster); err != nil {
		t.Fatalf("valid managed listeners rejected: %v", err)
	}

	want := map[string]int32{"s3": 4900, "admin": 4903, "k2v": 4904, "web": 4902}
	for _, port := range apiServicePorts(cluster) {
		if expected, ok := want[port.Name]; ok && port.Port != expected {
			t.Errorf("Service port %s = %d, want %d", port.Name, port.Port, expected)
		}
	}
	for _, port := range buildContainerPorts(cluster) {
		if expected, ok := want[port.Name]; ok && port.ContainerPort != expected {
			t.Errorf("container port %s = %d, want %d", port.Name, port.ContainerPort, expected)
		}
		if port.Name == rpcPortName && port.ContainerPort != 4901 {
			t.Errorf("RPC container port = %d, want 4901", port.ContainerPort)
		}
	}
}

func TestManagedListenersRejectUnreachableAddress(t *testing.T) {
	cluster := &garagev1beta2.GarageCluster{}
	cluster.Spec.Network.RPCBindAddress = "127.0.0.1:4901"
	if err := validateManagedListenerPorts(cluster); err == nil {
		t.Fatal("loopback RPC listener accepted")
	}
}

func TestManagedListenersRejectEveryDuplicateEffectivePort(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*garagev1beta2.GarageCluster)
	}{
		{name: "S3", mutate: func(cluster *garagev1beta2.GarageCluster) { cluster.Spec.S3API.BindAddress = "[::]:4901" }},
		{name: "K2V", mutate: func(cluster *garagev1beta2.GarageCluster) { cluster.Spec.K2VAPI.BindAddress = "0.0.0.0:4901" }},
		{name: "Web", mutate: func(cluster *garagev1beta2.GarageCluster) { cluster.Spec.WebAPI.BindAddress = "[::]:4901" }},
		{name: "Admin", mutate: func(cluster *garagev1beta2.GarageCluster) { cluster.Spec.Admin.BindAddress = "0.0.0.0:4901" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster := &garagev1beta2.GarageCluster{}
			cluster.Spec.Network.RPCBindAddress = "0.0.0.0:4901"
			cluster.Spec.S3API = &garagev1beta2.S3APIConfig{BindAddress: "[::]:4900"}
			cluster.Spec.K2VAPI = &garagev1beta2.K2VAPIConfig{BindAddress: "[::]:4904"}
			cluster.Spec.WebAPI = &garagev1beta2.WebAPIConfig{BindAddress: "[::]:4902"}
			cluster.Spec.Admin = &garagev1beta2.AdminConfig{BindAddress: "[::]:4903"}
			test.mutate(cluster)
			if err := validateManagedListenerPorts(cluster); err == nil || !strings.Contains(err.Error(), "conflicts") {
				t.Fatalf("duplicate %s/RPC effective listener error = %v", test.name, err)
			}
		})
	}
}

func TestManagedListenerEnablementMatchesRenderedConfig(t *testing.T) {
	cluster := &garagev1beta2.GarageCluster{}
	cluster.Spec.Network.RPCBindPort = DefaultS3Port
	if err := validateManagedListenerPorts(cluster); err == nil || !strings.Contains(err.Error(), "spec.s3Api") {
		t.Fatalf("default enabled S3 listener collision error = %v", err)
	}

	disabled := false
	cluster.Spec.Network.RPCBindAddress = "[::]:4901"
	cluster.Spec.S3API = &garagev1beta2.S3APIConfig{BindAddress: "[::]:4900"}
	cluster.Spec.WebAPI = &garagev1beta2.WebAPIConfig{Enabled: &disabled, BindAddress: "[::]:4903"}
	cluster.Spec.Admin = &garagev1beta2.AdminConfig{BindAddress: "[::]:4903"}
	if err := validateManagedListenerPorts(cluster); err != nil {
		t.Fatalf("explicitly disabled Web listener still participated in collision validation: %v", err)
	}

	cluster.Spec.ConnectTo = &garagev1beta2.ConnectToConfig{}
	cluster.Spec.Admin.BindAddress = "[::]:4901"
	if err := validateGarageClusterRuntimeConfig(cluster); err != nil {
		t.Fatalf("connection-only management handle validated inactive listeners: %v", err)
	}
}
