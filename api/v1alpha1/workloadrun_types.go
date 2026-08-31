// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadRun condition types. Only one of InProgress, Succeeded, or Failed
// can be True at any given time.
const (
	// WorkloadRunInProgress indicates the WorkloadRun is currently running.
	WorkloadRunInProgress = "InProgress"

	// WorkloadRunSucceeded indicates the WorkloadRun has completed successfully.
	WorkloadRunSucceeded = "Succeeded"

	// WorkloadRunFailed indicates the WorkloadRun has failed.
	WorkloadRunFailed = "Failed"

	// WorkloadRunValidationFailed indicates that one or more performance thresholds
	// were not met. This condition is independent of the execution state conditions.
	WorkloadRunValidationFailed = "ValidationFailed"
)

// FrameworkSpec is a discriminated union of workload framework types.
// Exactly one field must be set.
//
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:MaxProperties=1
type FrameworkSpec struct {
	// torch runs distributed training via torchrun.
	// Auto-generates a PyTorch TrainingRuntime with torch mlPolicy,
	// numProcPerNode, GPU resources, and shared memory.
	// +optional
	Torch *TorchFramework `json:"torch,omitempty"`

	// mpi runs MPI-based workloads (e.g., NCCL tests).
	// Auto-generates an MPI TrainingRuntime with launcher+worker pattern,
	// SSH auth, IPC_LOCK capability, and readiness probes.
	// +optional
	MPI *MPIFramework `json:"mpi,omitempty"`

	// exec runs an arbitrary command directly.
	// Auto-generates a simple TrainingRuntime with a single replicatedJob.
	// +optional
	Exec *ExecFramework `json:"exec,omitempty"`
}

// TorchFramework configures distributed training via torchrun.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.script) && has(self.module))",message="script and module are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.script) || has(self.module)",message="either script or module must be set"
type TorchFramework struct {
	// script is the path to run (torchrun <script>).
	// Mutually exclusive with module.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Script string `json:"script,omitempty"`

	// module is the Python module to run (torchrun -m <module>).
	// Mutually exclusive with script.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Module string `json:"module,omitempty"`

	// args are additional arguments passed to the script or module.
	// +optional
	Args []string `json:"args,omitempty"`
}

// MPIFramework configures MPI-based workloads.
type MPIFramework struct {
	// binary is the MPI binary to run (e.g., "/usr/local/bin/all_reduce_perf_mpi").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Binary string `json:"binary"`

	// args are arguments to the MPI binary (e.g., ["-b", "8", "-e", "32G"]).
	// +optional
	Args []string `json:"args,omitempty"`

	// mpiArgs are extra arguments to mpirun itself (e.g., ["--allow-run-as-root"]).
	// Standard args (--mca plm_rsh_args, -x NCCL vars) are auto-injected.
	// +optional
	MpiArgs []string `json:"mpiArgs,omitempty"`

	// mpiImplementation is the MPI implementation to use.
	// Default: "OpenMPI".
	// +optional
	// +kubebuilder:default="OpenMPI"
	MpiImplementation string `json:"mpiImplementation,omitempty"`

	// mpirunPath is the path to the mpirun binary in the container image.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	MpirunPath string `json:"mpirunPath"`
}

// ExecFramework configures arbitrary command execution.
type ExecFramework struct {
	// command is the entrypoint array (e.g., ["/bin/bash", "/config/train.sh"]).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Command []string `json:"command"`

	// args are arguments to the command.
	// +optional
	Args []string `json:"args,omitempty"`
}

// WorkloadConfig provides model configuration data to the workload.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.inline) && has(self.configMapRef))",message="inline and configMapRef are mutually exclusive"
type WorkloadConfig struct {
	// inline specifies config data as key-value pairs (filename -> content).
	// Creates a ConfigMap and mounts it at /config.
	// +optional
	Inline map[string]string `json:"inline,omitempty"`

	// configMapRef references an existing ConfigMap by name.
	// Mounted at /config.
	// +optional
	ConfigMapRef *corev1.LocalObjectReference `json:"configMapRef,omitempty"`
}

