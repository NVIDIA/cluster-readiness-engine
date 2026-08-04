// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
)

// Job condition types. Only one of these conditions can be True at any given time.
const (
	// JobInProgress indicates the Job is currently running.
	// This is the initial state after the workload is created.
	JobInProgress string = "InProgress"

	// JobSucceeded indicates the Job has completed successfully.
	JobSucceeded string = "Succeeded"

	// JobFailed indicates the Job has failed.
	JobFailed string = "Failed"

	// JobHardwareFailed indicates a hardware failure was detected on one or more
	// nodes running the job's workload. This condition takes precedence over
	// InProgress and is not automatically cleared.
	JobHardwareFailed string = "HardwareFailed"

	// JobValidationFailed indicates that one or more performance thresholds
	// were not met after the workload completed successfully. This condition
	// is independent of the job execution state — the job may be Succeeded
	// (workload completed) while also having ValidationFailed=True (performance
	// below threshold). Status=False means thresholds were evaluated and passed.
	// Like HardwareFailed, it is set independently and
	// aggregated up by the Workflow and Certification controllers.
	JobValidationFailed string = "ValidationFailed"
)

// NodeHealthMonitor configures how the controller monitors nodes for hardware failures.
type NodeHealthMonitor struct {
	// cel defines a CEL expression evaluated against Node objects.
	// The expression must return a boolean. When it evaluates to true,
	// the node is considered to have a hardware failure.
	//
	// The full corev1.Node object is available via the 'node' variable:
	// - node.metadata: ObjectMeta (name, labels, annotations, etc.)
	// - node.spec: NodeSpec (taints, unschedulable, podCIDR, etc.)
	// - node.status: NodeStatus (conditions, addresses, capacity, allocatable, nodeInfo, etc.)
	//
	// Examples:
	// - "node.spec.taints.exists(t, t.key == 'nvidia.com/gpu-unhealthy')"
	// - "'gpud.nvidia.com/unhealthy' in node.metadata.labels"
	// - "node.status.conditions.exists(c, c.type == 'GPUHealthy' && c.status == 'False')"
	// - "node.spec.unschedulable == true"
	//
	// +optional
	CEL *CELNodeHealthCheck `json:"cel,omitempty"`
}

// CELNodeHealthCheck defines CEL-based node health checking.
type CELNodeHealthCheck struct {
	// expression is the CEL expression to evaluate against Node objects.
	// Must return a boolean value. When true, the node is considered unhealthy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Expression string `json:"expression"`
}

// FailedNode identifies a node that failed during execution along with the
// reason it was marked as failed. Failed nodes originate at the Job tier and
// propagate up through Workflow to Certification, preserving the per-node reason.
// NodeFailureReason enumerates why a node was attributed a failure in a FailedNode
// entry. It matches the originating Job condition reason.
// +kubebuilder:validation:Enum=HardwareFailureDetected;ThresholdViolation;WorkloadFailed
type NodeFailureReason string

const (
	// NodeFailureHardwareDetected indicates a node health check (CEL) flagged the node.
	NodeFailureHardwareDetected NodeFailureReason = "HardwareFailureDetected"
	// NodeFailureThresholdViolation indicates a performance threshold (bandwidth/goodput) was not met.
	NodeFailureThresholdViolation NodeFailureReason = "ThresholdViolation"
	// NodeFailureWorkloadFailed indicates the workload exited non-zero, stalled, or otherwise failed.
	NodeFailureWorkloadFailed NodeFailureReason = "WorkloadFailed"
)

type FailedNode struct {
	// name is the Kubernetes node name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// reason is the failure type for this node, matching the originating Job
	// condition reason.
	//   - HardwareFailureDetected: a node health check (CEL) flagged the node.
	//   - ThresholdViolation: a performance threshold (bandwidth/goodput) was not met.
	//   - WorkloadFailed: the workload exited non-zero, stalled, or otherwise failed.
	// +kubebuilder:validation:Required
	Reason NodeFailureReason `json:"reason"`

	// message is the detailed failure message for this node, matching the originating Job condition message.
	// +optional
	Message string `json:"message,omitempty"`
}

