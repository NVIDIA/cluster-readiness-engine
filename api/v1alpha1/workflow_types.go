// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Workflow condition types. Only one of InProgress, Succeeded, or Failed
// can be True at any given time.
const (
	// WorkflowInProgress indicates the Workflow is currently running.
	WorkflowInProgress = "InProgress"

	// WorkflowSucceeded indicates the Workflow has completed successfully.
	WorkflowSucceeded = "Succeeded"

	// WorkflowFailed indicates the Workflow has failed.
	WorkflowFailed = "Failed"

	// WorkflowValidationFailed indicates that one or more Jobs had performance
	// threshold violations. This condition is independent of the execution state
	// conditions (InProgress/Succeeded/Failed) and provides a quality signal
	// that can be aggregated up to the Certification level.
	WorkflowValidationFailed = "ValidationFailed"
)

// GroupPhase represents the phase of a scheduling group's job.
type GroupPhase string

const (
	GroupPending   GroupPhase = "Pending"
	GroupRunning   GroupPhase = "Running"
	GroupSucceeded GroupPhase = "Succeeded"
	GroupFailed    GroupPhase = "Failed"
)

// DependencyResourceRef tracks a dependency resource created by a Workflow.
type DependencyResourceRef struct {
	// apiVersion of the created resource.
	APIVersion string `json:"apiVersion"`

	// kind of the created resource.
	Kind string `json:"kind"`

	// name of the created resource.
	Name string `json:"name"`

	// namespace of the created resource. Empty for cluster-scoped resources.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// scope is the lifecycle scope of this dependency (workflow, job).
	// +optional
	Scope string `json:"scope,omitempty"`

	// groupName identifies the group this dependency was created for (scope=job).
	// +optional
	GroupName string `json:"groupName,omitempty"`

	// iteration identifies the iteration this dependency was created for (scope=job).
	// +optional
	Iteration int `json:"iteration,omitempty"`
}

// JobTemplateSpec defines the template for Jobs created by a Workflow.
type JobTemplateSpec struct {
	// metadata is standard object metadata applied to created Jobs.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the Job specification used as a template for each execution group.
	// +kubebuilder:validation:Required
	Spec JobSpec `json:"spec"`
}

// OrchestrationSpec defines topology-aware orchestration strategy.
type OrchestrationSpec struct {
	// target specifies which nodes to include in the orchestration.
	// When nil, all nodes in the cluster are targeted.
	// +optional
	Target *TargetSpec `json:"target,omitempty"`

	// topology configures rack/zone-aware placement.
	// When set, jobs are packed into the minimum number of topology domains.
	// When nil, nodes are grouped by simple chunking (no topology awareness).
	// +optional
	Topology *TopologySpec `json:"topology,omitempty"`

	// diagnose enables adaptive fault isolation using topology-aware hierarchical
	// group testing. Stage 1a screens each topology domain in parallel, Stage 1b
	// tests inter-domain fabric, Stage 2 bisects failed groups, Stage 3 confirms
	// suspects. Identifies all faulty nodes in O(d log N) tests.
	// +optional
	Diagnose *DiagnoseSpec `json:"diagnose,omitempty"`

	// execution defines how jobs are scheduled across groups.
	// +optional
	Execution ExecutionSpec `json:"execution,omitempty"`

	// iterations is the number of times to run all jobs (one full pass = one iteration).
	// Default is 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	Iterations int `json:"iterations,omitempty"`
}

// TargetSpec specifies which nodes to include in orchestration.
type TargetSpec struct {
	// nodeSelector selects nodes by label key-value pairs.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// matchExpressions is a list of node selector requirements.
	// Supports In, NotIn, Exists, DoesNotExist operators.
	// All expressions must match (AND with nodeSelector).
	// +optional
	MatchExpressions []corev1.NodeSelectorRequirement `json:"matchExpressions,omitempty"`

	// nodeNames explicitly lists nodes to include.
	// If specified, nodeSelector is ignored.
	// +optional
	NodeNames []string `json:"nodeNames,omitempty"`

	// taintSelectors selects nodes that have ALL matching taints.
	// A node must have every taint in this list to be included.
	//
	// The controller auto-injects matching tolerations onto the workload pods so
	// they can schedule onto the selected tainted nodes. Setting this field is
	// the supported way to run training workloads (e.g. TrainJob) on tainted
	// nodes. When taintSelectors is set, the explicit tolerations replace the
	// blanket Operator: Exists toleration that MPI workloads (NCCL tests)
	// otherwise receive — declared selectors always win.
	// +optional
	TaintSelectors []TaintSelector `json:"taintSelectors,omitempty"`
}

