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
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/version"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	garagev1beta1 "github.com/rajsinghtech/garage-operator/api/v1beta1"
	garagev1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

const supportedKubernetesGitVersion = "v1.29.7"

type serverVersionFunc func() (*version.Info, error)

func (f serverVersionFunc) ServerVersion() (*version.Info, error) { return f() }

type createInterceptClient struct {
	client.Client
	create func(context.Context, client.Object, ...client.CreateOption) error
}

func (c *createInterceptClient) Create(
	ctx context.Context,
	object client.Object,
	opts ...client.CreateOption,
) error {
	return c.create(ctx, object, opts...)
}

type getInterceptReader struct {
	client.Reader
	get func(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
}

func (r *getInterceptReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	return r.get(ctx, key, object, opts...)
}

type staticNodeLocalPoolPrerequisiteChecker struct {
	result NodeLocalPoolPrerequisiteResult
	calls  int
}

func (c *staticNodeLocalPoolPrerequisiteChecker) Check(
	context.Context,
	*garagev1beta2.GarageCluster,
) NodeLocalPoolPrerequisiteResult {
	c.calls++
	return c.result
}

func supportedNodeLocalPoolPrerequisites() NodeLocalPoolPrerequisiteChecker {
	return &staticNodeLocalPoolPrerequisiteChecker{result: NodeLocalPoolPrerequisiteResult{Supported: true}}
}

func prerequisiteTestCluster(namespace, name, uid string) *garagev1beta2.GarageCluster {
	return &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name, UID: types.UID(uid),
	}}
}

func markSchedulerProbeGatedAt(object client.Object, observedAt time.Time) {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return
	}
	pod.CreationTimestamp = metav1.NewTime(observedAt)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonSchedulingGated, LastTransitionTime: metav1.NewTime(observedAt),
	}}
}

func markSchedulerProbeGated(object client.Object) {
	markSchedulerProbeGatedAt(object, time.Now())
}

