// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LogProfileSpec defines the desired state of LogProfile.
type LogProfileSpec struct {
	// timestamp configures how to parse timestamps captured by event patterns.
	// +kubebuilder:validation:Required
	Timestamp TimestampSpec `json:"timestamp"`

	// patterns defines regex patterns for extracting training events from log lines.
	// +kubebuilder:validation:Required
	Patterns LogPatternSet `json:"patterns"`

	// workerStrategy defines how to read logs from multi-worker jobs.
	// Default: single worker (read from worker-0 only).
	// +optional
	WorkerStrategy *WorkerStrategySpec `json:"workerStrategy,omitempty"`

	// containerName is the container to read logs from.
	// If empty, the controller auto-detects the container name from the workload.
	// +optional
	ContainerName string `json:"containerName,omitempty"`

	// warmupSteps is the number of initial training steps per run to flag as warmup.
	// Steps after applicationStart or checkpointRestore up to this count are marked
	// as warmup and excluded from average step time computation.
	// +optional
	WarmupSteps *int `json:"warmupSteps,omitempty"`

	// logInterval is the training framework's log interval (e.g., 10 for --log-interval 10).
	// When set, training steps whose GlobalStep is not a multiple of this value are
	// filtered out. This removes inflated "first after boundary" log lines that include
	// startup or restart overhead. Also used as the default step delta for the first
	// step in a window when computing total training time.
	// +optional
	LogInterval *int `json:"logInterval,omitempty"`
}

// TimestampSpec configures timestamp parsing for captured (?P<timestamp>...) groups.
//
// # Timestamp Resolution Strategy
//
// Pod logs are always read with Timestamps: true, so every line has a
// K8s RFC3339 prefix: "2026-02-05T15:30:00.123456Z <content>"
//
// For each matched event:
//  1. If the regex captures (?P<timestamp>...), parse it using Layout
//  2. Otherwise, use the K8s RFC3339 prefix (always available)
//
// For "last log timestamp" tracking (failure time detection):
//   - The K8s RFC3339 prefix is extracted from every line regardless of pattern match
type TimestampSpec struct {
	// layout is the Go time layout string for parsing (?P<timestamp>...) captures.
	// Examples: "2006-01-02T15:04:05.999999999Z" (NeMo/RFC3339),
	//           "2006-01-02 15:04:05.999999" (Megatron bracketed)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Layout string `json:"layout"`
}

// LogPatternSet defines the set of regex patterns for extracting training events.
// Each pattern uses Go named capture groups (?P<name>...) to extract typed fields.
type LogPatternSet struct {
	// trainingStep matches training iteration log lines.
	// Well-known captures: timestamp, globalStep, iteration, epoch,
	// stepTiming, loss, tflops, elapsedTime
	// +optional
	TrainingStep *EventPattern `json:"trainingStep,omitempty"`

	// checkpointSave matches checkpoint save start lines.
	// Well-known captures: timestamp, step, path, saveDuration
	// +optional
	CheckpointSave *EventPattern `json:"checkpointSave,omitempty"`

	// checkpointDone matches checkpoint save completion lines.
	// Well-known captures: timestamp, step, path
	// +optional
	CheckpointDone *EventPattern `json:"checkpointDone,omitempty"`

	// checkpointRestore matches checkpoint restore (load start) lines.
	// Well-known captures: timestamp, path, step
	// +optional
	CheckpointRestore *EventPattern `json:"checkpointRestore,omitempty"`

	// checkpointLoaded matches checkpoint load completed lines.
	// Well-known captures: timestamp, path, step
	// +optional
	CheckpointLoaded *EventPattern `json:"checkpointLoaded,omitempty"`

	// applicationStart matches a line indicating the application has started.
	// Well-known captures: timestamp
	// +optional
	ApplicationStart *EventPattern `json:"applicationStart,omitempty"`

	// warmupStep matches lines that indicate a warmup/startup iteration.
	// When a training step line also matches this pattern, the step is
	// flagged as warmup and excluded from average step time computation.
	// Well-known captures: none required (presence of match is sufficient).
	// +optional
	WarmupStep *EventPattern `json:"warmupStep,omitempty"`

	// bandwidthResult matches NCCL bandwidth test result lines.
	// Used by the BandwidthMeasurement controller (not the GoodputMeasurement controller).
	// Well-known captures: size (int, bytes), algBW (float, GB/s), busBW (float, GB/s)
	// +optional
	BandwidthResult *EventPattern `json:"bandwidthResult,omitempty"`
}

