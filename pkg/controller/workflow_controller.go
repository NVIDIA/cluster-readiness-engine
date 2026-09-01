// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleropts "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/naming"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/orchestration"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/threshold"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

const (
	workflowFinalizer              = "nvcre.nvidia.com/workflow-finalizer"
	defaultWorkflowRequeueInterval = 15 * time.Second

	// Workflow tier reason constants are in helpers.go.
)

// WorkflowReconciler reconciles a Workflow object
type WorkflowReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	Clientset          *kubernetes.Clientset
	Recorder           events.EventRecorder
	JobRequeueInterval time.Duration
	// MaxConcurrentReconciles bounds the number of Workflow objects reconciled concurrently.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=workflows/finalizers,verbs=update
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=jobs/status,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//
// Pods are read for the timeout failure-log capture and for the pod-drain
// barrier (shouldWaitForPodDrain) that gates scoped-dependency cleanup.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;patch;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//
// Event emission. controller-runtime's recorder writes through both the legacy
// core/v1 Event API and events.k8s.io/v1, so both are granted. These were
// previously covered only by a wildcard grant; without them the recorder fails
// silently and Workflow/Job events never appear in `kubectl describe`.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
//
// Dependency resources. The Workflow controller materialises DependencySpec
// entries as unstructured objects, so these grants define the complete set of
// kinds a Workflow may create. They are enumerated rather than wildcarded: a
// wildcard here is cluster-admin, which is not an acceptable posture for an
// operator running on a shared GPU cluster.
//
// Adding a new dependency kind to pkg/catalog/entries/_lib/deps/ or
// pkg/platform/overrides/ requires a matching grant below, otherwise dependency
// creation fails with a Forbidden error surfaced on the Workflow's
// DependencyCreationError condition.
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;create;delete
// +kubebuilder:rbac:groups=trainer.kubeflow.org,resources=trainingruntimes,verbs=get;list;create;update;patch;delete
// +kubebuilder:rbac:groups=trainer.kubeflow.org,resources=trainjobs,verbs=get;list;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.nvidia.com,resources=computedomains,verbs=get;list;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	workflow := &nvcrev1alpha1.Workflow{}
	if err := r.Get(ctx, req.NamespacedName, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Workflow resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Workflow: %w", err)
	}

	// Handle deletion
	if !workflow.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, workflow)
	}

	// Add finalizer if not present. A successful add ends this reconcile; the
	// resulting watch event drives the next one.
	if added, err := ensureFinalizer(ctx, r.Client, workflow, workflowFinalizer); err != nil || added {
		return ctrl.Result{}, err
	}

	// Reconcile the Job
	return r.reconcileJob(ctx, workflow)
}

// reconcileJob manages the multi-group iteration state machine.
func (r *WorkflowReconciler) reconcileJob(ctx context.Context, workflow *nvcrev1alpha1.Workflow) (ctrl.Result, error) {
	// Guard: if already terminal, do nothing
	if r.isTerminal(workflow) {
		return ctrl.Result{}, nil
	}

	// Initialize orchestration status if needed
	orch := r.ensureOrchestrationStatus(workflow)

	// Apply overrides so that override-added dependencies are visible for
	// validation checks. On the first reconcile DetectedPlatform is empty
	// so platform/GPU overrides won't match yet; discoverAndPartition
	// applies them authoritatively once detection completes.
	if err := applyOverrides(&workflow.Spec, buildOverrideContext(&workflow.Spec, orch, nil)); err != nil {
		if statusErr := r.setWorkflowFailed(ctx, workflow, "OverrideError",
			fmt.Sprintf("Failed to apply overrides: %v", err)); statusErr != nil {
			logf.FromContext(ctx).Error(statusErr, "Failed to update Workflow status after override failure")
		}
		return ctrl.Result{}, err
	}

	// Validate checkpoint PVC is declared in dependencies before creating any Job.
	// This is a spec validation that should fail fast, independent of node discovery.
	if workflow.Spec.JobTemplate.Spec.Checkpoint != nil {
		pvcName := workflow.Spec.JobTemplate.Spec.Checkpoint.PVCName
		if !r.hasPVCDependency(workflow, pvcName) {
			if err := r.setWorkflowFailed(ctx, workflow, "CheckpointPVCMissing",
				fmt.Sprintf("Checkpoint requires PVC %q in dependencies", pvcName)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// Discover nodes, detect platform/GPU, apply overrides, create dependencies,
	// and partition groups. Dependencies are created inside discoverAndPartition
	// AFTER overrides are applied so that override dependency merges (e.g. EFA
	// resources merged into TrainingRuntime) take effect before the resource is
	// created. This is required because some dependencies (e.g. TrainingRuntime)
	// are immutable after creation.
	if orch.TotalGroups == 0 {
		return r.discoverAndPartition(ctx, workflow, orch)
	}

	// Re-apply overrides on subsequent reconciles using cached detection results.
	// ensureWorkflowDependencies is called here for idempotency (already created
	// inside discoverAndPartition, but guards against partial failures).
	if err := r.ensureWorkflowDependencies(ctx, workflow); err != nil {
		log := logf.FromContext(ctx)
		log.Error(err, "Failed to create dependencies")
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonDependencyCreationError,
			fmt.Sprintf("Failed to create dependency: %v", err)); statusErr != nil {
			log.Error(statusErr, "Failed to update Workflow status after dependency failure")
		}
		return ctrl.Result{}, err
	}

	// Check for running groups — update their status from Jobs
	if r.hasRunningGroups(orch) {
		return r.updateStatusFromJobs(ctx, workflow)
	}

	// Check if current iteration is complete (all groups terminal)
	if r.allGroupsTerminal(orch) {
		return r.handleIterationComplete(ctx, workflow, orch)
	}

	// Launch pending groups respecting maxConcurrent and overflow constraints
	return r.launchPendingGroups(ctx, workflow, orch)
}

// exclusionSummary lists every node that was targeted but left uncertified, and
// says why. The report turns a non-empty list into an INCOMPLETE verdict, so
// this is what stops a partly-certified fleet reporting a clean PASSED.
//
// All three causes can apply to one run, which is why they are merged rather
// than assigned in turn: on a fleet that is part cordoned and part
// mixed-architecture, writing each cause as it is found lets the second
// silently drop the first.
func exclusionSummary(cordoned, archExcluded []string, gpuArch string, capacityExcluded []gpuCapacityExclusion, gpusPerNode int32) ([]string, string) {
	var nodes, reasons []string

	if len(cordoned) > 0 {
		nodes = append(nodes, cordoned...)
		reasons = append(reasons, fmt.Sprintf(
			"%d node(s) matched the target but were unschedulable (cordoned): %s",
			len(cordoned), strings.Join(cordoned, ", ")))
	}

	if len(archExcluded) > 0 {
		nodes = append(nodes, archExcluded...)
		reasons = append(reasons, fmt.Sprintf(
			"target set has more than one GPU architecture; certified %s only",
			gpuArch))
	}

	if len(capacityExcluded) > 0 {
		for _, e := range capacityExcluded {
			nodes = append(nodes, e.Node)
		}
		reasons = append(reasons, fmt.Sprintf(
			"%d node(s) matched the target but have insufficient GPU capacity, the workload requests %d nvidia.com/gpu per node: %s",
			len(capacityExcluded), gpusPerNode, gpuShortfallDetail(capacityExcluded)))
	}

	if len(nodes) == 0 {
		return nil, ""
	}
	// ". " rather than "; ", because the architecture reason already contains a
	// semicolon and joining on one leaves a reader unable to tell the separator
	// from the punctuation. notEnoughNodesMessage chains its clauses the same way.
	return nodes, strings.Join(reasons, ". ")
}

// gpuShortfallDetail formats capacity exclusions as "node002 has 1, node003
// has 1", so every message about them says what each node actually reports
// next to what was needed rather than only naming the node.
func gpuShortfallDetail(excluded []gpuCapacityExclusion) string {
	parts := make([]string, 0, len(excluded))
	for _, e := range excluded {
		parts = append(parts, fmt.Sprintf("%s has %d", e.Node, e.AllocatableGPUs))
	}
	return strings.Join(parts, ", ")
}

// notEnoughNodesMessage explains a node shortfall. "Schedulable" is Kubernetes
// vocabulary for cordons, taints and capacity, so the bare form sends an operator
// looking at the wrong thing when the real cause is that a heterogeneous target
// was filtered to one GPU architecture or that under-capacity nodes were
// dropped. When that is what happened, say so and name the nodes that were
// dropped. Causes and remedies are collected separately so that when both
// filters contributed, the "Set nodesPerJob to N" advice appears once with
// every applicable remedy after it, not once per cause.
func notEnoughNodesMessage(needed, found int, gpuArch string, archExcluded []string, capacityExcluded []gpuCapacityExclusion, gpusPerNode int32) string {
	msg := fmt.Sprintf("Not enough schedulable nodes: need %d, found %d", needed, found)
	var causes, remedies []string
	if len(archExcluded) > 0 {
		causes = append(causes, fmt.Sprintf(
			"%d node(s) matched the target but were excluded for not being GPU architecture %s: %s",
			len(archExcluded), gpuArch, strings.Join(archExcluded, ", ")))
		remedies = append(remedies, "narrow the target to one architecture")
	}
	if len(capacityExcluded) > 0 {
		causes = append(causes, fmt.Sprintf(
			"%d node(s) matched the target but were excluded for having fewer than the %d allocatable "+
				"nvidia.com/gpu the workload requests per node: %s",
			len(capacityExcluded), gpusPerNode, gpuShortfallDetail(capacityExcluded)))
		remedies = append(remedies, "lower gpusPerNode")
	}
	if len(causes) == 0 {
		return msg
	}
	return fmt.Sprintf("%s. %s. Set nodesPerJob to %d, or %s",
		msg, strings.Join(causes, ". "), found, strings.Join(remedies, ", or "))
}

// discoverAndPartition discovers target nodes, auto-detects nodesPerJob, and partitions nodes into groups.
func (r *WorkflowReconciler) discoverAndPartition(ctx context.Context, workflow *nvcrev1alpha1.Workflow, orch *nvcrev1alpha1.OrchestrationStatus) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	target := workflow.Spec.Orchestration.Target

	nodes, cordoned, err := discoverTargetNodes(ctx, r.Client, target)
	if err != nil {
		log.Error(err, "Failed to discover target nodes")
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonNodeDiscoveryError,
			fmt.Sprintf("Node discovery failed: %v", err)); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	if len(nodes) == 0 {
		if err := r.setWorkflowFailed(ctx, workflow, ReasonNodeDiscoveryError,
			"No target nodes found matching orchestration target"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Detect platform and GPU architecture from target nodes.
	// Fail if nodes report different platforms (likely misconfiguration).
	// For heterogeneous GPU architectures, warn and filter to the primary.
	platform, err := detectPlatformConsistent(nodes)
	if err != nil {
		r.eventf(workflow, corev1.EventTypeWarning, "HeterogeneousPlatform", "%v", err)
		if statusErr := r.setWorkflowFailed(ctx, workflow, "HeterogeneousPlatform",
			err.Error()); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}
	orch.DetectedPlatform = platform

	// A cordoned node matched the target and was never tested. discoverTargetNodes
	// dropped it before anything here saw it, which is why the run is otherwise
	// unaware of it.
	if len(cordoned) > 0 {
		log.Info("Cordoned nodes excluded from certification", "cordoned", cordoned)
		r.eventf(workflow, corev1.EventTypeWarning, "CordonedNodesExcluded",
			"Excluded %d cordoned node(s) from certification: %s",
			len(cordoned), strings.Join(cordoned, ", "))
	}

	// archExcluded records nodes dropped for having a different GPU architecture,
	// so a later shortfall can say that is why rather than blaming schedulability.
	var archExcluded []string
	gpuArch, filteredNodes := detectGPUArchConsistent(nodes)
	if len(filteredNodes) < len(nodes) {
		archExcluded = excludedNodeNames(nodes, filteredNodes)
		log.Info("Heterogeneous GPU architectures detected, filtering to primary",
			"primary", gpuArch, "total", len(nodes), "filtered", len(filteredNodes),
			"excluded", archExcluded)
		r.eventf(workflow, corev1.EventTypeWarning, "HeterogeneousGPU",
			"Filtered %d/%d nodes to primary GPU architecture %s; excluded: %s",
			len(filteredNodes), len(nodes), gpuArch, strings.Join(archExcluded, ", "))
		nodes = filteredNodes
	}
	orch.DetectedGPUArchitecture = gpuArch

	// Record the cordon and architecture exclusions before overrides run, so a
	// failure in override application still leaves the coverage record behind.
	// The GPU capacity check below has to read the post-override spec, so the
	// summary is recomputed with the full set once that filter has run.
	orch.ExcludedNodes, orch.ExclusionReason = exclusionSummary(cordoned, archExcluded, gpuArch, nil, 0)

	// Apply overrides now that platform/gpuArch are known, before computing nodesPerJob.
	// This is the authoritative call — emit events, log details, and populate status.
	octx := buildOverrideContext(&workflow.Spec, orch, nodes)
	applied, err := applyOverridesWithTracking(&workflow.Spec, octx)
	if err != nil {
		r.eventf(workflow, corev1.EventTypeWarning, "OverrideError", "Override failed: %v", err)
		if statusErr := r.setWorkflowFailed(ctx, workflow, "OverrideError",
			fmt.Sprintf("Failed to apply overrides: %v", err),
			applyExclusionRecord(orch.ExcludedNodes, orch.ExclusionReason)); statusErr != nil {
			log.Error(statusErr, "Failed to update Workflow status after override failure")
		}
		return ctrl.Result{}, err
	}
	orch.AppliedOverrides = applied
	r.logOverrideResults(ctx, workflow, applied, octx)

	// Drop nodes that cannot supply the workload's per-node GPU request. A node
	// with fewer allocatable GPUs than the pods ask for is partitioned into a
	// group whose pods stay Pending forever, and the run hangs at
	// InProgress/JobRunning with nothing naming the cause (issue #82). The
	// check runs after override application because overrides can rewrite the
	// resources block, and workloadGPUsPerNode must see the request the pods
	// will actually make; it cannot run inside discoverTargetNodes because the
	// request is only known once the spec is fully resolved.
	gpusPerNode := workloadGPUsPerNode(&workflow.Spec)
	capableNodes, capacityExcluded := filterNodesByGPUCapacity(nodes, gpusPerNode)
	if len(capacityExcluded) > 0 {
		log.Info("Nodes with insufficient GPU capacity excluded from certification",
			"gpusPerNode", gpusPerNode, "excluded", gpuShortfallDetail(capacityExcluded))
		r.eventf(workflow, corev1.EventTypeWarning, "InsufficientGPUCapacity",
			"Excluded %d node(s) with fewer than %d allocatable nvidia.com/gpu: %s",
			len(capacityExcluded), gpusPerNode, gpuShortfallDetail(capacityExcluded))
		nodes = capableNodes
	}

	// Record the dropped nodes on the status, not just in an event. A run that
	// excludes nodes still reports Succeeded, so without this the report says
	// PASSED and never mentions what went untested.
	orch.ExcludedNodes, orch.ExclusionReason = exclusionSummary(cordoned, archExcluded, gpuArch, capacityExcluded, gpusPerNode)

	// Every surviving node was dropped for capacity. Fail with the requirement
	// and the best the fleet offers instead of partitioning into groups that
	// can never schedule.
	if len(nodes) == 0 {
		msg := fmt.Sprintf(
			"No node can supply the %d nvidia.com/gpu the workload requests per node; best available is %d",
			gpusPerNode, maxAllocatableGPUs(capacityExcluded))
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonPartitionError, msg,
			applyExclusionRecord(orch.ExcludedNodes, orch.ExclusionReason)); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, fmt.Errorf("%s", msg)
	}

	// Create workflow-scoped dependencies AFTER overrides are applied.
	// Override dependency merges (e.g. EFA resources into TrainingRuntime) have
	// already modified spec.Dependencies in memory. Creating dependencies here
	// ensures immutable resources like TrainingRuntime include override changes.
	// Dependency refs are accumulated in memory; setWorkflowInProgress below
	// writes the full status (including refs) in a single update.
	if err := r.ensureWorkflowDependencies(ctx, workflow); err != nil {
		log.Error(err, "Failed to create dependencies")
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonDependencyCreationError,
			fmt.Sprintf("Failed to create dependency: %v", err),
			applyExclusionRecord(orch.ExcludedNodes, orch.ExclusionReason)); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	// Auto-detect nodesPerJob from workload template
	adapter, err := workload.ForSpec(&workflow.Spec.JobTemplate.Spec.Workload)
	if err != nil {
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonPartitionError,
			fmt.Sprintf("Failed to determine workload adapter: %v", err),
			applyExclusionRecord(orch.ExcludedNodes, orch.ExclusionReason)); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	nodesPerJob, err := adapter.NodesRequired(&workflow.Spec.JobTemplate.Spec.Workload)
	if err != nil {
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonPartitionError,
			fmt.Sprintf("Failed to detect nodesPerJob: %v", err),
			applyExclusionRecord(orch.ExcludedNodes, orch.ExclusionReason)); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}
	if nodesPerJob < 1 {
		nodesPerJob = len(nodes)
	}
	if nodesPerJob > len(nodes) {
		msg := notEnoughNodesMessage(nodesPerJob, len(nodes), gpuArch, archExcluded, capacityExcluded, gpusPerNode)
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonPartitionError, msg,
			applyExclusionRecord(orch.ExcludedNodes, orch.ExclusionReason)); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, fmt.Errorf("%s", msg)
	}

	// Build NodeInfo list for partitioning
	nodeInfos := make([]orchestration.NodeInfo, len(nodes))
	for i, n := range nodes {
		nodeInfos[i] = orchestration.NodeInfo{
			Name:   n.Name,
			Labels: n.Labels,
		}
	}

	// Generate groups based on strategy: diagnose or partition (simple/topology).
	var groups []orchestration.Group

	if workflow.Spec.Orchestration.Diagnose != nil {
		groups, nodesPerJob, err = initDiagnose(nodeInfos, workflow.Spec.Orchestration.Diagnose, orch)
	} else {
		// Partition mode: simple chunking or topology-aware.
		var topologyKey string
		if workflow.Spec.Orchestration.Topology != nil {
			topologyKey = workflow.Spec.Orchestration.Topology.TopologyKey
		}

		strictDomain := false
		if workflow.Spec.Orchestration.Topology != nil {
			strictDomain = workflow.Spec.Orchestration.Topology.StrictDomain
		}

		groups, err = orchestration.PartitionNodes(orchestration.PartitionInput{
			Nodes:        nodeInfos,
			NodesPerJob:  nodesPerJob,
			TopologyKey:  topologyKey,
			StrictDomain: strictDomain,
		})
	}
	if err != nil {
		if statusErr := r.setWorkflowFailed(ctx, workflow, ReasonPartitionError,
			fmt.Sprintf("Failed to partition nodes: %v", err),
			applyExclusionRecord(orch.ExcludedNodes, orch.ExclusionReason)); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	// Populate orchestration status
	orch.TotalNodes = len(nodes)
	orch.NodesPerJob = nodesPerJob
	orch.TotalGroups = len(groups)
	orch.CurrentIteration = 1
	orch.Groups = buildGroupStatuses(groups, nodeInfos, workflow.Spec.Orchestration.Topology)

	log.Info("Partitioned nodes into groups",
		"totalNodes", orch.TotalNodes,
		"nodesPerJob", orch.NodesPerJob,
		"totalGroups", orch.TotalGroups)

	if err := r.setWorkflowInProgress(ctx, workflow, ReasonGroupsPartitioned,
		fmt.Sprintf("Discovered %d nodes, partitioned into %d groups (%d nodes/job)",
			orch.TotalNodes, orch.TotalGroups, orch.NodesPerJob)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueImmediate}, nil
}

func (r *WorkflowReconciler) logOverrideResults(ctx context.Context, workflow *nvcrev1alpha1.Workflow, applied []nvcrev1alpha1.AppliedOverride, octx OverrideContext) {
	log := logf.FromContext(ctx)
	for _, a := range applied {
		if a.NoOp {
			log.Info("Override matched but produced no changes", "index", a.Index, "when", a.When)
			r.eventf(workflow, corev1.EventTypeWarning, "OverrideNoOp",
				"Override[%d] matched (%s) but produced no changes — check field paths", a.Index, a.When)
		} else {
			log.Info("Override applied", "index", a.Index, "when", a.When, "patches", a.Patches)
			r.eventf(workflow, corev1.EventTypeNormal, "OverrideApplied",
				"Override[%d] matched (%s), patching %s", a.Index, a.When, a.Patches)
		}
	}
	if len(applied) == 0 && len(workflow.Spec.Overrides) > 0 {
		log.V(1).Info("No overrides matched", "total", len(workflow.Spec.Overrides),
			"platform", octx.Platform, "gpuArchitecture", octx.GPUArchitecture)
		r.eventf(workflow, corev1.EventTypeNormal, "NoOverridesMatched",
			"0 of %d overrides matched (platform=%s, gpuArchitecture=%s)",
			len(workflow.Spec.Overrides), octx.Platform, octx.GPUArchitecture)
	}
}

// discoverTargetNodes lists and filters nodes based on the target spec.
// Accepts a client.Reader so it can be called from both the reconciler and CLI tools.
//
// The second return value names the nodes that matched the target but were
// dropped for being cordoned. Callers that record coverage need it: a cordoned
// node was targeted and never tested, and without the names the run reports a
// clean PASSED over a fleet it only partly certified.
func discoverTargetNodes(ctx context.Context, reader client.Reader, target *nvcrev1alpha1.TargetSpec) ([]corev1.Node, []string, error) {
	nodeList := &corev1.NodeList{}
	var opts []client.ListOption

	if target != nil && len(target.NodeSelector) > 0 {
		opts = append(opts, client.MatchingLabels(target.NodeSelector))
	}

	if err := reader.List(ctx, nodeList, opts...); err != nil {
		return nil, nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	nodes := nodeList.Items

	// If matchExpressions specified, post-filter nodes against each requirement
	if target != nil && len(target.MatchExpressions) > 0 {
		var filtered []corev1.Node
		for _, n := range nodes {
			if nodeMatchesExpressions(n, target.MatchExpressions) {
				filtered = append(filtered, n)
			}
		}
		nodes = filtered
	}

	// If nodeNames specified, filter to only those names
	if target != nil && len(target.NodeNames) > 0 {
		nameSet := make(map[string]bool, len(target.NodeNames))
		for _, n := range target.NodeNames {
			nameSet[n] = true
		}
		var filtered []corev1.Node
		for _, n := range nodes {
			if nameSet[n.Name] {
				filtered = append(filtered, n)
			}
		}
		nodes = filtered
	}

	// If taintSelectors, filter nodes that have ALL specified taints
	if target != nil && len(target.TaintSelectors) > 0 {
		var filtered []corev1.Node
		for _, n := range nodes {
			if nodeMatchesTaints(n, target.TaintSelectors) {
				filtered = append(filtered, n)
			}
		}
		nodes = filtered
	}

	// Filter out unschedulable (cordoned) nodes — they can't run workload pods
	// and would cause immediate JobHardwareFailed from the node health monitor.
	// Their names go back to the caller so the run can report what it skipped.
	//
	// Only GPU nodes count as skipped. A cordoned node without a GPU was never a
	// certification candidate, so losing it costs no coverage, and reporting it
	// would turn a fully certified fleet into INCOMPLETE over a node that could
	// never have been tested. That is reachable whenever the target is not the
	// usual gpu.present selector — a nodeNames list can pull in CPU nodes.
	var schedulable []corev1.Node
	var cordoned []string
	for _, n := range nodes {
		if n.Spec.Unschedulable {
			if n.Labels["nvidia.com/gpu.present"] == "true" {
				cordoned = append(cordoned, n.Name)
			}
			continue
		}
		schedulable = append(schedulable, n)
	}
	nodes = schedulable

	// Filter to GPU-equipped nodes only
	var gpuFiltered []corev1.Node
	for _, n := range nodes {
		if n.Labels["nvidia.com/gpu.present"] == "true" {
			gpuFiltered = append(gpuFiltered, n)
		}
	}
	nodes = gpuFiltered

	// Sort by name so discovery is reproducible. client.List gives no ordering
	// guarantee, and callers pick nodes[0] to decide the platform and use slice
	// order to break ties in the majority GPU-architecture rule (issue #77).
	// Unsorted, the same cluster could certify a different subset on each
	// reconcile: over one H100 node and one A100, two runs would flip between
	// certifying h100 and a100. Name order also matches how pkg/orchestration
	// already chunks nodes into groups.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	// Sort the cordoned names for the same reason: they are written to status and
	// printed in the report, so an unsorted list would make the same cluster
	// produce a different report on each reconcile.
	sort.Strings(cordoned)

	return nodes, cordoned, nil
}

// nodeMatchesTaints returns true if the node has ALL of the specified taints.
func nodeMatchesTaints(node corev1.Node, selectors []nvcrev1alpha1.TaintSelector) bool {
	for _, sel := range selectors {
		if !nodeHasTaint(node, sel) {
			return false
		}
	}
	return true
}

// nodeHasTaint returns true if the node has a taint matching the selector.
func nodeHasTaint(node corev1.Node, sel nvcrev1alpha1.TaintSelector) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key != sel.Key {
			continue
		}
		if sel.Value != "" && taint.Value != sel.Value {
			continue
		}
		if sel.Effect != "" && taint.Effect != sel.Effect {
			continue
		}
		return true
	}
	return false
}