// TaintSelector selects nodes by taint.
type TaintSelector struct {
	// key is the taint key to match.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// value is the taint value to match. If empty, matches any value.
	// +optional
	Value string `json:"value,omitempty"`

	// effect is the taint effect to match (NoSchedule, PreferNoSchedule, NoExecute).
	// If empty, matches any effect.
	// +optional
	Effect corev1.TaintEffect `json:"effect,omitempty"`
}

// TopologySpec configures topology-aware placement.
type TopologySpec struct {
	// topologyKey is the node label key that identifies the topology domain.
	// Examples: "nvidia.com/gpu.clique", "topology.kubernetes.io/zone"
	// Nodes with the same label value are in the same domain (rack/zone).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	TopologyKey string `json:"topologyKey"`

	// strictDomain creates one group per topology domain at its natural size.
	// Groups never cross domain boundaries. Set by testScale=intra-rack.
	// +optional
	StrictDomain bool `json:"strictDomain,omitempty"`
}

// DiagnoseSpec configures adaptive fault isolation using topology-aware
// hierarchical group testing. The algorithm runs three stages: intra-domain
// screening, inter-domain screening, bisection, and confirmation.
type DiagnoseSpec struct {
	// minGroupSize is the smallest group at which bisection stops.
	// Nodes in failed groups of this size become suspects for confirmation.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	// +optional
	MinGroupSize int `json:"minGroupSize,omitempty"`

	// topologyKey is the node label key for topology-aware screening.
	// When set, Stage 1 groups nodes by this label (e.g., one group per clique).
	// When empty, nodes are grouped into disjoint chunks of ceil(sqrt(N)).
	// Example: "nvidia.com/gpu.clique"
	// +optional
	TopologyKey string `json:"topologyKey,omitempty"`
}

// ExecutionSpec defines how jobs are scheduled across groups.
type ExecutionSpec struct {
	// maxConcurrent is the maximum number of jobs to run concurrently.
	// 0 means unlimited. Default is 0.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	MaxConcurrent int `json:"maxConcurrent,omitempty"`

	// timeoutPerJob is the maximum time to wait for each job to complete.
	// +optional
	TimeoutPerJob *metav1.Duration `json:"timeoutPerJob,omitempty"`

	// retryFailedGroups is the number of times to retry a failed group's job.
	// After the workload controller exhausts its own restarts (backoffLimit),
	// the orchestration re-creates the entire job up to this many times.
	// 0 means no retries. Default is 0.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	RetryFailedGroups int `json:"retryFailedGroups,omitempty"`
}

// DependencySpec defines a resource that must be created before the workload.
type DependencySpec struct {
	// Resource definition (inline, any Kubernetes resource)
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:EmbeddedResource
	runtime.RawExtension `json:",inline"`

	// when defines conditions for creating this dependency.
	// If not specified, the dependency is always created.
	// +optional
	When WhenSpec `json:"when,omitempty"`
}

// WhenSpec defines conditions for conditional resource creation.
type WhenSpec struct {
	// gpuArchitecture matches against detected GPU architecture.
	// +optional
	GPUArchitecture *StringMatchSpec `json:"gpuArchitecture,omitempty"`

	// platform matches against detected cloud platform.
	// +optional
	Platform *StringMatchSpec `json:"platform,omitempty"`

	// workloadKind matches against the workload kind.
	// Example: "TrainJob"
	// +optional
	WorkloadKind string `json:"workloadKind,omitempty"`

	// topology matches against cluster topology characteristics.
	// +optional
	Topology *TopologyMatchSpec `json:"topology,omitempty"`

	// config matches against values in validation.performance.config.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// expression is a CEL expression that evaluates to a boolean.
	// Available variables: gpuArchitecture, platform, workloadKind, topology, config
	// Example: "gpuArchitecture == 'gb200' && platform == 'aws'"
	// +optional
	Expression string `json:"expression,omitempty"`
}