func TestNodeLocalPoolPrerequisiteChecker(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	subject := prerequisiteTestCluster(membershipTestNamespace, "capability-subject", "capability-subject-uid")
	sameNamespaceSubject := prerequisiteTestCluster(membershipTestNamespace, "same-namespace", "same-namespace-uid")
	otherSubject := prerequisiteTestCluster("another-namespace", "other-subject", "other-subject-uid")

	t.Run("rejects Kubernetes 1.26 before a dry run", func(t *testing.T) {
		createCalls := 0
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			createCalls++
			markSchedulerProbeGated(object)
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, probeClient, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "26", GitVersion: "v1.26.15"}, nil
		}))
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolUnsupportedKubernetesVersion {
			t.Fatalf("unexpected result: %+v", result)
		}
		if createCalls != 0 {
			t.Fatalf("capability dry run executed %d time(s) for an unsupported version", createCalls)
		}
	})

	t.Run("reuses short-lived evidence only within the probed namespace", func(t *testing.T) {
		versionCalls := 0
		createCalls := 0
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, opts ...client.CreateOption,
		) error {
			createCalls++
			var spec corev1.PodSpec
			switch typed := object.(type) {
			case *appsv1.DaemonSet:
				spec = typed.Spec.Template.Spec
				createOptions := &client.CreateOptions{}
				for _, option := range opts {
					option.ApplyToCreate(createOptions)
				}
				if len(createOptions.DryRun) != 1 || createOptions.DryRun[0] != metav1.DryRunAll {
					t.Fatalf("DaemonSet probe was not server-side dry-run: %#v", createOptions.DryRun)
				}
			case *corev1.Pod:
				spec = typed.Spec
				if !nodeLocalPoolSchedulerProbeBelongsToCluster(typed, subject) &&
					!nodeLocalPoolSchedulerProbeBelongsToCluster(typed, sameNamespaceSubject) &&
					!nodeLocalPoolSchedulerProbeBelongsToCluster(typed, otherSubject) {
					t.Fatalf("real scheduler probe is not bound to its exact GarageCluster: %#v", typed)
				}
				markSchedulerProbeGated(typed)
			default:
				t.Fatalf("unexpected capability probe type %T", object)
			}
			if !podSpecHasSchedulingGate(spec, nodeLocalPoolCapabilityGateName) {
				t.Fatalf("probe did not carry the required scheduling gate: %#v", object)
			}
			podSecurity := spec.SecurityContext
			containerSecurity := spec.Containers[0].SecurityContext
			if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
				podSecurity == nil || podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot ||
				podSecurity.SeccompProfile == nil || podSecurity.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
				containerSecurity == nil || containerSecurity.AllowPrivilegeEscalation == nil || *containerSecurity.AllowPrivilegeEscalation ||
				containerSecurity.ReadOnlyRootFilesystem == nil || !*containerSecurity.ReadOnlyRootFilesystem ||
				containerSecurity.Capabilities == nil ||
				len(containerSecurity.Capabilities.Drop) != 1 || containerSecurity.Capabilities.Drop[0] != corev1.Capability("ALL") {
				t.Fatalf("probe is not compatible with restricted Pod Security admission: %#v", spec)
			}
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, probeClient, serverVersionFunc(func() (*version.Info, error) {
			versionCalls++
			return &version.Info{Major: "1", Minor: "29+", GitVersion: "v1.29.7-gke.1104000"}, nil
		}))
		for i := 0; i < 2; i++ {
			if result := checker.Check(context.Background(), subject); !result.Supported {
				t.Fatalf("expected supported result, got %+v", result)
			}
		}
		if versionCalls != 1 || createCalls != 2 {
			t.Fatalf("fresh same-namespace evidence was not reused: version=%d create=%d", versionCalls, createCalls)
		}
		if result := checker.Check(context.Background(), sameNamespaceSubject); !result.Supported {
			t.Fatalf("second cluster in the same namespace did not reuse capability evidence: %+v", result)
		}
		if versionCalls != 1 || createCalls != 2 {
			t.Fatalf("namespace evidence was not shared: version=%d create=%d", versionCalls, createCalls)
		}
		if result := checker.Check(context.Background(), otherSubject); !result.Supported {
			t.Fatalf("second namespace capability probe failed: %+v", result)
		}
		if versionCalls != 2 || createCalls != 4 {
			t.Fatalf("evidence leaked across namespaces: version=%d create=%d", versionCalls, createCalls)
		}
	})

	t.Run("expires success and detects a disabled scheduling-gate capability", func(t *testing.T) {
		now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		preserveGate := true
		createCalls := 0
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			createCalls++
			if daemonSet, ok := object.(*appsv1.DaemonSet); ok {
				if !preserveGate {
					daemonSet.Spec.Template.Spec.SchedulingGates = nil
				}
				return nil
			}
			markSchedulerProbeGatedAt(object, now)
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(
			probeClient,
			probeClient,
			serverVersionFunc(func() (*version.Info, error) {
				return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
			}),
		).(*nodeLocalPoolPrerequisiteChecker)
		checker.now = func() time.Time { return now }
		checker.evidenceTTL = time.Minute
		if result := checker.Check(context.Background(), subject); !result.Supported {
			t.Fatalf("initial capability evidence failed: %+v", result)
		}
		preserveGate = false
		if result := checker.Check(context.Background(), subject); !result.Supported {
			t.Fatalf("unexpired capability evidence was not reused: %+v", result)
		}
		now = now.Add(time.Minute + time.Nanosecond)
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable {
			t.Fatalf("expired evidence concealed disabled scheduling gates: %+v", result)
		}
		if createCalls != 3 {
			t.Fatalf("expired evidence performed %d API/scheduler probe creates, want 3", createCalls)
		}
	})

	t.Run("pins successful evidence for one reconcile while revalidating the next", func(t *testing.T) {
		now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		preserveGate := true
		createCalls := 0
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			createCalls++
			if daemonSet, ok := object.(*appsv1.DaemonSet); ok {
				if !preserveGate {
					daemonSet.Spec.Template.Spec.SchedulingGates = nil
				}
				return nil
			}
			markSchedulerProbeGatedAt(object, now)
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(
			probeClient,
			probeClient,
			serverVersionFunc(func() (*version.Info, error) {
				return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
			}),
		).(*nodeLocalPoolPrerequisiteChecker)
		checker.now = func() time.Time { return now }
		checker.evidenceTTL = time.Minute
		reconcileContext := withNodeLocalPoolPrerequisiteSession(context.Background())
		if result := checker.Check(reconcileContext, subject); !result.Supported {
			t.Fatalf("initial reconcile evidence failed: %+v", result)
		}

		preserveGate = false
		now = now.Add(2 * time.Minute)
		if result := checker.Check(reconcileContext, subject); !result.Supported ||
			!strings.Contains(result.Message, "pinned for this reconciliation") {
			t.Fatalf("long reconcile lost its pinned evidence: %+v", result)
		}
		if createCalls != 2 {
			t.Fatalf("same reconcile unexpectedly reprobed %d object(s)", createCalls)
		}

		result := checker.Check(withNodeLocalPoolPrerequisiteSession(context.Background()), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable {
			t.Fatalf("next reconcile did not revalidate expired evidence: %+v", result)
		}
		if createCalls != 3 {
			t.Fatalf("next reconcile probe creates=%d, want 3", createCalls)
		}
	})

	t.Run("expires success and detects a Kubernetes downgrade", func(t *testing.T) {
		now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		minor := "29"
		versionCalls := 0
		createCalls := 0
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			createCalls++
			markSchedulerProbeGatedAt(object, now)
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(
			probeClient,
			probeClient,
			serverVersionFunc(func() (*version.Info, error) {
				versionCalls++
				return &version.Info{Major: "1", Minor: minor, GitVersion: "v1." + minor}, nil
			}),
		).(*nodeLocalPoolPrerequisiteChecker)
		checker.now = func() time.Time { return now }
		checker.evidenceTTL = time.Minute
		if result := checker.Check(context.Background(), subject); !result.Supported {
			t.Fatalf("initial capability evidence failed: %+v", result)
		}
		minor = "26"
		now = now.Add(time.Minute + time.Nanosecond)
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolUnsupportedKubernetesVersion {
			t.Fatalf("expired evidence concealed a Kubernetes downgrade: %+v", result)
		}
		if versionCalls != 2 || createCalls != 2 {
			t.Fatalf("downgrade revalidation calls version=%d create=%d, want 2/2", versionCalls, createCalls)
		}
	})

	t.Run("fails closed when the API server removes the scheduling gate", func(t *testing.T) {
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			object.(*appsv1.DaemonSet).Spec.Template.Spec.SchedulingGates = nil
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, probeClient, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "27", GitVersion: "v1.27.16"}, nil
		}))
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable {
			t.Fatalf("unexpected result: %+v", result)
		}
	})

	t.Run("fails closed when the API preserves the gate but the scheduler evaluates the Pod", func(t *testing.T) {
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			if pod, ok := object.(*corev1.Pod); ok {
				pod.CreationTimestamp = metav1.Now()
				pod.Status.Conditions = []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonUnschedulable,
				}}
			}
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, probeClient, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
		}))
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable ||
			!strings.Contains(result.Message, corev1.PodReasonUnschedulable) {
			t.Fatalf("scheduler bypass was not rejected: %+v", result)
		}
	})

	t.Run("returns a short pending retry until the scheduler records positive evidence", func(t *testing.T) {
		now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		probeClient := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			if pod, ok := object.(*corev1.Pod); ok {
				pod.CreationTimestamp = metav1.NewTime(now)
			}
			return nil
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, probeClient, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
		})).(*nodeLocalPoolPrerequisiteChecker)
		checker.now = func() time.Time { return now }
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGateProbePending ||
			result.RetryAfter != nodeLocalPoolSchedulerProbeRetry {
			t.Fatalf("scheduler evidence wait was not a short fail-closed retry: %+v", result)
		}
	})

	t.Run("adopts and cleans an exact gated probe after manager restart", func(t *testing.T) {
		persistedProbe := nodeLocalPoolSchedulingGatePodProbe(subject)
		observedAt := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		markSchedulerProbeGatedAt(persistedProbe, observedAt)
		stored := fake.NewClientBuilder().WithScheme(scheme).WithObjects(persistedProbe).Build()
		probeClient := &createInterceptClient{Client: stored, create: func(
			ctx context.Context, object client.Object, opts ...client.CreateOption,
		) error {
			if _, ok := object.(*appsv1.DaemonSet); ok {
				return nil
			}
			return stored.Create(ctx, object, opts...)
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, stored, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
		})).(*nodeLocalPoolPrerequisiteChecker)
		checker.now = func() time.Time { return observedAt.Add(time.Second) }
		if result := checker.Check(context.Background(), subject); !result.Supported {
			t.Fatalf("persisted exact scheduler evidence was not adopted: %+v", result)
		}
		fresh := &corev1.Pod{}
		err := stored.Get(context.Background(), client.ObjectKeyFromObject(persistedProbe), fresh)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("adopted scheduler probe was not cleaned up: %v", err)
		}
	})

	t.Run("anchors cached freshness to scheduler observation across restart", func(t *testing.T) {
		observedAt := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		now := observedAt.Add(29 * time.Second)
		persistedProbe := nodeLocalPoolSchedulingGatePodProbe(subject)
		markSchedulerProbeGatedAt(persistedProbe, observedAt)
		stored := fake.NewClientBuilder().WithScheme(scheme).WithObjects(persistedProbe).Build()
		preserveGate := true
		probeClient := &createInterceptClient{Client: stored, create: func(
			ctx context.Context, object client.Object, opts ...client.CreateOption,
		) error {
			if daemonSet, ok := object.(*appsv1.DaemonSet); ok {
				if !preserveGate {
					daemonSet.Spec.Template.Spec.SchedulingGates = nil
				}
				return nil
			}
			return stored.Create(ctx, object, opts...)
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, stored, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
		})).(*nodeLocalPoolPrerequisiteChecker)
		checker.now = func() time.Time { return now }
		checker.evidenceTTL = 30 * time.Second

		if result := checker.Check(context.Background(), subject); !result.Supported {
			t.Fatalf("fresh persisted scheduler evidence was not adopted: %+v", result)
		}
		cached := checker.evidence[subject.Namespace]
		if !cached.observedAt.Equal(observedAt) || !cached.expiresAt.Equal(observedAt.Add(30*time.Second)) {
			t.Fatalf("cache lifetime was extended from consumption time: %#v", cached)
		}

		preserveGate = false
		now = observedAt.Add(31 * time.Second)
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable {
			t.Fatalf("expired scheduler observation concealed a disabled API capability: %+v", result)
		}
	})

	t.Run("deletes stale persisted evidence and reproves after manager restart", func(t *testing.T) {
		now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
		persistedProbe := nodeLocalPoolSchedulingGatePodProbe(subject)
		markSchedulerProbeGatedAt(persistedProbe, now.Add(-nodeLocalPoolSchedulerProbeTimeout-time.Second))
		stored := fake.NewClientBuilder().WithScheme(scheme).WithObjects(persistedProbe).Build()
		probeClient := &createInterceptClient{Client: stored, create: func(
			ctx context.Context, object client.Object, opts ...client.CreateOption,
		) error {
			if _, ok := object.(*appsv1.DaemonSet); ok {
				return nil
			}
			markSchedulerProbeGatedAt(object, now)
			return stored.Create(ctx, object, opts...)
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, stored, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
		})).(*nodeLocalPoolPrerequisiteChecker)
		checker.now = func() time.Time { return now }

		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGateProbePending ||
			!strings.Contains(result.Message, "stale") {
			t.Fatalf("stale scheduler evidence was accepted: %+v", result)
		}
		if err := stored.Get(context.Background(), client.ObjectKeyFromObject(persistedProbe), &corev1.Pod{}); !apierrors.IsNotFound(err) {
			t.Fatalf("stale probe was not deleted: %v", err)
		}
		if result := checker.Check(context.Background(), subject); !result.Supported {
			t.Fatalf("fresh replacement scheduler evidence failed: %+v", result)
		}
	})

	t.Run("treats create-already-exists read lag as a short pending probe", func(t *testing.T) {
		writer := &createInterceptClient{Client: base, create: func(
			_ context.Context, object client.Object, _ ...client.CreateOption,
		) error {
			if _, ok := object.(*corev1.Pod); ok {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, object.GetName())
			}
			return nil
		}}
		reader := &getInterceptReader{Reader: base, get: func(
			_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption,
		) error {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
		}}
		checker := NewNodeLocalPoolPrerequisiteChecker(writer, reader, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
		}))
		result := checker.Check(context.Background(), subject)
		if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGateProbePending ||
			result.RetryAfter != nodeLocalPoolSchedulerProbeRetry {
			t.Fatalf("API visibility lag was misclassified: %+v", result)
		}
	})

	t.Run("distinguishes an API rejection from an inconclusive probe", func(t *testing.T) {
		invalidClient := &createInterceptClient{Client: base, create: func(
			context.Context, client.Object, ...client.CreateOption,
		) error {
			return apierrors.NewInvalid(
				schema.GroupKind{Group: "apps", Kind: "DaemonSet"}, "probe",
				field.ErrorList{field.NotSupported(field.NewPath("spec", "template", "spec", "schedulingGates"), "gate", []string{})},
			)
		}}
		versionSource := serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "1", Minor: "27", GitVersion: "v1.27.16"}, nil
		})
		result := NewNodeLocalPoolPrerequisiteChecker(invalidClient, invalidClient, versionSource).
			Check(context.Background(), subject)
		if result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable {
			t.Fatalf("unexpected rejected-probe result: %+v", result)
		}

		unknownClient := &createInterceptClient{Client: base, create: func(
			context.Context, client.Object, ...client.CreateOption,
		) error {
			return errors.New("temporary transport failure")
		}}
		result = NewNodeLocalPoolPrerequisiteChecker(unknownClient, unknownClient, versionSource).
			Check(context.Background(), subject)
		if result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGateCapabilityUnknown {
			t.Fatalf("unexpected inconclusive-probe result: %+v", result)
		}
	})

	t.Run("fails closed on discovery errors and malformed versions", func(t *testing.T) {
		checker := NewNodeLocalPoolPrerequisiteChecker(base, base, serverVersionFunc(func() (*version.Info, error) {
			return nil, errors.New("discovery unavailable")
		}))
		if result := checker.Check(context.Background(), subject); result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGateCapabilityUnknown {
			t.Fatalf("unexpected discovery-error result: %+v", result)
		}
		checker = NewNodeLocalPoolPrerequisiteChecker(base, base, serverVersionFunc(func() (*version.Info, error) {
			return &version.Info{Major: "one", Minor: "27", GitVersion: "vendor"}, nil
		}))
		if result := checker.Check(context.Background(), subject); result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGateCapabilityUnknown {
			t.Fatalf("unexpected malformed-version result: %+v", result)
		}
	})
}