// nodeMatchesExpressions returns true if the node satisfies ALL expressions.
// Converts NodeSelectorRequirements to a labels.Selector via apimachinery.
func nodeMatchesExpressions(node corev1.Node, exprs []corev1.NodeSelectorRequirement) bool {
	sel := k8slabels.NewSelector()
	for _, expr := range exprs {
		op, err := convertOperator(expr.Operator)
		if err != nil {
			return false
		}
		req, err := k8slabels.NewRequirement(expr.Key, op, expr.Values)
		if err != nil {
			return false
		}
		sel = sel.Add(*req)
	}
	return sel.Matches(k8slabels.Set(node.Labels))
}

func convertOperator(op corev1.NodeSelectorOperator) (selection.Operator, error) {
	switch op {
	case corev1.NodeSelectorOpIn:
		return selection.In, nil
	case corev1.NodeSelectorOpNotIn:
		return selection.NotIn, nil
	case corev1.NodeSelectorOpExists:
		return selection.Exists, nil
	case corev1.NodeSelectorOpDoesNotExist:
		return selection.DoesNotExist, nil
	default:
		return "", fmt.Errorf("unsupported operator: %s", op)
	}
}

// buildTolerations creates tolerations from taint selectors.
func buildTolerations(selectors []nvcrev1alpha1.TaintSelector) []corev1.Toleration {
	tolerations := make([]corev1.Toleration, len(selectors))
	for i, sel := range selectors {
		t := corev1.Toleration{
			Key:      sel.Key,
			Operator: corev1.TolerationOpEqual,
		}
		if sel.Value != "" {
			t.Value = sel.Value
		} else {
			t.Operator = corev1.TolerationOpExists
		}
		if sel.Effect != "" {
			t.Effect = sel.Effect
		}
		tolerations[i] = t
	}
	return tolerations
}

// hasRunningGroups returns true if any group is in Running phase.
// isBelowBandwidthThreshold checks if a Job's BandwidthMeasurement peak BusBW
// fails the given CEL threshold expression. Returns (below, pending, err) where:
//   - pending=true means to requeue and try again later (BM absent or has no results yet)
//   - err!=nil means the gate could not be evaluated; the caller should fail closed
func (r *WorkflowReconciler) isBelowBandwidthThreshold(ctx context.Context, jobName, namespace, expr string) (below bool, pending bool, err error) {
	var bwList nvcrev1alpha1.BandwidthMeasurementList
	if listErr := r.List(ctx, &bwList, matchingJobRef(namespace, jobName)...); listErr != nil {
		return false, false, fmt.Errorf("list BandwidthMeasurements: %w", listErr)
	}
	if len(bwList.Items) == 0 {
		// No BandwidthMeasurement found for this job yet — requeue and wait for it.
		return false, true, nil
	}
	// The index constrains the list to this Job; a Job has at most one
	// BandwidthMeasurement, so evaluate the first and ignore any duplicate.
	bm := &bwList.Items[0]

	if len(bm.Status.Results) == 0 {
		return false, true, nil
	}
	measured := maxBusBandwidth(bm.Status.Results)
	passed, evalErr := threshold.Evaluate(measured, expr)
	if evalErr != nil {
		return false, false, fmt.Errorf("evaluate bandwidth threshold for job %s: %w", jobName, evalErr)
	}
	return !passed, false, nil
}

// deleteWorkloadForJob deletes the workload (e.g., TrainJob) referenced by a Job
// to free GPU resources. The Job object itself is preserved.
func (r *WorkflowReconciler) deleteWorkloadForJob(ctx context.Context, job *nvcrev1alpha1.Job) {
	ref := job.Status.WorkloadRef
	if ref == nil {
		return
	}
	wl := &unstructured.Unstructured{}
	wl.SetAPIVersion(ref.APIVersion)
	wl.SetKind(ref.Kind)
	wl.SetName(ref.Name)
	wl.SetNamespace(ref.Namespace)
	if err := r.Delete(ctx, wl); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "Failed to delete timed-out workload",
			"kind", ref.Kind, "name", ref.Name)
	}
}

// isJobTimedOut returns true if the group's job has exceeded timeoutPerJob.
//
// The clock starts when the Job first observed its workload running
// (job.status.workloadStartTime), not when the Job was created: a workload
// suspended by an admission controller (e.g. Kueue holding a TrainJob until
// quota is available) has not started, and failing healthy hardware for
// sitting in a queue would be a false certification result. While the
// workload exists but has not been observed running, no timeout accrues.
// Only when no workload has been created at all does the group's StartTime
// remain the bound, so a Job whose workload can never be created still
// terminates.
func (r *WorkflowReconciler) isJobTimedOut(workflow *nvcrev1alpha1.Workflow, g *nvcrev1alpha1.GroupStatus, job *nvcrev1alpha1.Job) bool {
	timeout := workflow.Spec.Orchestration.Execution.TimeoutPerJob
	if timeout == nil {
		return false
	}
	if job.Status.WorkloadStartTime != nil {
		return time.Since(job.Status.WorkloadStartTime.Time) > timeout.Duration
	}
	if job.Status.WorkloadRef != nil {
		// Workload created but not yet observed running (e.g. suspended,
		// pending admission): the job has not started, do not time it out.
		return false
	}
	if g.StartTime == nil {
		return false
	}
	return time.Since(g.StartTime.Time) > timeout.Duration
}