// WorkloadRunCheckpoint configures checkpoint storage for the workload.
type WorkloadRunCheckpoint struct {
	// storageSize is the PVC size (e.g., "10Ti", "500Gi").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?)(Ki|Mi|Gi|Ti|Pi|Ei)?$`
	StorageSize string `json:"storageSize"`

	// storageClassName is the StorageClass to use. Defaults to cluster default.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// maxRestarts is how many times to restart from checkpoint on failure.
	// Default: 0.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRestarts *int32 `json:"maxRestarts,omitempty"`
}

// WorkloadOrchestration controls how nodes are grouped and tested.
type WorkloadOrchestration struct {
	// testScale controls the node grouping strategy.
	//   - "intra-node": each node tested independently (numNodes=1 per job)
	//   - "intra-rack": one job per topology domain (nvidia.com/gpu.clique)
	//   - "full-scale": all nodes in a single group (default)
	// +optional
	// +kubebuilder:validation:Enum=intra-node;intra-rack;full-scale
	TestScale string `json:"testScale,omitempty"`

	// repeatCount runs the entire orchestration N times. Default: 1.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RepeatCount *int32 `json:"repeatCount,omitempty"`

	// maxConcurrent limits the number of simultaneously running jobs.
	// 0 means unlimited. Default: 0.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxConcurrent *int32 `json:"maxConcurrent,omitempty"`

	// timeoutPerJob is the maximum time to wait for each job to complete.
	// Accepts Go duration strings (e.g., "30m", "1h"). Default: "1h".
	// +optional
	TimeoutPerJob string `json:"timeoutPerJob,omitempty"`
}

// WorkloadRunSpec defines the desired state of WorkloadRun.
type WorkloadRunSpec struct {
	// target specifies which nodes to run the workload on.
	// When omitted, all GPU nodes (nvidia.com/gpu.present=true) are selected.
	// GPU architecture and CSP platform are auto-detected from target nodes.
	// +optional
	Target *TargetSpec `json:"target,omitempty"`

	// image is the container image for the workload.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// framework specifies how to run the workload.
	// Exactly one of torch, mpi, or exec must be set.
	// +kubebuilder:validation:Required
	Framework FrameworkSpec `json:"framework"`

	// numNodes is the number of nodes to use for the workload.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	NumNodes int32 `json:"numNodes"`

	// config provides model configuration data.
	// When inline is specified, a ConfigMap is created and mounted at /config.
	// When configMapRef is specified, the referenced ConfigMap is mounted at /config.
	// +optional
	Config *WorkloadConfig `json:"config,omitempty"`

	// gpusPerNode overrides the auto-detected GPU count per node.
	// If not specified, derived from GPU architecture (8 for H100/A100, 4 for GB200/GB300).
	// +optional
	// +kubebuilder:validation:Minimum=1
	GpusPerNode *int32 `json:"gpusPerNode,omitempty"`

	// mlnxPerNode overrides the auto-detected Mellanox NIC count per node.
	// Used by platforms with InfiniBand or RoCE networking (Azure, OCI, TogetherAI).
	// If not specified, derived from GPU architecture and platform via the
	// catalog's gpu-defaults.yaml.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MlnxPerNode *int32 `json:"mlnxPerNode,omitempty"`

	// imagePullSecrets references secrets for pulling the container image.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// env specifies additional environment variables for the workload container.
	// These are merged with auto-detected NCCL/platform env vars.
	// User-specified values take precedence over auto-detected values.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// volumes defines additional volumes for the workload pods.
	// A shared-memory volume (/dev/shm) is always auto-created.
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// volumeMounts defines additional volume mounts for the workload container.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// enableMNNVL explicitly controls Multi-Node NVLink (NCCL_MNNVL_ENABLE).
	// If nil, auto-detected from GPU architecture (enabled for GB200/GB300).
	// +optional
	EnableMNNVL *bool `json:"enableMNNVL,omitempty"`

	// checkpoint configures checkpoint storage. When set, provisions a PVC
	// and enables checkpoint-based restart.
	// +optional
	Checkpoint *WorkloadRunCheckpoint `json:"checkpoint,omitempty"`

	// resources overrides GPU/memory/CPU resource requests and limits for the
	// workload container. If not specified, GPU resources are auto-set from gpusPerNode.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// initContainers specifies init containers to run before the workload.
	// +optional
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// orchestration controls how nodes are grouped and tested.
	// +optional
	Orchestration *WorkloadOrchestration `json:"orchestration,omitempty"`

	// thresholds defines performance pass/fail criteria as CEL expressions.
	// Keys are metric names (e.g., "busBandwidthGBps", "avgTFLOPSPerGPU").
	// Values are CEL expressions using a `value` variable (e.g., "value >= 900").
	// +optional
	Thresholds map[string]string `json:"thresholds,omitempty"`

	// goodputMeasurement enables training goodput tracking.
	// When set, a GoodputMeasurement resource is auto-created for each Job.
	// +optional
	GoodputMeasurement *GoodputMeasurementConfig `json:"goodputMeasurement,omitempty"`

	// bandwidthMeasurement enables NCCL bandwidth tracking.
	// When set, a BandwidthMeasurement resource is auto-created for each Job.
	// +optional
	BandwidthMeasurement *BandwidthMeasurementConfig `json:"bandwidthMeasurement,omitempty"`

	// overrides allows advanced users to inject additional CSP/GPU-specific patches.
	// These are appended to the auto-generated platform overrides on the Workflow.
	// +optional
	Overrides []OverrideSpec `json:"overrides,omitempty"`

	// gangScheduler opts workload pods into a gang-aware scheduler such as KAI Scheduler.
	// When set, schedulerName is injected into every pod template and the queue label
	// is applied so the scheduler holds all pods until the full gang can be placed.
	// +optional
	GangScheduler *GangSchedulerSpec `json:"gangScheduler,omitempty"`
}

