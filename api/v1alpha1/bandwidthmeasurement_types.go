// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BandwidthMeasurement condition types.
const (
	// BandwidthMeasurementMeasuring indicates that the measurement is in progress (the referenced Job is running).
	BandwidthMeasurementMeasuring = "Measuring"

	// BandwidthMeasurementComplete indicates that the measurement has completed (the referenced Job reached a terminal state).
	BandwidthMeasurementComplete = "Complete"
)

// BandwidthMeasurementSpec defines the desired state of BandwidthMeasurement
type BandwidthMeasurementSpec struct {
	// jobRef is the reference to the burnin Job for which bandwidth should be measured.
	// +optional
	JobRef corev1.TypedLocalObjectReference `json:"jobRef,omitempty"`

	// logProfileRef is the name of the cluster-scoped LogProfile to use for log parsing.
	// The LogProfile defines a bandwidthResult regex pattern for extracting NCCL bandwidth data.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	LogProfileRef string `json:"logProfileRef"`

	// sampleInterval is how often to sample pod logs while the Job is running.
	// Default: 60s.
	// +optional
	SampleInterval *metav1.Duration `json:"sampleInterval,omitempty"`

	// testType identifies the NCCL collective operation being measured (e.g., "all_reduce", "alltoall").
	// Used as the "nccl_test" Prometheus label to distinguish results on dashboards.
	// +optional
	TestType string `json:"testType,omitempty"`
}

// BandwidthResult stores the average bandwidth for a single message size.
type BandwidthResult struct {
	// sizeBytes is the message size in bytes.
	SizeBytes int64 `json:"sizeBytes"`

	// algBW is the average algorithmic bandwidth in GB/s.
	AlgBW string `json:"algBW"`

	// busBW is the average bus bandwidth in GB/s.
	BusBW string `json:"busBW"`

	// samples is the number of measurements averaged for this size.
	Samples int `json:"samples"`
}

// BandwidthMeasurementStatus defines the observed state of BandwidthMeasurement.
type BandwidthMeasurementStatus struct {
	// results contains per-message-size average bandwidth measurements.
	// Each entry represents the running average of all observed measurements for that size.
	// +optional
	Results []BandwidthResult `json:"results,omitempty"`

	// startTime is the time when the measurement started (when the referenced Job began running).
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is the time when the measurement completed (when the referenced Job reached a terminal state).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// conditions represent the current state of the BandwidthMeasurement resource.
	//
	// Condition types:
	// - "Measuring": the measurement is in progress (referenced Job is running)
	// - "Complete": the measurement has finished (referenced Job succeeded or failed)
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// BandwidthMeasurement is the Schema for the bandwidthmeasurements API
type BandwidthMeasurement struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BandwidthMeasurement
	// +required
	Spec BandwidthMeasurementSpec `json:"spec"`

	// status defines the observed state of BandwidthMeasurement
	// +optional
	Status BandwidthMeasurementStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BandwidthMeasurementList contains a list of BandwidthMeasurement
type BandwidthMeasurementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BandwidthMeasurement `json:"items"`
}

func init() {
	Register(&BandwidthMeasurement{}, &BandwidthMeasurementList{})
}
