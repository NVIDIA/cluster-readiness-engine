// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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
	// +kubebuilder:validation:Pattern='^[0-9]+(K|M|G|T)?$'
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
	TimeoutPerJob string `json:"timeoutPerJob,omitempty"`

	// measurementTimeout is the maximum time to wait after a Job succeeds for
	// measurement data (bandwidth, goodput, etc.) before failing threshold validation.
	// Accepts Go duration strings (e.g., "5m", "10m", "30m").
	// When not set, defaults to "5m".
	// +optional
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

	// Categories are the list of certificate categories required for the Target
	// +kubebuilder:validation:Required
	Categories []CertificateCategory `json:"categories,omitempty"`

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

	// spec defines the desired state of Certification
	// +required
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