// EventPattern defines a regex pattern for matching a specific training event.
// Uses Go named capture groups (?P<name>...) following the same convention
// as Promtail, FluentBit, and Vector for structured log extraction.
type EventPattern struct {
	// regex is the regular expression with Go named capture groups.
	// Uses (?P<name>...) syntax to name extracted fields.
	//
	// Well-known capture names used by the goodput calculator:
	//   Common:     timestamp
	//   Training:   globalStep, iteration, epoch, stepTiming, loss, tflops, elapsedTime
	//   Checkpoint: step, path, saveDuration
	//
	// The controller parses named captures into typed values:
	//   int:     globalStep, iteration, epoch, step
	//   float64: stepTiming, loss, tflops, elapsedTime, saveDuration
	//   string:  path
	//   time:    timestamp (parsed using LogProfile.spec.timestamp.layout)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Regex string `json:"regex"`

	// example is a sample log line that the regex should match.
	// Serves as documentation and is validated when the LogProfile is loaded.
	// +optional
	Example string `json:"example,omitempty"`

	// units specifies the unit for duration/timing captures.
	// The controller normalizes all durations to seconds internally.
	// Supported values: "s" (seconds), "ms" (milliseconds), "us" (microseconds).
	//
	// Only needed for duration-type captures: stepTiming, elapsedTime, saveDuration.
	// If a duration capture is not listed here, it defaults to "s" (seconds).
	//
	// Example: {"elapsedTime": "ms"} means the captured value is in milliseconds.
	// +optional
	Units map[string]string `json:"units,omitempty"`
}

// WorkerStrategySpec defines how to read logs from multi-worker jobs.
type WorkerStrategySpec struct {
	// type determines the log reading strategy.
	// "Single": read all data from worker-0.
	// "Multi": read lifecycle data from one pod, training steps from another.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Single;Multi
	Type string `json:"type"`

	// lifecyclePod specifies which worker has lifecycle data (checkpoints, app start).
	// "first" (worker-0) or "last" (highest index). Default: "first".
	// Only used when type is "Multi".
	// +kubebuilder:validation:Enum=first;last
	// +kubebuilder:default=first
	// +optional
	LifecyclePod string `json:"lifecyclePod,omitempty"`

	// trainingStepPod specifies which worker has training step data (iterations, metrics).
	// "first" or "last". Default: "first".
	// Only used when type is "Multi".
	// +kubebuilder:validation:Enum=first;last
	// +kubebuilder:default=first
	// +optional
	TrainingStepPod string `json:"trainingStepPod,omitempty"`

	// replicatedJobName selects which JobSet replicatedJob to read logs from.
	// For MPI workloads, set to "launcher" (output goes to the launcher pod).
	// For torch workloads, use the default "trainer".
	// Only applies to TrainJob workloads that use JobSet.
	// +kubebuilder:default=trainer
	// +optional
	ReplicatedJobName string `json:"replicatedJobName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster

// LogProfile is the Schema for the logprofiles API.
// A cluster-scoped resource that defines regex patterns for parsing training logs
// of a specific framework (e.g., Nemotron 4/NeMo, Nemotron 6/Megatron).
type LogProfile struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the log parsing configuration
	// +required
	Spec LogProfileSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// LogProfileList contains a list of LogProfile
type LogProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LogProfile `json:"items"`
}

func init() {
	Register(&LogProfile{}, &LogProfileList{})
}
