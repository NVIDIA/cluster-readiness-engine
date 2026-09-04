// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Certification condition types. Only one of InProgress, Succeeded, or Failed
// can be True at any given time.
const (
	// CertificationInProgress indicates the Certification is currently running.
	CertificationInProgress = "InProgress"

	// CertificationSucceeded indicates the Certification has completed successfully.
	CertificationSucceeded = "Succeeded"

	// CertificationFailed indicates the Certification has failed.
	CertificationFailed = "Failed"

	// CertificationValidationFailed indicates that one or more Workflows had
	// performance threshold violations. This condition is independent of the
	// execution state conditions and provides a quality signal.
	CertificationValidationFailed = "ValidationFailed"
)

// TestScale values for orchestration strategy.
const (
	TestScaleIntraNode = "intra-node"
	TestScaleIntraRack = "intra-rack"
	TestScaleDiagnose  = "diagnose"
	TestScaleFullScale = "full-scale"
)

// WorkflowReference references a Workflow resource created by the Certification.
type WorkflowReference struct {
	// name is the name of the Workflow resource.
	Name string `json:"name"`

	// namespace is the namespace of the Workflow resource.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// CertificationCategoryStatus tracks the status of a single certification category.
type CertificationCategoryStatus struct {
	// domain is the category domain.
	Domain string `json:"domain"`

	// variant is the category variant.
	Variant string `json:"variant"`

	// workflowRef references the Workflow created for this category.
	// +optional
	WorkflowRef *WorkflowReference `json:"workflowRef,omitempty"`

	// status is the current status of this category.
	// Values: "Pending", "InProgress", "Succeeded", "Failed"
	Status string `json:"status"`

	// succeededNodesRef references the ConfigMap holding this category's
	// succeeded-nodes list (comma-separated node names). Copied from the Workflow.
	// +optional
	SucceededNodesRef *corev1.TypedLocalObjectReference `json:"succeededNodesRef,omitempty"`

	// failedNodesRef references the ConfigMap holding this category's failed-nodes
	// list (name, reason, message). Copied from the Workflow.
	// +optional
	FailedNodesRef *corev1.TypedLocalObjectReference `json:"failedNodesRef,omitempty"`
}

// CategoryResourceList holds CPU and memory quantities for one side
// (limits or requests) of a training container's resources.
// The MaxLength bounds keep the CEL request/limit comparison on
// CategoryResources within the API server's validation cost budget; any
// realistic quantity is far shorter.
type CategoryResourceList struct {
	// cpu is the CPU quantity (e.g., "6", "1500m").
	// +optional
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).compareTo(quantity(\"0\")) >= 0",message="cpu must be a non-negative quantity"
	CPU *resource.Quantity `json:"cpu,omitempty"`

	// memory is the memory quantity (e.g., "48Gi").
	// +optional
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).compareTo(quantity(\"0\")) >= 0",message="memory must be a non-negative quantity"
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// CategoryResources overrides the CPU and memory resources applied to
// training workload containers. Each value that is unset keeps the catalog
// default for that value. GPU count is controlled by gpusPerNode, not here.
// When a request and its matching limit are both set, the request must not
// exceed the limit; the CRD rejects the inverted pair at admission. A request
// set without its matching limit is only checked against the resolved default
// at pod admission, so raise the limit alongside the request when overriding
// upward.
// +kubebuilder:validation:XValidation:rule="!has(self.requests) || !has(self.limits) || !has(self.requests.cpu) || !has(self.limits.cpu) || quantity(string(self.requests.cpu)).compareTo(quantity(string(self.limits.cpu))) <= 0",message="requests.cpu must not exceed limits.cpu"
// +kubebuilder:validation:XValidation:rule="!has(self.requests) || !has(self.limits) || !has(self.requests.memory) || !has(self.limits.memory) || quantity(string(self.requests.memory)).compareTo(quantity(string(self.limits.memory))) <= 0",message="requests.memory must not exceed limits.memory"
type CategoryResources struct {
	// limits overrides the container resource limits.
	// +optional
	Limits *CategoryResourceList `json:"limits,omitempty"`

	// requests overrides the container resource requests.
	// +optional
	Requests *CategoryResourceList `json:"requests,omitempty"`
}

// CategoryOptions holds configuration for catalog workloads.
// Used as global defaults in CertificationSpec (embedded inline)
// and as per-category overrides in CertificateCategory.Options.
// Per-category values take precedence over globals. Nil means "use global"
// (or auto-select for nodesPerJob).
type CategoryOptions struct {
	// nodesPerJob is the number of nodes per job for multi-node workloads.
	// When nil at both global and per-category level, the controller auto-selects:
	//   - Entries with per-node-count configs (training): largest config <= matching nodes.
	//   - All other entries: all matching nodes.
	// When set, clamped to min(nodesPerJob, matchingNodes).
	// +optional
	// +kubebuilder:validation:Minimum=1
	NodesPerJob *int32 `json:"nodesPerJob,omitempty"`

	// enableCheckpoint enables checkpointing for training workloads.
	// When true, provisions a PVC for checkpoint storage, adds checkpoint config
	// (pvcName/maxRestarts), and enables --save/--load in training scripts.
	// When false (default), uses emptyDir and disables save/load.
	// Non-training entries ignore this field.
	// +optional
	EnableCheckpoint *bool `json:"enableCheckpoint,omitempty"`

	// maxSteps sets the maximum training steps. Maps to trainer.max_steps in the
	// NeMo config. Default: 50.
	// +optional
	// +kubebuilder:validation:Minimum=-1
	MaxSteps *int32 `json:"maxSteps,omitempty"`

	// exitDurationMins sets the training duration in minutes. Maps to
	// EXIT_DURATION_MINS env var. Default: 30.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ExitDurationMins *int32 `json:"exitDurationMins,omitempty"`

	// gpusPerNode optionally overrides the number of GPUs per node used by catalog
	// workloads. If not specified, the controller derives the default from the GPU
	// architecture in target.nodeSelector (e.g., 4 for GB200/GB300, 8 for H100).
	// Use this field when your hardware has a non-standard GPU count per node.
	// +optional
	// +kubebuilder:validation:Minimum=1
	GpusPerNode *int32 `json:"gpusPerNode,omitempty"`

	// mlnxPerNode overrides the auto-detected Mellanox NIC count per node.
	// Used by platforms with InfiniBand or RoCE networking (Azure, OCI, TogetherAI).
	// If not specified, derived from GPU architecture and platform via the
	// catalog's gpu-defaults.yaml (e.g., 8 for most architectures, 2 for OCI L40s).
	// +optional
	// +kubebuilder:validation:Minimum=0
	MlnxPerNode *int32 `json:"mlnxPerNode,omitempty"`

	// resources overrides the CPU and memory resources of training workload
	// containers. Training entries default to DGX-class sizing (limits:
	// cpu "128" / memory "800Gi"; requests: cpu "64" / memory "500Gi") — set
	// this on smaller nodes so training pods can schedule. Each of the four
	// values independently falls back to its default when unset. GPU count is
	// controlled by gpusPerNode. Non-training entries ignore this field.
	// Platform-specific catalog overrides (for example AWS with H100 GPUs)
	// may supersede these values.
	// +optional
	Resources *CategoryResources `json:"resources,omitempty"`

	// enableMNNVL enables Multi-Node NVLink (NCCL_MNNVL_ENABLE=1) for training
	// and communication workloads. Defaults to false (NCCL_MNNVL_ENABLE=0).
	// Enable this when running on platforms with multi-node NVLink connectivity
	// (e.g., GB300 NVL72). Can be overridden per-category via
	// categories[].options.enableMNNVL.
	// +optional
	EnableMNNVL *bool `json:"enableMNNVL,omitempty"`

	// imagePullSecrets is an optional list of references to secrets for pulling
	// container images used by catalog workloads. If not specified, the cluster's
	// default image pull configuration (e.g., ServiceAccount) is used.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// storageClassName is the StorageClass to use for PersistentVolumeClaim
	// dependencies created by catalog entries. If not specified, catalog entries
	// that require PVCs will use the cluster's default StorageClass.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// saveInterval sets the checkpoint save frequency in training steps.
	// NeMo 6: maps to --save-interval; NeMo 4: maps to every_n_train_steps.
	// Only used when enableCheckpoint is true. Default: 250.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SaveInterval *int32 `json:"saveInterval,omitempty"`

	// saveRetainInterval retains checkpoints at multiples of this value,
	// deleting intermediate checkpoints (NeMo 6 only, --save-retain-interval).
	// The most recent checkpoint is always kept regardless.
	// Only used when enableCheckpoint is true. Default: 1000.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SaveRetainInterval *int32 `json:"saveRetainInterval,omitempty"`

	// saveTopK keeps only the top K checkpoints by monitored metric
	// (NeMo 4 only, save_top_k). Older checkpoints that aren't the best
	// are deleted to save storage.
	// Only used when enableCheckpoint is true. Default: 1.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SaveTopK *int32 `json:"saveTopK,omitempty"`

	// storageSize sets the PVC size for checkpoint storage (e.g., "10Ti", "500Gi").
	// Must be a valid Kubernetes resource quantity. Only used when enableCheckpoint
	// is true. Default: "10Ti".
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?)(Ki|Mi|Gi|Ti|Pi|Ei)?$`
	StorageSize string `json:"storageSize,omitempty"`

	// maxRestarts sets the maximum number of checkpoint-based restarts for
	// training workloads. Maps to checkpoint.maxRestarts in the Job spec.
	// Only used when enableCheckpoint is true. Default: catalog-defined.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRestarts *int32 `json:"maxRestarts,omitempty"`

	// repeatCount sets the number of orchestration iterations for the Workflow.
	// Each iteration runs all groups and collects results. Multiple iterations
	// allow repeated testing for intermittent failures. Default: 1.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RepeatCount *int32 `json:"repeatCount,omitempty"`

	// testScale controls the orchestration strategy for NCCL communication tests.
	// Supported values:
	//   - "intra-node": each node tested independently (nodesPerJob=1)
	//   - "intra-rack": topology-partitioned per nvidia.com/gpu.clique
	//   - "diagnose": adaptive fault isolation (topology-aware hierarchical group testing)
	//   - "full-scale": all nodes in a single group (default)
	// Non-communication entries ignore this field.
	// +optional
	// +kubebuilder:validation:Enum=intra-node;intra-rack;diagnose;full-scale
	TestScale string `json:"testScale,omitempty"`

	// maxBytes sets the maximum message size for NCCL tests (e.g., "16G", "32G").
	// Maps to the NCCL perf test `-e` flag. Default: "16G" (GB200/GB300 override: "32G").
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(K|M|G|T)?$`
	MaxBytes string `json:"maxBytes,omitempty"`

	// numIterations sets the number of timed iterations per message size for NCCL tests.
	// Maps to the NCCL perf test `-n` flag. Default: 100.
	// +optional
	// +kubebuilder:validation:Minimum=1
	NumIterations *int32 `json:"numIterations,omitempty"`

	// numCycles sets the number of run cycles for NCCL tests. Each cycle runs
	// numIterations iterations and prints results separately.
	// Maps to the NCCL perf test `-N` flag. Default: 10.
	// +optional
	// +kubebuilder:validation:Minimum=1
	NumCycles *int32 `json:"numCycles,omitempty"`

	// thresholds defines performance thresholds as CEL expressions.
	// Keys are metric names (e.g., "busBandwidthGBps", "goodputRatio").
	// Values are CEL expressions using a `value` variable (float64).
	// Example: {"busBandwidthGBps": "value >= 900", "avgStepTimeSec": "value <= 3.0"}
	// +optional
	Thresholds map[string]string `json:"thresholds,omitempty"`

	// maxConcurrent limits the number of simultaneously running jobs within
	// a Workflow. Useful for diagnose-mode parallel screening to avoid
	// fabric saturation. 0 means unlimited. Default: 0.
	// +optional
	MaxConcurrent *int32 `json:"maxConcurrent,omitempty"`

	// minGroupSize sets the smallest group size at which diagnose's internal
	// bisection stops splitting. Groups at this size that fail become suspects.
	// Default: 2.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinGroupSize *int32 `json:"minGroupSize,omitempty"`

	// timeoutPerJob is the maximum time to wait for each job to complete.
	// Accepts Go duration strings (e.g., "1h", "30m", "2h30m").
	// When not set, defaults to "1h" (or "15m" for diagnose test scale).
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(h|m|s|ms))+$`
	TimeoutPerJob string `json:"timeoutPerJob,omitempty"`

	// measurementTimeout is the maximum time to wait after a Job succeeds for
	// measurement data (bandwidth, goodput, etc.) before failing threshold validation.
	// Accepts Go duration strings (e.g., "5m", "10m", "30m").
	// When not set, defaults to "5m".
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(h|m|s|ms))+$`
	MeasurementTimeout string `json:"measurementTimeout,omitempty"`
}

type CertificateCategory struct {
	// Domain is the high level Domain that the certificate belongs to like training, inference etc
	// +kubebuilder:validation:Required
	Domain string `json:"domain"`

	// Variant is the lower level type such as nemotron, deepseek, nccl etc.
	// +kubebuilder:validation:Required
	Variant string `json:"variant"`

	// options provides per-category configuration overrides.
	// Fields set here take precedence over their CertificationSpec counterparts.
	// +optional
	Options *CategoryOptions `json:"options,omitempty"`
}

// CertificationSpec defines the desired state of Certification
type CertificationSpec struct {
	// target specifies which nodes to include in the orchestration.
	// +kubebuilder:validation:Required
	Target TargetSpec `json:"target"`

	// Categories are the list of certificate categories required for the Target.
	// The MaxItems bound keeps the CEL rules nested under each category (the
	// resources request/limit comparison) within the API server's validation
	// cost budget; the catalog defines far fewer entries than 64.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Categories []CertificateCategory `json:"categories,omitempty"`

	// gangScheduler opts every category's workload pods into a gang-aware
	// scheduler such as KAI Scheduler. When set, schedulerName is injected into
	// every pod template of every category's resolved TrainingRuntime and the
	// queue label is applied, so the scheduler holds all pods until the full
	// gang can be placed. Applied after catalog and platform overrides resolve,
	// so it also replaces the scheduler a catalog entry hardcodes.
	// +optional
	GangScheduler *GangSchedulerSpec `json:"gangScheduler,omitempty"`

	// Global defaults for all categories. Per-category options override these.
	CategoryOptions `json:",inline"`
}

// CertificationStatus defines the observed state of Certification.
type CertificationStatus struct {
	// conditions represent the current state of the Certification resource.
	//
	// Condition types:
	// - "InProgress": the Certification is currently running
	// - "Succeeded": the Certification completed successfully
	// - "Failed": the Certification has failed
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// categoryStatuses tracks the status of each certification category.
	// +optional
	CategoryStatuses []CertificationCategoryStatus `json:"categoryStatuses,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Certification is the Schema for the certifications API
type Certification struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Certification.
	// The entire spec is immutable after creation: the controller never updates
	// an active Workflow from an edited parent, so accepting edits would either
	// silently ignore them or apply them only to later categories. To run with
	// different inputs, delete the Certification and create a new one.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable after creation"
	Spec CertificationSpec `json:"spec"`

	// status defines the observed state of Certification
	// +optional
	Status CertificationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CertificationList contains a list of Certification
type CertificationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Certification `json:"items"`
}

func init() {
	Register(&Certification{}, &CertificationList{})
}
