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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
	"github.com/rajsinghtech/garage-operator/internal/garage"
)

// DaemonSet-backed GarageNodes (storage workload: DaemonSet) do not own a
// per-node StatefulSet: the pods come from a cluster-owned DaemonSet, and the
// GarageNode only manages the layout role for one Kubernetes node. These tests
// exercise the GarageNodeReconciler directly — the GarageNode CR is created by
// hand, exactly as the cluster controller would stamp it out.
const (
	dsWorker1 = "worker-1"
	dsWorker2 = "worker-2"
)

var _ = Describe("GarageNode DaemonSet-backed mode", func() {
	const (
		clusterName = "ds-backed-cluster"
		nodeName    = "ds-backed-node"
	)

	nodeNN := types.NamespacedName{Name: nodeName, Namespace: testNamespace}

	AfterEach(func() {
		n := &garagev1beta1.GarageNode{}
		if err := k8sClient.Get(ctx, nodeNN, n); err == nil {
			n.Finalizers = nil
			_ = k8sClient.Update(ctx, n)
			_ = k8sClient.Delete(ctx, n)
		}
		c := &garagev1beta2.GarageCluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: testNamespace}, c); err == nil {
			c.Finalizers = nil
			_ = k8sClient.Update(ctx, c)
			_ = k8sClient.Delete(ctx, c)
		}
		_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: testNamespace}})
	})

	It("does not create a StatefulSet for a DaemonSet-backed GarageNode", func() {
		By("creating a minimal cluster for the clusterRef (never reconciled)")
		cluster := &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: testNamespace},
			Spec: garagev1beta2.GarageClusterSpec{
				LayoutPolicy: LayoutPolicyManual,
				Replication:  &garagev1beta2.ReplicationConfig{Factor: 1},
				Storage: &garagev1beta2.StorageSpec{
					Replicas: 1,
					Metadata: &garagev1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse("1Gi"))},
					Data:     &garagev1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse("10Gi"))},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		By("creating a DaemonSet-backed GarageNode pinned to a Kubernetes node")
		capacity := resource.MustParse("100Gi")
		node := &garagev1beta1.GarageNode{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: testNamespace},
			Spec: garagev1beta1.GarageNodeSpec{
				ClusterRef:         garagev1beta1.ClusterReference{Name: clusterName},
				Zone:               testNodeZone,
				Capacity:           &capacity,
				Backing:            garagev1beta1.NodeBackingDaemonSet,
				KubernetesNodeName: dsWorker1,
				// A storage config a StatefulSet-backed node would use — present to
				// prove the DaemonSet backing alone suppresses the StatefulSet.
				Storage: &garagev1beta1.NodeStorageConfig{
					Data: &garagev1beta1.NodeVolumeConfig{Size: ptrQuantity(resource.MustParse("100Gi"))},
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		r := &GarageNodeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		req := reconcile.Request{NamespacedName: nodeNN}

		By("first reconcile adds the finalizer and requeues")
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{Requeue: true}))

		By("second reconcile runs the workload phase; the Garage admin API is unreachable in envtest, which is fine — workload objects are created before that point")
		_, _ = r.Reconcile(ctx, req)

		By("verifying no StatefulSet was created for the node")
		sts := &appsv1.StatefulSet{}
		getErr := k8sClient.Get(ctx, nodeNN, sts)
		Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
			"expected no StatefulSet for a DaemonSet-backed GarageNode, got err=%v", getErr)
	})
})

