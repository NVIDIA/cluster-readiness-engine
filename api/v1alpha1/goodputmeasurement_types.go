// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GoodputMeasurement condition types.
const (
	// GoodputMeasurementMeasuring indicates that the measurement is in progress (the referenced Job is running).
	GoodputMeasurementMeasuring = "Measuring"

	// GoodputMeasurementComplete indicates that the measurement has completed (the referenced Job reached a terminal state).
	GoodputMeasurementComplete = "Complete"
)

// GoodputMeasurementSpec defines the desired state of GoodputMeasurement
type GoodputMeasurementSpec struct {
	// jobRef is the reference to the NVCRE Job for which goodput should be calculated.
	// +optional
	JobRef corev1.TypedLocalObjectReference `json:"jobRef,omitempty"`

	// logProfileRef is the name of the cluster-scoped LogProfile to use for log parsing.
	// The LogProfile defines regex patterns for extracting training metrics from pod logs.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	LogProfileRef string `json:"logProfileRef"`

	// sampleInterval is how often to sample pod logs while the Job is running.
	// Default: 60s.
	// +optional
	SampleInterval *metav1.Duration `json:"sampleInterval,omitempty"`
}

// PendingInterruptionStatus stores an in-progress interruption event that hasn't been
// completed by a job restart yet. Persisted so state survives controller restarts.
type PendingInterruptionStatus struct {
	// tCheckpoint is the time of the last checkpoint before the interruption.
	// +optional
	TCheckpoint *metav1.Time `json:"tCheckpoint,omitempty"`

	// tInterrupt is the time the job was interrupted (workload failure time).
	// +optional
	TInterrupt *metav1.Time `json:"tInterrupt,omitempty"`

	// checkpointStep is the step number of the last saved checkpoint before interruption.
	// +optional
	CheckpointStep int `json:"checkpointStep,omitempty"`

	// lastStep is the last training step observed before interruption.
	// +optional
	LastStep int `json:"lastStep,omitempty"`

	// tCh is the lost work time in seconds (time between last checkpoint and interruption).
	// +optional
	TCh string `json:"tCh,omitempty"`

	// reason is the reason for the interruption.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// GoodputMeasurementStatus defines the observed state of GoodputMeasurement.
type GoodputMeasurementStatus struct {
	// result is the current runtime goodput ratio (0.0 to 1.0) as a string.
	// Formula: (t_w - t_ch - t_rm - t_re - t_save) / (t_w - t_re)
	// where t_w=trainingTime, t_ch=lostWorkTime, t_rm=resumeTime, t_re=rescheduleTime, t_save=checkpointSaveTime
	// +optional
	Result string `json:"result,omitempty"`

	// currentStep is the latest training step observed from logs.
	// +optional
	CurrentStep int `json:"currentStep,omitempty"`

	// highestStep is the highest training step ever reached (may be > currentStep after restart).
	// +optional
	HighestStep int `json:"highestStep,omitempty"`

	// trainingTimeSec is the total training wall-clock time in seconds (t_w).
	// +optional
	TrainingTimeSec string `json:"trainingTimeSec,omitempty"`

	// lostWorkTimeSec is cumulative lost work in seconds (sum of t_ch across all interruptions).
	// +optional
	LostWorkTimeSec string `json:"lostWorkTimeSec,omitempty"`

	// rescheduleTimeSec is cumulative scheduling delay in seconds (sum of t_re).
	// +optional
	RescheduleTimeSec string `json:"rescheduleTimeSec,omitempty"`

	// resumeTimeSec is cumulative resume time in seconds (sum of t_rm: checkpoint load + resume).
	// +optional
	ResumeTimeSec string `json:"resumeTimeSec,omitempty"`

	// checkpointSaveTimeSec is cumulative checkpoint save overhead in seconds (sum of t_save).
	// +optional
	CheckpointSaveTimeSec string `json:"checkpointSaveTimeSec,omitempty"`

	// interruptionCount is the number of interruptions detected.
	// +optional
	InterruptionCount int `json:"interruptionCount,omitempty"`

	// lastCheckpointStep is the step number of the last saved checkpoint.
	// +optional
	LastCheckpointStep int `json:"lastCheckpointStep,omitempty"`

	// lastCheckpointTime is the timestamp of the last saved checkpoint.
	// Persisted so lost work time (t_ch) can be computed after controller restarts.
	// +optional
	LastCheckpointTime *metav1.Time `json:"lastCheckpointTime,omitempty"`

	// startTime is the time when the measurement started (when the referenced Job began running).
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is the time when the measurement completed (when the referenced Job reached a terminal state).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// lastStepTimestamp is the timestamp of the most recently observed training step.
	// Used by the Job controller for stall detection.
	// +optional
	LastStepTimestamp *metav1.Time `json:"lastStepTimestamp,omitempty"`

	// avgStepTimeSec is the average time per training step in seconds (excluding warmup).
	// Computed from observed step timings or consecutive step timestamps.
	// +optional
	AvgStepTimeSec string `json:"avgStepTimeSec,omitempty"`

	// logInterval is the training framework's log interval from the LogProfile.
	// Persisted so the Job controller can scale the stall detection threshold
	// by the number of iterations between consecutive logged steps.
	// +optional
	LogInterval int `json:"logInterval,omitempty"`

	// warmupBaseStep is the GlobalStep of the first training step after the most recent
	// run boundary (applicationStart or checkpointRestore). Steps within warmupSteps of
	// this base are warmup. Persisted so warmup detection survives across SinceTime-based
	// incremental log reads where applicationStart is no longer in the read window.
	// +optional
	WarmupBaseStep *int `json:"warmupBaseStep,omitempty"`

	// warmupTimeSec is the cumulative time spent on warmup steps in seconds (across all runs).
	// +optional
	WarmupTimeSec string `json:"warmupTimeSec,omitempty"`

	// nonWarmupTimeSec is the cumulative time spent on non-warmup steps in seconds (across all runs).
	// +optional
	NonWarmupTimeSec string `json:"nonWarmupTimeSec,omitempty"`

	// lastNonWarmupStep is the highest step whose timing has been accumulated into nonWarmupTimeSec.
	// Used as a watermark to avoid double-counting steps across reconcile cycles.
	// +optional
	LastNonWarmupStep int `json:"lastNonWarmupStep,omitempty"`

	// priorWarmupTimeSec is the accumulated warmup time from completed runs (before the current run).
	// +optional
	PriorWarmupTimeSec string `json:"priorWarmupTimeSec,omitempty"`

	// priorNonWarmupTimeSec is the accumulated non-warmup time from completed runs (before the current run).
	// +optional
	PriorNonWarmupTimeSec string `json:"priorNonWarmupTimeSec,omitempty"`

	// avgTFLOPSPerGPU is the average TFLOPS per GPU observed from training step logs (including warmup).
	// +optional
	AvgTFLOPSPerGPU string `json:"avgTFLOPSPerGPU,omitempty"`

	// applicationStartTime is the timestamp of the first application framework log marker.
	// Persisted so startup resume time can be computed even if the log line scrolls out of the tail window.
	// +optional
	ApplicationStartTime *metav1.Time `json:"applicationStartTime,omitempty"`

	// applicationStopTime is the timestamp of the last application log line observed.
	// Used as the interruption time for goodput calculations — more accurate than
	// container termination time or wall clock for stalls and crashes.
	// +optional
	ApplicationStopTime *metav1.Time `json:"applicationStopTime,omitempty"`

	// pendingInterruption stores an in-progress interruption event that hasn't been
	// completed by a job restart yet. Persisted so state survives controller restarts.
	// +optional
	PendingInterruption *PendingInterruptionStatus `json:"pendingInterruption,omitempty"`

	// conditions represent the current state of the GoodputMeasurement resource.
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

// GoodputMeasurement is the Schema for the goodputmeasurements API
type GoodputMeasurement struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of GoodputMeasurement
	// +required
	Spec GoodputMeasurementSpec `json:"spec"`

	// status defines the observed state of GoodputMeasurement
	// +optional
	Status GoodputMeasurementStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// GoodputMeasurementList contains a list of GoodputMeasurement
type GoodputMeasurementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []GoodputMeasurement `json:"items"`
}

func init() {
	Register(&GoodputMeasurement{}, &GoodputMeasurementList{})
}
