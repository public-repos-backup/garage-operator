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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const (
	legacyMigrationNamespace = "legacy-migration"
	legacyMigrationCluster   = "store"
	legacyMigrationSecret    = "verified-rpc"
	legacyMigrationValue     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func legacyMigrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme,
		garagev1beta1.AddToScheme, garagev1beta2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func legacyMigrationClusterObject() *garagev1beta2.GarageCluster {
	return &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: legacyMigrationCluster, Namespace: legacyMigrationNamespace,
			UID: types.UID("legacy-cluster-uid"),
			Annotations: map[string]string{
				garagev1beta1.AnnotationMigrateLegacyRPCSecret: annotationTrue,
			},
		},
		Spec: garagev1beta2.GarageClusterSpec{
			Storage: &garagev1beta2.StorageSpec{
				Replicas: 0,
				PodTemplate: garagev1beta2.PodTemplate{Env: []corev1.EnvVar{{
					Name: envGarageRPCSecret, Value: legacyMigrationValue,
				}}},
			},
			Network: garagev1beta2.NetworkConfig{RPCSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: legacyMigrationSecret},
				Key:                  RPCSecretKey,
			}},
			Replication: &garagev1beta2.ReplicationConfig{Factor: 1},
		},
	}
}

func legacyMigrationSecretObject(name string, cluster *garagev1beta2.GarageCluster) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: cluster.Namespace, UID: types.UID(name + "-uid"),
		},
		Data: map[string][]byte{RPCSecretKey: []byte(legacyMigrationValue)},
	}
}

func legacyManagedSnapshot(cluster *garagev1beta2.GarageCluster) *corev1.Secret {
	secret := legacyMigrationSecretObject(managedRPCSecretName(cluster), cluster)
	secret.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(
		cluster, garagev1beta2.GroupVersion.WithKind(kindGarageCluster),
	)}
	return secret
}

func TestLegacyRPCEnvironmentMigrationRequiresExactCredentialProof(t *testing.T) {
	ctx := context.Background()
	cluster := legacyMigrationClusterObject()
	source := legacyMigrationSecretObject(legacyMigrationSecret, cluster)
	target := legacyManagedSnapshot(cluster)
	kube := fake.NewClientBuilder().WithScheme(legacyMigrationScheme(t)).
		WithObjects(cluster, source, target).Build()
	reconciler := &GarageClusterReconciler{Client: kube, APIReader: kube}

	state, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, false)
	if err != nil {
		t.Fatalf("matching staged credential was rejected: %v", err)
	}
	if !state.blocked || !strings.Contains(state.message, "verified every exact managed Garage RPC environment") {
		t.Fatalf("retained legacy override was not held for two-step removal: %+v", state)
	}

	bad := source.DeepCopy()
	bad.Data[RPCSecretKey] = []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := kube.Update(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, false); err == nil ||
		!strings.Contains(err.Error(), "split mesh") {
		t.Fatalf("mismatched staged credential was accepted: %v", err)
	}
}

func TestLegacyRPCEnvironmentMigrationUsesOwnerInventoryAndFencesGarageNodes(t *testing.T) {
	ctx := context.Background()
	cluster := legacyMigrationClusterObject()
	cluster.Spec.Storage.Env = nil
	source := legacyMigrationSecretObject(legacyMigrationSecret, cluster)
	target := legacyManagedSnapshot(cluster)
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hidden-old-workload", Namespace: cluster.Namespace, UID: types.UID("hidden-sts-uid"),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
				cluster, garagev1beta2.GroupVersion.WithKind(kindGarageCluster),
			)},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hidden-old-pod", Namespace: cluster.Namespace,
			UID:    types.UID("hidden-pod-uid"),
			Labels: map[string]string{"garage.rajsingh.info/cluster": "wrong-label"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
				statefulSet, appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
			)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: defaultAppName,
			Env:  []corev1.EnvVar{{Name: envGarageRPCSecret, Value: legacyMigrationValue}},
		}}},
	}
	kube := fake.NewClientBuilder().WithScheme(legacyMigrationScheme(t)).
		WithObjects(cluster, source, target, statefulSet, pod).Build()
	reconciler := &GarageClusterReconciler{Client: kube, APIReader: kube}

	state, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, false)
	if err != nil || !state.completeRPCSnapshot {
		t.Fatalf("parent did not prove the label-drifted exact Pod: state=%+v err=%v", state, err)
	}
	if _, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, true); err == nil ||
		!strings.Contains(err.Error(), "not immutable yet") {
		t.Fatalf("GarageNode fence accepted a mutable RPC snapshot: %v", err)
	}
	target.Immutable = ptr.To(true)
	if err := kube.Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, true); err != nil {
		t.Fatalf("immutable exact snapshot did not release the GarageNode fence: %v", err)
	}
}