// GangSchedulerSpec configures gang scheduling for WorkloadRun pods.
type GangSchedulerSpec struct {
	// schedulerName is the name of the gang-aware scheduler to use (e.g. "kai-scheduler").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SchedulerName string `json:"schedulerName"`

	// queue is the scheduler queue to submit the workload to.
	// Defaults to "default-queue" if not specified.
	// When non-empty, must be a valid Kubernetes label value: at most 63 characters,
	// beginning and ending with an alphanumeric character, and containing only
	// alphanumerics, hyphens, underscores, or dots.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^$|^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`
	Queue string `json:"queue,omitempty"`
}

// WorkloadRunStatus defines the observed state of WorkloadRun.
type WorkloadRunStatus struct {
	// conditions represent the current state of the WorkloadRun.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// workflowRef references the Workflow created by this WorkloadRun.
	// +optional
	WorkflowRef *WorkflowReference `json:"workflowRef,omitempty"`

	// detectedPlatform is the auto-detected CSP platform (e.g., "aws", "gcp", "azure").
	// +optional
	DetectedPlatform string `json:"detectedPlatform,omitempty"`

	// detectedGPUArchitecture is the auto-detected GPU type (e.g., "h100", "gb200").
	// +optional
	DetectedGPUArchitecture string `json:"detectedGPUArchitecture,omitempty"`

	// resolvedGpusPerNode is the final GPU count per node used for the workload.
	// +optional
	ResolvedGpusPerNode int32 `json:"resolvedGpusPerNode,omitempty"`

	// succeededNodesRef references the ConfigMap holding the underlying Workflow's
	// succeeded-nodes list (comma-separated node names).
	// +optional
	SucceededNodesRef *corev1.TypedLocalObjectReference `json:"succeededNodesRef,omitempty"`

	// failedNodesRef references the ConfigMap holding the underlying Workflow's
	// failed-nodes list (name, reason, message).
	// +optional
	FailedNodesRef *corev1.TypedLocalObjectReference `json:"failedNodesRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// WorkloadRun is the Schema for the workloadruns API.
// It provides a simplified interface for running training, MPI, or arbitrary
// workloads on GPU clusters with automatic CSP and GPU architecture adaptation.
type WorkloadRun struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WorkloadRun.
	// The entire spec is immutable after creation: once status.workflowRef is
	// set the controller only mirrors the existing Workflow and never rebuilds
	// it, so accepting edits would silently ignore them. To run with different
	// inputs, delete the WorkloadRun and create a new one.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable after creation"
	Spec WorkloadRunSpec `json:"spec"`

	// status defines the observed state of WorkloadRun
	// +optional
	Status WorkloadRunStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkloadRunList contains a list of WorkloadRun
type WorkloadRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkloadRun `json:"items"`
}

func init() {
	Register(&WorkloadRun{}, &WorkloadRunList{})
}
