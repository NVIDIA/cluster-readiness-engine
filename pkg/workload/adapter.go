// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// WorkloadPhase represents the normalized phase of a workload.
type WorkloadPhase string

const (
	WorkloadRunning   WorkloadPhase = "Running"
	WorkloadSucceeded WorkloadPhase = "Succeeded"
	WorkloadFailed    WorkloadPhase = "Failed"
)

// WorkloadStatus is the normalized status returned by adapters.
type WorkloadStatus struct {
	Phase   WorkloadPhase
	Reason  string
	Message string
}

// Adapter provides a typed interface for creating and inspecting workloads.
type Adapter interface {
	// GVK returns the GroupVersionKind of the workload type.
	GVK() schema.GroupVersionKind

	// NewObject returns a new empty typed object (for Get/Delete).
	NewObject() client.Object

	// Build creates a fully typed workload object from the WorkloadSpec.
	// The name and namespace are set on the returned object.
	Build(name, namespace string, spec *crev1alpha1.WorkloadSpec) (client.Object, error)

	// InjectPodLabel ensures the given label reaches pod templates.
	// Mutates spec in-place — call on a DeepCopy before Build.
	InjectPodLabel(spec *crev1alpha1.WorkloadSpec, key, value string)

	// SetNodeSelector sets nodeSelector on the workload's pod templates.
	// Mutates spec in-place — call on a DeepCopy before Build.
	SetNodeSelector(spec *crev1alpha1.WorkloadSpec, selector map[string]string)

	// SetNodeAffinity sets node affinity on the workload's pod templates.
	// Mutates spec in-place — call on a DeepCopy before Build.
	SetNodeAffinity(spec *crev1alpha1.WorkloadSpec, affinity *corev1.NodeAffinity)

	// SetTolerations appends tolerations to the workload's pod templates.
	// Mutates spec in-place — call on a DeepCopy before Build.
	SetTolerations(spec *crev1alpha1.WorkloadSpec, tolerations []corev1.Toleration)

	// NodesRequired returns the number of nodes required by the workload spec.
	// This is auto-detected from the replica count in the workload definition.
	NodesRequired(spec *crev1alpha1.WorkloadSpec) (int, error)

	// SetNumNodes overrides the number of nodes/replicas in the workload spec.
	// Used by bisection to match the workload's node count to the group size,
	// which changes each round. Mutates spec in-place.
	SetNumNodes(spec *crev1alpha1.WorkloadSpec, numNodes int)

	// GetStatus reads typed status conditions and returns a normalized WorkloadStatus.
	GetStatus(obj client.Object) (*WorkloadStatus, error)
}

// ForSpec returns the appropriate Adapter for the given WorkloadSpec.
func ForSpec(spec *crev1alpha1.WorkloadSpec) (Adapter, error) {
	switch {
	case spec.TrainJob != nil:
		return &TrainJobAdapter{}, nil
	default:
		return nil, fmt.Errorf("no workload type specified in WorkloadSpec")
	}
}
