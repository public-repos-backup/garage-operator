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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

var _ = Describe("GarageNode rollout-to-drain handoff", func() {
	const (
		clusterName = "rollout-drain-cluster"
		nodeName    = "rollout-drain-node"
	)
	key := types.NamespacedName{Name: nodeName, Namespace: testNamespace}

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: testNamespace},
		})
		_ = k8sClient.Delete(ctx, &garagev1beta1.GarageNode{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName + cycleSiblingSuffix, Namespace: testNamespace},
		})
		_ = deleteTestGarageConfigResourcesForCluster(ctx, k8sClient, clusterName)
		node := &garagev1beta1.GarageNode{}
		if err := k8sClient.Get(ctx, key, node); err == nil {
			node.Finalizers = nil
			_ = k8sClient.Update(ctx, node)
			_ = k8sClient.Delete(ctx, node)
		}
		cluster := &garagev1beta2.GarageCluster{}
		clusterKey := types.NamespacedName{Name: clusterName, Namespace: testNamespace}
		if err := k8sClient.Get(ctx, clusterKey, cluster); err == nil {
			cluster.Finalizers = nil
			_ = k8sClient.Update(ctx, cluster)
			_ = k8sClient.Delete(ctx, cluster)
		}
		_ = deleteTestManagedNodePVCs(ctx, k8sClient, testNamespace, nodeName)
	})

	DescribeTable("publishes the retiring StatefulSet's current-generation acknowledgment before preparing its drain",
		func(cycling bool) {
			cluster := &garagev1beta2.GarageCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: testNamespace},
				Spec: garagev1beta2.GarageClusterSpec{
					LayoutPolicy: LayoutPolicyAuto,
					Replication:  &garagev1beta2.ReplicationConfig{Factor: 1},
					Storage: &garagev1beta2.StorageSpec{
						Replicas: 4,
						Metadata: &garagev1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse("1Gi"))},
						Data:     &garagev1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse("10Gi"))},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			Expect(publishTestClusterConfig(ctx, k8sClient, cluster)).To(Succeed())

			capacity := resource.MustParse("10Gi")
			node := &garagev1beta1.GarageNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:       nodeName,
					Namespace:  testNamespace,
					Finalizers: []string{garageNodeFinalizer},
				},
				Spec: garagev1beta1.GarageNodeSpec{
					ClusterRef: garagev1beta1.ClusterReference{Name: clusterName},
					Zone:       testNodeZone,
					Capacity:   &capacity,
					Storage: &garagev1beta1.NodeStorageConfig{
						Metadata: &garagev1beta1.NodeVolumeConfig{Size: ptrQuantity(resource.MustParse("1Gi"))},
						Data:     &garagev1beta1.NodeVolumeConfig{Size: ptrQuantity(resource.MustParse("10Gi"))},
					},
				},
			}
			Expect(controllerutil.SetControllerReference(cluster, node, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			reconciler := &GarageNodeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(reconciler.reconcileStatefulSet(ctx, node, cluster)).To(Succeed())
			statefulSet := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key, statefulSet)).To(Succeed())
			oldAcknowledgment := statefulSet.Annotations[annotationStorageRolloutInput]
			Expect(oldAcknowledgment).NotTo(BeEmpty())

			By("changing the parent generation through the same 4-to-3 scale-down that requests retirement")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: testNamespace}, cluster)).To(Succeed())
			cluster.Spec.Storage.Replicas = 3
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: testNamespace}, cluster)).To(Succeed())
			Expect(cluster.Generation).To(BeNumerically(">", 1))
			Expect(publishTestClusterConfig(ctx, k8sClient, cluster)).To(Succeed())

			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:               garagev1beta1.ConditionStorageRolloutReady,
				Status:             metav1.ConditionFalse,
				Reason:             garagev1beta1.ReasonStorageRolloutWaiting,
				Message:            "waiting for current managed workload acknowledgments",
				ObservedGeneration: cluster.Generation,
			})
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}
			node.Annotations[garagev1beta1.AnnotationDrain] = annotationTrue
			if cycling {
				node.Annotations[garagev1beta1.AnnotationCycle] = annotationTrue
			}
			Expect(k8sClient.Update(ctx, node)).To(Succeed())
			Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
			node.Status.ParentDeletionRequestGeneration = cluster.Generation
			if cycling {
				node.Status.CyclePhase = garagev1beta1.CyclePhaseProvisioning
			}
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

			By("reconciling the drain request while rollout prepare is waiting")
			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})

			Expect(k8sClient.Get(ctx, key, statefulSet)).To(Succeed())
			newAcknowledgment := statefulSet.Annotations[annotationStorageRolloutInput]
			Expect(newAcknowledgment).NotTo(BeEmpty())
			Expect(newAcknowledgment).NotTo(Equal(oldAcknowledgment),
				"the retiring workload must acknowledge the new parent generation so rollout and drain cannot deadlock")
			Expect(newAcknowledgment).To(Equal(storageRolloutInputToken(
				cluster,
				node,
				statefulSet.Spec.Template.Annotations[annotationPodSpecHash],
				statefulSet.Spec.Template.Annotations[annotationConfigHash],
			)))

			Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
			condition := meta.FindStatusCondition(node.Status.Conditions, garagev1beta1.ConditionDrainPrepared)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(garagev1beta1.ReasonNodeDrainPreparing))
			Expect(condition.Message).To(ContainSubstring("parent managed pod rollout"))
			Expect(node.DeletionTimestamp.IsZero()).To(BeTrue(), "rollout preparation must not delete the drain actor")

			storedCluster := &garagev1beta2.GarageCluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: testNamespace}, storedCluster)).To(Succeed())
			Expect(storedCluster.Status.StorageDrain).To(BeNil(), "rollout preparation must not start a drain transaction")
			if cycling {
				Expect(node.Status.CyclePhase).To(Equal(garagev1beta1.CyclePhaseProvisioning),
					"rollout preparation must pause rather than advance the cycle state machine")
				sibling := &garagev1beta1.GarageNode{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: nodeName + cycleSiblingSuffix, Namespace: testNamespace,
				}, sibling)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "rollout preparation must not provision a cycle sibling")
			}
		},
		Entry("for an ordinary retiring member", false),
		Entry("without advancing an already-requested cycle", true),
	)
})
