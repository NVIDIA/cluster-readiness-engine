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

// WorkloadLabelValue is a Kubernetes label value in a WorkloadMetadata labels
// map. It exists as a named type only so controller-gen emits the length and
// pattern constraints under the map's additionalProperties: there is no marker
// that attaches value constraints to a map[string]string.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^$|^[a-zA-Z0-9]([-a-zA-Z0-9_.]{0,61}[a-zA-Z0-9])?$`
type WorkloadLabelValue string

// WorkloadMetadata describes metadata for the workload object a Job creates
// from spec.workload — today a Kubeflow TrainJob. It carries labels only. It
// deliberately does not expose metav1.ObjectMeta: the generated object's name,
// namespace, owner references and finalizers are controller-owned.
//
// These labels are executable policy in most clusters, not decoration. They
// are how a workload reaches a Kueue local queue
// (kueue.x-k8s.io/queue-name), a KAI Scheduler queue (kai.scheduler/queue),
// or any other integration keyed on the submitted object's labels — all of
// which are read before workload pods exist. They are not pod-label
// injection: they label the workload object and nothing below it.
type WorkloadMetadata struct {
	// labels are merged onto the generated workload object's metadata.labels.
	//
	// Keys must be valid Kubernetes label keys: an optional DNS-subdomain
	// prefix of at most 253 characters followed by "/", then a name of at most
	// 63 characters. Values follow Kubernetes label-value syntax and may be
	// empty; an empty value is a real label, not a deletion.
	//
	// The controller-owned keys "app.kubernetes.io/managed-by" and anything
	// under the "nvcre.nvidia.com/" prefix are rejected rather than silently
	// overwritten — they identify controller ownership and Job association.
	//
	// At most 32 entries. That bound is an enforced API limit, chosen as the
	// product decision for how many workload labels one object may carry.
	// (A finite maxProperties is separately required to bound the static cost
	// of the CEL rules below, but the cost estimator accepts far larger
	// values, so it does not pick this number.)
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k != 'app.kubernetes.io/managed-by' && !k.startsWith('nvcre.nvidia.com/'))",message="labels must not set the controller-owned keys app.kubernetes.io/managed-by or any key under the nvcre.nvidia.com/ prefix"
	// The key grammar uses [.] rather than \. because the marker parser
	// collapses the escape and CEL then rejects \. as an invalid escape in a
	// string literal. The two are equivalent to the regex engine.
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[a-zA-Z0-9]([-a-zA-Z0-9_.]{0,61}[a-zA-Z0-9])?$'))",message="each label key must be a valid Kubernetes label key: an optional DNS-subdomain prefix followed by '/', then a name of at most 63 characters beginning and ending with an alphanumeric character"
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.contains('/') ? k.split('/')[0].size() <= 253 : true)",message="each label key prefix must be at most 253 characters"
	Labels map[string]WorkloadLabelValue `json:"labels,omitempty"`
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
//
// The workloadMetadata presence rule lives here rather than on the field
// because a transition rule scoped to an optional field does not run when that
// field is added or removed. Pairing it with the field's own self == oldSelf
// forbids all three transitions, so a Job cannot drop workloadMetadata and
// re-add it under a different queue.
// +kubebuilder:validation:XValidation:rule="has(self.workloadMetadata) == has(oldSelf.workloadMetadata)",message="workloadMetadata cannot be added or removed after creation"
type JobSpec struct {
	// workload defines the workload to run.
	// The workload is created as a child resource of the Job.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workload is immutable"
	Workload WorkloadSpec `json:"workload"`

	// workloadMetadata sets labels on the workload object created from
	// spec.workload. This is the canonical boundary for workload-object
	// labels: WorkloadRun and Certification both resolve their own
	// workloadMetadata into this field, and a hand-authored Workflow writes
	// spec.jobTemplate.spec.workloadMetadata directly.
	//
	// Ordinary Job labels (Workflow spec.jobTemplate.metadata.labels) are not
	// copied here. Labels are executable policy, so opting a workload into an
	// admission controller or a queue has to be written down explicitly.
	//
	// Immutable in both presence and value, so a checkpoint restart recreates
	// the workload with the labels it was admitted with.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadMetadata is immutable"
	WorkloadMetadata *WorkloadMetadata `json:"workloadMetadata,omitempty"`

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

	// schedulingStallGraceSeconds is how long the workload's pods may remain
	// unschedulable (PodScheduled=False/Unschedulable) before the Job surfaces
	// an InProgress condition with reason "WorkloadSchedulingBlocked". The
	// grace window only delays the condition: blocked time is excluded from
	// timeoutPerJob and stall detection from the first blocked observation.
	// Default: 300 (5 minutes). See ADR-083.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SchedulingStallGraceSeconds *int32 `json:"schedulingStallGraceSeconds,omitempty"`

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

	// workloadStartTime is when the workload was first observed running rather
	// than pending. A workload held by an admission controller (for example a
	// TrainJob suspended by Kueue until quota is available) is pending, and its
	// queued time does not count against timeoutPerJob or stall detection:
	// timeoutPerJob is measured from this timestamp, and the stall clock never
	// starts before it. Cleared on checkpoint restart so the replacement
	// workload gets a fresh budget. When a scheduling-blocked episode ends,
	// it is advanced by the paused interval, so the workload keeps only the
	// budget it had left before the block (ADR-083).
	// +optional
	WorkloadStartTime *metav1.Time `json:"workloadStartTime,omitempty"`

	// schedulingBlockedSince records when the workload's pods were first
	// observed unschedulable in the current blocked episode. Set by the
	// controller when the blocked state is first detected, cleared when no
	// pod is blocked. While set, the timeoutPerJob clock is paused at this
	// instant. Persisted so controller restarts do not reset the grace
	// window. See ADR-083.
	// +optional
	SchedulingBlockedSince *metav1.Time `json:"schedulingBlockedSince,omitempty"`

	// schedulingResumedTime records when the most recent blocked episode
	// ended. Training-stall detection measures from this instant when it is
	// later than the last observed training step, so time spent unschedulable
	// is not charged to the stall budget. See ADR-083.
	// +optional
	SchedulingResumedTime *metav1.Time `json:"schedulingResumedTime,omitempty"`

	// restartCount tracks the number of times the workload has been restarted from checkpoint.
	// +optional
	RestartCount int32 `json:"restartCount,omitempty"`

	// failureLog captures the tail of pod logs from the most recent workload failure.
	// Populated when the Job transitions to Failed. Only the pod that caused the
	// failure (earliest non-zero exit code) is captured. Overwritten on each retry.
	// Always set on failure: when no pod was available to read, reason and tail
	// record why rather than leaving the field unset.
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
	// tail is the end of the failed container's logs, up to 32 KB.
	// When no pod was available to read, this explains why instead.
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