func TestNodeLocalPoolPrerequisiteReconcileBoundary(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "unsupported-pool",
			Namespace:  membershipTestNamespace,
			UID:        types.UID("cluster-uid"),
			Generation: 3,
			Finalizers: []string{garageClusterFinalizer},
		},
		Spec: garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{
			NodeLocalPools: []garagev1beta2.NodeLocalPoolSpec{{Name: testTagLocal}},
		}},
	}
	kubernetesNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   testKubernetesWorkerA,
		Labels: map[string]string{membershipTestDiskLabel: testTagLocal},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&garagev1beta2.GarageCluster{}).
		WithObjects(cluster, kubernetesNode).Build()
	checker := &staticNodeLocalPoolPrerequisiteChecker{result: NodeLocalPoolPrerequisiteResult{
		Reason:  garagev1beta1.ReasonNodeLocalPoolUnsupportedKubernetesVersion,
		Message: "Kubernetes v1.26.15 is below the required 1.27 capability boundary",
	}}
	reconciler := &GarageClusterReconciler{
		Client:                     kubeClient,
		APIReader:                  kubeClient,
		ClusterScoped:              true,
		NodeLocalPoolPrerequisites: checker,
	}

	blocked, retryAfter, err := reconciler.blockForNodeLocalPoolPrerequisites(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked || retryAfter != 0 || checker.calls != 1 {
		t.Fatalf("expected one blocking capability decision, blocked=%v retry=%s calls=%d", blocked, retryAfter, checker.calls)
	}
	daemonSets := &appsv1.DaemonSetList{}
	if err := kubeClient.List(context.Background(), daemonSets, client.InNamespace(cluster.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(daemonSets.Items) != 0 {
		t.Fatalf("unsupported cluster created %d DaemonSet(s)", len(daemonSets.Items))
	}
	freshNode := &corev1.Node{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(kubernetesNode), freshNode); err != nil {
		t.Fatal(err)
	}
	if len(freshNode.Annotations) != 0 || len(freshNode.Labels) != 1 {
		t.Fatalf("unsupported cluster mutated Kubernetes Node activation state: %#v", freshNode.ObjectMeta)
	}
	freshCluster := &garagev1beta2.GarageCluster{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(cluster), freshCluster); err != nil {
		t.Fatal(err)
	}
	condition := findNodeLocalPoolsReadyCondition(freshCluster)
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != garagev1beta1.ReasonNodeLocalPoolUnsupportedKubernetesVersion {
		t.Fatalf("prerequisite condition not persisted: %#v", condition)
	}

	plainCluster := &garagev1beta2.GarageCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "plain", Namespace: membershipTestNamespace, UID: types.UID("plain-uid"),
	}}
	if err := kubeClient.Create(context.Background(), plainCluster); err != nil {
		t.Fatal(err)
	}
	blocked, retryAfter, err = reconciler.blockForNodeLocalPoolPrerequisites(context.Background(), plainCluster)
	if err != nil || blocked || retryAfter != 0 || checker.calls != 1 {
		t.Fatalf("plain Kubernetes-1.25-compatible cluster invoked the checker: blocked=%v retry=%s calls=%d err=%v", blocked, retryAfter, checker.calls, err)
	}
}