func (r *WorkflowReconciler) hasRunningGroups(orch *nvcrev1alpha1.OrchestrationStatus) bool {
	for _, g := range orch.Groups {
		if g.Phase == nvcrev1alpha1.GroupRunning {
			return true
		}
	}
	return false
}

// allGroupsTerminal returns true if all groups have reached Succeeded or Failed phase.
func (r *WorkflowReconciler) allGroupsTerminal(orch *nvcrev1alpha1.OrchestrationStatus) bool {
	for _, g := range orch.Groups {
		if g.Phase != nvcrev1alpha1.GroupSucceeded && g.Phase != nvcrev1alpha1.GroupFailed {
			return false
		}
	}
	return len(orch.Groups) > 0
}

// countRunningGroups returns the number of groups with Running phase.
func countRunningGroups(orch *nvcrev1alpha1.OrchestrationStatus) int {
	count := 0
	for _, g := range orch.Groups {
		if g.Phase == nvcrev1alpha1.GroupRunning {
			count++
		}
	}
	return count
}

// hasNodeOverlap returns true if any node in the candidate group is
// already used by a Running group. This prevents scheduling conflicts
// where concurrent jobs compete for GPU resources on the same node.
func hasNodeOverlap(candidate *nvcrev1alpha1.GroupStatus, allGroups []nvcrev1alpha1.GroupStatus) bool {
	runningNodes := make(map[string]bool)
	for _, g := range allGroups {
		if g.Phase == nvcrev1alpha1.GroupRunning && g.Name != candidate.Name {
			for _, node := range g.Nodes {
				runningNodes[node] = true
			}
		}
	}
	for _, node := range candidate.Nodes {
		if runningNodes[node] {
			return true
		}
	}
	return false
}

// launchPendingGroups creates Jobs for pending groups, respecting maxConcurrent and overflow constraints.
func (r *WorkflowReconciler) launchPendingGroups(ctx context.Context, workflow *nvcrev1alpha1.Workflow, orch *nvcrev1alpha1.OrchestrationStatus) (ctrl.Result, error) {
	maxConcurrent := workflow.Spec.Orchestration.Execution.MaxConcurrent
	running := countRunningGroups(orch)

	launched := false
	for i := range orch.Groups {
		g := &orch.Groups[i]
		if g.Phase != nvcrev1alpha1.GroupPending {
			continue
		}

		// Respect maxConcurrent limit
		if maxConcurrent > 0 && running >= maxConcurrent {
			break
		}

		// Skip groups that share nodes with currently running groups.
		// This prevents GPU resource conflicts in diagnose-mode confirmation
		// stage where buddy pairings can reference the same nodes.
		if hasNodeOverlap(g, orch.Groups) {
			continue
		}

		if err := r.createJobForGroup(ctx, workflow, g, orch); err != nil {
			if errors.Is(err, errDependencyNotReady) {
				logf.FromContext(ctx).Info("Waiting for ComputeDomain controller to create channel templates", "group", g.Name)
				return ctrl.Result{RequeueAfter: r.getJobRequeueInterval()}, nil
			}
			return ctrl.Result{}, err
		}
		running++
		launched = true
	}

	if launched {
		if err := r.setWorkflowInProgress(ctx, workflow, ReasonJobCreated,
			fmt.Sprintf("Iteration %d/%d: %d groups running",
				orch.CurrentIteration, effectiveIterations(workflow.Spec.Orchestration), running)); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: r.getJobRequeueInterval()}, nil
}

// createJobForGroup creates a Job for a specific group, pinned to that group's nodes.
func (r *WorkflowReconciler) createJobForGroup(ctx context.Context, workflow *nvcrev1alpha1.Workflow, group *nvcrev1alpha1.GroupStatus, orch *nvcrev1alpha1.OrchestrationStatus) error {
	log := logf.FromContext(ctx)

	jobName := r.getGroupJobName(workflow, group.Name, orch.CurrentIteration)

	specCopy := workflow.Spec.JobTemplate.Spec.DeepCopy()

	// Propagate performance thresholds and measurement timeout from ValidationSpec to Job spec.
	if v := workflow.Spec.Validation; v != nil && v.Performance != nil {
		if v.Performance.Thresholds != nil && len(v.Performance.Thresholds.Thresholds) > 0 {
			specCopy.Thresholds = v.Performance.Thresholds.Thresholds
		}
		if v.Performance.MeasurementTimeout != nil {
			specCopy.MeasurementTimeout = v.Performance.MeasurementTimeout
		}
	}

	// Create per-job dependency copies and patch the job spec references
	patchedSpec, jobRefs, err := r.ensureJobDependencies(ctx, workflow, group, orch, specCopy)
	if err != nil {
		return fmt.Errorf("failed to ensure job dependencies for group %s: %w", group.Name, err)
	}
	if len(jobRefs) > 0 {
		workflow.Status.DependencyRefs = append(workflow.Status.DependencyRefs, jobRefs...)
		// Dep refs are written together with group status by setWorkflowInProgress.
		// An intermediate Status().Update() here would replace workflow.Status with
		// the API response, invalidating the caller's orch/group pointers and causing
		// group.Phase = Running (set below) to write to stale memory.
		// Crash recovery: if we crash before the final write, ensureJobDependencies
		// handles AlreadyExists on re-create and re-adds the refs.
	}

	job := &nvcrev1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: workflow.Namespace,
		},
		Spec: *patchedSpec,
	}

	// Pin to this group's specific nodes via NodeAffinity
	adapter, err := workload.ForSpec(&job.Spec.Workload)
	if err != nil {
		return fmt.Errorf("failed to get workload adapter: %w", err)
	}

	affinity := &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "kubernetes.io/hostname",
					Operator: corev1.NodeSelectorOpIn,
					Values:   group.Nodes,
				}},
			}},
		},
	}
	adapter.SetNodeAffinity(&job.Spec.Workload, affinity)

	// Override workload numNodes to match the actual group size.
	// Handles: clamped values, "all nodes" mode, overflow groups, and bisection.
	adapter.SetNumNodes(&job.Spec.Workload, len(group.Nodes))

	// Toleration injection precedence (see ADR-063):
	//   1. If target.taintSelectors is set, inject matching tolerations for all
	//      workload types (training and MPI alike) — explicit opt-in wins.
	//   2. Otherwise, MPI workloads (launcher + worker) get a blanket
	//      Operator: Exists toleration as a fallback so existing NCCL deployments
	//      that rely on it keep working.
	//   3. Otherwise, no controller-injected tolerations.
	if target := workflow.Spec.Orchestration.Target; target != nil && len(target.TaintSelectors) > 0 {
		adapter.SetTolerations(&job.Spec.Workload, buildTolerations(target.TaintSelectors))
	} else if workload.HasLauncherTarget(&job.Spec.Workload) {
		adapter.SetTolerations(&job.Spec.Workload, []corev1.Toleration{{
			Operator: corev1.TolerationOpExists,
		}})
	}

	// Disable MNNVL for diagnose stages that require it.
	if orch != nil && orch.Diagnose != nil {
		applyDiagnoseMNNVLOverride(&job.Spec.Workload, orch.Diagnose, group.Nodes)
	}

	// Set default node health monitor if not already configured
	if job.Spec.NodeHealthMonitor == nil {
		job.Spec.NodeHealthMonitor = &nvcrev1alpha1.NodeHealthMonitor{
			CEL: &nvcrev1alpha1.CELNodeHealthCheck{
				Expression: `node.spec.unschedulable == true`,
			},
		}
	}

	// Merge labels from template metadata
	labels := make(map[string]string)
	maps.Copy(labels, workflow.Spec.JobTemplate.Labels)
	labels["app.kubernetes.io/managed-by"] = "nvcre"
	labels["nvcre.nvidia.com/workflow"] = workflow.Name
	labels["nvcre.nvidia.com/group"] = group.Name
	job.SetLabels(labels)

	// Merge annotations from template metadata and store group nodes.
	annotations := make(map[string]string)
	if len(workflow.Spec.JobTemplate.Annotations) > 0 {
		maps.Copy(annotations, workflow.Spec.JobTemplate.Annotations)
	}
	annotations["nvcre.nvidia.com/group-nodes"] = strings.Join(group.Nodes, ",")
	job.SetAnnotations(annotations)

	// Set owner reference so the Job is garbage collected when the Workflow is deleted
	if err := controllerutil.SetControllerReference(workflow, job, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on Job: %w", err)
	}

	log.Info("Creating Job for group", "name", jobName, "group", group.Name, "iteration", orch.CurrentIteration)
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.Info("Job already exists, proceeding", "name", jobName)
			// Fetch the existing Job so we have its UID for owner references
			if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
				return fmt.Errorf("failed to get existing Job %s: %w", jobName, err)
			}
		} else {
			log.Error(err, "Failed to create Job", "name", jobName)
			return fmt.Errorf("failed to create Job %s: %w", jobName, err)
		}
	}

	// Set the Job as owner of its job-scoped dependencies so they cascade on Job deletion
	for _, ref := range jobRefs {
		if ref.Kind == "" {
			continue
		}
		dep := &unstructured.Unstructured{}
		dep.SetAPIVersion(ref.APIVersion)
		dep.SetKind(ref.Kind)
		dep.SetName(ref.Name)
		dep.SetNamespace(ref.Namespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(dep), dep); err != nil {
			return fmt.Errorf("failed to get dependency %s for owner reference: %w", ref.Name, err)
		}
		// Skip cluster-scoped resources — a namespaced Job cannot own them.
		// Cleanup is handled by cleanupScopedDependencies via DependencyRefs.
		if dep.GetNamespace() == "" {
			continue
		}
		if err := controllerutil.SetOwnerReference(job, dep, r.Scheme); err != nil {
			return fmt.Errorf("failed to set Job owner on dependency %s: %w", ref.Name, err)
		}
		if err := r.Update(ctx, dep); err != nil {
			return fmt.Errorf("failed to update dependency %s with Job owner: %w", ref.Name, err)
		}
	}

	// Update group status
	now := metav1.Now()
	group.Phase = nvcrev1alpha1.GroupRunning
	group.JobRef = &nvcrev1alpha1.WorkloadReference{
		APIVersion: "nvcre.nvidia.com/v1alpha1",
		Kind:       "Job",
		Name:       jobName,
		Namespace:  workflow.Namespace,
	}
	group.StartTime = &now

	return nil
}

// updateStatusFromJobs checks all running groups and updates their status from their Jobs.
func (r *WorkflowReconciler) updateStatusFromJobs(ctx context.Context, workflow *nvcrev1alpha1.Workflow) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	orch := r.ensureOrchestrationStatus(workflow)

	anyRunning := false
	statusChanged := false

	for i := range orch.Groups {
		g := &orch.Groups[i]
		if g.Phase != nvcrev1alpha1.GroupRunning || g.JobRef == nil {
			continue
		}

		ref := g.JobRef
		job := &nvcrev1alpha1.Job{}
		ns := ref.Namespace
		if ns == "" {
			ns = workflow.Namespace
		}

		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, job); err != nil {
			if apierrors.IsNotFound(err) {
				// No pod-drain barrier needed here: the Job finalizer only
				// unregisters after the workload's pods are gone (bounded by
				// podDrainGracePeriod), so a NotFound Job implies its pods
				// have already drained.
				log.Info("Job was deleted, marking group as failed", "group", g.Name, "job", ref.Name)
				r.cleanupScopedDependencies(ctx, workflow, "job", g.Name, orch.CurrentIteration)
				now := metav1.Now()
				g.Phase = nvcrev1alpha1.GroupFailed
				g.CompletionTime = &now
				g.JobRef = nil
				statusChanged = true
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get Job %s: %w", ref.Name, err)
		}

		// Treat a Job with DeletionTimestamp as deleted — its finalizer will
		// clean up the workload, but the Workflow should not wait for it
		// beyond the pod-drain barrier: the scoped dependencies below provide
		// DRA allocations to the workload's pods, and deleting them while
		// pods are still terminating causes CUDA error 719 (issue #121).
		if !job.DeletionTimestamp.IsZero() {
			if shouldWaitForPodDrain(ctx, r.Client, job) {
				anyRunning = true
				continue
			}
			log.Info("Job is being deleted, marking group as failed", "group", g.Name, "job", ref.Name)
			r.cleanupScopedDependencies(ctx, workflow, "job", g.Name, orch.CurrentIteration)
			now := metav1.Now()
			g.Phase = nvcrev1alpha1.GroupFailed
			g.CompletionTime = &now
			g.JobRef = nil
			statusChanged = true
			continue
		}

		// Check terminal state
		ts := getJobTerminalState(job)

		if !ts.terminal {
			if r.isJobTimedOut(workflow, g, job) {
				log.Info("Job timed out, terminating workload",
					"group", g.Name, "job", ref.Name)
				// Capture logs from running pods BEFORE deleting the workload.
				// For MPI tests, the launcher pod has the actual NCCL output.
				r.captureTimeoutLog(ctx, job)
				// Mark Job as failed. The Job object stays for the report.
				meta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
					Type:    nvcrev1alpha1.JobFailed,
					Status:  metav1.ConditionTrue,
					Reason:  ReasonJobTimedOut,
					Message: "Job exceeded timeoutPerJob",
				})
				if err := r.Status().Update(ctx, job); err != nil {
					return ctrl.Result{}, fmt.Errorf("failed to update timed-out Job %s status: %w", ref.Name, err)
				}
				// Complete through the same path a re-observed terminal Job
				// takes: completeTerminalGroup deletes the workload (the Job
				// object is preserved for the report; only the TrainJob is
				// removed) and holds the group and its scoped dependencies
				// behind the pod-drain barrier (issue #121). Routing both the
				// no-pods and the pods-draining timeout completions through
				// one path keeps the side effects identical regardless of
				// drain timing; the ReasonJobTimedOut condition set above
				// keeps the job from being retried on either pass.
				draining, err := r.completeTerminalGroup(ctx, workflow, orch, g, job, getJobTerminalState(job))
				if err != nil {
					return ctrl.Result{}, err
				}
				if draining {
					anyRunning = true
					continue
				}
				statusChanged = true
				continue
			}
			anyRunning = true
			continue
		}

		// Keep the group running until the Job controller finishes threshold
		// evaluation (ValidationFailed=True/False). Measurement collection and
		// CEL evaluation happen only in the Job controller.
		if ts.succeeded && isJobAwaitingThresholdEvaluation(job) {
			log.V(1).Info("Waiting for job threshold evaluation", "group", g.Name, "job", ref.Name)
			anyRunning = true
			continue
		}

		draining, err := r.completeTerminalGroup(ctx, workflow, orch, g, job, ts)
		if err != nil {
			return ctrl.Result{}, err
		}
		if draining {
			anyRunning = true
			continue
		}
		statusChanged = true
	}

	if statusChanged || anyRunning {
		runningCount := countRunningGroups(orch)
		msg := fmt.Sprintf("Iteration %d/%d: %d groups running",
			orch.CurrentIteration, effectiveIterations(workflow.Spec.Orchestration), runningCount)
		// The loop above mutated orch.Groups on workflow.Status directly. Hand
		// those phases to the condition write so they land in the same update:
		// updateStatusWithRetry skips the write when its mutate reports no change,
		// and refetches on conflict, so status mutated outside the callback is
		// dropped on both paths. That left a completed group stuck Running and the
		// Workflow never finished — a passing run reported as a timeout.
		want := make([]nvcrev1alpha1.GroupStatus, len(orch.Groups))
		copy(want, orch.Groups)
		applyGroups := func(w *nvcrev1alpha1.Workflow) bool {
			if w.Status.Orchestration == nil || !statusChanged {
				return false
			}
			w.Status.Orchestration.Groups = want
			return true
		}
		if err := r.setWorkflowInProgress(ctx, workflow, ReasonJobRunning, msg, applyGroups); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Requeue to check for more pending groups to launch or iteration completion
	return ctrl.Result{RequeueAfter: r.getJobRequeueInterval()}, nil
}

