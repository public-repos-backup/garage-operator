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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

type staticCredentialDeleteRecordingClient struct {
	client.Client
	preconditions []*metav1.Preconditions
}

func (c *staticCredentialDeleteRecordingClient) Delete(
	ctx context.Context,
	obj client.Object,
	opts ...client.DeleteOption,
) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	c.preconditions = append(c.preconditions, options.Preconditions)
	return c.Client.Delete(ctx, obj, opts...)
}

func TestCleanupUnusedStaticCredentialSnapshotsUsesAuthoritativePinAndPodEnvReferences(t *testing.T) {
	ctx := context.Background()
	scheme := managedPVCTestScheme(t)
	controller := true
	clusterUID := types.UID("cluster-uid")
	owner := metav1.OwnerReference{
		APIVersion: garagev1beta2.GroupVersion.String(),
		Kind:       "GarageCluster",
		Name:       "store",
		UID:        clusterUID,
		Controller: &controller,
	}
	labels := map[string]string{
		labelCluster:                   "store",
		labelStaticCredentialsSnapshot: annotationTrue,
	}
	newSnapshot := "store-credentials-new"
	staleSnapshot := "store-credentials-stale"
	mountedSnapshot := "store-credentials-mounted"
	unusedSnapshot := "store-credentials-unused"
	authoritative := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "store", Namespace: "default", UID: clusterUID,
		Annotations: map[string]string{annotationStaticCredentialsSecret: newSnapshot},
	}}
	objects := make([]client.Object, 0, 6)
	objects = append(objects, authoritative)
	for _, name := range []string{newSnapshot, staleSnapshot, mountedSnapshot, unusedSnapshot} {
		objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"),
			Labels: labels, OwnerReferences: []metav1.OwnerReference{owner},
		}})
	}
	objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "store-0", Namespace: "default", Labels: map[string]string{labelCluster: "store"},
	}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: defaultAppName,
		Env: []corev1.EnvVar{{
			Name: envGarageAdminToken,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: mountedSnapshot},
			}},
		}},
	}}}})

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	recorder := &staticCredentialDeleteRecordingClient{Client: base}
	r := &GarageClusterReconciler{Client: recorder, APIReader: base}
	stale := authoritative.DeepCopy()
	stale.Annotations[annotationStaticCredentialsSecret] = staleSnapshot

	if err := r.cleanupUnusedStaticCredentialSnapshots(ctx, stale); err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{newSnapshot, staleSnapshot, mountedSnapshot} {
		if err := base.Get(ctx, types.NamespacedName{Name: retained, Namespace: "default"}, &corev1.Secret{}); err != nil {
			t.Errorf("retained snapshot %q was deleted: %v", retained, err)
		}
	}
	if err := base.Get(ctx, types.NamespacedName{Name: unusedSnapshot, Namespace: "default"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unused snapshot still exists or lookup failed unexpectedly: %v", err)
	}
	if len(recorder.preconditions) != 1 || recorder.preconditions[0] == nil ||
		recorder.preconditions[0].UID == nil || *recorder.preconditions[0].UID != types.UID(unusedSnapshot+"-uid") {
		t.Fatalf("delete preconditions = %#v, want exact unused Secret UID", recorder.preconditions)
	}
}