func TestNodeLocalPoolSchedulerProbeIsOwnedAndCleanedWhenNoLongerRequired(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := prerequisiteTestCluster(
		membershipTestNamespace, "probe-cleanup", "probe-cleanup-cluster-uid",
	)
	probe := nodeLocalPoolSchedulingGatePodProbe(cluster)
	if !metav1.IsControlledBy(probe, cluster) {
		t.Fatalf("scheduler probe lacks a GarageCluster garbage-collection owner: %#v", probe.OwnerReferences)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, probe).Build()
	checker := &staticNodeLocalPoolPrerequisiteChecker{result: NodeLocalPoolPrerequisiteResult{Supported: true}}
	reconciler := &GarageClusterReconciler{
		Client: kubeClient, APIReader: kubeClient, NodeLocalPoolPrerequisites: checker,
	}

	blocked, retryAfter, err := reconciler.blockForNodeLocalPoolPrerequisites(context.Background(), cluster)
	if err != nil || blocked || retryAfter != 0 {
		t.Fatalf("no-feature cleanup unexpectedly blocked: blocked=%v retry=%s err=%v", blocked, retryAfter, err)
	}
	if checker.calls != 0 {
		t.Fatalf("cleanup invoked the scheduling-gate checker %d time(s)", checker.calls)
	}
	err = kubeClient.Get(context.Background(), client.ObjectKeyFromObject(probe), &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("probe left by a manager crash was not deleted after spec removal: %v", err)
	}
}

