/*
Copyright 2026 Raj Singh.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1beta1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta2 "github.com/rajsinghtech/garage-operator/api/v1beta2"
)

// daemonSetHub is a v1beta2 storage-DaemonSet cluster — a shape v1beta1
// cannot express (workload, hostPath volumes, uniform capacity).
func daemonSetHub() *v1beta2.GarageCluster {
	capacity := resource.MustParse("500Gi")
	return &v1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: testNS},
		Spec: v1beta2.GarageClusterSpec{
			Storage: &v1beta2.StorageSpec{
				Workload: v1beta2.WorkloadTypeDaemonSet,
				Capacity: &capacity,
				Metadata: &v1beta2.VolumeConfig{Type: v1beta2.VolumeTypeHostPath, HostPath: "/var/lib/garage/meta"},
				Data:     &v1beta2.VolumeConfig{Type: v1beta2.VolumeTypeHostPath, HostPath: "/var/lib/garage/data"},
			},
		},
	}
}

// The v1beta1 view of a DaemonSet storage cluster renders as storage-present
// and is stamped with the v1beta2-only annotation, because v1beta1 cannot
// express the workload/hostPath fields. The view must remain valid against
// the v1beta1 schema: no HostPath volume type may leak through.
func TestConvertFrom_DaemonSetStorage_AnnotatedAndSchemaSafe(t *testing.T) {
	dst := &GarageCluster{}
	if err := dst.ConvertFrom(daemonSetHub()); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}

	if dst.Annotations[v1beta2AnnotationGatewayTierPresent] == "" {
		t.Errorf("v1beta1 view of a DaemonSet storage cluster must carry the %s annotation", v1beta2AnnotationGatewayTierPresent)
	}
	if dst.Spec.Gateway {
		t.Errorf("must render as a storage cluster, not gateway")
	}
	if dst.Spec.Storage.Metadata != nil && string(dst.Spec.Storage.Metadata.Type) == "HostPath" {
		t.Errorf("HostPath volume type leaked into the v1beta1 view (schema-invalid): %+v", dst.Spec.Storage.Metadata)
	}
	if dst.Spec.Storage.Data != nil && string(dst.Spec.Storage.Data.Type) == "HostPath" {
		t.Errorf("HostPath volume type leaked into the v1beta1 view (schema-invalid): %+v", dst.Spec.Storage.Data)
	}
}

// A plain StatefulSet storage cluster must NOT be stamped — only shapes
// v1beta1 cannot express get the annotation.
func TestConvertFrom_StatefulSetStorage_NotAnnotated(t *testing.T) {
	hub := &v1beta2.GarageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "sts", Namespace: testNS},
		Spec: v1beta2.GarageClusterSpec{
			Storage: &v1beta2.StorageSpec{
				Replicas: 3,
				Metadata: &v1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse(test10Gi))},
				Data:     &v1beta2.VolumeConfig{Size: ptrQuantity(resource.MustParse("100Gi"))},
			},
		},
	}
	dst := &GarageCluster{}
	if err := dst.ConvertFrom(hub); err != nil {
		t.Fatalf("ConvertFrom: %v", err)
	}
	if dst.Annotations[v1beta2AnnotationGatewayTierPresent] != "" {
		t.Errorf("StatefulSet storage cluster wrongly annotated as v1beta2-only")
	}
}