// completeTerminalGroup finishes a group whose Job reached a terminal state.
// On failure it terminates the workload first and holds the group open —
// draining=true, no phase change, no dependency cleanup — until the workload's
// pods are gone (the pod-drain barrier, issue #121): the job-scoped
// dependencies provide DRA allocations (ComputeDomain channels) to those pods,
// and deleting them while pods are still terminating kills every pod process
// with CUDA error 719. The Job keeps its terminal conditions, so the next
// reconcile re-enters here and retries the check; podDrainGracePeriod bounds
// the wait. Once drained (or on success, whose pods have already exited) it
// cleans up job-scoped dependencies and moves the group to its final phase, or
// resets it to Pending for a retry.
func (r *WorkflowReconciler) completeTerminalGroup(
	ctx context.Context,
	workflow *nvcrev1alpha1.Workflow,
	orch *nvcrev1alpha1.OrchestrationStatus,
	g *nvcrev1alpha1.GroupStatus,
	job *nvcrev1alpha1.Job,
	ts jobTerminalState,
) (draining bool, err error) {
	log := logf.FromContext(ctx)
	jobFailed := ts.failed || ts.hwFailed || ts.validationFailed

	if jobFailed {
		// Delete the workload to free GPUs and stop hanging pods. This happens
		// before the retry/terminal decision so pods start draining either way.
		r.deleteWorkloadForJob(ctx, job)
		if shouldWaitForPodDrain(ctx, r.Client, job) {
			return true, nil
		}
	}

	now := metav1.Now()
	g.CompletionTime = &now

	if len(job.Status.FailedNodes) > 0 {
		if err := r.recordFailedNodes(ctx, workflow, job.Status.FailedNodes); err != nil {
			log.Error(err, "Failed to record failed nodes to ConfigMap")
		}
	}

	retryLimit := workflow.Spec.Orchestration.Execution.RetryFailedGroups
	switch {
	case jobFailed && retryLimit > 0 && g.Retries < retryLimit && !isJobTimedOutFailure(job):
		// Retry. Timed-out jobs are excluded: the timeout path terminates them
		// for good, and without the exclusion a timed-out job re-entering here
		// during the pod-drain wait would be retried.
		log.Info("Retrying failed group", "group", g.Name, "retry", g.Retries+1, "limit", retryLimit)
		// The workload is already deleted and its pods have drained (barrier
		// above). Delete the failed Job BEFORE cleaning up deps (ADR-053 ordering).
		if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to delete Job %s for retry: %w", job.Name, err)
		}
		// Clean up job-scoped deps after Job deletion
		r.cleanupScopedDependencies(ctx, workflow, "job", g.Name, orch.CurrentIteration)
		g.Retries++
		g.Phase = nvcrev1alpha1.GroupPending
		g.JobRef = nil
		g.StartTime = nil
		g.CompletionTime = nil
	case jobFailed:
		// Clean up job-scoped deps on terminal failure (workload already
		// deleted and drained above).
		r.cleanupScopedDependencies(ctx, workflow, "job", g.Name, orch.CurrentIteration)
		g.Phase = nvcrev1alpha1.GroupFailed
		r.updateTopologyMetric(ctx, workflow)
	default:
		// Clean up job-scoped deps on success
		r.cleanupScopedDependencies(ctx, workflow, "job", g.Name, orch.CurrentIteration)
		g.Phase = nvcrev1alpha1.GroupSucceeded
		r.updateTopologyMetric(ctx, workflow)
	}

	log.Info("Group job completed", "group", g.Name, "phase", g.Phase, "job", job.Name)
	return false, nil
}

type jobTerminalState struct {
	hwFailed         bool
	failed           bool
	succeeded        bool
	validationFailed bool
	terminal         bool // hwFailed || failed || succeeded
}

// getJobTerminalState inspects Job conditions and returns a summary of its terminal state.
func getJobTerminalState(job *nvcrev1alpha1.Job) jobTerminalState {
	hwFailedCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobHardwareFailed)
	jobFailedCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobFailed)
	jobSucceededCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobSucceeded)
	validationFailedCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobValidationFailed)

	s := jobTerminalState{
		hwFailed:         hwFailedCond != nil && hwFailedCond.Status == metav1.ConditionTrue,
		failed:           jobFailedCond != nil && jobFailedCond.Status == metav1.ConditionTrue,
		succeeded:        jobSucceededCond != nil && jobSucceededCond.Status == metav1.ConditionTrue,
		validationFailed: validationFailedCond != nil && validationFailedCond.Status == metav1.ConditionTrue,
	}
	s.terminal = s.hwFailed || s.failed || s.succeeded
	return s
}

// isJobTimedOutFailure reports whether the Job's Failed condition was set by
// the Workflow timeout path (updateStatusFromJobs). Timed-out jobs are
// terminated by the Workflow and are never retried; the retry branch checks
// this because a timed-out job re-enters the terminal path while its pods
// drain (issue #121) instead of completing inside the timeout branch.
func isJobTimedOutFailure(job *nvcrev1alpha1.Job) bool {
	c := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobFailed)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == ReasonJobTimedOut
}

// handleIterationComplete handles the transition when all groups in the current iteration are terminal.
func (r *WorkflowReconciler) handleIterationComplete(ctx context.Context, workflow *nvcrev1alpha1.Workflow, orch *nvcrev1alpha1.OrchestrationStatus) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	orch.CompletedIterations++
	log.Info("Iteration completed", "iteration", orch.CompletedIterations,
		"total", effectiveIterations(workflow.Spec.Orchestration))

	// Diagnose mode: advance through screening → bisection → confirmation stages.
	if workflow.Spec.Orchestration.Diagnose != nil {
		return r.handleDiagnoseRoundComplete(ctx, workflow, orch)
	}

	if orch.CompletedIterations < effectiveIterations(workflow.Spec.Orchestration) {
		// Snapshot iteration results before resetting groups.
		orch.IterationHistory = append(orch.IterationHistory, snapshotIteration(orch))

		// Delete completed Jobs BEFORE cleaning up iteration-scoped deps.
		// Dependencies (ComputeDomain, ResourceClaimTemplate) provide DRA allocations
		// that workloads need; deleting them first causes CUDA failure 719.
		for _, g := range orch.Groups {
			if g.JobRef == nil {
				continue
			}
			job := &nvcrev1alpha1.Job{}
			job.Name = g.JobRef.Name
			job.Namespace = g.JobRef.Namespace
			log.Info("Deleting completed iteration Job", "job", g.JobRef.Name, "iteration", orch.CompletedIterations)
			if err := r.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete Job %s: %w", g.JobRef.Name, err)
			}
		}

		// Clean up iteration-scoped deps in reverse topological order.
		for _, g := range orch.Groups {
			r.cleanupScopedDependencies(ctx, workflow, "iteration", g.Name, orch.CompletedIterations)
		}

		// More iterations to go — reset all group phases to Pending
		orch.CurrentIteration = orch.CompletedIterations + 1
		for i := range orch.Groups {
			orch.Groups[i].Phase = nvcrev1alpha1.GroupPending
			orch.Groups[i].JobRef = nil
			orch.Groups[i].StartTime = nil
			orch.Groups[i].CompletionTime = nil
			orch.Groups[i].Retries = 0
		}

		// The mutations above (CompletedIterations, IterationHistory, group
		// resets) were applied to the cached object. Hand them to the condition
		// write via applyOrchestration so they are re-applied inside the
		// updateStatusWithRetry closure and survive a 409 conflict re-fetch.
		if err := r.setWorkflowInProgress(ctx, workflow, ReasonIterationCompleted,
			fmt.Sprintf("Iteration %d/%d completed, starting next",
				orch.CompletedIterations, effectiveIterations(workflow.Spec.Orchestration)),
			applyOrchestration(orch)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueImmediate}, nil
	}

	// All iterations complete — set final status
	return r.setFinalStatus(ctx, workflow)
}

// initDiagnose creates screening groups for adaptive fault isolation.
func initDiagnose(nodeInfos []orchestration.NodeInfo, spec *nvcrev1alpha1.DiagnoseSpec, orch *nvcrev1alpha1.OrchestrationStatus) ([]orchestration.Group, int, error) {
	groups, err := orchestration.ScreenGroups(orchestration.DiagnoseScreenInput{
		Nodes:       nodeInfos,
		TopologyKey: spec.TopologyKey,
	})
	if err != nil {
		return nil, 0, err
	}
	nodesPerJob := 0
	if len(groups) > 0 {
		nodesPerJob = len(groups[0].Nodes)
	}
	orch.Diagnose = &nvcrev1alpha1.DiagnoseStatus{
		Stage: nvcrev1alpha1.DiagnoseStageIntraScreening,
	}
	return groups, nodesPerJob, nil
}

// handleDiagnoseRoundComplete advances the diagnose algorithm through its stages:
// intra-screening → intra-screening-no-nvl → inter-screening → bisection → confirmation → done.
func (r *WorkflowReconciler) handleDiagnoseRoundComplete(ctx context.Context, workflow *nvcrev1alpha1.Workflow, orch *nvcrev1alpha1.OrchestrationStatus) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	diag := orch.Diagnose
	spec := workflow.Spec.Orchestration.Diagnose
	diag.Round++

	minGroupSize := max(spec.MinGroupSize, 1)

	failedGroups, pending := r.collectDiagnoseRoundResults(ctx, workflow, orch, diag)
	if pending {
		diag.Round-- // undo increment; round hasn't actually completed
		log.Info("Bandwidth measurement results pending, requeueing before advancing diagnose stage")
		return ctrl.Result{RequeueAfter: r.getJobRequeueInterval()}, nil
	}

	switch diag.Stage {

	// --- Stage 1a complete: re-run intra-clique without NVLink ---
	case nvcrev1alpha1.DiagnoseStageIntraScreening:
		recordScreeningResults(orch, diag, failedGroups)
		// Skip the no-NVL stage if MNNVL is already disabled in the base job template —
		// the intra-screening already ran without NVLink, so re-testing would be identical.
		if isMNNVLEnabledInJobTemplate(&workflow.Spec.JobTemplate) {
			noNVLGroups := buildNoNVLGroups(diag)
			if len(noNVLGroups) > 0 {
				diag.Stage = nvcrev1alpha1.DiagnoseStageIntraScreeningNoNVL
				return r.diagnoseSetGroups(ctx, workflow, orch, diag, noNVLGroups,
					fmt.Sprintf("Intra-screening with NVL done: %d healthy, %d suspect. Re-testing without NVL.",
						len(diag.HealthyNodes), len(diag.SuspectNodes)))
			}
		} else {
			log.Info("MNNVL not enabled in job template, skipping no-NVL stage")
		}
		// No groups to test or MNNVL disabled — fall through to inter-screening logic.
		fallthrough

	// --- Stage 1b complete: build inter-domain test from healthy representatives ---
	case nvcrev1alpha1.DiagnoseStageIntraScreeningNoNVL:
		if diag.Stage == nvcrev1alpha1.DiagnoseStageIntraScreeningNoNVL {
			processNoNVLScreeningResults(orch, diag, failedGroups)
		}

		// Select one healthy representative per screening group.
		interGroup := buildInterDomainGroup(diag.HealthyNodes, orch.Groups)
		diag.RepresentativeNodes = interGroup.Nodes

		if len(interGroup.Nodes) < 2 {
			// Not enough healthy domains — skip inter-screening.
			if len(diag.SuspectNodes) == 0 {
				return r.diagnoseDone(ctx, workflow, diag, "All domains healthy, no faults detected")
			}
			// Preserve per-clique boundaries for bisection.
			perClique := failedScreeningGroups(diag)
			if len(perClique) == 0 {
				perClique = []orchestration.Group{{Name: "intra-suspects", Nodes: diag.SuspectNodes}}
			}
			diag.SuspectNodes = nil
			return r.diagnoseNextGroups(ctx, workflow, orch, diag, minGroupSize, perClique)
		}

		diag.Stage = nvcrev1alpha1.DiagnoseStageInterScreening
		return r.diagnoseSetGroups(ctx, workflow, orch, diag,
			[]orchestration.Group{interGroup},
			fmt.Sprintf("Screening done: %d healthy, %d suspect nodes. Testing inter-domain fabric.",
				len(diag.HealthyNodes), len(diag.SuspectNodes)))

	// --- Stage 1b complete: merge intra + inter failures, start bisection ---
	case nvcrev1alpha1.DiagnoseStageInterScreening:
		// Rebuild per-clique groups from stashed suspects (preserve clique boundaries).
		var toSplit []orchestration.Group
		if len(diag.SuspectNodes) > 0 {
			perClique := failedScreeningGroups(diag)
			if len(perClique) > 0 {
				toSplit = perClique
			} else {
				toSplit = []orchestration.Group{{Name: "intra-suspects", Nodes: diag.SuspectNodes}}
			}
			diag.SuspectNodes = nil
		}
		toSplit = append(toSplit, failedGroups...)

		if len(toSplit) == 0 {
			return r.diagnoseDone(ctx, workflow, diag, "All screening passed, no faults detected")
		}
		return r.diagnoseNextGroups(ctx, workflow, orch, diag, minGroupSize, toSplit)

	// --- Bisection round complete: continue splitting or move to confirmation ---
	case nvcrev1alpha1.DiagnoseStageBisection:
		return r.handleDiagnoseBisection(ctx, workflow, orch, diag, minGroupSize, failedGroups)

	// --- Cross-boundary probing complete: record infrastructure faults ---
	case nvcrev1alpha1.DiagnoseStageCrossBoundary:
		return r.handleDiagnoseCrossBoundary(ctx, workflow, orch, diag, minGroupSize, failedGroups)

	// --- Confirmation complete: report results ---
	case nvcrev1alpha1.DiagnoseStageConfirmation:
		var confirmedFaulty []string
		var cleared []string
		for _, g := range failedGroups {
			if len(g.Nodes) > 0 {
				confirmedFaulty = append(confirmedFaulty, g.Nodes[0])
			}
		}
		for _, g := range orch.Groups {
			if g.Phase == nvcrev1alpha1.GroupSucceeded && len(g.Nodes) > 0 {
				cleared = append(cleared, g.Nodes[0])
			}
		}
		// Update suspects: remove cleared nodes, keep only confirmed faulty.
		diag.SuspectNodes = confirmedFaulty
		diag.HealthyNodes = appendUniqueNodes(diag.HealthyNodes, cleared...)

		if len(confirmedFaulty) == 0 {
			return r.diagnoseDone(ctx, workflow, diag,
				"All suspects cleared, no faults confirmed")
		}

		msg := fmt.Sprintf("Diagnosis complete: %d faulty nodes confirmed", len(confirmedFaulty))
		log.Info(msg)

		if err := r.recordFailedNodes(ctx, workflow, noderesults.NodesWithFailureDetails(confirmedFaulty, ReasonWorkloadFailed, msg)); err != nil {
			log.Error(err, "Failed to record failed nodes to ConfigMap")
		}
		if err := r.recordSucceededNodes(ctx, workflow, succeededNodesForWorkflow(workflow)); err != nil {
			log.Error(err, "Failed to record succeeded nodes to ConfigMap")
		}
		diag.Stage = nvcrev1alpha1.DiagnoseStageComplete

		return ctrl.Result{}, r.setWorkflowFailed(ctx, workflow, ReasonIterationsFailed, msg,
			applySucceededNodesRef(workflow.Status.SucceededNodesRef),
			applyFailedNodesRef(workflow.Status.FailedNodesRef),
			applyOrchestration(orch))

	default:
		return ctrl.Result{}, fmt.Errorf("unknown diagnose stage: %s", diag.Stage)
	}
}