func TestNodeLocalPoolSchedulerProbeForegroundDeletionProofIsOwnerless(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	active := prerequisiteTestCluster(
		membershipTestNamespace, "foreground-proof", "foreground-proof-cluster-uid",
	)
	ownedProbe := nodeLocalPoolSchedulingGatePodProbe(active)
	if !metav1.IsControlledBy(ownedProbe, active) {
		t.Fatal("pre-deletion scheduler probe is not controller-owned")
	}
	deleting := active.DeepCopy()
	deletionTime := metav1.NewTime(time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &deletionTime
	deletionProbe := nodeLocalPoolSchedulingGatePodProbe(deleting)
	if len(deletionProbe.OwnerReferences) != 0 {
		t.Fatalf("deletion-time scheduler probe can be reaped by foreground GC: %#v", deletionProbe.OwnerReferences)
	}
	if !nodeLocalPoolSchedulerProbeBelongsToCluster(deletionProbe, deleting) ||
		!nodeLocalPoolSchedulerProbeBelongsToCluster(ownedProbe, deleting) {
		t.Fatal("exact owned-to-ownerless deletion transition was not recognized")
	}

	stored := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ownedProbe).Build()
	now := deletionTime.Add(time.Second)
	probeClient := &createInterceptClient{Client: stored, create: func(
		ctx context.Context, object client.Object, opts ...client.CreateOption,
	) error {
		if _, ok := object.(*appsv1.DaemonSet); ok {
			return nil
		}
		pod := object.(*corev1.Pod)
		if len(pod.OwnerReferences) != 0 {
			t.Fatalf("foreground-deletion replacement retained an owner: %#v", pod.OwnerReferences)
		}
		markSchedulerProbeGatedAt(pod, now)
		return stored.Create(ctx, pod, opts...)
	}}
	checker := NewNodeLocalPoolPrerequisiteChecker(probeClient, stored, serverVersionFunc(func() (*version.Info, error) {
		return &version.Info{Major: "1", Minor: "29", GitVersion: supportedKubernetesGitVersion}, nil
	})).(*nodeLocalPoolPrerequisiteChecker)
	checker.now = func() time.Time { return now }

	result := checker.Check(context.Background(), deleting)
	if result.Supported || result.Reason != garagev1beta1.ReasonNodeLocalPoolSchedulingGateProbePending ||
		!strings.Contains(result.Message, "foreground deletion proof") {
		t.Fatalf("owned probe was not fenced out of deletion-time proof: %+v", result)
	}
	if err := stored.Get(context.Background(), client.ObjectKeyFromObject(ownedProbe), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned probe was not removed before deletion-time proof: %v", err)
	}

	if result := checker.Check(context.Background(), deleting); !result.Supported {
		t.Fatalf("ownerless scheduler proof could not complete during foreground deletion: %+v", result)
	}
	if err := stored.Get(context.Background(), client.ObjectKeyFromObject(deletionProbe), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ownerless deletion-time probe was not explicitly cleaned: %v", err)
	}
}