// WorkloadReference references a workload resource.
type WorkloadReference struct {
	// apiVersion is the API version of the workload resource.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// kind is the kind of the workload resource.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// name is the name of the workload resource.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// namespace is the namespace of the workload resource.
	// If not specified, defaults to the Job's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// WorkloadSpec is a discriminated union of typed workload specs.
// Exactly one field must be set.
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:MaxProperties=1
type WorkloadSpec struct {
	// trainJob configures a Kubeflow TrainJob workload.
	// +optional
	TrainJob *trainerv1alpha1.TrainJobSpec `json:"trainJob,omitempty"`
}

// CheckpointConfig configures checkpoint-based restart for the workload.
// The user is responsible for defining the PVC volume and mounts in their
// workload spec. The controller uses this config to validate the PVC
// exists in Workflow dependencies and to determine restart behavior.
type CheckpointConfig struct {
	// pvcName is the name of the PersistentVolumeClaim used for checkpoint storage.
	// Must match a PVC in the Workflow's dependencies.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PVCName string `json:"pvcName"`

	// maxRestarts is the maximum number of times the Job will restart on failure
	// when a checkpoint exists. Default: 0 (no restarts).
	// +optional
	MaxRestarts *int32 `json:"maxRestarts,omitempty"`
}

// GoodputMeasurementConfig configures automatic creation of a GoodputMeasurement
// child resource for the Job. When set, the controller creates a GoodputMeasurement
// that tracks training goodput metrics by parsing pod logs.
type GoodputMeasurementConfig struct {
	// logProfileRef is the name of the cluster-scoped LogProfile to use for log parsing.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	LogProfileRef string `json:"logProfileRef"`

	// sampleInterval is how often to sample pod logs while the Job is running.
	// Default: 60s.
	// +optional
	SampleInterval *metav1.Duration `json:"sampleInterval,omitempty"`
}

// JobSpec defines the desired state of Job
type JobSpec struct {
	// workload defines the workload to run.
	// The workload is created as a child resource of the Job.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workload is immutable"
	Workload WorkloadSpec `json:"workload"`

	// nodeHealthMonitor configures hardware failure detection for nodes
	// running this job's pods. When a failure is detected, the job will
	// be marked with the HardwareFailed condition.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="nodeHealthMonitor is immutable"
	NodeHealthMonitor *NodeHealthMonitor `json:"nodeHealthMonitor,omitempty"`

	// checkpoint configures checkpoint-based restart for the workload.
	// When set, the Job controller restarts the workload on failure if a
	// checkpoint was logged by an associated GoodputMeasurement. The user
	// must define the PVC volume and mounts in the workload spec directly.
	// +optional
	Checkpoint *CheckpointConfig `json:"checkpoint,omitempty"`

	// stallMultiplier configures stall detection for the workload. When set,
	// the Job controller compares the time since the last training step against
	// stallMultiplier * avgStepTime (from the associated GoodputMeasurement).
	// If the elapsed time exceeds this product, the workload is marked as Failed
	// with reason "WorkloadStalled".
	// Requires a GoodputMeasurement for this Job; if none exists, stall
	// detection is skipped.
	// Example: a value of 10 means the job is stalled if no step has been
	// logged for 10x the average step duration.
	// +optional
	// +kubebuilder:validation:Minimum=1
	StallMultiplier *int32 `json:"stallMultiplier,omitempty"`

	// startupStallTimeoutSeconds is the maximum time (in seconds) to wait after
	// the application framework starts for the first training step. If no step
	// is observed within this timeout, the workload is marked Failed with reason
	// "WorkloadStalled". Only active when stallMultiplier is set.
	// Default: 1200 (20 minutes).
	// +optional
	// +kubebuilder:validation:Minimum=1
	StartupStallTimeoutSeconds *int32 `json:"startupStallTimeoutSeconds,omitempty"`

	// goodputMeasurement configures automatic creation of a GoodputMeasurement
	// child resource that tracks training goodput metrics by parsing pod logs.
	// When absent, no measurement is created (suitable for non-training jobs).
	// Manually-created GoodputMeasurements continue to work via the existing
	// List-based lookup.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="goodputMeasurement is immutable"
	GoodputMeasurement *GoodputMeasurementConfig `json:"goodputMeasurement,omitempty"`

	// bandwidthMeasurement configures automatic creation of a BandwidthMeasurement
	// child resource that tracks NCCL bandwidth metrics by parsing pod logs.
	// When absent, no measurement is created (suitable for non-NCCL jobs).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bandwidthMeasurement is immutable"
	BandwidthMeasurement *BandwidthMeasurementConfig `json:"bandwidthMeasurement,omitempty"`

	// thresholds maps metric names to CEL expressions for performance validation.
	// Propagated from WorkflowSpec.Validation.Performance.Thresholds by the
	// Workflow controller during Job creation. The Job controller evaluates them
	// after workload success and sets ValidationFailed if any threshold is violated.
	// Keys: "busBandwidthGBps", "goodputRatio", "avgTFLOPsPerGPU", "avgStepTimeSec", etc.
	// Values: CEL expressions using `value` variable, e.g. "value >= 900"
	// +optional
	Thresholds map[string]string `json:"thresholds,omitempty"`

	// measurementTimeout is the maximum time to wait after the Job succeeds for
	// measurement data before failing threshold validation. Propagated from the
	// Workflow's ValidationSpec by the Workflow controller during Job creation.
	// When nil, the Job controller uses its default (5m).
	// +optional
	MeasurementTimeout *metav1.Duration `json:"measurementTimeout,omitempty"`
}

// BandwidthMeasurementConfig configures automatic creation of a BandwidthMeasurement
// child resource for the Job. When set, the controller creates a BandwidthMeasurement
// that tracks NCCL bandwidth metrics by parsing pod logs.
type BandwidthMeasurementConfig struct {
	// logProfileRef is the name of the cluster-scoped LogProfile to use for log parsing.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	LogProfileRef string `json:"logProfileRef"`

	// sampleInterval is how often to sample pod logs while the Job is running.
	// Default: 60s.
	// +optional
	SampleInterval *metav1.Duration `json:"sampleInterval,omitempty"`

	// testType identifies the NCCL collective operation (e.g., "all_reduce", "alltoall").
	// Propagated to the BandwidthMeasurement spec for Prometheus metric labeling.
	// +optional
	TestType string `json:"testType,omitempty"`
}

// JobStatus defines the observed state of Job.
type JobStatus struct {
	// conditions represent the current state of the Job.
	// Only one of the following conditions can be True at any given time:
	// - "InProgress": the Job is currently running
	// - "Succeeded": the Job has completed successfully
	// - "Failed": the Job has failed
	// - "HardwareFailed": a hardware failure was detected on a node
	//
	// The condition message contains details about the current state for debugging.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// failedNodes lists nodes that failed during this Job, each with the reason
	// for failure. Populated when the Job fails, a hardware failure is detected,
	// or a performance threshold is violated. A node may appear multiple times
	// with different reasons; entries are keyed by the (name, reason) pair.
	// +listType=map
	// +listMapKey=name
	// +listMapKey=reason
	// +optional
	FailedNodes []FailedNode `json:"failedNodes,omitempty"`

	// workloadRef references the workload resource created by this Job.
	// Populated after the workload is created. Used to track workload status.
	// +optional
	WorkloadRef *WorkloadReference `json:"workloadRef,omitempty"`

	// restartCount tracks the number of times the workload has been restarted from checkpoint.
	// +optional
	RestartCount int32 `json:"restartCount,omitempty"`

	// failureLog captures the tail of pod logs from the most recent workload failure.
	// Populated when the Job transitions to Failed. Only the pod that caused the
	// failure (earliest non-zero exit code) is captured. Overwritten on each retry.
	// +optional
	FailureLog *FailureLog `json:"failureLog,omitempty"`
}

// FailureLog captures diagnostic information from a failed workload pod.
type FailureLog struct {
	// podName is the name of the pod that failed.
	PodName string `json:"podName"`
	// nodeName is the node the failed pod was running on.
	NodeName string `json:"nodeName"`
	// exitCode is the container's exit code.
	ExitCode int32 `json:"exitCode"`
	// reason is the termination reason (e.g., "OOMKilled", "Error").
	// +optional
	Reason string `json:"reason,omitempty"`
	// tail is the last ~30 lines of the failed container's logs.
	// +optional
	Tail string `json:"tail,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Job is the Schema for the jobs API
type Job struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Job
	// +required
	Spec JobSpec `json:"spec"`

	// status defines the observed state of Job
	// +optional
	Status JobStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// JobList contains a list of Job
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Job `json:"items"`
}

func init() {
	Register(&Job{}, &JobList{})
}
