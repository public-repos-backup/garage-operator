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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

// unprovenTokenFixture builds a storage cluster whose dynamic operator token is
// intact and marked ready, but was proven against a Pod incarnation set that no
// longer exists — the state every storage process lands in after a deliberate
// whole-cluster restart, and unavoidably after a replication-factor purge.
func unprovenTokenFixture() (*garagev1beta2.GarageCluster, []client.Object, string) {
	const (
		staticToken  = "static-token"
		dynamicID    = "dynamic-id"
		dynamicToken = dynamicID + ".dynamic-secret"
	)
	controller := true
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: tierStorage, Namespace: testOperatorPodSetNamespace, UID: types.UID("cluster-uid"),
			Annotations: map[string]string{annotationOperatorAdminTokenID: dynamicID},
		},
		Spec: garagev1beta2.GarageClusterSpec{
			Storage: &garagev1beta2.StorageSpec{},
			Admin: &garagev1beta2.AdminConfig{AdminTokenSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: testStaticRevisionSecret},
				Key:                  DefaultAdminTokenKey,
			}},
		},
	}
	node := &garagev1beta1.GarageNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: tierStorage + "-0", Namespace: cluster.Namespace, UID: types.UID("node-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: garagev1beta2.GroupVersion.String(), Kind: kindGarageCluster,
				Name: cluster.Name, UID: cluster.UID, Controller: &controller,
			}},
		},
		Spec: garagev1beta1.GarageNodeSpec{
			ClusterRef: garagev1beta1.ClusterReference{Name: cluster.Name}, Zone: testZone,
		},
		Status: garagev1beta1.GarageNodeStatus{NodeID: testTerminalNodeID},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name, Namespace: node.Namespace, UID: types.UID("statefulset-uid"),
			Labels: map[string]string{labelCluster: cluster.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: garagev1beta1.GroupVersion.String(), Kind: kindGarageNode,
				Name: node.Name, UID: node.UID, Controller: &controller,
			}},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name + "-0", Namespace: node.Namespace, UID: types.UID("restarted-pod-uid"),
			Labels: map[string]string{labelCluster: cluster.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(), Kind: kindStatefulSet,
				Name: sts.Name, UID: sts.UID, Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: defaultAppName,
			Env: []corev1.EnvVar{{
				Name: envGarageAdminToken,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: testStaticRevisionSecret},
					Key:                  DefaultAdminTokenKey,
				}},
			}},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	staticSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testStaticRevisionSecret, Namespace: cluster.Namespace},
		Data:       map[string][]byte{DefaultAdminTokenKey: []byte(staticToken)},
	}
	dynamicSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: operatorAdminTokenSecretName(cluster), Namespace: cluster.Namespace,
			Labels: map[string]string{labelOperatorAdminToken: operatorAdminTokenReadyValue},
			Annotations: map[string]string{
				annotationOperatorAdminTokenName:  operatorAdminTokenName(cluster),
				annotationOperatorAdminTokenReady: operatorAdminTokenReadyValue,
				// Proven against the Pods that the restart replaced.
				annotationOperatorAdminTokenPodSet: "pre-restart-pod-set-hash",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: garagev1beta2.GroupVersion.String(), Kind: kindGarageCluster,
				Name: cluster.Name, UID: cluster.UID, Controller: &controller,
			}},
		},
		Immutable: ptr.To(true),
		Data: map[string][]byte{
			operatorAdminTokenIDKey: []byte(dynamicID), DefaultAdminTokenKey: []byte(dynamicToken),
		},
	}
	return cluster, []client.Object{cluster, node, sts, pod, staticSecret, dynamicSecret}, staticToken
}

// A replication-factor purge deletes cluster_layout on every storage node, so
// Garage answers every admin-token read with "Layout not ready" until a layout is
// committed. Re-proving the dynamic token needs that table; committing a layout
// needs an Admin client. If an unproven-but-intact token is fatal to client
// construction, those two facts close a loop and RebuildingLayout waits forever
// — observed as a 40-minute e2e shard timeout with the cluster fully healthy.
//
// The static bootstrap credential is mounted in the very Pods being dialed and is
// what the token's own re-verification authenticates with, so it is the correct
// fallback here, exactly as it is before any dynamic token exists.
func TestAdminTokenFallsBackToStaticWhenDynamicTokenIsUnprovenOnLivePods(t *testing.T) {
	cluster, objects, staticToken := unprovenTokenFixture()
	kubeClient := fake.NewClientBuilder().WithScheme(operatorPodSetTestScheme(t)).WithObjects(objects...).Build()

	if _, ready, err := getReadyOperatorAdminToken(context.Background(), kubeClient, cluster); ready || err == nil {
		t.Fatalf("stale Pod-set proof must not resolve as ready: ready=%t err=%v", ready, err)
	}

	token, err := GetAdminToken(context.Background(), kubeClient, cluster)
	if err != nil {
		t.Fatalf("GetAdminToken must fall back to the mounted static credential, got error: %v", err)
	}
	if token != staticToken {
		t.Fatalf("GetAdminToken = %q, want the static bootstrap credential %q", token, staticToken)
	}
}

// The fallback above must not become a blanket one. A Secret that lost its
// immutability, ownership, shape, or pinned identity means something mutated the
// operator's own credential, which is an integrity failure and has to stay fatal
// — no silent downgrade to the static token.
func TestAdminTokenStillFailsClosedWhenDynamicSecretLosesItsContract(t *testing.T) {
	cluster, objects, _ := unprovenTokenFixture()
	for _, object := range objects {
		secret, ok := object.(*corev1.Secret)
		if !ok || secret.Name != operatorAdminTokenSecretName(cluster) {
			continue
		}
		secret.Immutable = ptr.To(false)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(operatorPodSetTestScheme(t)).WithObjects(objects...).Build()

	_, err := GetAdminToken(context.Background(), kubeClient, cluster)
	if err == nil {
		t.Fatal("a dynamic token Secret that lost its immutable contract must not fall back to the static token")
	}
	if !strings.Contains(err.Error(), "lost its exact immutable ownership/data/name contract") {
		t.Fatalf("want the integrity-contract failure, got: %v", err)
	}
}