func TestNodeLocalPoolFinalizationCleansOwnerlessProbeAfterNamespaceCacheHit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := deletionTestScheme(t)
	cluster := nodeLocalPoolActivationTestCluster("cached-finalize-proof", "a")
	deletionTime := metav1.NewTime(time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC))
	cluster.DeletionTimestamp = &deletionTime
	pool := &cluster.Spec.Storage.NodeLocalPools[0]
	claim, err := newNodeLocalPoolHostPathClaim(cluster, pool, "")
	if err != nil {
		t.Fatal(err)
	}
	claimValue, err := encodeNodeLocalPoolHostPathClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	kubernetesNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "cached-finalize-worker",
		Annotations: map[string]string{
			nodeLocalPoolHostPathClaimAnnotation(cluster, pool.Name): claimValue,
		},
	}}
	probe := nodeLocalPoolSchedulingGatePodProbe(cluster)
	if len(probe.OwnerReferences) != 0 {
		t.Fatal("deletion-time probe unexpectedly has a garbage-collection owner")
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(kubernetesNode, probe).Build()
	checker := &nodeLocalPoolPrerequisiteChecker{
		client: kubeClient, reader: kubeClient,
		now:         func() time.Time { return deletionTime.Add(time.Second) },
		evidenceTTL: time.Minute,
		evidence: map[string]nodeLocalPoolCapabilityEvidence{
			cluster.Namespace: {
				observedServerVersion: supportedKubernetesGitVersion,
				observedAt:            deletionTime.Time,
				expiresAt:             deletionTime.Add(time.Minute),
			},
		},
	}
	reconciler := &GarageClusterReconciler{
		Client: kubeClient, APIReader: kubeClient, Scheme: scheme, ClusterScoped: true,
		NodeLocalPoolPrerequisites: checker,
	}
	required, err := reconciler.nodeLocalPoolPrerequisitesRequired(ctx, cluster)
	if err != nil || !required {
		t.Fatalf("retained HostPath claim did not require deletion-time capability proof: required=%v err=%v", required, err)
	}
	if err := reconciler.finalize(ctx, cluster); err != nil {
		t.Fatalf("terminal finalization failed after cached prerequisite evidence: %v", err)
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(probe), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ownerless probe leaked when namespace cache bypassed its read: %v", err)
	}
	freshNode := &corev1.Node{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(kubernetesNode), freshNode); err != nil {
		t.Fatal(err)
	}
	if _, retained := freshNode.Annotations[nodeLocalPoolHostPathClaimAnnotation(cluster, pool.Name)]; retained {
		t.Fatal("test did not reach the terminal retained-claim cleanup transition")
	}
}