// StringMatchSpec defines string matching criteria.
type StringMatchSpec struct {
	// equals checks for exact match.
	// +optional
	Equals string `json:"equals,omitempty"`

	// in checks if value is in the list.
	// +optional
	In []string `json:"in,omitempty"`

	// notIn checks if value is not in the list.
	// +optional
	NotIn []string `json:"notIn,omitempty"`
}

// TopologyMatchSpec defines topology matching criteria.
type TopologyMatchSpec struct {
	// mode matches against topology mode.
	// Examples: "single-rack", "multi-rack", "multi-zone"
	// +optional
	Mode string `json:"mode,omitempty"`

	// domainCount matches against the number of topology domains (racks, zones, etc.).
	// +optional
	DomainCount *IntMatchSpec `json:"domainCount,omitempty"`
}

// IntMatchSpec defines integer matching criteria.
type IntMatchSpec struct {
	// equals checks for exact match.
	// +optional
	Equals *int `json:"equals,omitempty"`

	// greaterThan checks if value is greater than threshold.
	// +optional
	GreaterThan *int `json:"greaterThan,omitempty"`

	// lessThan checks if value is less than threshold.
	// +optional
	LessThan *int `json:"lessThan,omitempty"`
}

// ValidationSpec defines performance validation and metrics collection.
type ValidationSpec struct {
	// performance configures performance validation and metrics collection.
	// +optional
	Performance *PerformanceValidationSpec `json:"performance,omitempty"`
}

// PerformanceValidationSpec defines performance validation configuration.
type PerformanceValidationSpec struct {
	// enabled determines if performance validation is active.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// plugin is the name of the benchmark plugin to use.
	// Examples: "nemo-training", "nccl-tests"
	// +optional
	Plugin string `json:"plugin,omitempty"`

	// config contains plugin-specific configuration parameters.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// tracking defines what results to track.
	// +optional
	Tracking TrackingSpec `json:"tracking,omitempty"`

	// thresholds defines minimum acceptable performance thresholds.
	// When set, the Workflow controller propagates these to each Job's
	// measurement configs during creation. The Job controller evaluates
	// them after the workload succeeds and sets the ValidationFailed
	// condition if any threshold is not met.
	// +optional
	Thresholds *ThresholdSpec `json:"thresholds,omitempty"`

	// measurementTimeout is the maximum time to wait after a Job succeeds for
	// measurement data before failing threshold validation. Propagated to each
	// Job's spec during creation.
	// +optional
	MeasurementTimeout *metav1.Duration `json:"measurementTimeout,omitempty"`
}

// ThresholdSpec defines performance thresholds as CEL expressions.
// These are propagated by the Workflow controller to each Job's measurement
// configs during Job creation. The Job controller evaluates them after
// workload success and sets the ValidationFailed condition if any threshold
// is violated.
//
// Keys are standardized metric names (e.g., "busBandwidthGBps", "goodputRatio").
// Values are CEL expressions using a `value` variable (float64).
// Example: {"busBandwidthGBps": "value >= 900", "avgStepTimeSec": "value <= 3.0"}
type ThresholdSpec struct {
	// thresholds maps metric names to CEL expressions.
	// +optional
	Thresholds map[string]string `json:"thresholds,omitempty"`
}

// TrackingSpec defines what results to track.
type TrackingSpec struct {
	// perNode tracks results for each individual node.
	// +kubebuilder:default=false
	// +optional
	PerNode bool `json:"perNode,omitempty"`

	// perDomain tracks aggregated results per topology domain.
	// Domains are defined by the topology key in orchestration.
	// For example, if using "nvidia.com/gpu.clique", results
	// are aggregated per unique clique value.
	// +kubebuilder:default=false
	// +optional
	PerDomain bool `json:"perDomain,omitempty"`
}

// AppliedOverride records that a specific override was applied during reconciliation.
type AppliedOverride struct {
	// index is the zero-based position of the override in spec.overrides[].
	Index int `json:"index"`

	// when is a human-readable summary of the conditions that matched.
	// Example: "gpuArchitecture=h100, platform=aws"
	When string `json:"when"`

	// patches describes what the override modifies.
	// Example: "jobTemplate, 2 dependencies"
	Patches string `json:"patches"`

	// noOp is true when the override matched but produced no changes.
	// This usually indicates a misconfigured override with wrong field paths.
	// +optional
	NoOp bool `json:"noOp,omitempty"`
}