func TestLegacyConfigEnvironmentRequiresExplicitSemanticAttestation(t *testing.T) {
	ctx := context.Background()
	cluster := legacyMigrationClusterObject()
	cluster.Spec.Storage.Env = nil
	delete(cluster.Annotations, garagev1beta1.AnnotationMigrateLegacyRPCSecret)
	cluster.Spec.Network.RPCSecretRef = nil
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "old-config-workload", Namespace: cluster.Namespace, UID: types.UID("config-sts-uid"),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
				cluster, garagev1beta2.GroupVersion.WithKind(kindGarageCluster),
			)},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "old-config-pod", Namespace: cluster.Namespace, UID: types.UID("config-pod-uid"),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
				statefulSet, appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
			)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: defaultAppName,
			Env:  []corev1.EnvVar{{Name: envGarageConfigFile, Value: "/tmp/released.toml"}},
		}}},
	}
	kube := fake.NewClientBuilder().WithScheme(legacyMigrationScheme(t)).
		WithObjects(cluster, statefulSet, pod).Build()
	reconciler := &GarageClusterReconciler{Client: kube, APIReader: kube}

	if _, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, false); err == nil ||
		!strings.Contains(err.Error(), garagev1beta1.AnnotationAcknowledgeLegacyConfigMigration) {
		t.Fatalf("live released config override was accepted without attestation: %v", err)
	}
	cluster.Annotations[garagev1beta1.AnnotationAcknowledgeLegacyConfigMigration] = annotationTrue
	if _, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, false); err != nil {
		t.Fatalf("explicit config-equivalence attestation was rejected: %v", err)
	}

	storedPod := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(pod), storedPod); err != nil {
		t.Fatal(err)
	}
	storedPod.Spec.Containers[0].Env = nil
	storedPod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "opaque-env"}},
	}}
	if err := kube.Update(ctx, storedPod); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, false); err == nil ||
		!strings.Contains(err.Error(), "acknowledgement cannot prove RPC identity") {
		t.Fatalf("broad envFrom was authorized by a config acknowledgement: %v", err)
	}

	if err := kube.Get(ctx, client.ObjectKeyFromObject(pod), storedPod); err != nil {
		t.Fatal(err)
	}
	storedPod.Spec.Containers[0].EnvFrom = nil
	storedPod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: envGarageAdminToken, Value: "released-admin"}}
	if err := kube.Update(ctx, storedPod); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.evaluateLegacyEnvironmentMigration(ctx, cluster, false); err == nil ||
		!strings.Contains(err.Error(), "cannot authorize credential replacement") {
		t.Fatalf("non-RPC credential replacement was authorized by a config acknowledgement: %v", err)
	}
}

func TestGarageClusterReconcileAdoptsVerifiedSnapshotBeforeWorkloads(t *testing.T) {
	ctx := context.Background()
	cluster := legacyMigrationClusterObject()
	cluster.Finalizers = []string{garageClusterFinalizer}
	cluster.Spec.Storage.Env = nil
	cluster.Spec.Network.RPCSecretRef.Name = managedRPCSecretName(cluster)
	target := legacyManagedSnapshot(cluster)
	scheme := legacyMigrationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&garagev1beta2.GarageCluster{}).
		WithObjects(cluster, target).Build()
	reconciler := &GarageClusterReconciler{Client: kube, APIReader: kube, Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cluster),
	}); err != nil {
		t.Fatalf("verified migration reconcile failed: %v", err)
	}
	storedSecret := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(target), storedSecret); err != nil {
		t.Fatal(err)
	}
	if storedSecret.Immutable == nil || !*storedSecret.Immutable {
		t.Fatal("verified released RPC snapshot was not pinned immutable")
	}
	if storedSecret.Annotations[annotationRPCIdentitySource] == "" {
		t.Fatal("verified released RPC snapshot lacks its source descriptor")
	}
	storedCluster := &garagev1beta2.GarageCluster{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cluster), storedCluster); err != nil {
		t.Fatal(err)
	}
	if _, found := storedCluster.Annotations[garagev1beta1.AnnotationMigrateLegacyRPCSecret]; found {
		t.Fatal("completed legacy RPC migration annotation was not consumed")
	}
	configMaps := &corev1.ConfigMapList{}
	if err := kube.List(ctx, configMaps, client.InNamespace(cluster.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(configMaps.Items) != 0 {
		t.Fatalf("migration completion published ConfigMaps in the snapshot-adoption pass: %+v", configMaps.Items)
	}
}
