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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

// fmValidatePreparedSTS re-reads the prepared StatefulSet and requires the
// stored purge init container to DeepEqual a freshly built one. That comparison
// only holds if the builder spells out every field the API server defaults on
// write. When it did not, each purge was rejected as "stale or altered" and the
// factor migration stalled in Purging with the storage tier scaled to zero —
// the exact state the migration is documented never to leave behind.
//
// This runs against envtest specifically because the bug lives in API-server
// defaulting; comparing the builder's output to itself in memory cannot see it.
var _ = Describe("Factor migration purge init container", func() {
	It("survives API server defaulting so the prepared StatefulSet still validates", func() {
		name := "purge-init-defaulting"
		replicas := int32(0)
		labels := map[string]string{nsApp: name}
		source := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: appsv1.StatefulSetSpec{
				Replicas:    &replicas,
				ServiceName: name,
				Selector:    &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(1000))},
						Containers: []corev1.Container{{
							Name:  defaultAppName,
							Image: "example.invalid/garage:test",
							VolumeMounts: []corev1.VolumeMount{
								{Name: metadataVolName, MountPath: metadataPath},
							},
						}},
						Volumes: []corev1.Volume{{
							Name:         metadataVolName,
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						}},
					},
				},
			},
		}
		expected := factorMigrationPurgeInitContainer(source, "p-test-1")
		source.Spec.Template.Spec.InitContainers = []corev1.Container{expected}

		Expect(k8sClient.Create(ctx, source)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			})
		})

		stored := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, stored)).To(Succeed())
		Expect(stored.Spec.Template.Spec.InitContainers).To(HaveLen(1))

		// Rebuild from the stored object exactly as the validator does.
		rebuilt := factorMigrationPurgeInitContainer(stored, "p-test-1")
		Expect(equality.Semantic.DeepEqual(stored.Spec.Template.Spec.InitContainers[0], rebuilt)).To(BeTrue(),
			"stored purge init container must DeepEqual a freshly built one after API server defaulting; "+
				"stored=%#v rebuilt=%#v", stored.Spec.Template.Spec.InitContainers[0], rebuilt)
	})
})

// reconcileNodeStatefulSet decides "did this change?" by DeepEqualing the
// desired StatefulSet against the stored one. Any field the operator leaves nil
// but the API server defaults therefore reads as a change forever, and the
// StatefulSet is rewritten on every reconcile. That hot loop is invisible in
// unit tests (nothing defaults) and invisible in a passing e2e run (OnDelete
// means no pod restart) — it only shows up as constant API writes slowing
// everything else down.
var _ = Describe("GarageNode StatefulSet desired-state stability", func() {
	It("keeps the PVC retention policy equal to what the API server stores", func() {
		name := "pvc-retention-defaulting"
		cluster := &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: garagev1beta2.GarageClusterSpec{
				// No pvcRetentionPolicy anywhere — the overwhelmingly common case,
				// and the one that used to thrash.
				Storage: &garagev1beta2.StorageSpec{Replicas: 1},
			},
		}
		node := &garagev1beta1.GarageNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		}
		desired := stsPVCRetentionPolicy(cluster, node)
		Expect(desired).NotTo(BeNil(),
			"a nil desired policy can never equal the API server's defaulted value, so every reconcile rewrites the StatefulSet")

		replicas := int32(0)
		labels := map[string]string{nsApp: name}
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: appsv1.StatefulSetSpec{
				Replicas:                             &replicas,
				ServiceName:                          name,
				Selector:                             &metav1.LabelSelector{MatchLabels: labels},
				PersistentVolumeClaimRetentionPolicy: desired,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: defaultAppName, Image: "example.invalid/garage:test",
					}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sts)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			})
		})

		stored := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, stored)).To(Succeed())
		Expect(equality.Semantic.DeepEqual(
			stored.Spec.PersistentVolumeClaimRetentionPolicy,
			stsPVCRetentionPolicy(cluster, node),
		)).To(BeTrue(),
			"stored=%#v desired=%#v — unequal means reconcileNodeStatefulSet rewrites this StatefulSet every pass",
			stored.Spec.PersistentVolumeClaimRetentionPolicy, stsPVCRetentionPolicy(cluster, node))
	})
})