func TestNodeLocalPoolPrerequisitesBlockPersistedRecoveryAndFinalization(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "persisted-rollout", Namespace: membershipTestNamespace,
			UID: types.UID("persisted-uid"), Generation: 4,
		},
		Status: garagev1beta2.GarageClusterStatus{StorageRollout: &garagev1beta2.StorageRolloutStatus{
			NodeLocalPoolName: testTagLocal, KubernetesNodeName: testKubernetesWorkerA,
		}},
	}
	controller := true
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "persisted-rollout-storage-local", Namespace: cluster.Namespace,
			UID:    types.UID("daemonset-uid"),
			Labels: map[string]string{labelCluster: cluster.Name, labelTier: tierStorage, labelNodeLocalPool: testTagLocal},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: garagev1beta2.GroupVersion.String(), Kind: kindGarageCluster,
				Name: cluster.Name, UID: cluster.UID, Controller: &controller,
			}},
		},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: nodeLocalPoolSchedulingGateName}},
		}}},
	}
	kubernetesNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: testKubernetesWorkerA, Labels: map[string]string{nodeLocalPoolActivationLabel(cluster, testTagLocal): "active"},
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&garagev1beta2.GarageCluster{}).
		WithObjects(cluster, daemonSet, kubernetesNode).Build()
	checker := &staticNodeLocalPoolPrerequisiteChecker{result: NodeLocalPoolPrerequisiteResult{
		Reason:  garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable,
		Message: "the API server rejected scheduling gates",
	}}
	reconciler := &GarageClusterReconciler{
		Client: kubeClient, APIReader: kubeClient, ClusterScoped: true,
		NodeLocalPoolPrerequisites: checker,
	}
	blocked, _, err := reconciler.blockForNodeLocalPoolPrerequisites(context.Background(), cluster)
	if err != nil || !blocked {
		t.Fatalf("persisted rollout was not blocked: blocked=%v err=%v", blocked, err)
	}
	if err := reconciler.finalize(context.Background(), cluster); err == nil ||
		!strings.Contains(err.Error(), garagev1beta1.ReasonNodeLocalPoolSchedulingGatesUnavailable) {
		t.Fatalf("finalization bypassed prerequisite evidence: %v", err)
	}
	freshDaemonSet := &appsv1.DaemonSet{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(daemonSet), freshDaemonSet); err != nil {
		t.Fatalf("prerequisite block deleted the persisted DaemonSet: %v", err)
	}
	if !podSpecHasSchedulingGate(freshDaemonSet.Spec.Template.Spec, nodeLocalPoolSchedulingGateName) {
		t.Fatal("prerequisite block changed the persisted DaemonSet gate")
	}
	freshNode := &corev1.Node{}
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(kubernetesNode), freshNode); err != nil {
		t.Fatal(err)
	}
	if freshNode.Labels[nodeLocalPoolActivationLabel(cluster, testTagLocal)] != "active" {
		t.Fatal("prerequisite block mutated persisted Node activation")
	}
}