// handleDiagnoseBisection handles bisection round completion.
func (r *WorkflowReconciler) handleDiagnoseBisection(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	orch *nvcrev1alpha1.OrchestrationStatus, diag *nvcrev1alpha1.DiagnoseStatus,
	minGroupSize int, failedGroups []orchestration.Group,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Detect "both-halves-pass" (BHP) pattern: sibling bisection groups
	// that both succeeded indicate an infrastructure fault at the boundary.
	groupResults := make([]orchestration.GroupResult, 0, len(orch.Groups))
	for _, g := range orch.Groups {
		groupResults = append(groupResults, orchestration.GroupResult{
			Name:    g.Name,
			Nodes:   g.Nodes,
			Domains: g.Domains,
			Passed:  g.Phase == nvcrev1alpha1.GroupSucceeded,
		})
	}
	// Process failed groups first: move minGroupSize groups to suspects,
	// keep larger groups for further splitting. This must happen BEFORE
	// BHP detection so suspects are captured even if cross-boundary probing triggers.
	var toSplit []orchestration.Group
	for _, g := range failedGroups {
		if len(g.Nodes) <= minGroupSize {
			diag.SuspectNodes = appendUniqueNodes(diag.SuspectNodes, g.Nodes...)
		} else {
			toSplit = append(toSplit, g)
		}
	}

	// Detect "both-halves-pass" (BHP) pattern: sibling bisection groups
	// that both succeeded indicate an infrastructure fault at the boundary.
	bhpResults := orchestration.DetectBothHalvesPass(groupResults)
	if len(bhpResults) > 0 {
		probes := make([]nvcrev1alpha1.CrossBoundaryProbe, 0, len(bhpResults))
		for _, bhp := range bhpResults {
			log.Info("Both-halves-pass detected — infrastructure fault candidate",
				"domain", bhp.Domain, "halfA", len(bhp.HalfA), "halfB", len(bhp.HalfB))
			probes = append(probes, nvcrev1alpha1.CrossBoundaryProbe{
				Domain: bhp.Domain, HalfA: bhp.HalfA, HalfB: bhp.HalfB, ProbeRound: 0,
			})
			// Remove BHP nodes from healthy (they were marked healthy by bisection pass)
			// — they're part of an infrastructure fault, not confirmed healthy.
			diag.HealthyNodes = removeNodes(diag.HealthyNodes, bhp.HalfA)
			diag.HealthyNodes = removeNodes(diag.HealthyNodes, bhp.HalfB)
		}
		// Build cross-boundary probe groups for all BHP results.
		probeGroups := make([]orchestration.Group, 0, len(probes)*2)
		for i, p := range probes {
			probeGroups = append(probeGroups, orchestration.BuildCrossBoundaryGroups(p.HalfA, p.HalfB, i)...)
		}
		diag.CrossBoundaryState = &nvcrev1alpha1.CrossBoundaryState{
			PendingProbes: probes,
			OriginStage:   nvcrev1alpha1.DiagnoseStageBisection,
		}
		diag.Stage = nvcrev1alpha1.DiagnoseStageCrossBoundary
		return r.diagnoseSetGroups(ctx, workflow, orch, diag, probeGroups,
			fmt.Sprintf("Both-halves-pass detected in %d groups. Probing cross-boundary infrastructure.",
				len(bhpResults)))
	}

	if len(toSplit) == 0 && len(failedGroups) == 0 && len(diag.SuspectNodes) == 0 {
		return r.diagnoseDone(ctx, workflow, diag, "Bisection complete: all groups passed")
	}
	if len(toSplit) == 0 {
		return r.diagnoseNextGroups(ctx, workflow, orch, diag, minGroupSize, nil)
	}
	return r.diagnoseNextGroups(ctx, workflow, orch, diag, minGroupSize, toSplit)
}

// handleDiagnoseCrossBoundary processes cross-boundary probing results.
// Failed mixed groups narrow the infrastructure fault; passing groups clear
// the boundary. When all probes resolve, results are stored as InfrastructureFaults
// and the algorithm returns to bisection/confirmation for remaining work.
func (r *WorkflowReconciler) handleDiagnoseCrossBoundary(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	orch *nvcrev1alpha1.OrchestrationStatus, diag *nvcrev1alpha1.DiagnoseStatus,
	minGroupSize int, failedGroups []orchestration.Group,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	cbState := diag.CrossBoundaryState
	if cbState == nil {
		return ctrl.Result{}, fmt.Errorf("cross-boundary state is nil")
	}

	failedSet := map[string]bool{}
	for _, g := range failedGroups {
		failedSet[g.Name] = true
	}

	// Process each pending probe: check if its mixed groups passed or failed.
	var nextProbes []nvcrev1alpha1.CrossBoundaryProbe
	for i, probe := range cbState.PendingProbes {
		mix0 := fmt.Sprintf("cross-%d-mix0", i)
		mix1 := fmt.Sprintf("cross-%d-mix1", i)
		mix0Failed := failedSet[mix0]
		mix1Failed := failedSet[mix1]

		if !mix0Failed && !mix1Failed {
			// Both passed — transient issue, restore nodes to healthy.
			log.Info("Cross-boundary probe cleared (transient)", "domain", probe.Domain)
			diag.HealthyNodes = appendUniqueNodes(diag.HealthyNodes, probe.HalfA...)
			diag.HealthyNodes = appendUniqueNodes(diag.HealthyNodes, probe.HalfB...)
			continue
		}

		if mix0Failed && mix1Failed {
			// Both failed — full infrastructure fault across the boundary.
			log.Info("Infrastructure fault confirmed (full boundary)", "domain", probe.Domain)
			diag.InfrastructureFaults = append(diag.InfrastructureFaults, nvcrev1alpha1.InfrastructureFault{
				Domain: probe.Domain,
				GroupA: probe.HalfA,
				GroupB: probe.HalfB,
				Stage:  cbState.OriginStage,
			})
			continue
		}

		// One failed, one passed — narrow down.
		// The failing mixed group exercised the faulty boundary segment.
		// If we can still narrow further, queue another probe round.
		probe.ProbeRound++
		if probe.ProbeRound > 4 || (len(probe.HalfA) <= 2 && len(probe.HalfB) <= 2) {
			// Max rounds or min size — record as infrastructure fault.
			log.Info("Infrastructure fault localized", "domain", probe.Domain,
				"halfA", len(probe.HalfA), "halfB", len(probe.HalfB))
			diag.InfrastructureFaults = append(diag.InfrastructureFaults, nvcrev1alpha1.InfrastructureFault{
				Domain: probe.Domain,
				GroupA: probe.HalfA,
				GroupB: probe.HalfB,
				Stage:  cbState.OriginStage,
			})
			continue
		}

		// Narrow: find which mixed group failed and split its contributing halves.
		if mix0Failed {
			// Mix-0 used first halves of A and B. Narrow to those.
			midA := max(len(probe.HalfA)/2, 1)
			midB := max(len(probe.HalfB)/2, 1)
			probe.HalfA = probe.HalfA[:midA]
			probe.HalfB = probe.HalfB[:midB]
		} else {
			// Mix-1 used second halves. Narrow to those.
			midA := max(len(probe.HalfA)/2, 1)
			midB := max(len(probe.HalfB)/2, 1)
			probe.HalfA = probe.HalfA[midA:]
			probe.HalfB = probe.HalfB[midB:]
		}
		nextProbes = append(nextProbes, probe)
	}

	if len(nextProbes) > 0 {
		// More probing needed — build next round of mixed groups.
		var probeGroups []orchestration.Group
		for i, p := range nextProbes {
			probeGroups = append(probeGroups, orchestration.BuildCrossBoundaryGroups(p.HalfA, p.HalfB, i)...)
		}
		cbState.PendingProbes = nextProbes
		return r.diagnoseSetGroups(ctx, workflow, orch, diag, probeGroups,
			fmt.Sprintf("Cross-boundary probing: narrowing %d infrastructure faults", len(nextProbes)))
	}

	// All probes resolved — clear cross-boundary state and return to bisection/confirmation.
	diag.CrossBoundaryState = nil

	// If there are pending suspects from earlier bisection, proceed to confirmation.
	if len(diag.SuspectNodes) > 0 {
		return r.diagnoseNextGroups(ctx, workflow, orch, diag, minGroupSize, nil)
	}

	// Check if infrastructure faults were found.
	if len(diag.InfrastructureFaults) > 0 {
		diag.Stage = nvcrev1alpha1.DiagnoseStageComplete
		msg := fmt.Sprintf("Diagnosis complete: %d infrastructure faults detected", len(diag.InfrastructureFaults))
		log.Info(msg)
		return ctrl.Result{}, r.setWorkflowFailed(ctx, workflow, ReasonIterationsFailed, msg,
			applySucceededNodesRef(workflow.Status.SucceededNodesRef),
			applyFailedNodesRef(workflow.Status.FailedNodesRef),
			applyOrchestration(orch))
	}

	return r.diagnoseDone(ctx, workflow, diag, "Cross-boundary probing complete: no faults confirmed")
}

