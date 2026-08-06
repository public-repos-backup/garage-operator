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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

// spec.zoneFrom (#294): derive a node's layout zone from the Kubernetes Node its
// pod landed on, so one cluster can express internal failure domains.
var _ = Describe("GarageNode zoneFrom resolution", func() {
	const (
		ns           = "zf-ns"
		clusterName  = "zf"
		nodeName     = "zf-storage-0"
		podName      = "zf-storage-0-0"
		k8sNodeName  = "worker-3"
		rackLabel    = "example.com/rack"
		specZone     = "fallback-zone"
		rackB        = "rack-b"
		clusterUID   = types.UID("test-cluster-uid")
		daemonSetUID = types.UID("test-daemonset-uid")
	)

	var (
		bctx   context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		bctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(garagev1beta1.AddToScheme(scheme)).To(Succeed())
		Expect(garagev1beta2.AddToScheme(scheme)).To(Succeed())
	})

	cluster := func() *garagev1beta2.GarageCluster {
		return &garagev1beta2.GarageCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns, UID: clusterUID},
		}
	}

	garageNode := func(zoneFrom *garagev1beta1.ZoneSource) *garagev1beta1.GarageNode {
		cap := resource.MustParse("1Gi")
		return &garagev1beta1.GarageNode{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: ns},
			Spec: garagev1beta1.GarageNodeSpec{
				ClusterRef: garagev1beta1.ClusterReference{Name: clusterName},
				Zone:       specZone,
				ZoneFrom:   zoneFrom,
				Capacity:   &cap,
			},
		}
	}

	pod := func(scheduledTo string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns},
			Spec:       corev1.PodSpec{NodeName: scheduledTo},
		}
	}

	k8sNode := func(labels map[string]string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: k8sNodeName, Labels: labels},
		}
	}

	resolve := func(objs ...client.Object) string {
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &GarageNodeReconciler{Client: fc, Scheme: scheme}
		return r.effectiveNodeZone(bctx, objs[0].(*garagev1beta1.GarageNode), cluster())
	}

	It("uses the Kubernetes Node's label value when it is set", func() {
		zone := resolve(
			garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel}),
			pod(k8sNodeName),
			k8sNode(map[string]string{rackLabel: rackB}),
		)
		Expect(zone).To(Equal(rackB))
	})

	It("supports the standard topology label", func() {
		zone := resolve(
			garageNode(&garagev1beta1.ZoneSource{NodeLabel: corev1.LabelTopologyZone}),
			pod(k8sNodeName),
			k8sNode(map[string]string{corev1.LabelTopologyZone: "us-east-1c"}),
		)
		Expect(zone).To(Equal("us-east-1c"))
	})

	It("resolves through the shared DaemonSet pod on kubernetesNodeName", func() {
		gn := garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel})
		gn.Spec.Backing = garagev1beta1.NodeBackingNodeLocalPool
		gn.Spec.KubernetesNodeName = k8sNodeName
		gn.Spec.NodeLocalPoolName = testTagLocal
		dsPod := pod(k8sNodeName)
		dsPod.Name = "zf-storage-daemonset-abcde"
		dsPod.Labels = map[string]string{
			labelCluster:       clusterName,
			labelTier:          tierStorage,
			labelNodeLocalPool: testTagLocal,
		}
		dsPod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1",
			Kind:       daemonSetKind,
			Name:       clusterName + "-storage-local",
			UID:        daemonSetUID,
			Controller: ptr.To(true),
		}}
		daemonSet := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName + "-storage-local",
				Namespace: ns,
				UID:       daemonSetUID,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: garagev1beta2.GroupVersion.String(),
					Kind:       testGarageClusterKind,
					Name:       clusterName,
					UID:        clusterUID,
					Controller: ptr.To(true),
				}},
			},
		}

		zone := resolve(
			gn,
			daemonSet,
			dsPod,
			k8sNode(map[string]string{rackLabel: rackB}),
		)
		Expect(zone).To(Equal(rackB))
	})

	It("returns spec.zone unchanged when zoneFrom is unset", func() {
		zone := resolve(
			garageNode(nil),
			pod(k8sNodeName),
			k8sNode(map[string]string{rackLabel: rackB}),
		)
		Expect(zone).To(Equal(specZone), "an unset zoneFrom must not consult Node labels at all")
	})

	// Each of the next four cases is a reason the label cannot be read. All of
	// them must degrade to spec.zone: a GarageNode without a zone cannot be
	// written to the Garage layout at all.
	It("falls back when the pod is not scheduled yet", func() {
		zone := resolve(
			garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel}),
			pod(""),
			k8sNode(map[string]string{rackLabel: rackB}),
		)
		Expect(zone).To(Equal(specZone))
	})

	It("falls back when the pod does not exist yet", func() {
		zone := resolve(
			garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel}),
			k8sNode(map[string]string{rackLabel: rackB}),
		)
		Expect(zone).To(Equal(specZone))
	})

	It("retains the last observed zone across a transient pod replacement gap", func() {
		gn := garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel})
		gn.Status.Zone = rackB
		zone := resolve(
			gn,
			k8sNode(map[string]string{rackLabel: "rack-c"}),
		)
		Expect(zone).To(Equal(rackB),
			"a missing replacement pod must not flip the committed role to spec.zone and back")
	})

	It("falls back when the Kubernetes Node does not carry the label", func() {
		gn := garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel})
		gn.Status.Zone = rackB
		zone := resolve(
			gn,
			pod(k8sNodeName),
			k8sNode(map[string]string{"other/label": rackB}),
		)
		Expect(zone).To(Equal(specZone), "a readable Node with the label removed uses the explicit fallback")
	})

	It("falls back when Nodes are not readable (namespace-scoped install)", func() {
		// The low-level resolver keeps an already-running process on its safe
		// fallback if a Node read transiently fails. Normal reconciliation rejects
		// new namespace-scoped zoneFrom use explicitly below.
		gn := garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel})
		fc := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(gn, pod(k8sNodeName), k8sNode(map[string]string{rackLabel: rackB})).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isNode := obj.(*corev1.Node); isNode {
						return apiForbidden("nodes")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
		r := &GarageNodeReconciler{Client: fc, Scheme: scheme}
		Expect(r.effectiveNodeZone(bctx, gn, cluster())).To(Equal(specZone))
	})

	It("reports an actionable failure for Manual or SMB nodes in a namespace-scoped install", func() {
		gn := garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel})
		gn.Generation = 3
		parent := cluster()
		parent.Spec.LayoutPolicy = LayoutPolicyManual
		fc := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&garagev1beta1.GarageNode{}).
			WithObjects(gn, parent).
			Build()
		r := &GarageNodeReconciler{Client: fc, Scheme: scheme, ClusterScoped: false}
		_, err := r.Reconcile(bctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gn)})
		Expect(err).NotTo(HaveOccurred())

		fresh := &garagev1beta1.GarageNode{}
		Expect(fc.Get(bctx, client.ObjectKeyFromObject(gn), fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(PhaseFailed))
		condition := meta.FindStatusCondition(fresh.Status.Conditions, PhaseReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Message).To(ContainSubstring("cluster-scoped operator install"))
	})

	It("falls back on external nodes, which have no pod", func() {
		gn := garageNode(&garagev1beta1.ZoneSource{NodeLabel: rackLabel})
		gn.Spec.External = &garagev1beta1.ExternalNodeConfig{Address: "10.0.0.1", Port: 3901}
		zone := resolve(gn, k8sNode(map[string]string{rackLabel: rackB}))
		Expect(zone).To(Equal(specZone))
	})
})

// apiForbidden builds the 403 a namespace-scoped install gets for a
// cluster-scoped resource.
func apiForbidden(resource string) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: resource}, "",
		errors.New("cluster-scoped resource not permitted for a namespace-scoped install"))
}