func TestUnsupportedDesiredOnlyNodeLocalPoolCanFinalize(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := garagev1beta2.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	cluster := &garagev1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "never-activated", Namespace: membershipTestNamespace,
			UID: types.UID("never-activated-uid"), Finalizers: []string{garageClusterFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: garagev1beta2.GarageClusterSpec{Storage: &garagev1beta2.StorageSpec{
			Replicas: 0,
			NodeLocalPools: []garagev1beta2.NodeLocalPoolSpec{{
				Name: testTagLocal,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{
					membershipTestDiskLabel: testTagLocal,
				}},
			}},
		}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&garagev1beta2.GarageCluster{}).
		WithObjects(cluster).Build()
	checker := &staticNodeLocalPoolPrerequisiteChecker{result: NodeLocalPoolPrerequisiteResult{
		Reason:  garagev1beta1.ReasonNodeLocalPoolUnsupportedKubernetesVersion,
		Message: "Kubernetes v1.26.15 is below the required 1.27 capability boundary",
	}}
	reconciler := &GarageClusterReconciler{
		Client: kubeClient, APIReader: kubeClient, Scheme: scheme, ClusterScoped: true,
		NodeLocalPoolPrerequisites: checker,
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(cluster),
	}); err != nil {
		t.Fatalf("desired-only unsupported pool could not finalize: %v", err)
	}
	if checker.calls != 0 {
		t.Fatalf("desired-only deletion invoked the capability checker %d time(s)", checker.calls)
	}
	fresh := &garagev1beta2.GarageCluster{}
	err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(cluster), fresh)
	if err == nil && slices.Contains(fresh.Finalizers, garageClusterFinalizer) {
		t.Fatalf("desired-only unsupported pool retained its finalizer: %#v", fresh.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}
