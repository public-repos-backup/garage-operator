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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

// Storage-DaemonSet workload (one Garage storage pod per matching Kubernetes
// node, hostPath-backed identity). The cluster controller owns a DaemonSet
// named <cr>-storage plus one GarageNode per matching K8s Node.
var _ = Describe("GarageCluster storage DaemonSet workload", func() {
	const (
		metaHostPath = "/var/lib/garage/meta"
		dataHostPath = "/var/lib/garage/data"
	)

	var (
		reconciler *GarageClusterReconciler
		cluster    *garagev1beta2.GarageCluster
		clusterNN  types.NamespacedName
	)

	BeforeEach(func() {
		reconciler = &GarageClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ClusterScoped: true}
	})

	makeDSCluster := func(name string) *garagev1beta2.GarageCluster {
		capacity := resource.MustParse("500Gi")
		return &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: garagev1beta2.GarageClusterSpec{
				Zone:        testZone,
				Replication: &garagev1beta2.ReplicationConfig{Factor: 1},
				Storage: &garagev1beta2.StorageSpec{
					Workload: garagev1beta2.WorkloadTypeDaemonSet,
					Capacity: &capacity,
					Metadata: &garagev1beta2.VolumeConfig{Type: garagev1beta2.VolumeTypeHostPath, HostPath: metaHostPath},
					Data:     &garagev1beta2.VolumeConfig{Type: garagev1beta2.VolumeTypeHostPath, HostPath: dataHostPath},
				},
				Discovery: &garagev1beta2.DiscoveryConfig{
					Kubernetes: &garagev1beta2.KubernetesDiscoveryConfig{Enabled: ptr.To(true)},
				},
			},
		}
	}

	teardown := func() {
		fresh := &garagev1beta2.GarageCluster{}
		if err := k8sClient.Get(ctx, clusterNN, fresh); err == nil {
			fresh.Finalizers = nil
			_ = k8sClient.Update(ctx, fresh)
			_ = k8sClient.Delete(ctx, fresh)
		}
		gnList := &garagev1beta1.GarageNodeList{}
		_ = k8sClient.List(ctx, gnList, client.InNamespace(testNamespace), client.MatchingLabels(map[string]string{labelCluster: clusterNN.Name}))
		for i := range gnList.Items {
			n := gnList.Items[i]
			n.Finalizers = nil
			_ = k8sClient.Update(ctx, &n)
			_ = k8sClient.Delete(ctx, &n)
		}
		_ = k8sClient.Delete(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: clusterNN.Name + "-storage", Namespace: testNamespace}})
	}

	AfterEach(func() {
		teardown()
	})

	Context("DaemonSet creation", func() {
		BeforeEach(func() {
			clusterNN = types.NamespacedName{Name: uniqueClusterName("ds-storage"), Namespace: testNamespace}
			cluster = makeDSCluster(clusterNN.Name)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("creates a cluster-owned DaemonSet with hostPath volumes and storage-tier pod labels", func() {
			Expect(reconciler.reconcileStorageDaemonSet(ctx, cluster, "test-config-hash")).To(Succeed())

			ds := &appsv1.DaemonSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterNN.Name + "-storage", Namespace: testNamespace}, ds)).To(Succeed())
			Expect(metav1.IsControlledBy(ds, cluster)).To(BeTrue(), "DaemonSet must be owned by the GarageCluster")

			By("carrying the storage-tier labels the GarageNode discovery selects on")
			Expect(ds.Spec.Template.Labels).To(HaveKeyWithValue(labelCluster, clusterNN.Name))
			Expect(ds.Spec.Template.Labels).To(HaveKeyWithValue(labelTier, tierStorage))

			By("mounting hostPath volumes for metadata and data")
			var meta, data *corev1.HostPathVolumeSource
			for _, v := range ds.Spec.Template.Spec.Volumes {
				switch v.Name {
				case metadataVolName:
					meta = v.HostPath
				case dataVolName:
					data = v.HostPath
				}
			}
			Expect(meta).NotTo(BeNil(), "metadata volume must be hostPath-backed")
			Expect(meta.Path).To(Equal(metaHostPath))
			Expect(data).NotTo(BeNil(), "data volume must be hostPath-backed")
			Expect(data.Path).To(Equal(dataHostPath))

			By("stamping the config hash so config changes roll pods")
			Expect(ds.Spec.Template.Annotations).To(HaveKeyWithValue("garage.rajsingh.info/config-hash", "test-config-hash"))
		})
	})

	Context("GarageNodes derived from Kubernetes nodes", func() {
		var k8sNodeNames []string

		mkNode := func(name string, labels map[string]string) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			k8sNodeNames = append(k8sNodeNames, name)
		}

		listStorageNodes := func() []garagev1beta1.GarageNode {
			gnList := &garagev1beta1.GarageNodeList{}
			Expect(k8sClient.List(ctx, gnList, client.InNamespace(testNamespace), client.MatchingLabels(map[string]string{
				labelCluster:      clusterNN.Name,
				labelTier:         tierStorage,
				labelAppManagedBy: managedByOperatorValue,
			}))).To(Succeed())
			return gnList.Items
		}

		BeforeEach(func() {
			k8sNodeNames = nil
			clusterNN = types.NamespacedName{Name: uniqueClusterName("ds-nodes"), Namespace: testNamespace}
			cluster = makeDSCluster(clusterNN.Name)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		AfterEach(func() {
			for _, n := range k8sNodeNames {
				_ = k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}})
			}
		})

		It("creates one DaemonSet-backed GarageNode per Kubernetes node, zone from topology label with cluster fallback", func() {
			zonedNode := clusterNN.Name + "-worker-a"
			plainNode := clusterNN.Name + "-worker-b"
			mkNode(zonedNode, map[string]string{"topology.kubernetes.io/zone": "zone-a"})
			mkNode(plainNode, nil)

			Expect(reconciler.reconcileDaemonSetStorageNodes(ctx, cluster)).To(Succeed())

			nodes := listStorageNodes()
			Expect(nodes).To(HaveLen(2))

			byK8sNode := map[string]*garagev1beta1.GarageNode{}
			for i := range nodes {
				n := &nodes[i]
				Expect(n.Spec.Backing).To(Equal(garagev1beta1.NodeBackingDaemonSet), "GarageNode %s must be DaemonSet-backed", n.Name)
				Expect(n.Spec.KubernetesNodeName).NotTo(BeEmpty())
				Expect(metav1.IsControlledBy(n, cluster)).To(BeTrue())
				Expect(n.Spec.Capacity).NotTo(BeNil())
				Expect(n.Spec.Capacity.String()).To(Equal("500Gi"), "capacity must come from spec.storage.capacity, uniform per node")
				byK8sNode[n.Spec.KubernetesNodeName] = n
			}
			Expect(byK8sNode).To(HaveKey(zonedNode))
			Expect(byK8sNode).To(HaveKey(plainNode))
			Expect(byK8sNode[zonedNode].Spec.Zone).To(Equal("zone-a"), "zone must come from the node's topology label")
			Expect(byK8sNode[plainNode].Spec.Zone).To(Equal(testZone), "zone must fall back to spec.zone")
		})

		It("deletes the GarageNode when its Kubernetes node is deleted, so the finalizer drains the layout role", func() {
			keep := clusterNN.Name + "-keep"
			doomed := clusterNN.Name + "-doomed"
			mkNode(keep, nil)
			mkNode(doomed, nil)

			Expect(reconciler.reconcileDaemonSetStorageNodes(ctx, cluster)).To(Succeed())
			Expect(listStorageNodes()).To(HaveLen(2))

			Expect(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: doomed}})).To(Succeed())
			Expect(reconciler.reconcileDaemonSetStorageNodes(ctx, cluster)).To(Succeed())

			nodes := listStorageNodes()
			Expect(nodes).To(HaveLen(1), "the GarageNode for the deleted K8s node must be removed")
			Expect(nodes[0].Spec.KubernetesNodeName).To(Equal(keep))
		})

		It("updates existing GarageNodes on capacity/zone drift without recreating them", func() {
			worker := clusterNN.Name + "-worker"
			mkNode(worker, nil)

			Expect(reconciler.reconcileDaemonSetStorageNodes(ctx, cluster)).To(Succeed())
			nodes := listStorageNodes()
			Expect(nodes).To(HaveLen(1))
			originalUID := nodes[0].UID

			By("raising the uniform capacity and labelling the node with a zone")
			newCap := resource.MustParse("1Ti")
			cluster.Spec.Storage.Capacity = &newCap
			k8sNode := &corev1.Node{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: worker}, k8sNode)).To(Succeed())
			k8sNode.Labels = map[string]string{"topology.kubernetes.io/zone": "zone-b"}
			Expect(k8sClient.Update(ctx, k8sNode)).To(Succeed())

			Expect(reconciler.reconcileDaemonSetStorageNodes(ctx, cluster)).To(Succeed())

			nodes = listStorageNodes()
			Expect(nodes).To(HaveLen(1))
			Expect(nodes[0].UID).To(Equal(originalUID), "drift must update in place, not recreate (recreate would drain and resync the layout role)")
			Expect(nodes[0].Spec.Capacity.String()).To(Equal("1Ti"))
			Expect(nodes[0].Spec.Zone).To(Equal("zone-b"))
		})

		It("only creates GarageNodes for Kubernetes nodes matching the storage nodeSelector", func() {
			const nodeRoleLabel = "role"
			matching := clusterNN.Name + "-storage-node"
			other := clusterNN.Name + "-app-node"
			mkNode(matching, map[string]string{nodeRoleLabel: "garage-storage"})
			mkNode(other, map[string]string{nodeRoleLabel: "apps"})
			cluster.Spec.Storage.NodeSelector = map[string]string{nodeRoleLabel: "garage-storage"}

			Expect(reconciler.reconcileDaemonSetStorageNodes(ctx, cluster)).To(Succeed())

			nodes := listStorageNodes()
			Expect(nodes).To(HaveLen(1))
			Expect(nodes[0].Spec.KubernetesNodeName).To(Equal(matching))
		})

		It("cleans up a stray ordinal-named GarageNode not in the DaemonSet's desired set", func() {
			// spec.storage.workload is immutable (webhook-enforced), so an
			// ordinal (StatefulSet-shaped) GarageNode can't arise from a live
			// workload switch. This plants one directly to exercise
			// reconcileDaemonSetStorageNodes' desired-set diff defensively:
			// any GarageNode it doesn't recognize as DaemonSet-backed gets
			// deleted, regardless of how it got there (e.g. a stray object
			// predating this immutability rule).
			worker := clusterNN.Name + "-worker"
			mkNode(worker, nil)

			By("planting a stray ordinal-named GarageNode")
			staleCap := resource.MustParse("100Gi")
			stale := &garagev1beta1.GarageNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterNN.Name + "-storage-0",
					Namespace: testNamespace,
					Labels: map[string]string{
						labelCluster:      clusterNN.Name,
						labelTier:         tierStorage,
						labelAppManagedBy: managedByOperatorValue,
					},
				},
				Spec: garagev1beta1.GarageNodeSpec{
					ClusterRef: garagev1beta1.ClusterReference{Name: clusterNN.Name},
					Zone:       testZone,
					Capacity:   &staleCap,
				},
			}
			Expect(k8sClient.Create(ctx, stale)).To(Succeed())

			Expect(reconciler.reconcileDaemonSetStorageNodes(ctx, cluster)).To(Succeed())

			nodes := listStorageNodes()
			Expect(nodes).To(HaveLen(1), "the stray ordinal GarageNode must be deleted, leaving only the DaemonSet-backed one")
			Expect(nodes[0].Spec.Backing).To(Equal(garagev1beta1.NodeBackingDaemonSet))
			Expect(nodes[0].Spec.KubernetesNodeName).To(Equal(worker))
		})
	})

	Context("workload dispatch through Reconcile", func() {
		var k8sNodeName string

		reconcileTwice := func() {
			// First pass adds the finalizer, second pass reconciles resources.
			for i := 0; i < 2; i++ {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: clusterNN})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		BeforeEach(func() {
			clusterNN = types.NamespacedName{Name: uniqueClusterName("ds-dispatch"), Namespace: testNamespace}
			cluster = makeDSCluster(clusterNN.Name)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			k8sNodeName = clusterNN.Name + "-worker"
			Expect(k8sClient.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: k8sNodeName}})).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: k8sNodeName}})
		})

		It("drives the DaemonSet path and skips auto-mode ordinal GarageNodes", func() {
			reconcileTwice()

			By("creating the storage DaemonSet")
			ds := &appsv1.DaemonSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterNN.Name + "-storage", Namespace: testNamespace}, ds)).To(Succeed())

			By("creating only DaemonSet-backed GarageNodes")
			gnList := &garagev1beta1.GarageNodeList{}
			Expect(k8sClient.List(ctx, gnList, client.InNamespace(testNamespace), client.MatchingLabels(map[string]string{
				labelCluster: clusterNN.Name,
				labelTier:    tierStorage,
			}))).To(Succeed())
			Expect(gnList.Items).To(HaveLen(1))
			Expect(gnList.Items[0].Spec.Backing).To(Equal(garagev1beta1.NodeBackingDaemonSet))
			Expect(gnList.Items[0].Spec.KubernetesNodeName).To(Equal(k8sNodeName))
		})

		It("tears down a stray storage DaemonSet if the workload field is ever changed to StatefulSet", func() {
			// spec.storage.workload is immutable — the v1beta2 validating
			// webhook (api/v1beta2/garagecluster_webhook.go) rejects this
			// transition and is the real safeguard in production. This
			// envtest suite doesn't install webhooks, so the Update below
			// bypasses it; the point of this test is the reconciler's
			// defense-in-depth cleanup (e.g. the webhook being briefly
			// unavailable during an operator upgrade, or an object edited
			// directly against etcd), not a supported user-facing switch.
			reconcileTwice()
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterNN.Name + "-storage", Namespace: testNamespace}, &appsv1.DaemonSet{})).To(Succeed())

			By("bypassing the (unregistered-in-this-suite) webhook to force spec.storage.workload to StatefulSet")
			fresh := &garagev1beta2.GarageCluster{}
			Expect(k8sClient.Get(ctx, clusterNN, fresh)).To(Succeed())
			fresh.Spec.Storage.Workload = garagev1beta2.WorkloadTypeStatefulSet
			fresh.Spec.Storage.Replicas = 1
			fresh.Spec.Storage.Capacity = nil
			fresh.Spec.Storage.Metadata = &garagev1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse("1Gi"))}
			fresh.Spec.Storage.Data = &garagev1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse("10Gi"))}
			Expect(k8sClient.Update(ctx, fresh)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: clusterNN})
			Expect(err).NotTo(HaveOccurred())

			Expect(errors.IsNotFound(
				k8sClient.Get(ctx, types.NamespacedName{Name: clusterNN.Name + "-storage", Namespace: testNamespace}, &appsv1.DaemonSet{}),
			)).To(BeTrue(), "the stray storage DaemonSet must be removed")
		})
	})

	Context("status aggregation", func() {
		BeforeEach(func() {
			clusterNN = types.NamespacedName{Name: uniqueClusterName("ds-status"), Namespace: testNamespace}
			cluster = makeDSCluster(clusterNN.Name)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("reports desired storage replicas from the DaemonSet's scheduled count, not spec.storage.replicas", func() {
			Expect(reconciler.reconcileStorageDaemonSet(ctx, cluster, "hash")).To(Succeed())

			By("simulating the DaemonSet controller scheduling 2 pods with 1 ready")
			ds := &appsv1.DaemonSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterNN.Name + "-storage", Namespace: testNamespace}, ds)).To(Succeed())
			ds.Status.DesiredNumberScheduled = 2
			ds.Status.CurrentNumberScheduled = 2
			ds.Status.NumberReady = 1
			Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

			_, err := reconciler.updateStatusFromCluster(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())

			fresh := &garagev1beta2.GarageCluster{}
			Expect(k8sClient.Get(ctx, clusterNN, fresh)).To(Succeed())
			Expect(fresh.Status.StorageReplicas).To(Equal(int32(2)), "desired must track DaemonSet.Status.DesiredNumberScheduled (spec.storage.replicas is ignored under DaemonSet)")
		})

		It("does not report kubelet-Ready DaemonSet pods as ready storage replicas until their GarageNodes actually connect", func() {
			// Regression: a pod can be kubelet-Ready (container started, probes
			// passing) while its Garage process has never joined the cluster —
			// e.g. peer discovery is broken. StorageReadyReplicas/Phase must
			// reflect Garage-level connectivity (GarageNode.Status.Connected),
			// never fall back to the DaemonSet's own pod-readiness count, or a
			// fully disconnected cluster silently reports Phase: Running.
			Expect(reconciler.reconcileStorageDaemonSet(ctx, cluster, "hash")).To(Succeed())

			By("simulating the DaemonSet controller scheduling 2 kubelet-Ready pods, neither connected to Garage")
			ds := &appsv1.DaemonSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterNN.Name + "-storage", Namespace: testNamespace}, ds)).To(Succeed())
			ds.Status.DesiredNumberScheduled = 2
			ds.Status.CurrentNumberScheduled = 2
			ds.Status.NumberReady = 2
			Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

			_, err := reconciler.updateStatusFromCluster(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())

			fresh := &garagev1beta2.GarageCluster{}
			Expect(k8sClient.Get(ctx, clusterNN, fresh)).To(Succeed())
			Expect(fresh.Status.StorageReadyReplicas).To(Equal(int32(0)), "no GarageNode has Status.Connected=true, so ready must be 0 regardless of kubelet pod readiness")
			Expect(fresh.Status.Phase).To(Equal("Pending"), "cluster must not report Phase: Running while zero GarageNodes are connected")
		})
	})

	Context("cluster finalization", func() {
		BeforeEach(func() {
			clusterNN = types.NamespacedName{Name: uniqueClusterName("ds-finalize"), Namespace: testNamespace}
			cluster = makeDSCluster(clusterNN.Name)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		})

		It("deletes the storage DaemonSet", func() {
			Expect(reconciler.reconcileStorageDaemonSet(ctx, cluster, "hash")).To(Succeed())

			Expect(reconciler.finalize(ctx, cluster)).To(Succeed())

			Expect(errors.IsNotFound(
				k8sClient.Get(ctx, types.NamespacedName{Name: clusterNN.Name + "-storage", Namespace: testNamespace}, &appsv1.DaemonSet{}),
			)).To(BeTrue(), "finalize must tear down the storage DaemonSet")
		})
	})
})