// IterationResult records the outcome of a completed iteration.
type IterationResult struct {
	// iteration is the 1-based iteration number.
	Iteration int `json:"iteration"`
	// groups contains the outcome of each group in this iteration.
	Groups []GroupIterationResult `json:"groups"`
}

// GroupIterationResult records one group's outcome for a single iteration.
type GroupIterationResult struct {
	// name is the group identifier (matches GroupStatus.Name).
	Name string `json:"name"`
	// phase is the terminal phase (Succeeded or Failed).
	Phase GroupPhase `json:"phase"`
	// jobName is the Job that ran for this group in this iteration.
	// Empty if the Job was deleted before the snapshot.
	// +optional
	JobName string `json:"jobName,omitempty"`
	// startTime when the group's job started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// completionTime when the group's job finished.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// OrchestrationStatus tracks the state of orchestration execution.
type OrchestrationStatus struct {
	// totalNodes discovered from target.
	TotalNodes int `json:"totalNodes"`

	// nodesPerJob auto-detected from workload template.
	NodesPerJob int `json:"nodesPerJob"`

	// totalGroups created.
	TotalGroups int `json:"totalGroups"`

	// currentIteration is the iteration currently in progress (1-based).
	// +optional
	CurrentIteration int `json:"currentIteration,omitempty"`

	// completedIterations is the number of iterations fully completed.
	// +optional
	CompletedIterations int `json:"completedIterations,omitempty"`

	// iterationHistory records per-group outcomes for each completed iteration.
	// Appended when an iteration completes and more iterations remain.
	// The final iteration's results stay in Groups (not duplicated here).
	// +optional
	IterationHistory []IterationResult `json:"iterationHistory,omitempty"`

	// detectedPlatform is the cloud platform detected from node labels.
	// Used by WhenSpec for conditional dependency creation.
	// Examples: "aws", "gcp", "onprem", "unknown"
	// +optional
	DetectedPlatform string `json:"detectedPlatform,omitempty"`

	// detectedGPUArchitecture is the GPU architecture detected from node labels.
	// Examples: "h100", "gb200", "a100", "l40s", "unknown"
	// +optional
	DetectedGPUArchitecture string `json:"detectedGPUArchitecture,omitempty"`

	// excludedNodes lists nodes that matched the target but were dropped before
	// scheduling, and exclusionReason says why. A Workflow can succeed while
	// excluding nodes, so these record what was left untested. Today the only
	// cause is a target set with more than one GPU architecture, where the run
	// continues on the primary architecture alone.
	// +optional
	ExcludedNodes []string `json:"excludedNodes,omitempty"`

	// exclusionReason explains why excludedNodes were dropped.
	// +optional
	ExclusionReason string `json:"exclusionReason,omitempty"`

	// appliedOverrides records which overrides from spec.overrides[] were applied
	// after platform and GPU architecture detection completed.
	// +optional
	AppliedOverrides []AppliedOverride `json:"appliedOverrides,omitempty"`

	// diagnose tracks adaptive fault isolation algorithm progress.
	// +optional
	Diagnose *DiagnoseStatus `json:"diagnose,omitempty"`

	// Group phases are reset to Pending at the start of each new iteration.
	// +optional
	Groups []GroupStatus `json:"groups,omitempty"`
}

// DiagnoseStatus tracks the adaptive fault isolation algorithm's progress.
type DiagnoseStatus struct {
	// stage is the current stage of the diagnose algorithm.
	// Values: "intra-screening", "inter-screening", "bisection", "confirmation"
	Stage string `json:"stage"`

	// round is the total number of completed rounds across all stages.
	Round int `json:"round"`

	// healthyNodes are nodes confirmed healthy at any stage (accumulated).
	// +listType=set
	// +optional
	HealthyNodes []string `json:"healthyNodes,omitempty"`

	// suspectNodes are nodes in minGroupSize groups that failed bisection,
	// pending confirmation in Stage 3.
	// +listType=set
	// +optional
	SuspectNodes []string `json:"suspectNodes,omitempty"`

	// noNVLSuspectNodes are suspect nodes that originated from the
	// intra-screening-no-nvl stage. Confirmation tests for these nodes
	// run with NCCL_MNNVL_ENABLE=0 to isolate fabric issues.
	// +listType=set
	// +optional
	NoNVLSuspectNodes []string `json:"noNVLSuspectNodes,omitempty"`

	// representativeNodes are one healthy node per topology domain, selected
	// after intra-domain screening for inter-domain fabric testing.
	// +optional
	RepresentativeNodes []string `json:"representativeNodes,omitempty"`

	// screeningResults preserves per-domain screening outcomes across stages.
	// Key is the domain name (e.g., clique UUID), value is the domain's screening result.
	// +optional
	ScreeningResults map[string]DomainScreeningResult `json:"screeningResults,omitempty"`

	// noNVLScreeningResults preserves per-domain screening outcomes from the
	// intra-screening-no-nvl stage (MNNVL disabled). Same structure as screeningResults.
	// +optional
	NoNVLScreeningResults map[string]DomainScreeningResult `json:"noNVLScreeningResults,omitempty"`

	// infrastructureFaults records detected infrastructure-level faults where
	// bisection both-halves-pass but the full group fails. Each entry identifies
	// the node groups whose interconnecting infrastructure is suspected faulty.
	// +optional
	InfrastructureFaults []InfrastructureFault `json:"infrastructureFaults,omitempty"`

	// crossBoundaryState tracks in-progress cross-boundary probing.
	// Nil when not in cross-boundary stage.
	// +optional
	CrossBoundaryState *CrossBoundaryState `json:"crossBoundaryState,omitempty"`
}

// DomainScreeningResult records the outcome of screening one topology domain.
type DomainScreeningResult struct {
	// nodes in this domain.
	Nodes []string `json:"nodes"`
	// passed is true if the screening test succeeded.
	Passed bool `json:"passed"`
}

// InfrastructureFault records a detected infrastructure-level fault between
// two node groups in the same topology domain. The fault is in the interconnect
// (NVSwitch trunk, spine switch, cable) rather than in individual nodes.
type InfrastructureFault struct {
	// domain is the topology domain where the fault was detected.
	Domain string `json:"domain"`
	// groupA is one set of nodes on one side of the faulty boundary.
	GroupA []string `json:"groupA"`
	// groupB is the other set of nodes.
	GroupB []string `json:"groupB"`
	// stage is the diagnose stage where the fault was detected.
	Stage string `json:"stage"`
}

// CrossBoundaryState tracks in-progress cross-boundary probing for
// infrastructure faults (both-halves-pass pattern in bisection).
type CrossBoundaryState struct {
	// pendingProbes are boundary investigations still in progress.
	// +optional
	PendingProbes []CrossBoundaryProbe `json:"pendingProbes,omitempty"`
	// originStage records which diagnose stage triggered the cross-boundary
	// probing, used to determine MNNVL settings for probe jobs.
	OriginStage string `json:"originStage"`
}

// CrossBoundaryProbe tracks one cross-boundary investigation between
// two halves of a bisection group that both passed individually.
type CrossBoundaryProbe struct {
	// domain is the topology domain being investigated.
	Domain string `json:"domain"`
	// halfA is the first half of the bisection that passed.
	HalfA []string `json:"halfA"`
	// halfB is the second half of the bisection that passed.
	HalfB []string `json:"halfB"`
	// probeRound is the current probe round within this investigation.
	ProbeRound int `json:"probeRound"`
}

// Diagnose stage constants.
const (
	DiagnoseStageIntraScreening      = "intra-screening"
	DiagnoseStageIntraScreeningNoNVL = "intra-screening-no-nvl"
	DiagnoseStageInterScreening      = "inter-screening"
	DiagnoseStageBisection           = "bisection"
	DiagnoseStageConfirmation        = "confirmation"
	DiagnoseStageCrossBoundary       = "cross-boundary"
	DiagnoseStageComplete            = "complete"
)

// GroupStatus tracks the state of a scheduling group's job within the current iteration.
type GroupStatus struct {
	// name is a unique identifier (e.g., "group-0", "group-1-overflow").
	Name string `json:"name"`

	// nodes in this group.
	Nodes []string `json:"nodes"`

	// domains lists topology domain values this group spans.
	// +optional
	Domains []string `json:"domains,omitempty"`

	// domainNodeCounts records the number of nodes per topology domain in
	// this group, keyed by domain value. Populated at partition time so
	// reports can show per-clique node counts without fetching nodes.
	// +optional
	DomainNodeCounts map[string]int `json:"domainNodeCounts,omitempty"`

	// overflow indicates this group contains borrowed nodes and must wait
	// for the jobs using those nodes to complete before launching.
	// +optional
	Overflow bool `json:"overflow,omitempty"`

	// phase is the current phase of this group's job in the current iteration.
	// Reset to Pending at the start of each iteration.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
	Phase GroupPhase `json:"phase"`

	// retries is the number of times the current iteration's job has been retried.
	// Reset to 0 at the start of each iteration.
	// +optional
	Retries int `json:"retries,omitempty"`

	// jobRef references the current job for this group.
	// +optional
	JobRef *WorkloadReference `json:"jobRef,omitempty"`

	// startTime when this group's job started in the current iteration.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime when this group's job finished in the current iteration.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// NodeResult contains performance metrics for a single node.
type NodeResult struct {
	// domain is the topology domain this node belongs to.
	// For example, if using "nvidia.com/gpu.clique", this is the clique value.
	Domain string `json:"domain"`

	// group is the group name that ran on this node.
	Group string `json:"group"`

	// status is the validation status for this node.
	// Examples: "Passed", "Failed", "Running"
	Status string `json:"status"`

	// iterations contains metrics for each iteration on this node.
	// +optional
	Iterations []IterationMetrics `json:"iterations,omitempty"`
}

// IterationMetrics contains performance metrics for one iteration.
type IterationMetrics struct {
	// iterationNumber is the iteration index (1-based).
	IterationNumber int `json:"iterationNumber"`

	// tflops is the measured TFLOPs performance as a string.
	// Example: "1500.5"
	// +optional
	TFLOPs string `json:"tflops,omitempty"`

	// duration is the iteration duration in seconds as a string.
	// Example: "3600.5"
	// +optional
	Duration string `json:"duration,omitempty"`

	// memoryUsedGB is the peak GPU memory usage in GB as a string.
	// Example: "640.25"
	// +optional
	MemoryUsedGB string `json:"memoryUsedGB,omitempty"`

	// status is the status of this iteration.
	// Examples: "Succeeded", "Failed"
	Status string `json:"status"`
}

// DomainResult contains aggregated metrics for a topology domain.
type DomainResult struct {
	// nodeCount is the number of nodes in this domain.
	NodeCount int `json:"nodeCount"`

	// status is the overall status for this domain.
	// Examples: "Passed", "Failed", "Running"
	Status string `json:"status"`

	// averageTFLOPs is the average TFLOPs across all nodes in the domain as a string.
	// Example: "1500.5"
	// +optional
	AverageTFLOPs string `json:"averageTflops,omitempty"`
}

// OrchestrationOverrideSpec defines optional overrides for orchestration settings.
// All fields are pointers — nil means "don't change the base value."
type OrchestrationOverrideSpec struct {
	// target overrides which nodes to include in the orchestration.
	// +optional
	Target *TargetSpec `json:"target,omitempty"`

	// topology overrides rack/zone-aware placement configuration.
	// +optional
	Topology *TopologySpec `json:"topology,omitempty"`

	// execution overrides how jobs are scheduled across groups.
	// +optional
	Execution *ExecutionSpec `json:"execution,omitempty"`

	// iterations overrides the number of times to run all jobs.
	// +optional
	Iterations *int `json:"iterations,omitempty"`
}

// OverrideSpec defines a conditional patch to the base WorkflowSpec.
type OverrideSpec struct {
	// when defines conditions for applying this override.
	// +kubebuilder:validation:Required
	When WhenSpec `json:"when"`

	// jobTemplate is a strategic merge patch applied to the base jobTemplate.
	// Maps merge recursively, named arrays (e.g., env vars) merge by key,
	// unnamed arrays (e.g., args) replace entirely.
	// Stored as raw JSON to avoid CRD validation cost budget issues.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	JobTemplate *apiextensionsv1.JSON `json:"jobTemplate,omitempty"`

	// jobTemplatePatch is an RFC 6902 JSON Patch applied to the base jobTemplate.
	// Use for precise operations: remove specific env vars, add at index,
	// test a value before patching (preconditions).
	// Applied AFTER the jobTemplate strategic merge patch.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	JobTemplatePatch *apiextensionsv1.JSON `json:"jobTemplatePatch,omitempty"`

	// dependencies are additional dependencies appended when this override matches.
	// +optional
	Dependencies []DependencySpec `json:"dependencies,omitempty"`

	// orchestration overrides orchestration settings (target, topology, execution, iterations).
	// Non-nil fields replace the base value; nil fields are preserved.
	// +optional
	Orchestration *OrchestrationOverrideSpec `json:"orchestration,omitempty"`
}

// WorkflowSpec defines the desired state of Workflow.
type WorkflowSpec struct {
	// namespace is the target namespace for Jobs and dependencies created by this Workflow.
	// If not specified, the controller auto-generates one.
	// The controller creates the namespace if it doesn't exist and sets an owner reference
	// so it's cleaned up when the Workflow is deleted.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// jobTemplate defines the Job template used to create Jobs for each execution group.
	// +kubebuilder:validation:Required
	JobTemplate JobTemplateSpec `json:"jobTemplate"`

	// orchestration defines topology-aware orchestration strategy.
	// +kubebuilder:validation:Required
	Orchestration OrchestrationSpec `json:"orchestration"`

	// dependencies are resources required for the workload (ComputeDomain, etc.)
	// Resources are conditionally created based on "when" clauses.
	// +optional
	Dependencies []DependencySpec `json:"dependencies,omitempty"`

	// overrides defines conditional patches to the base spec.
	// Overrides are evaluated in order; all matching overrides are applied sequentially.
	// +optional
	Overrides []OverrideSpec `json:"overrides,omitempty"`

	// validation defines performance validation and metrics collection.
	// +optional
	Validation *ValidationSpec `json:"validation,omitempty"`
}

// WorkflowStatus defines the observed state of Workflow.
type WorkflowStatus struct {
	// conditions represent the current state of the Workflow resource.
	//
	// Condition types:
	// - "InProgress": the Workflow is currently running
	// - "Succeeded": the Workflow completed successfully
	// - "Failed": the Workflow has failed
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// namespace is the resolved namespace where Jobs and dependencies are created.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// succeededNodesRef references the ConfigMap holding this Workflow's
	// succeeded-nodes list (comma-separated node names). It is written on terminal
	// success and on terminal failure (capturing the nodes that passed).
	// +optional
	SucceededNodesRef *corev1.TypedLocalObjectReference `json:"succeededNodesRef,omitempty"`

	// failedNodesRef references the ConfigMap holding this Workflow's failed-nodes
	// list (name, reason, message). It is written incrementally as Jobs
	// fail.
	// +optional
	FailedNodesRef *corev1.TypedLocalObjectReference `json:"failedNodesRef,omitempty"`

	// dependencyRefs lists the dependency resources created by this Workflow.
	// +optional
	DependencyRefs []DependencyResourceRef `json:"dependencyRefs,omitempty"`

	// orchestration contains the state of orchestration execution.
	// +optional
	Orchestration *OrchestrationStatus `json:"orchestration,omitempty"`

	// nodeResults contains per-node performance metrics.
	// Only populated when spec.validation.performance.tracking.perNode is true.
	// +optional
	NodeResults map[string]NodeResult `json:"nodeResults,omitempty"`

	// domainResults contains aggregated metrics grouped by topology domain values.
	// Key is the domain value (e.g., "clique-0", "rack-1", "zone-a").
	// Only populated when spec.validation.performance.tracking.perDomain is true.
	// +optional
	DomainResults map[string]DomainResult `json:"domainResults,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Workflow is the Schema for the workflows API
type Workflow struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Workflow
	// +required
	Spec WorkflowSpec `json:"spec"`

	// status defines the observed state of Workflow
	// +optional
	Status WorkflowStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkflowList contains a list of Workflow
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Workflow `json:"items"`
}

func init() {
	Register(&Workflow{}, &WorkflowList{})
}
