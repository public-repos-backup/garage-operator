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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

func TestSensitiveGarageConfigRevisionIsKeyedByPinnedRPCIdentity(t *testing.T) {
	ctx := context.Background()
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testSiteName, Namespace: testGarageValue},
		Spec: garagev1beta2.GarageClusterSpec{Discovery: &garagev1beta2.DiscoveryConfig{
			Consul: &garagev1beta2.ConsulDiscoveryConfig{
				Enabled: ptr.To(true), TokenSecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "consul"}, Key: remoteAdminTokenKey,
				},
			},
		}},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	clientForKey := func(key string) *fake.ClientBuilder {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: managedRPCSecretName(cluster), Namespace: cluster.Namespace},
			Data:       map[string][]byte{RPCSecretKey: []byte(key)},
		})
	}
	body := "[consul_discovery]\ntoken = \"guessable-token\"\n"
	revision, err := garageConfigRevision(ctx, clientForKey(strings.Repeat("ab", 32)).Build(), cluster, body)
	if err != nil {
		t.Fatal(err)
	}
	if revision == garageConfigHash(body) {
		t.Fatal("sensitive public revision exposed the unkeyed SHA-256 of garage.toml")
	}
	otherKeyRevision, err := garageConfigRevision(ctx, clientForKey(strings.Repeat("cd", 32)).Build(), cluster, body)
	if err != nil {
		t.Fatal(err)
	}
	if revision == otherKeyRevision {
		t.Fatal("sensitive revision did not depend on the private pinned RPC identity")
	}
	otherTokenRevision, err := garageConfigRevision(
		ctx, clientForKey(strings.Repeat("ab", 32)).Build(), cluster,
		"[consul_discovery]\ntoken = \"another-token\"\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	if revision == otherTokenRevision {
		t.Fatal("sensitive revision did not authenticate the exact config body")
	}
	annotationRevision, err := garageConfigAnnotationRevision(
		ctx, clientForKey(strings.Repeat("ab", 32)).Build(), cluster, body,
	)
	if err != nil {
		t.Fatal(err)
	}
	if annotationRevision != revision[:16] {
		t.Fatalf("annotation revision %q is not the keyed public revision prefix %q", annotationRevision, revision[:16])
	}
}

func TestNonSensitiveGarageConfigRevisionRemainsContentAddressed(t *testing.T) {
	body := "replication_factor = 3\n"
	revision, err := garageConfigRevision(
		context.Background(), nil,
		&garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{Name: testSiteName, Namespace: testGarageValue}},
		body,
	)
	if err != nil {
		t.Fatal(err)
	}
	if revision != garageConfigHash(body) {
		t.Fatalf("non-sensitive revision = %q, want SHA-256 %q", revision, garageConfigHash(body))
	}
}
