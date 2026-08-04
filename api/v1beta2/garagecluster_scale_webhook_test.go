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

package v1beta2

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func scalableStorageCluster(name string) *GarageCluster {
	metadata := resource.MustParse("1Gi")
	data := resource.MustParse("10Gi")
	return &GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, ResourceVersion: "7", Generation: 1},
		Spec: GarageClusterSpec{
			LayoutPolicy: layoutPolicyAuto,
			Zone:         testSiteA,
			Storage: &StorageSpec{
				Replicas: 2,
				Metadata: &VolumeConfig{Size: &metadata},
				Data:     &VolumeConfig{Size: &data},
			},
			Replication: &ReplicationConfig{Factor: 1, ConsistencyMode: consistencyModeConsistent},
		},
	}
}

func scaleAdmissionRequest(t *testing.T, cluster *GarageCluster, version string, replicas int32, resourceVersion string) admission.Request {
	t.Helper()
	scale := autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cluster.Name,
			Namespace:       cluster.Namespace,
			ResourceVersion: resourceVersion,
		},
		Spec: autoscalingv1.ScaleSpec{Replicas: replicas},
	}
	raw, err := json.Marshal(scale)
	if err != nil {
		t.Fatal(err)
	}
	resourceGVR := metav1.GroupVersionResource{Group: GroupVersion.Group, Version: version, Resource: "garageclusters"}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Name:            cluster.Name,
		Namespace:       cluster.Namespace,
		Operation:       admissionv1.Update,
		Resource:        resourceGVR,
		RequestResource: &resourceGVR,
		SubResource:     "scale",
		Object:          runtime.RawExtension{Raw: raw},
	}}
}

func scaleValidatorFor(t *testing.T, cluster *GarageCluster) *garageClusterScaleValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &garageClusterScaleValidator{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()}
}

func TestGarageClusterScaleValidatorUsesFullTopologySafetyGates(t *testing.T) {
	t.Run("active drain freezes scale", func(t *testing.T) {
		cluster := scalableStorageCluster("draining")
		cluster.Status.StorageDrain = &StorageDrainStatus{TransactionID: "drain-1"}
		response := scaleValidatorFor(t, cluster).Handle(
			context.Background(),
			scaleAdmissionRequest(t, cluster, GroupVersion.Version, 3, cluster.ResourceVersion),
		)
		if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "status.storageDrain") {
			t.Fatalf("scale bypassed active drain freeze: %#v", response.AdmissionResponse)
		}
	})

	t.Run("scale down requires consistent mode", func(t *testing.T) {
		cluster := scalableStorageCluster("degraded-scale")
		cluster.Spec.Replication.ConsistencyMode = "degraded"
		response := scaleValidatorFor(t, cluster).Handle(
			context.Background(),
			scaleAdmissionRequest(t, cluster, GroupVersion.Version, 1, cluster.ResourceVersion),
		)
		if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "consistent") {
			t.Fatalf("scale bypassed consistency preparation: %#v", response.AdmissionResponse)
		}
	})

	t.Run("safe scale up is admitted", func(t *testing.T) {
		cluster := scalableStorageCluster("scale-up")
		response := scaleValidatorFor(t, cluster).Handle(
			context.Background(),
			scaleAdmissionRequest(t, cluster, GroupVersion.Version, 3, cluster.ResourceVersion),
		)
		if !response.Allowed {
			t.Fatalf("safe scale up denied: %#v", response.AdmissionResponse)
		}
	})

	t.Run("stale resource version fails closed", func(t *testing.T) {
		cluster := scalableStorageCluster("stale-scale")
		response := scaleValidatorFor(t, cluster).Handle(
			context.Background(),
			scaleAdmissionRequest(t, cluster, GroupVersion.Version, 3, "6"),
		)
		if response.Allowed || response.Result == nil || response.Result.Code != 409 {
			t.Fatalf("stale scale request did not conflict: %#v", response.AdmissionResponse)
		}
	})
}

func TestGarageClusterWithScaleReplicasUsesVersionProjection(t *testing.T) {
	storage := scalableStorageCluster("storage")
	storage.Spec.Gateway = &GatewaySpec{Replicas: 4}
	candidate, err := garageClusterWithScaleReplicas(storage, "v1beta1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Spec.Storage.Replicas != 3 || candidate.Spec.Gateway.Replicas != 4 {
		t.Fatalf("v1beta1 unified scale did not use its storage-first projection: storage=%d gateway=%d",
			candidate.Spec.Storage.Replicas, candidate.Spec.Gateway.Replicas)
	}

	edge := &GarageCluster{Spec: GarageClusterSpec{LayoutPolicy: layoutPolicyAuto, Gateway: &GatewaySpec{Replicas: 2}}}
	candidate, err = garageClusterWithScaleReplicas(edge, "v1beta1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Spec.Gateway.Replicas != 5 {
		t.Fatalf("v1beta1 edge scale replicas = %d, want 5", candidate.Spec.Gateway.Replicas)
	}
	if _, err := garageClusterWithScaleReplicas(edge, GroupVersion.Version, 5); err == nil ||
		!strings.Contains(err.Error(), "only for the Auto default storage group") {
		t.Fatalf("v1beta2 gateway-only scale was not rejected: %v", err)
	}

	manual := scalableStorageCluster("manual")
	manual.Spec.Storage.LayoutPolicy = layoutPolicyManual
	if _, err := garageClusterWithScaleReplicas(manual, GroupVersion.Version, 0); err == nil ||
		!strings.Contains(err.Error(), "Manual") {
		t.Fatalf("Manual storage scale was not rejected: %v", err)
	}
}