// failedScreeningGroups rebuilds per-clique groups from screening results.
func failedScreeningGroups(diag *nvcrev1alpha1.DiagnoseStatus) []orchestration.Group {
	var groups []orchestration.Group
	for domain, sr := range diag.ScreeningResults {
		if !sr.Passed {
			groups = append(groups, orchestration.Group{
				Name:    "intra-" + domain,
				Nodes:   sr.Nodes,
				Domains: []string{domain},
			})
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups
}

// diagnoseNextGroups bisects failed groups or transitions to confirmation if converged.
func (r *WorkflowReconciler) diagnoseNextGroups(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	orch *nvcrev1alpha1.OrchestrationStatus, diag *nvcrev1alpha1.DiagnoseStatus,
	minGroupSize int, failedGroups []orchestration.Group,
) (ctrl.Result, error) {
	result := orchestration.Bisect(orchestration.BisectInput{
		FailedGroups: failedGroups,
		MinGroupSize: minGroupSize,
		Round:        diag.Round,
	})

	if result.Converged {
		for _, g := range result.Groups {
			diag.SuspectNodes = appendUniqueNodes(diag.SuspectNodes, g.Nodes...)
		}
		// Remove any nodes already confirmed healthy from suspects.
		healthySet := make(map[string]bool, len(diag.HealthyNodes))
		for _, n := range diag.HealthyNodes {
			healthySet[n] = true
		}
		var filtered []string
		for _, n := range diag.SuspectNodes {
			if !healthySet[n] {
				filtered = append(filtered, n)
			}
		}
		diag.SuspectNodes = filtered
		// Build confirmation pairs: each suspect paired with a healthy reference.
		groups := orchestration.BuildConfirmationGroups(diag.SuspectNodes, diag.HealthyNodes)
		if len(groups) == 0 {
			msg := fmt.Sprintf("No healthy reference node; %d suspects marked failed", len(diag.SuspectNodes))
			if err := r.recordFailedNodes(ctx, workflow, noderesults.NodesWithFailureDetails(diag.SuspectNodes, ReasonWorkloadFailed, msg)); err != nil {
				logf.FromContext(ctx).Error(err, "Failed to record failed nodes to ConfigMap")
			}
			if err := r.recordSucceededNodes(ctx, workflow, succeededNodesForWorkflow(workflow)); err != nil {
				logf.FromContext(ctx).Error(err, "Failed to record succeeded nodes to ConfigMap")
			}
			diag.Stage = nvcrev1alpha1.DiagnoseStageComplete
			return ctrl.Result{}, r.setWorkflowFailed(ctx, workflow, ReasonIterationsFailed, msg,
				applySucceededNodesRef(workflow.Status.SucceededNodesRef),
				applyFailedNodesRef(workflow.Status.FailedNodesRef),
				applyOrchestration(orch))
		}
		diag.Stage = nvcrev1alpha1.DiagnoseStageConfirmation
		return r.diagnoseSetGroups(ctx, workflow, orch, diag, groups,
			fmt.Sprintf("Bisection converged: %d suspects, starting confirmation", len(diag.SuspectNodes)))
	}

	diag.Stage = nvcrev1alpha1.DiagnoseStageBisection
	return r.diagnoseSetGroups(ctx, workflow, orch, diag, result.Groups,
		fmt.Sprintf("Bisection round %d: %d groups", diag.Round, len(result.Groups)))
}

// diagnoseSetGroups replaces the current groups and advances the iteration.
func (r *WorkflowReconciler) diagnoseSetGroups(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	orch *nvcrev1alpha1.OrchestrationStatus, diag *nvcrev1alpha1.DiagnoseStatus,
	groups []orchestration.Group, msg string,
) (ctrl.Result, error) {
	logf.FromContext(ctx).Info(msg, "stage", diag.Stage, "round", diag.Round)

	orch.Groups = make([]nvcrev1alpha1.GroupStatus, len(groups))
	for i, g := range groups {
		orch.Groups[i] = nvcrev1alpha1.GroupStatus{
			Name: g.Name, Nodes: g.Nodes, Domains: g.Domains,
			Phase: nvcrev1alpha1.GroupPending,
		}
	}
	orch.TotalGroups = len(groups)
	orch.CurrentIteration = orch.CompletedIterations + 1

	// Groups, TotalGroups, CurrentIteration above — plus the CompletedIterations
	// increment and diagnose stage/round transitions made by callers — live on
	// the cached object. Re-apply them inside the retry closure so a 409
	// conflict re-fetch does not silently drop them.
	if err := r.setWorkflowInProgress(ctx, workflow, ReasonIterationCompleted, msg, applyOrchestration(orch)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueImmediate}, nil
}

// diagnoseDone sets the workflow as succeeded with the given message.
func (r *WorkflowReconciler) diagnoseDone(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	diag *nvcrev1alpha1.DiagnoseStatus, msg string,
) (ctrl.Result, error) {
	diag.Stage = nvcrev1alpha1.DiagnoseStageComplete
	logf.FromContext(ctx).Info(msg, "round", diag.Round,
		"healthy", len(diag.HealthyNodes), "suspects", len(diag.SuspectNodes))

	if err := r.recordSucceededNodes(ctx, workflow, succeededNodesForWorkflow(workflow)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to record succeeded nodes to ConfigMap")
	}
	return ctrl.Result{}, r.setWorkflowSucceeded(ctx, workflow, ReasonJobCompleted, msg,
		applySucceededNodesRef(workflow.Status.SucceededNodesRef),
		applyOrchestration(workflow.Status.Orchestration))
}

// buildInterDomainGroup selects one healthy representative per screening group.
func buildInterDomainGroup(healthyNodes []string, groups []nvcrev1alpha1.GroupStatus) orchestration.Group {
	healthySet := make(map[string]bool, len(healthyNodes))
	for _, n := range healthyNodes {
		healthySet[n] = true
	}

	var reps []string
	var domains []string
	for _, g := range groups {
		for _, node := range g.Nodes {
			if healthySet[node] {
				reps = append(reps, node)
				if len(g.Domains) > 0 {
					domains = append(domains, g.Domains[0])
				}
				break
			}
		}
	}
	return orchestration.Group{Name: "inter-domain", Nodes: reps, Domains: domains}
}

// snapshotIteration captures the current group outcomes as an IterationResult.
func snapshotIteration(orch *nvcrev1alpha1.OrchestrationStatus) nvcrev1alpha1.IterationResult {
	result := nvcrev1alpha1.IterationResult{
		Iteration: orch.CurrentIteration,
		Groups:    make([]nvcrev1alpha1.GroupIterationResult, len(orch.Groups)),
	}
	for i, g := range orch.Groups {
		var jobName string
		if g.JobRef != nil {
			jobName = g.JobRef.Name
		}
		result.Groups[i] = nvcrev1alpha1.GroupIterationResult{
			Name:           g.Name,
			Phase:          g.Phase,
			JobName:        jobName,
			StartTime:      g.StartTime.DeepCopy(),
			CompletionTime: g.CompletionTime.DeepCopy(),
		}
	}
	return result
}

// ensureOrchestrationStatus initializes the orchestration status if nil.
func (r *WorkflowReconciler) ensureOrchestrationStatus(workflow *nvcrev1alpha1.Workflow) *nvcrev1alpha1.OrchestrationStatus {
	if workflow.Status.Orchestration == nil {
		workflow.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{}
	}
	return workflow.Status.Orchestration
}

// setFinalStatus sets the terminal Workflow status after all iterations complete.
// Jobs and dependencies are NOT deleted here — they are cleaned up by handleDeletion()
// when the Workflow is eventually deleted by the Certification controller. This preserves
// GoodputMeasurement and BandwidthMeasurement resources (owned by Workflow via OwnerReference)
// so the certification report can read metrics across all iterations before teardown.
func (r *WorkflowReconciler) setFinalStatus(ctx context.Context, workflow *nvcrev1alpha1.Workflow) (ctrl.Result, error) {
	totalIter := effectiveIterations(workflow.Spec.Orchestration)

	// Count failed groups across all iterations
	failedGroups := r.countFailedGroups(workflow)

	if failedGroups > 0 {
		isHardware := r.hasHardwareFailures(ctx, workflow)
		hasValidation := r.hasValidationFailures(ctx, workflow)

		reason := ReasonIterationsFailed
		if isHardware {
			reason = ReasonJobHardwareFailed
		} else if hasValidation {
			reason = ReasonJobValidationFailed
		}

		msg := fmt.Sprintf("%d groups failed across %d iterations", failedGroups, totalIter)
		if isHardware {
			msg += "; hardware failures occurred on one or more nodes"
		}

		if err := r.recordSucceededNodes(ctx, workflow, succeededNodesForWorkflow(workflow)); err != nil {
			logf.FromContext(ctx).Error(err, "Failed to record succeeded nodes to ConfigMap")
		}

		extras := []func(*nvcrev1alpha1.Workflow) bool{
			applySucceededNodesRef(workflow.Status.SucceededNodesRef),
			applyFailedNodesRef(workflow.Status.FailedNodesRef),
			// applyOrchestration persists the final iteration's CompletedIterations
			// increment made by handleIterationComplete through a 409 re-fetch.
			applyOrchestration(workflow.Status.Orchestration),
		}
		if hasValidation {
			// Supplementary quality signal alongside the exclusive Failed
			// condition: it lets consumers (the WorkloadRun controller,
			// certification reports) tell a threshold miss apart from an
			// execution failure. Set whenever any Job failed threshold
			// validation, even when hardware failures win the Failed reason,
			// so mixed-failure runs do not lose the quality signal.
			extras = append(extras, applyWorkflowValidationFailed(
				"One or more Jobs failed performance threshold validation"))
		}

		return ctrl.Result{}, r.setWorkflowFailed(ctx, workflow, reason, msg, extras...)
	}

	msg := fmt.Sprintf("All %d iterations completed successfully", totalIter)

	if err := r.recordSucceededNodes(ctx, workflow, succeededNodesForWorkflow(workflow)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to record succeeded nodes to ConfigMap")
	}
	return ctrl.Result{}, r.setWorkflowSucceeded(ctx, workflow, ReasonJobCompleted, msg,
		applySucceededNodesRef(workflow.Status.SucceededNodesRef),
		applyOrchestration(workflow.Status.Orchestration))
}

// countFailedGroups counts groups with Failed phase across all iterations.
func (r *WorkflowReconciler) countFailedGroups(workflow *nvcrev1alpha1.Workflow) int {
	if workflow.Status.Orchestration == nil {
		return 0
	}
	count := 0
	// Count from iteration history (completed non-final iterations).
	for _, iter := range workflow.Status.Orchestration.IterationHistory {
		for _, g := range iter.Groups {
			if g.Phase == nvcrev1alpha1.GroupFailed {
				count++
			}
		}
	}
	// Count from current (final) groups.
	for _, g := range workflow.Status.Orchestration.Groups {
		if g.Phase == nvcrev1alpha1.GroupFailed {
			count++
		}
	}
	return count
}

// hasValidationFailures checks if any failed group's Job has the ValidationFailed condition.
// Used by setFinalStatus to distinguish validation failures from other failure types.
func (r *WorkflowReconciler) hasValidationFailures(ctx context.Context, workflow *nvcrev1alpha1.Workflow) bool {
	return r.anyFailedGroupJobHasCondition(ctx, workflow, nvcrev1alpha1.JobValidationFailed)
}

// hasHardwareFailures checks if any failed group's Job has the HardwareFailed condition.
func (r *WorkflowReconciler) hasHardwareFailures(ctx context.Context, workflow *nvcrev1alpha1.Workflow) bool {
	return r.anyFailedGroupJobHasCondition(ctx, workflow, nvcrev1alpha1.JobHardwareFailed)
}

// anyFailedGroupJobHasCondition reports whether the Job behind any failed group
// carries conditionType with status True.
//
// Jobs that cannot be fetched are skipped rather than treated as a failure:
// setFinalStatus uses this to pick a more specific failure reason, and a
// transient read error should leave the generic reason in place rather than
// assert a cause that was never observed.
func (r *WorkflowReconciler) anyFailedGroupJobHasCondition(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow, conditionType string,
) bool {
	if workflow.Status.Orchestration == nil {
		return false
	}
	for _, g := range workflow.Status.Orchestration.Groups {
		if g.Phase != nvcrev1alpha1.GroupFailed || g.JobRef == nil {
			continue
		}
		ns := g.JobRef.Namespace
		if ns == "" {
			ns = workflow.Namespace
		}
		job := &nvcrev1alpha1.Job{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: g.JobRef.Name}, job); err != nil {
			continue
		}
		if CondIsTrue(job.Status.Conditions, conditionType) {
			return true
		}
	}
	return false
}

// deleteTerminalJobs deletes all Jobs referenced by the current iteration's groups.
// The Job controller's finalizer handles workload cleanup and metrics.
// updateTopologyMetric recomputes the topology validated nodes gauge from all succeeded groups.
func (r *WorkflowReconciler) updateTopologyMetric(ctx context.Context, workflow *nvcrev1alpha1.Workflow) {
	log := logf.FromContext(ctx)
	topologyKey := getTopologyKey(workflow)
	if topologyKey == "" {
		log.V(1).Info("Skipping topology metrics: no topology key")
		return
	}
	orch := workflow.Status.Orchestration
	if orch == nil {
		log.V(1).Info("Skipping topology metrics: no orchestration status")
		return
	}

	// Step 1: Collect nodes from failed groups per domain.
	failedNodes := make(map[string]bool)
	failedDomainNodes := make(map[string]map[string]bool) // domain -> set of node names
	for _, g := range orch.Groups {
		if g.Phase == nvcrev1alpha1.GroupFailed {
			for _, nodeName := range g.Nodes {
				failedNodes[nodeName] = true
				domain := r.getNodeDomain(ctx, nodeName, topologyKey, g.Domains)
				if domain != "" {
					if failedDomainNodes[domain] == nil {
						failedDomainNodes[domain] = make(map[string]bool)
					}
					failedDomainNodes[domain][nodeName] = true
				}
			}
		}
	}

	// Step 2: Collect unique validated nodes per domain from succeeded groups,
	// excluding any node that appears in a failed group.
	domainNodes := make(map[string]map[string]bool) // domain -> set of node names
	for _, g := range orch.Groups {
		if g.Phase != nvcrev1alpha1.GroupSucceeded {
			continue
		}
		for _, nodeName := range g.Nodes {
			if failedNodes[nodeName] {
				continue // Node failed in another group — not validated.
			}
			domain := r.getNodeDomain(ctx, nodeName, topologyKey, g.Domains)
			if domain != "" {
				if domainNodes[domain] == nil {
					domainNodes[domain] = make(map[string]bool)
				}
				domainNodes[domain][nodeName] = true
			}
		}
	}

	// Step 3: Set per-node gauges.
	toSliceMap := func(m map[string]map[string]bool) map[string][]string {
		result := make(map[string][]string, len(m))
		for domain, nodeSet := range m {
			nodes := make([]string, 0, len(nodeSet))
			for n := range nodeSet {
				nodes = append(nodes, n)
			}
			result[domain] = nodes
		}
		return result
	}
	validatedMap := toSliceMap(domainNodes)
	failedMap := toSliceMap(failedDomainNodes)
	log.V(1).Info("Recording topology metrics",
		"topologyKey", topologyKey,
		"validatedDomains", len(validatedMap),
		"failedDomains", len(failedMap),
		"totalGroups", len(orch.Groups))
	recordTopologyValidatedNodes(workflow.Namespace, workflow.Name, topologyKey, validatedMap)
	recordTopologyFailedNodes(workflow.Namespace, workflow.Name, topologyKey, failedMap)
}

// getTopologyKey returns the topology key from the workflow spec, or "" if not set.
func getTopologyKey(workflow *nvcrev1alpha1.Workflow) string {
	if workflow.Spec.Orchestration.Topology != nil {
		return workflow.Spec.Orchestration.Topology.TopologyKey
	}
	return ""
}

// getNodeDomain returns the topology domain for a node. When the group spans a
// single domain, it returns that domain directly (fast path). For multi-domain
// groups (overflow), it looks up the node's actual label.
func (r *WorkflowReconciler) getNodeDomain(ctx context.Context, nodeName, topologyKey string, groupDomains []string) string {
	if len(groupDomains) == 1 {
		return groupDomains[0]
	}
	// Multi-domain group (overflow): look up node's actual label.
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return ""
	}
	return node.Labels[topologyKey]
}

// buildGroupStatuses converts orchestration groups into GroupStatus objects,
// populating DomainNodeCounts for multi-domain groups using node topology labels.
func buildGroupStatuses(groups []orchestration.Group, nodeInfos []orchestration.NodeInfo, topo *nvcrev1alpha1.TopologySpec) []nvcrev1alpha1.GroupStatus {
	// Build node→domain lookup for DomainNodeCounts.
	nodeDomainMap := make(map[string]string)
	if topo != nil && topo.TopologyKey != "" {
		for _, ni := range nodeInfos {
			if d := ni.Labels[topo.TopologyKey]; d != "" {
				nodeDomainMap[ni.Name] = d
			}
		}
	}

	statuses := make([]nvcrev1alpha1.GroupStatus, len(groups))
	for i, g := range groups {
		statuses[i] = nvcrev1alpha1.GroupStatus{
			Name:     g.Name,
			Nodes:    g.Nodes,
			Domains:  g.Domains,
			Overflow: g.Overflow,
			Phase:    nvcrev1alpha1.GroupPending,
		}
		if len(g.Domains) > 1 && len(nodeDomainMap) > 0 {
			counts := make(map[string]int)
			for _, n := range g.Nodes {
				if d := nodeDomainMap[n]; d != "" {
					counts[d]++
				}
			}
			statuses[i].DomainNodeCounts = counts
		}
	}
	return statuses
}

// collectAllDomainNodes returns domain → node list for all groups.
// Most groups span a single domain, so assigns all nodes to that domain.
// Multi-domain groups assign each node to every domain (conservative for cleanup).
func collectAllDomainNodes(groups []nvcrev1alpha1.GroupStatus) map[string][]string {
	result := make(map[string][]string)
	for _, g := range groups {
		if len(g.Domains) == 1 {
			result[g.Domains[0]] = append(result[g.Domains[0]], g.Nodes...)
		} else {
			for _, domain := range g.Domains {
				result[domain] = append(result[domain], g.Nodes...)
			}
		}
	}
	return result
}

// setWorkflowInProgress sets the Workflow to InProgress state.
func (r *WorkflowReconciler) setWorkflowInProgress(ctx context.Context, workflow *nvcrev1alpha1.Workflow, reason, message string, extra ...func(*nvcrev1alpha1.Workflow) bool) error {
	return r.setExclusiveCondition(ctx, workflow, nvcrev1alpha1.WorkflowInProgress, reason, message, extra...)
}

// setWorkflowSucceeded sets the Workflow to Succeeded state.
// extra funcs are applied inside the updateStatusWithRetry closure so their
// status mutations survive 409 conflict re-fetches.
func (r *WorkflowReconciler) setWorkflowSucceeded(ctx context.Context, workflow *nvcrev1alpha1.Workflow, reason, message string, extra ...func(*nvcrev1alpha1.Workflow) bool) error {
	return r.setExclusiveCondition(ctx, workflow, nvcrev1alpha1.WorkflowSucceeded, reason, message, extra...)
}

// setWorkflowFailed sets the Workflow to Failed state.
// extra funcs are applied inside the updateStatusWithRetry closure so their
// status mutations survive 409 conflict re-fetches.
func (r *WorkflowReconciler) setWorkflowFailed(ctx context.Context, workflow *nvcrev1alpha1.Workflow, reason, message string, extra ...func(*nvcrev1alpha1.Workflow) bool) error {
	return r.setExclusiveCondition(ctx, workflow, nvcrev1alpha1.WorkflowFailed, reason, message, extra...)
}

// applySucceededNodesRef returns an extra func suitable for setWorkflowSucceeded
// and setWorkflowFailed that re-applies ref inside the updateStatusWithRetry
// closure. Without this, SucceededNodesRef set on the stale object before the
// call is silently discarded when the API server returns a 409 conflict and the
// object is re-fetched.
func applySucceededNodesRef(ref *corev1.TypedLocalObjectReference) func(*nvcrev1alpha1.Workflow) bool {
	if ref == nil {
		return nil
	}
	return func(w *nvcrev1alpha1.Workflow) bool {
		cur := w.Status.SucceededNodesRef
		if cur != nil && cur.Name == ref.Name && cur.Kind == ref.Kind {
			return false
		}
		w.Status.SucceededNodesRef = ref
		return true
	}
}

// applyExclusionRecord returns an extra func suitable for setWorkflowFailed
// that re-applies the excludedNodes/exclusionReason coverage record inside the
// updateStatusWithRetry closure. discoverAndPartition writes the record onto
// the in-memory orchestration status and then fails; without this, a 409
// conflict re-fetches the object in place (wiping the orchestration fields)
// and the retry re-applies only the conditions — and since the Workflow is
// terminal afterwards, nothing ever recomputes the record. The values are
// captured, not read through the orchestration pointer, because the re-fetch
// overwrites what that pointer refers to.
func applyExclusionRecord(excludedNodes []string, exclusionReason string) func(*nvcrev1alpha1.Workflow) bool {
	if len(excludedNodes) == 0 && exclusionReason == "" {
		return nil
	}
	return func(w *nvcrev1alpha1.Workflow) bool {
		if w.Status.Orchestration == nil {
			w.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{}
		}
		orch := w.Status.Orchestration
		if slices.Equal(orch.ExcludedNodes, excludedNodes) && orch.ExclusionReason == exclusionReason {
			return false
		}
		orch.ExcludedNodes = excludedNodes
		orch.ExclusionReason = exclusionReason
		return true
	}
}