// node_id discovery for DaemonSet-backed nodes: the pod is not named
// <node>-0 (that's the StatefulSet convention) — it is whichever DaemonSet
// pod is scheduled on spec.kubernetesNodeName. Uses the fake in-memory Garage
// layout server + a fake client holding the pods, mirroring the stale-role
// tests.
var _ = Describe("GarageNode DaemonSet-backed node_id discovery", func() {
	const (
		ns        = "ds-discovery-ns"
		dsCluster = "ds-disc-cluster"
		idWorker1 = "1111111111111111111111111111111111111111111111111111111111111111"
		idWorker2 = "2222222222222222222222222222222222222222222222222222222222222222"
	)

	var (
		bctx   context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		bctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(garagev1beta1.AddToScheme(scheme)).To(Succeed())
		Expect(garagev1beta2.AddToScheme(scheme)).To(Succeed())
	})

	// mkDSPod builds a pod as the cluster-owned storage DaemonSet would
	// produce it: storage-tier labels, scheduled on a specific K8s node, with
	// the node-id annotation the operator uses for first-choice discovery.
	mkDSPod := func(name, k8sNodeName, nodeID string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Labels: map[string]string{
					labelCluster: dsCluster,
					labelTier:    tierStorage,
				},
				Annotations: map[string]string{"garage.rajsingh.info/node-id": nodeID},
			},
			Spec: corev1.PodSpec{
				NodeName:   k8sNodeName,
				Containers: []corev1.Container{{Name: fmGarageContainer, Image: "garage:test"}},
			},
		}
	}

	It("discovers the node_id from the DaemonSet pod on spec.kubernetesNodeName and stages that role only", func() {
		fakeLayout := newFakeGarageLayout()
		srv := fakeLayout.server()
		defer srv.Close()
		gcl := garage.NewClient(srv.URL, "test-token")

		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			mkDSPod("ds-disc-cluster-storage-abcde", dsWorker1, idWorker1),
			mkDSPod("ds-disc-cluster-storage-fghij", dsWorker2, idWorker2),
		).WithStatusSubresource(&garagev1beta1.GarageNode{}).Build()

		capacity := resource.MustParse("100Gi")
		node := &garagev1beta1.GarageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "ds-node-worker-1", Namespace: ns},
			Spec: garagev1beta1.GarageNodeSpec{
				ClusterRef:         garagev1beta1.ClusterReference{Name: dsCluster},
				Zone:               testNodeZone,
				Capacity:           &capacity,
				Backing:            garagev1beta1.NodeBackingDaemonSet,
				KubernetesNodeName: dsWorker1,
			},
		}
		cluster := &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: dsCluster, Namespace: ns},
		}

		r := &GarageNodeReconciler{Client: fc, Scheme: scheme}
		Expect(r.reconcileNode(bctx, node, cluster, gcl)).To(Succeed())

		Expect(node.Status.NodeID).To(Equal(idWorker1), "must pick the pod scheduled on worker-1")
		Expect(fakeLayout.hasRole(idWorker1)).To(BeTrue(), "worker-1's identity must get the layout role")
		Expect(fakeLayout.hasRole(idWorker2)).To(BeFalse(), "the pod on the other K8s node must not be touched")
	})

	It("falls back to admin-API discovery by the DaemonSet pod's IP when the annotation is absent", func() {
		fakeLayout := newFakeGarageLayout()
		fakeLayout.statusNodes = []garage.NodeInfo{
			{ID: idWorker1, Address: ptr.To("10.0.0.11:3901"), IsUp: true},
			{ID: idWorker2, Address: ptr.To("10.0.0.12:3901"), IsUp: true},
		}
		srv := fakeLayout.server()
		defer srv.Close()
		gcl := garage.NewClient(srv.URL, "test-token")

		// Pods carry no node-id annotation — discovery must go through the
		// pod's IP and the cluster status.
		pod1 := mkDSPod("ds-disc-cluster-storage-abcde", dsWorker1, "")
		pod1.Annotations = nil
		pod1.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.11"}
		pod2 := mkDSPod("ds-disc-cluster-storage-fghij", dsWorker2, "")
		pod2.Annotations = nil
		pod2.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.12"}

		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod1, pod2).
			WithStatusSubresource(&garagev1beta1.GarageNode{}).Build()

		capacity := resource.MustParse("100Gi")
		node := &garagev1beta1.GarageNode{
			ObjectMeta: metav1.ObjectMeta{Name: "ds-node-worker-1", Namespace: ns},
			Spec: garagev1beta1.GarageNodeSpec{
				ClusterRef:         garagev1beta1.ClusterReference{Name: dsCluster},
				Zone:               testNodeZone,
				Capacity:           &capacity,
				Backing:            garagev1beta1.NodeBackingDaemonSet,
				KubernetesNodeName: dsWorker1,
			},
		}
		cluster := &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: dsCluster, Namespace: ns},
		}

		r := &GarageNodeReconciler{Client: fc, Scheme: scheme}
		Expect(r.reconcileNode(bctx, node, cluster, gcl)).To(Succeed())

		Expect(node.Status.NodeID).To(Equal(idWorker1), "must resolve worker-1's identity via its pod IP")
		Expect(fakeLayout.hasRole(idWorker1)).To(BeTrue())
		Expect(fakeLayout.hasRole(idWorker2)).To(BeFalse())
	})
})
