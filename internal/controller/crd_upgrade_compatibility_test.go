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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

var _ = Describe("CRD upgrade compatibility", func() {
	const cleanupFinalizer = "example.test/cleanup"

	It("does not let new schema rules strand v1beta2 objects with released GARAGE_CONFIG_FILE env", func() {
		name := "legacy-v2-env-schema"
		oneGi := resource.MustParse("1Gi")
		cluster := &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: garagev1beta2.GarageClusterSpec{
				Storage: &garagev1beta2.StorageSpec{
					Replicas: 1,
					Metadata: &garagev1beta2.VolumeConfig{Size: &oneGi},
					Data:     &garagev1beta2.VolumeConfig{Size: &oneGi},
					PodTemplate: garagev1beta2.PodTemplate{
						Env: []corev1.EnvVar{{Name: envGarageConfigFile, Value: "/tmp/released.toml"}},
					},
				},
				Replication: &garagev1beta2.ReplicationConfig{Factor: 1},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			stored := &garagev1beta2.GarageCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, stored); err == nil {
				stored.Finalizers = nil
				_ = k8sClient.Update(ctx, stored)
				_ = k8sClient.Delete(ctx, stored)
			}
		})

		stored := &garagev1beta2.GarageCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, stored)).To(Succeed())
		stored.Finalizers = []string{cleanupFinalizer}
		Expect(k8sClient.Update(ctx, stored)).To(Succeed(), "CRD schema must leave transition-aware update handling to the webhook")
	})

	It("does not let new schema rules strand v1beta2 objects with a released GARAGE_RPC_SECRET override", func() {
		name := "legacy-v2-rpc-env-schema"
		oneGi := resource.MustParse("1Gi")
		cluster := &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: garagev1beta2.GarageClusterSpec{
				Storage: &garagev1beta2.StorageSpec{
					Replicas: 1,
					Metadata: &garagev1beta2.VolumeConfig{Size: &oneGi},
					Data:     &garagev1beta2.VolumeConfig{Size: &oneGi},
					PodTemplate: garagev1beta2.PodTemplate{Env: []corev1.EnvVar{{
						Name: envGarageRPCSecret, Value: legacyMigrationValue,
					}},
					},
				},
				Replication: &garagev1beta2.ReplicationConfig{Factor: 1},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			stored := &garagev1beta2.GarageCluster{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, stored); err == nil {
				stored.Finalizers = nil
				_ = k8sClient.Update(ctx, stored)
				_ = k8sClient.Delete(ctx, stored)
			}
		})

		stored := &garagev1beta2.GarageCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, stored)).To(Succeed())
		stored.Finalizers = []string{cleanupFinalizer}
		Expect(k8sClient.Update(ctx, stored)).To(Succeed(), "CRD schema must not block the controller's fail-closed RPC migration path")
	})
})