// applyFailedNodesRef returns an extra func suitable for setWorkflowFailed that
// re-applies ref inside the updateStatusWithRetry closure. Without this,
// FailedNodesRef set on the stale object before the call (e.g. by
// recordFailedNodes) is silently discarded when the API server returns a 409
// conflict and the object is re-fetched.
func applyFailedNodesRef(ref *corev1.TypedLocalObjectReference) func(*nvcrev1alpha1.Workflow) bool {
	if ref == nil {
		return nil
	}
	return func(w *nvcrev1alpha1.Workflow) bool {
		cur := w.Status.FailedNodesRef
		if cur != nil && cur.Name == ref.Name && cur.Kind == ref.Kind {
			return false
		}
		w.Status.FailedNodesRef = ref
		return true
	}
}

// applyWorkflowValidationFailed returns an extra func for setWorkflowFailed that
// sets the supplementary WorkflowValidationFailed condition alongside the
// exclusive Failed condition. ValidationFailed is deliberately NOT part of the
// exclusive InProgress/Succeeded/Failed trio (see workflow_types.go): it is an
// independent quality signal that distinguishes "the workload ran but missed
// its thresholds" from "the workload broke". Applied inside the
// updateStatusWithRetry closure so it survives 409 conflict re-fetches.
func applyWorkflowValidationFailed(message string) func(*nvcrev1alpha1.Workflow) bool {
	return func(w *nvcrev1alpha1.Workflow) bool {
		return meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.WorkflowValidationFailed,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonJobValidationFailed,
			Message:            message,
			ObservedGeneration: w.GetGeneration(),
		})
	}
}

// applyOrchestration returns an extra func suitable for the setWorkflow* status
// writers that re-applies the caller-computed orchestration state inside the
// updateStatusWithRetry closure. Iteration and diagnose transitions mutate
// workflow.Status.Orchestration on the cached object before the condition
// write; on a 409 conflict the object is re-fetched in place and only closure
// mutations are re-applied, so the CompletedIterations increment, group
// resets, and diagnose stage transitions are silently dropped. Snapshotting
// the desired state and re-applying it inside the closure makes those
// mutations survive the retry.
//
// It reports a change unconditionally: every call site has just advanced the
// orchestration state, so the persisted state always lags what the caller
// computed and the write must not be skipped as a no-op even when the
// conditions happen to be unchanged. Comparing against the passed object
// cannot detect this, because that object already carries the mutation.
//
// Replacing the whole struct is safe against concurrent writers: the Workflow
// reconciler is the sole writer of status.orchestration and controller-runtime
// serialises reconciles per object. The remaining window is a serial
// stale-cache replay — a reconcile starting from a cached object that predates
// this reconciler's own prior writes recomputes the same transition and, on a
// 409 re-fetch, overwrites fresher orchestration state with the recomputed
// snapshot. The recomputation is deterministic, so counters and history
// converge to identical values; the only state that can be reverted is a group
// launch (Running phase, JobRef) from an intervening reconcile, which
// self-heals on the next pass because createJobForGroup adopts the existing
// Job on AlreadyExists and re-adds its dependency refs.
func applyOrchestration(orch *nvcrev1alpha1.OrchestrationStatus) func(*nvcrev1alpha1.Workflow) bool {
	if orch == nil {
		return nil
	}
	want := orch.DeepCopy()
	return func(w *nvcrev1alpha1.Workflow) bool {
		w.Status.Orchestration = want.DeepCopy()
		return true
	}
}

// setExclusiveCondition sets one condition True and all others False (mutually exclusive).
func (r *WorkflowReconciler) setExclusiveCondition(ctx context.Context, workflow *nvcrev1alpha1.Workflow, conditionType, reason, message string, extra ...func(*nvcrev1alpha1.Workflow) bool) error {
	changed, err := setExclusiveStatusCondition(ctx, r.Client, workflow,
		func(w *nvcrev1alpha1.Workflow) *[]metav1.Condition { return &w.Status.Conditions },
		[]string{
			nvcrev1alpha1.WorkflowInProgress,
			nvcrev1alpha1.WorkflowSucceeded,
			nvcrev1alpha1.WorkflowFailed,
		},
		conditionType, reason, message, extra...,
	)
	if err != nil {
		return err
	}
	if changed {
		logf.FromContext(ctx).Info("Workflow status updated", "status", conditionType, "reason", reason)
	}
	return nil
}

// getGroupJobName returns the name for a Job created for a specific group and iteration.
func (r *WorkflowReconciler) getGroupJobName(workflow *nvcrev1alpha1.Workflow, groupName string, iteration int) string {
	var raw string
	isDiagnose := workflow.Spec.Orchestration.Diagnose != nil
	if !isDiagnose && !hasMultipleIterations(workflow.Spec.Orchestration) && workflow.Status.Orchestration != nil && workflow.Status.Orchestration.TotalGroups <= 1 {
		raw = workflow.Name + "-job"
	} else {
		raw = fmt.Sprintf("%s-%s-iter-%d", workflow.Name, groupName, iteration)
	}
	return naming.Truncate(raw, naming.MaxJobNameLen)
}

// getJobRequeueInterval returns the configured requeue interval or the default.
func (r *WorkflowReconciler) getJobRequeueInterval() time.Duration {
	if r.JobRequeueInterval > 0 {
		return r.JobRequeueInterval
	}
	return defaultWorkflowRequeueInterval
}

// captureTimeoutLog captures tail logs from running pods before a timed-out workload
// is deleted. For MPI tests, targets the launcher pod (which has the actual NCCL
// output). Falls back to the first running node pod for non-MPI workloads.
// Best-effort: errors are logged, not returned.
func (r *WorkflowReconciler) captureTimeoutLog(ctx context.Context, job *nvcrev1alpha1.Job) {
	log := logf.FromContext(ctx)
	if r.Clientset == nil {
		return
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"nvcre.nvidia.com/job": job.Name},
	); err != nil {
		log.V(1).Info("Failed to list pods for timeout log capture", "error", err)
		return
	}

	// Find the best pod to capture logs from:
	// 1. Prefer the launcher pod (MPI tests — has the actual test output)
	// 2. Fall back to the first running node pod
	var targetPod *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if pod.Labels["trainer.kubeflow.org/trainjob-ancestor-step"] == "trainer" {
			targetPod = pod
			break // launcher found, use it
		}
		if targetPod == nil {
			targetPod = pod
		}
	}
	if targetPod == nil {
		log.V(1).Info("No running pods found for timeout log capture")
		return
	}

	// Find the main container name (prefer "node", fall back to first)
	containerName := ""
	for _, c := range targetPod.Spec.Containers {
		if containerName == "" || c.Name == "node" {
			containerName = c.Name
		}
	}

	tailLines := int64(failureLogTailLines)
	logOpts := &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLines,
	}
	req := r.Clientset.CoreV1().Pods(targetPod.Namespace).GetLogs(targetPod.Name, logOpts)
	stream, err := podlogs.OpenStream(ctx, req)
	if err != nil {
		log.V(1).Info("Failed to stream pod logs for timeout capture", "pod", targetPod.Name, "error", err)
		job.Status.FailureLog = &nvcrev1alpha1.FailureLog{
			PodName:  targetPod.Name,
			NodeName: targetPod.Spec.NodeName,
			Reason:   "Timeout",
		}
		return
	}
	defer stream.Close() //nolint:errcheck

	tail, err := readLogTail(stream, failureLogMaxBytes)
	if err != nil {
		log.V(1).Info("Failed to read pod logs", "pod", targetPod.Name, "error", err)
	}

	job.Status.FailureLog = &nvcrev1alpha1.FailureLog{
		PodName:  targetPod.Name,
		NodeName: targetPod.Spec.NodeName,
		Reason:   "Timeout",
		Tail:     tail,
	}
	log.Info("Captured timeout log", "pod", targetPod.Name, "node", targetPod.Spec.NodeName, "bytes", len(tail))
}

// handleDeletion handles the cleanup when a Workflow is being deleted.
// Phase 1: Delete all owned Jobs and wait for them to be fully removed (workloads terminated).
// Phase 2: Delete all dependency resources in reverse topological order.
// Phase 3: Patch PVs for deleted PVCs (only works after PV transitions to Released).
// Phase 4: Remove the finalizer once all PVs are handled.
//
// Jobs must be deleted before dependencies because dependencies (ComputeDomain,
// ResourceClaimTemplate) provide DRA allocations for GPU interconnects. Deleting
// them while workload pods are still running causes CUDA failure 719.
//
// The Phase 1 wait is also the pod-drain barrier for this path (issue #121):
// the Job finalizer only unregisters once the workload's pods are gone
// (bounded by podDrainGracePeriod, see shouldWaitForPodDrain), so "no Job
// objects remain" implies "no workload pods remain" and Phase 2 cannot revoke
// a DRA allocation that a terminating pod still holds. This keeps the
// finalizer path correct without garbage collection: the explicit deletes
// below are still required under envtest, and the drain wait is bounded, so a
// stuck Terminating pod delays Workflow deletion by at most the grace period.
func (r *WorkflowReconciler) handleDeletion(ctx context.Context, workflow *nvcrev1alpha1.Workflow) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(workflow, workflowFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("Handling deletion of Workflow")

	// Phase 1: Delete ALL owned Jobs (including completed iteration Jobs) by label selector.
	// This is necessary because envtest has no GC controller, and with multiple iterations
	// there may be completed Jobs from previous iterations that are no longer referenced.
	// The Job controller's finalizer handles workload (TrainJob) deletion and pod termination.
	jobList := &nvcrev1alpha1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(workflow.Namespace),
		client.MatchingLabels{"nvcre.nvidia.com/workflow": workflow.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list Jobs for deletion: %w", err)
	}
	for i := range jobList.Items {
		if jobList.Items[i].DeletionTimestamp.IsZero() {
			log.Info("Deleting owned Job", "name", jobList.Items[i].Name)
			if err := r.Delete(ctx, &jobList.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete Job %s: %w", jobList.Items[i].Name, err)
			}
		}
	}

	// Wait for all Jobs to be fully removed. The Job finalizer deletes the
	// workload and then holds until the workload's pods are gone (the
	// pod-drain barrier in JobReconciler.handleDeletion, bounded by
	// podDrainGracePeriod), so once no Job objects remain it is safe to
	// remove the dependencies that provide their DRA allocations.
	if len(jobList.Items) > 0 {
		log.Info("Waiting for Jobs to complete deletion before removing dependencies", "remaining", len(jobList.Items))
		return ctrl.Result{RequeueAfter: r.getJobRequeueInterval()}, nil
	}

	// Phase 2: Delete all dependency resources in reverse topological order.
	// Since DependencyRefs are stored in creation (topological) order,
	// reversing ensures dependents are deleted before their dependencies.
	var depErrs []error
	for _, ref := range reverseDependencyRefs(workflow.Status.DependencyRefs) {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(ref.APIVersion)
		obj.SetKind(ref.Kind)
		obj.SetName(ref.Name)
		obj.SetNamespace(ref.Namespace)
		log.Info("Deleting dependency resource", "scope", ref.Scope, "kind", ref.Kind, "name", ref.Name)
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete dependency resource", "name", ref.Name)
			depErrs = append(depErrs, err)
		}
	}
	if err := errors.Join(depErrs...); err != nil {
		return ctrl.Result{}, err
	}

	// Phase 3: Patch PVs for deleted PVCs. The PV must be Released (not Bound)
	// for the reclaim policy patch to stick — if still Bound, requeue.
	var pvPending bool
	for _, ref := range workflow.Status.DependencyRefs {
		if ref.Kind != kindPVC {
			continue
		}
		if !r.cleanupPVForPVC(ctx, ref.Namespace, ref.Name) {
			pvPending = true
		}
	}
	if pvPending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Clean up topology metrics.
	if orch := workflow.Status.Orchestration; orch != nil {
		if topologyKey := getTopologyKey(workflow); topologyKey != "" {
			allDomainNodes := collectAllDomainNodes(orch.Groups)
			cleanupTopologyMetrics(workflow.Namespace, workflow.Name, topologyKey, allDomainNodes)
		}
	}

	// Phase 4: Remove finalizer
	log.Info("Removing finalizer from Workflow")
	patch := client.MergeFrom(workflow.DeepCopy())
	controllerutil.RemoveFinalizer(workflow, workflowFinalizer)
	if err := r.Patch(ctx, workflow, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// effectiveIterations returns the number of normal iterations to run, ensuring at least 1.
func effectiveIterations(orch nvcrev1alpha1.OrchestrationSpec) int {
	if orch.Iterations < 1 {
		return 1
	}
	return orch.Iterations
}

// hasMultipleIterations returns true if the orchestration uses multiple iterations.
func hasMultipleIterations(orch nvcrev1alpha1.OrchestrationSpec) bool {
	return orch.Iterations > 1
}

// appendUniqueNodes appends nodes to a slice, skipping duplicates.
func appendUniqueNodes(existing []string, newNodes ...string) []string {
	seen := make(map[string]bool, len(existing))
	for _, s := range existing {
		seen[s] = true
	}
	for _, s := range newNodes {
		if !seen[s] {
			existing = append(existing, s)
			seen[s] = true
		}
	}
	return existing
}

// removeNodes removes specified nodes from a slice.
// recordScreeningResults saves per-domain screening results and stashes failed nodes.
func recordScreeningResults(orch *nvcrev1alpha1.OrchestrationStatus, diag *nvcrev1alpha1.DiagnoseStatus, failedGroups []orchestration.Group) {
	failedNames := failedGroupNameSet(failedGroups)
	diag.ScreeningResults = make(map[string]nvcrev1alpha1.DomainScreeningResult)
	for _, g := range orch.Groups {
		domain := g.Name
		if len(g.Domains) > 0 {
			domain = g.Domains[0]
		}
		diag.ScreeningResults[domain] = nvcrev1alpha1.DomainScreeningResult{
			Nodes:  g.Nodes,
			Passed: g.Phase == nvcrev1alpha1.GroupSucceeded && !failedNames[g.Name],
		}
	}
	for _, g := range failedGroups {
		diag.SuspectNodes = appendUniqueNodes(diag.SuspectNodes, g.Nodes...)
	}
}

// failedGroupNameSet builds a lookup set of group names that
// collectDiagnoseRoundResults classified as failed (including groups whose
// Job succeeded but whose measured bandwidth fell below the configured
// threshold). Used by screening-result recorders to flag those groups as
// not-passed instead of relying solely on g.Phase.
func failedGroupNameSet(failedGroups []orchestration.Group) map[string]bool {
	set := make(map[string]bool, len(failedGroups))
	for _, g := range failedGroups {
		set[g.Name] = true
	}
	return set
}

// buildNoNVLGroups rebuilds screening groups for the no-NVL stage.
func buildNoNVLGroups(diag *nvcrev1alpha1.DiagnoseStatus) []orchestration.Group {
	groups := make([]orchestration.Group, 0, len(diag.ScreeningResults))
	for domain, result := range diag.ScreeningResults {
		groups = append(groups, orchestration.Group{
			Name: "screen-no-nvl-" + domain, Nodes: result.Nodes, Domains: []string{domain},
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups
}

// processNoNVLScreeningResults records no-NVL screening outcomes and updates suspects.
// failedGroups carries the threshold-aware verdict from collectDiagnoseRoundResults
// so groups that passed by phase but fell below the bandwidth threshold are still
// recorded as not-passed and routed into the suspect set for follow-up.
func processNoNVLScreeningResults(orch *nvcrev1alpha1.OrchestrationStatus, diag *nvcrev1alpha1.DiagnoseStatus, failedGroups []orchestration.Group) {
	failedNames := failedGroupNameSet(failedGroups)
	diag.NoNVLScreeningResults = make(map[string]nvcrev1alpha1.DomainScreeningResult)
	for _, g := range orch.Groups {
		domain := g.Name
		if len(g.Domains) > 0 {
			domain = g.Domains[0]
		}
		result := nvcrev1alpha1.DomainScreeningResult{
			Nodes:  g.Nodes,
			Passed: g.Phase == nvcrev1alpha1.GroupSucceeded && !failedNames[g.Name],
		}
		diag.NoNVLScreeningResults[domain] = result
		if !result.Passed {
			diag.SuspectNodes = appendUniqueNodes(diag.SuspectNodes, g.Nodes...)
			diag.NoNVLSuspectNodes = appendUniqueNodes(diag.NoNVLSuspectNodes, g.Nodes...)
			diag.HealthyNodes = removeNodes(diag.HealthyNodes, g.Nodes)
		}
	}
}

func removeNodes(existing []string, toRemove []string) []string {
	rm := make(map[string]bool, len(toRemove))
	for _, n := range toRemove {
		rm[n] = true
	}
	var result []string
	for _, n := range existing {
		if !rm[n] {
			result = append(result, n)
		}
	}
	return result
}

// collectDiagnoseRoundResults collects pass/fail outcomes from a completed diagnose
// round, applying bandwidth threshold reclassification. Returns failed groups and
// whether any bandwidth measurements are still pending (caller should requeue).
func (r *WorkflowReconciler) collectDiagnoseRoundResults(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	orch *nvcrev1alpha1.OrchestrationStatus, diag *nvcrev1alpha1.DiagnoseStatus,
) ([]orchestration.Group, bool) {
	log := logf.FromContext(ctx)

	var bwThresholdExpr string
	if v := workflow.Spec.Validation; v != nil && v.Performance != nil &&
		v.Performance.Thresholds != nil {
		bwThresholdExpr = v.Performance.Thresholds.Thresholds["busBandwidthGBps"]
	}

	var failedGroups []orchestration.Group
	for _, g := range orch.Groups {
		switch g.Phase {
		case nvcrev1alpha1.GroupSucceeded:
			if bwThresholdExpr != "" && g.JobRef != nil {
				below, pending, bwErr := r.isBelowBandwidthThreshold(ctx, g.JobRef.Name, workflow.Namespace, bwThresholdExpr)
				if bwErr != nil {
					log.Error(bwErr, "Bandwidth threshold could not be evaluated, treating group as failed", "group", g.Name)
					failedGroups = append(failedGroups, orchestration.Group{
						Name: g.Name, Nodes: g.Nodes, Domains: g.Domains,
					})
					continue
				}
				if pending {
					log.V(1).Info("Bandwidth measurement pending for group, requeueing", "group", g.Name)
					return nil, true
				}
				if below {
					log.Info("Group passed but bandwidth below threshold, treating as failed",
						"group", g.Name)
					failedGroups = append(failedGroups, orchestration.Group{
						Name: g.Name, Nodes: g.Nodes, Domains: g.Domains,
					})
					continue
				}
			}
			diag.HealthyNodes = appendUniqueNodes(diag.HealthyNodes, g.Nodes...)
		case nvcrev1alpha1.GroupFailed:
			failedGroups = append(failedGroups, orchestration.Group{
				Name: g.Name, Nodes: g.Nodes, Domains: g.Domains,
			})
		}
	}
	return failedGroups, false
}

// applyDiagnoseMNNVLOverride disables MNNVL for diagnose stages that require it:
// no-NVL screening, cross-boundary probing (when origin stage ran without NVL),
// and bisection/confirmation when the group contains no-NVL suspects.
func applyDiagnoseMNNVLOverride(spec *nvcrev1alpha1.WorkloadSpec, diag *nvcrev1alpha1.DiagnoseStatus, nodes []string) {
	switch diag.Stage {
	case nvcrev1alpha1.DiagnoseStageIntraScreeningNoNVL:
		overrideMNNVL(spec, "0")
	case nvcrev1alpha1.DiagnoseStageCrossBoundary:
		if diag.CrossBoundaryState != nil &&
			diag.CrossBoundaryState.OriginStage != nvcrev1alpha1.DiagnoseStageIntraScreening {
			overrideMNNVL(spec, "0")
		}
	case nvcrev1alpha1.DiagnoseStageBisection, nvcrev1alpha1.DiagnoseStageConfirmation:
		if len(diag.NoNVLSuspectNodes) > 0 {
			noNVLSet := make(map[string]bool, len(diag.NoNVLSuspectNodes))
			for _, n := range diag.NoNVLSuspectNodes {
				noNVLSet[n] = true
			}
			for _, n := range nodes {
				if noNVLSet[n] {
					overrideMNNVL(spec, "0")
					return
				}
			}
		}
	}
}

// overrideMNNVL sets NCCL_MNNVL_ENABLE on the trainer. It checks Args first,
// then Env, and appends as an env var if not found in either.
func overrideMNNVL(spec *nvcrev1alpha1.WorkloadSpec, value string) {
	if spec.TrainJob == nil || spec.TrainJob.Trainer == nil {
		return
	}
	trainer := spec.TrainJob.Trainer

	// Check Args for inline NCCL_MNNVL_ENABLE=...
	for i, a := range trainer.Args {
		if strings.HasPrefix(a, "NCCL_MNNVL_ENABLE=") {
			trainer.Args[i] = "NCCL_MNNVL_ENABLE=" + value
			return
		}
	}

	// Check Env vars.
	for i, e := range trainer.Env {
		if e.Name == "NCCL_MNNVL_ENABLE" {
			trainer.Env[i].Value = value
			return
		}
	}

	// Not found — add as env var.
	trainer.Env = append(trainer.Env, corev1.EnvVar{
		Name: "NCCL_MNNVL_ENABLE", Value: value,
	})
}

// isMNNVLEnabledInJobTemplate checks whether the base job template has
// NCCL_MNNVL_ENABLE set to a non-zero value in either Trainer.Args or Trainer.Env.
// Returns false if the env var is absent or set to "0".
func isMNNVLEnabledInJobTemplate(tmpl *nvcrev1alpha1.JobTemplateSpec) bool {
	if tmpl.Spec.Workload.TrainJob == nil || tmpl.Spec.Workload.TrainJob.Trainer == nil {
		return false
	}
	trainer := tmpl.Spec.Workload.TrainJob.Trainer
	for _, a := range trainer.Args {
		if a == "NCCL_MNNVL_ENABLE=0" {
			return false
		}
		if strings.HasPrefix(a, "NCCL_MNNVL_ENABLE=") {
			return true
		}
	}
	for _, e := range trainer.Env {
		if e.Name == "NCCL_MNNVL_ENABLE" {
			return e.Value != "0"
		}
	}
	return false
}

// ensureWorkflowDependencies creates workflow-scoped dependency resources before the first Job starts.
// Idempotent: skips dependencies already tracked in status.DependencyRefs.
// Dependency refs are accumulated in the workflow object in memory but NOT persisted.
// The caller's subsequent Status().Update() (e.g. setWorkflowInProgress, job status
// updates) writes the full status including any new dependency refs.
func (r *WorkflowReconciler) ensureWorkflowDependencies(ctx context.Context, workflow *nvcrev1alpha1.Workflow) error {
	// Classify deps by reachability from the job template
	jobSpecJSON, err := json.Marshal(&workflow.Spec.JobTemplate.Spec)
	if err != nil {
		return fmt.Errorf("failed to marshal job spec for classification: %w", err)
	}
	workflowDeps, _ := classifyDependencies(workflow.Spec.Dependencies, jobSpecJSON)
	if len(workflowDeps) == 0 {
		return nil
	}

	// Order for safe creation
	workflowDeps = orderDependencies(workflowDeps)

	// Count existing workflow-scoped refs for idempotency
	existingCount := 0
	for _, ref := range workflow.Status.DependencyRefs {
		if ref.Scope == "" || ref.Scope == "workflow" {
			existingCount++
		}
	}
	if existingCount >= len(workflowDeps) {
		return nil
	}

	for i, dep := range workflowDeps {
		// Skip already-created dependencies (partial resume after crash)
		if i < existingCount {
			continue
		}

		ref, _, err := r.createDependencyResource(ctx, workflow, workflow, dep)
		if err != nil {
			return err
		}
		ref.Scope = "workflow"
		workflow.Status.DependencyRefs = append(workflow.Status.DependencyRefs, *ref)
	}
	return nil
}

// hasPVCDependency checks if the Workflow has a PVC dependency with the given name.
func (r *WorkflowReconciler) hasPVCDependency(workflow *nvcrev1alpha1.Workflow, pvcName string) bool {
	for _, dep := range workflow.Spec.Dependencies {
		var obj unstructured.Unstructured
		if err := json.Unmarshal(dep.Raw, &obj.Object); err != nil {
			continue
		}
		if obj.GetKind() == kindPVC && obj.GetName() == pvcName {
			return true
		}
	}
	return false
}

// createDependencyResource creates a single dependency resource from its RawExtension.
// If an optional pre-built unstructured object is provided, it is used instead of
// unmarshaling from dep.Raw (used for job-scoped deps with suffixed names).
// The owner parameter sets an owner reference on the resource for GC cascading.
// Pass nil to skip owner reference (e.g. for job-scoped deps that get the Job as owner later).
func (r *WorkflowReconciler) createDependencyResource(ctx context.Context, owner client.Object, workflow *nvcrev1alpha1.Workflow, dep nvcrev1alpha1.DependencySpec, prebuilt ...*unstructured.Unstructured) (*nvcrev1alpha1.DependencyResourceRef, bool, error) {
	log := logf.FromContext(ctx)

	var obj *unstructured.Unstructured
	if len(prebuilt) > 0 && prebuilt[0] != nil {
		obj = prebuilt[0]
	} else {
		obj = &unstructured.Unstructured{}
		if err := json.Unmarshal(dep.Raw, &obj.Object); err != nil {
			return nil, false, fmt.Errorf("failed to unmarshal dependency resource: %w", err)
		}
	}

	// Detect cluster-scoped resources (e.g. ClusterTrainingRuntime) so we can
	// skip namespace defaulting and owner references. Fall back to assuming
	// namespaced if the REST mapper can't resolve the GVK.
	clusterScoped := false
	if namespaced, err := r.IsObjectNamespaced(obj); err == nil {
		clusterScoped = !namespaced
	}

	// Default namespace to workflow namespace for namespaced resources
	if obj.GetNamespace() == "" && !clusterScoped {
		obj.SetNamespace(workflow.Namespace)
	}

	// Set owner reference for GC cascading.
	// Skip cluster-scoped resources — a namespaced Workflow/Job cannot own them.
	// Cleanup is handled by cleanupScopedDependencies/handleDeletion via DependencyRefs.
	if owner != nil && !clusterScoped {
		if err := controllerutil.SetOwnerReference(owner, obj, r.Scheme); err != nil {
			return nil, false, fmt.Errorf("failed to set owner reference on dependency: %w", err)
		}
	}

	// Add tracking label
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["nvcre.nvidia.com/workflow"] = workflow.Name
	obj.SetLabels(labels)

	log.Info("Creating dependency resource", "apiVersion", obj.GetAPIVersion(), "kind", obj.GetKind(), "name", obj.GetName())
	created := true
	if err := r.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.Info("Dependency resource already exists, proceeding", "name", obj.GetName())
			created = false
		} else {
			return nil, false, fmt.Errorf("failed to create dependency %s/%s %s: %w",
				obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
		}
	}

	return &nvcrev1alpha1.DependencyResourceRef{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Name:       obj.GetName(),
		Namespace:  obj.GetNamespace(),
	}, created, nil
}

// cleanupPVForPVC patches the PV backing a PVC from Retain to Delete.
// It finds the PV via the PVC (if it still exists) or by scanning claimRefs.
// Returns true if the PV is handled (patched, already Delete, or not found).
// Returns false if the PV is still Bound and needs a retry.
func (r *WorkflowReconciler) cleanupPVForPVC(ctx context.Context, namespace, name string) bool {
	log := logf.FromContext(ctx)
	var pvName string

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pvc); err == nil {
		pvName = pvc.Spec.VolumeName
	} else {
		// PVC already gone; find PV by claimRef.
		pvName = r.findPVByClaimRef(ctx, namespace, name)
	}

	if pvName == "" {
		return true
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		return true // PV gone
	}

	if pv.Status.Phase == corev1.VolumeBound {
		log.V(1).Info("PV still bound, will retry", "pv", pvName, "pvc", name)
		return false
	}

	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return true // already Delete
	}

	patch := client.MergeFrom(pv.DeepCopy())
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
	if err := r.Patch(ctx, pv, patch); err != nil {
		log.Error(err, "Failed to patch PV reclaim policy", "pv", pvName)
		return false
	}
	log.Info("Patched PV reclaim policy from Retain to Delete", "pv", pvName, "pvc", name)
	return true
}

// findPVByClaimRef finds the PV that was bound to the given PVC.
//
// Uses the pvClaimRefIndexField index rather than listing every PersistentVolume
// in the cluster: PVs are cluster-scoped and a full scan on a large cluster is
// both slow and allocation-heavy for what is a single-key lookup.
func (r *WorkflowReconciler) findPVByClaimRef(ctx context.Context, pvcNamespace, pvcName string) string {
	pvList := &corev1.PersistentVolumeList{}
	if err := r.List(ctx, pvList,
		client.MatchingFields{pvClaimRefIndexField: pvcNamespace + "/" + pvcName},
	); err != nil {
		logf.FromContext(ctx).V(1).Info("Failed to look up PV by claimRef",
			"pvc", pvcNamespace+"/"+pvcName, "error", err)
		return ""
	}
	if len(pvList.Items) == 0 {
		return ""
	}
	return pvList.Items[0].Name
}

// isTerminal returns true if the Workflow has reached a terminal state (Succeeded or Failed).
func (r *WorkflowReconciler) isTerminal(workflow *nvcrev1alpha1.Workflow) bool {
	if cond := meta.FindStatusCondition(workflow.Status.Conditions, nvcrev1alpha1.WorkflowSucceeded); cond != nil && cond.Status == metav1.ConditionTrue {
		return true
	}
	if cond := meta.FindStatusCondition(workflow.Status.Conditions, nvcrev1alpha1.WorkflowFailed); cond != nil && cond.Status == metav1.ConditionTrue {
		return true
	}
	return false
}

// eventf emits a Kubernetes event if the Recorder is configured.
// Safe to call when Recorder is nil (e.g. in unit tests).
func (r *WorkflowReconciler) eventf(obj runtime.Object, eventType, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, eventType, reason, reason, messageFmt, args...)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nvcrev1alpha1.Workflow{}).
		Owns(&nvcrev1alpha1.Job{}).
		Named("workflow").
		WithOptions(controlleropts.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}
