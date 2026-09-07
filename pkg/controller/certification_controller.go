// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleropts "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/naming"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

// waitingForNodesMessage explains a wait that is otherwise invisible: which
// selector matched nothing, and how much of the discovery window is left. Note
// that discoverTargetNodes filters unschedulable nodes before counting, so this
// covers a cordoned fleet as well as a selector that matches nothing at all.
func waitingForNodesMessage(cert *nvcrev1alpha1.Certification) string {
	remaining := max((nodeDiscoveryTimeout - time.Since(cert.CreationTimestamp.Time)).Round(time.Second), 0)
	sel := "any node"
	if len(cert.Spec.Target.NodeSelector) > 0 {
		parts := make([]string, 0, len(cert.Spec.Target.NodeSelector))
		for k, v := range cert.Spec.Target.NodeSelector {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(parts)
		sel = strings.Join(parts, ",")
	}
	return fmt.Sprintf(
		"No schedulable nodes match %s; retrying for up to %s more."+
			" Nodes that are cordoned or unschedulable do not count.",
		sel, remaining)
}

// errNoNodesMatch is returned when discoverTargetNodes finds zero nodes matching
// the certification's target nodeSelector. This is retryable because the
// informer cache may not have synced or nodes may still be joining the cluster.
var errNoNodesMatch = errors.New("no nodes match target")

const (
	certificationFinalizer              = "nvcre.nvidia.com/certification-finalizer"
	defaultCertificationRequeueInterval = 15 * time.Second
	nodeDiscoveryTimeout                = 5 * time.Minute

	categoryStatusPending    = "Pending"
	categoryStatusInProgress = "InProgress"
	categoryStatusSucceeded  = "Succeeded"
	categoryStatusFailed     = "Failed"

	labelCertification   = "nvcre.nvidia.com/certification"
	labelCategoryDomain  = "nvcre.nvidia.com/category-domain"
	labelCategoryVariant = "nvcre.nvidia.com/category-variant"
	// annotationRequestedTestScale carries the testScale the operator asked for
	// through to the report, which otherwise infers it from what was applied.
	annotationRequestedTestScale = "nvcre.nvidia.com/requested-test-scale"

	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "nvcre"
)

// CertificationReconciler reconciles a Certification object
type CertificationReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	Recorder                events.EventRecorder
	WorkflowRequeueInterval time.Duration
	// MaxConcurrentReconciles bounds the number of Certification objects reconciled concurrently.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=certifications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=certifications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=certifications/finalizers,verbs=update
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=workflows,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=workflows/status,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CertificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	certification := &nvcrev1alpha1.Certification{}
	if err := r.Get(ctx, req.NamespacedName, certification); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Certification resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Certification: %w", err)
	}

	// Handle deletion
	if !certification.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, certification)
	}

	// Add finalizer if not present. A successful add ends this reconcile; the
	// resulting watch event drives the next one.
	if added, err := ensureFinalizer(ctx, r.Client, certification, certificationFinalizer); err != nil || added {
		return ctrl.Result{}, err
	}

	// Reconcile the Workflows
	return r.reconcileWorkflows(ctx, certification)
}

// reconcileWorkflows ensures Workflows are created sequentially for each category
// and their status is reflected on the Certification.
func (r *CertificationReconciler) reconcileWorkflows(ctx context.Context, certification *nvcrev1alpha1.Certification) (ctrl.Result, error) {
	if r.isTerminal(certification) {
		// Verify child Workflows are actually terminal. A Workflow with repeatCount
		// may restart after a failed iteration, making the Certification's terminal
		// state stale. If any Workflow is still running, recover to InProgress.
		if r.recoverIfWorkflowStillRunning(ctx, certification) {
			return ctrl.Result{RequeueAfter: requeueImmediate}, nil
		}
		return ctrl.Result{}, nil
	}

	// If we already have category statuses, process the next category
	if len(certification.Status.CategoryStatuses) > 0 {
		return r.processNextCategory(ctx, certification)
	}

	// No category statuses yet — initialize and create the first Workflow
	return r.initializeCategoryStatuses(ctx, certification)
}

// initializeCategoryStatuses validates all categories, initializes their statuses as Pending,
// and creates the Workflow for the first category.
func (r *CertificationReconciler) initializeCategoryStatuses(ctx context.Context, certification *nvcrev1alpha1.Certification) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Validate all categories in the catalog upfront (fail-fast on unknown category)
	for _, category := range certification.Spec.Categories {
		if entry := catalog.Lookup(category.Domain, category.Variant); entry == nil {
			log.Error(nil, "Unknown certification category", "domain", category.Domain, "variant", category.Variant)
			if statusErr := r.setCertificationFailed(ctx, certification, ReasonCategoryNotFound,
				fmt.Sprintf("Unknown certification category: %s/%s", category.Domain, category.Variant)); statusErr != nil {
				log.Error(statusErr, "Failed to update Certification status after category lookup failure")
			}
			return ctrl.Result{}, fmt.Errorf("unknown certification category: %s/%s", category.Domain, category.Variant)
		}
	}

	// Initialize all categories as Pending
	categoryStatuses := make([]nvcrev1alpha1.CertificationCategoryStatus, 0, len(certification.Spec.Categories))
	for _, category := range certification.Spec.Categories {
		categoryStatuses = append(categoryStatuses, nvcrev1alpha1.CertificationCategoryStatus{
			Domain:  category.Domain,
			Variant: category.Variant,
			Status:  categoryStatusPending,
		})
	}

	// Create Workflow for the first category
	if len(certification.Spec.Categories) == 0 {
		return ctrl.Result{}, fmt.Errorf("certification has no categories")
	}
	firstCategory := certification.Spec.Categories[0]
	workflowName, err := r.createWorkflowForCategory(ctx, certification, firstCategory)
	if err != nil {
		if errors.Is(err, errNoNodesMatch) && time.Since(certification.CreationTimestamp.Time) < nodeDiscoveryTimeout {
			log.Info("No nodes match target yet, will retry", "domain", firstCategory.Domain, "variant", firstCategory.Variant)
			if statusErr := r.setCertificationInProgress(ctx, certification, ReasonWaitingForNodes,
				waitingForNodesMessage(certification)); statusErr != nil {
				log.Error(statusErr, "Failed to record waiting-for-nodes status")
			}
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}
		// A foreign Workflow holds the generated name: terminal, no retry, and
		// the collision is never recorded so cleanup cannot touch the object.
		if collision, ok := errors.AsType[*nameCollisionError](err); ok {
			log.Error(err, "Workflow name collision", "domain", firstCategory.Domain, "variant", firstCategory.Variant)
			if statusErr := r.setCertificationFailed(ctx, certification, collision.Reason, err.Error()); statusErr != nil {
				log.Error(statusErr, "Failed to update Certification status after Workflow name collision")
				// setCertificationFailed already retries conflicts, so a
				// surviving error is a real write failure. This branch is
				// terminal (no requeue): return the error so the Failed status
				// is retried rather than silently dropped.
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to build Workflow for category", "domain", firstCategory.Domain, "variant", firstCategory.Variant)
		if statusErr := r.setCertificationFailed(ctx, certification, ReasonWorkflowValidationFailed, err.Error()); statusErr != nil {
			log.Error(statusErr, "Failed to update Certification status after Workflow build failure")
		}
		return ctrl.Result{}, err
	}

	categoryStatuses[0].Status = categoryStatusInProgress
	categoryStatuses[0].WorkflowRef = &nvcrev1alpha1.WorkflowReference{
		Name:      workflowName,
		Namespace: certification.Namespace,
	}

	certification.Status.CategoryStatuses = categoryStatuses
	if err := r.setCertificationInProgress(ctx, certification, ReasonWorkflowCreated,
		fmt.Sprintf("Created Workflow for category %s/%s (1 of %d)", firstCategory.Domain, firstCategory.Variant, len(categoryStatuses))); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
	}

	return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
}

// processNextCategory finds the first non-terminal category and either checks its
// active Workflow or creates the next Workflow. When all categories are terminal,
// it finalizes the Certification.
func (r *CertificationReconciler) processNextCategory(ctx context.Context, certification *nvcrev1alpha1.Certification) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Find the first non-terminal category
	activeIdx := -1
	for i, catStatus := range certification.Status.CategoryStatuses {
		if catStatus.Status != categoryStatusSucceeded && catStatus.Status != categoryStatusFailed {
			activeIdx = i
			break
		}
	}

	// All categories are terminal — finalize
	if activeIdx == -1 {
		return r.finalizeCertification(ctx, certification)
	}

	catStatus := &certification.Status.CategoryStatuses[activeIdx]

	// If the category is InProgress, check its Workflow status
	if catStatus.Status == categoryStatusInProgress {
		return r.checkActiveWorkflow(ctx, certification, activeIdx)
	}

	// Category is Pending — create its Workflow
	if activeIdx >= len(certification.Spec.Categories) {
		return ctrl.Result{}, fmt.Errorf("category index %d out of range (spec has %d categories)", activeIdx, len(certification.Spec.Categories))
	}
	category := certification.Spec.Categories[activeIdx]
	workflowName, err := r.createWorkflowForCategory(ctx, certification, category)
	if err != nil {
		if errors.Is(err, errNoNodesMatch) && time.Since(certification.CreationTimestamp.Time) < nodeDiscoveryTimeout {
			log.Info("No nodes match target yet, will retry", "domain", category.Domain, "variant", category.Variant)
			if statusErr := r.setCertificationInProgress(ctx, certification, ReasonWaitingForNodes,
				waitingForNodesMessage(certification)); statusErr != nil {
				log.Error(statusErr, "Failed to record waiting-for-nodes status")
			}
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}
		// A foreign Workflow holds the generated name: terminal, no retry, and
		// the collision is never recorded so cleanup cannot touch the object.
		if collision, ok := errors.AsType[*nameCollisionError](err); ok {
			log.Error(err, "Workflow name collision", "domain", category.Domain, "variant", category.Variant)
			if statusErr := r.setCertificationFailed(ctx, certification, collision.Reason, err.Error()); statusErr != nil {
				log.Error(statusErr, "Failed to update Certification status after Workflow name collision")
				// setCertificationFailed already retries conflicts, so a
				// surviving error is a real write failure. This branch is
				// terminal (no requeue): return the error so the Failed status
				// is retried rather than silently dropped.
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to build Workflow for category", "domain", category.Domain, "variant", category.Variant)
		if statusErr := r.setCertificationFailed(ctx, certification, ReasonWorkflowValidationFailed, err.Error()); statusErr != nil {
			log.Error(statusErr, "Failed to update Certification status after Workflow build failure")
		}
		return ctrl.Result{}, err
	}

	catStatus.Status = categoryStatusInProgress
	catStatus.WorkflowRef = &nvcrev1alpha1.WorkflowReference{
		Name:      workflowName,
		Namespace: certification.Namespace,
	}

	if err := r.setCertificationInProgress(ctx, certification, ReasonWorkflowCreated,
		fmt.Sprintf("Created Workflow for category %s/%s (%d of %d)", category.Domain, category.Variant,
			activeIdx+1, len(certification.Status.CategoryStatuses))); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
	}

	return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
}

// checkActiveWorkflow fetches the active Workflow and updates the category status
// based on its conditions. If the Workflow is terminal, it requeues immediately
// to advance to the next category.
func (r *CertificationReconciler) checkActiveWorkflow(ctx context.Context, certification *nvcrev1alpha1.Certification, idx int) (ctrl.Result, error) {
	catStatus := &certification.Status.CategoryStatuses[idx]

	workflow := &nvcrev1alpha1.Workflow{}
	ns := catStatus.WorkflowRef.Namespace
	if ns == "" {
		ns = certification.Namespace
	}

	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: catStatus.WorkflowRef.Name}, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			catStatus.Status = categoryStatusFailed
			if err := r.setCertificationInProgress(ctx, certification, ReasonWorkflowRunning,
				fmt.Sprintf("Workflow for category %s/%s not found", catStatus.Domain, catStatus.Variant)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
			}
			return ctrl.Result{RequeueAfter: requeueImmediate}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Workflow %s: %w", catStatus.WorkflowRef.Name, err)
	}

	if workflow.Status.SucceededNodesRef != nil {
		catStatus.SucceededNodesRef = workflow.Status.SucceededNodesRef
	}
	if workflow.Status.FailedNodesRef != nil {
		catStatus.FailedNodesRef = workflow.Status.FailedNodesRef
	}

	// Check for terminal Workflow conditions
	if cond := meta.FindStatusCondition(workflow.Status.Conditions, nvcrev1alpha1.WorkflowFailed); cond != nil && cond.Status == metav1.ConditionTrue {
		catStatus.Status = categoryStatusFailed
		if err := r.setCertificationInProgress(ctx, certification, ReasonWorkflowRunning,
			fmt.Sprintf("Workflow for category %s/%s failed, advancing to next category", catStatus.Domain, catStatus.Variant)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueImmediate}, nil
	}

	if cond := meta.FindStatusCondition(workflow.Status.Conditions, nvcrev1alpha1.WorkflowSucceeded); cond != nil && cond.Status == metav1.ConditionTrue {
		catStatus.Status = categoryStatusSucceeded
		if err := r.setCertificationInProgress(ctx, certification, ReasonWorkflowRunning,
			fmt.Sprintf("Workflow for category %s/%s succeeded, advancing to next category", catStatus.Domain, catStatus.Variant)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueImmediate}, nil
	}

	// Workflow still running
	if err := r.setCertificationInProgress(ctx, certification, ReasonWorkflowRunning,
		"Certification in progress"); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
	}
	return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
}

// finalizeCertification aggregates results when all categories are terminal and sets
// the final Certification status.
func (r *CertificationReconciler) finalizeCertification(ctx context.Context, certification *nvcrev1alpha1.Certification) (ctrl.Result, error) {
	anyFailed := false
	for _, catStatus := range certification.Status.CategoryStatuses {
		if catStatus.Status == categoryStatusFailed {
			anyFailed = true
			break
		}
	}

	if anyFailed {
		if err := r.setCertificationFailed(ctx, certification, ReasonWorkflowFailed,
			"One or more certification categories failed"); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if err := r.setCertificationSucceeded(ctx, certification, ReasonAllWorkflowsSucceeded,
		"All certification categories completed successfully"); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update Certification status: %w", err)
	}
	return ctrl.Result{}, nil
}

// DefaultEnableMNNVL is defined in helpers.go.

// createWorkflowForCategory creates a single Workflow for the given category.
// It performs full resolution: discovers nodes, detects platform/GPU, resolves
// options, renders templates, applies overlays, and prunes applied overlays.
func (r *CertificationReconciler) createWorkflowForCategory(ctx context.Context, certification *nvcrev1alpha1.Certification, category nvcrev1alpha1.CertificateCategory) (string, error) {
	log := logf.FromContext(ctx)

	entry := catalog.Lookup(category.Domain, category.Variant)
	if entry == nil {
		return "", fmt.Errorf("unknown certification category: %s/%s", category.Domain, category.Variant)
	}

	// --- 1. Discover nodes (shared context for nodesPerJob + overlays) ---
	// Cordoned nodes are discarded here. The Workflow runs the same discovery and
	// records them on its own status, which is where the report reads coverage
	// from, so recording them twice would only risk the two disagreeing.
	nodes, _, err := discoverTargetNodes(ctx, r.Client, &certification.Spec.Target)
	if err != nil {
		return "", fmt.Errorf("discovering target nodes: %w", err)
	}
	if len(nodes) == 0 {
		return "", fmt.Errorf("%s/%s: %w", category.Domain, category.Variant, errNoNodesMatch)
	}

	// Best-effort platform + GPU detection for overlay context.
	detectedPlatform := DetectPlatform(nodes)
	// Size the job against the nodes that will actually run it. The Workflow
	// filters a heterogeneous target down to one GPU architecture before it
	// partitions, so a nodesPerJob derived from the whole target can ask for more
	// nodes than survive that filter, and partitioning then fails with a shortfall
	// the operator did not cause. Apply the same filter here so the two agree.
	gpuArch, archNodes := detectGPUArchConsistent(nodes)
	if gpuArch == "" || gpuArch == gpuArchUnknown {
		// Fall back to nodeSelector-based detection
		gpuArch = catalog.GPUArchFromNodeSelector(certification.Spec.Target.NodeSelector)
		archNodes = nodes
	}
	if gpuArch == "" {
		return "", fmt.Errorf(
			"cannot determine GPU architecture from target nodeSelector" +
				" (nvidia.com/gpu.product label is required)",
		)
	}

	// --- 2. Resolve options + nodesPerJob ---
	opts := ResolveOptions(&certification.Spec.CategoryOptions, category.Options)

	nd := catalog.GPUDefaults(gpuArch, detectedPlatform)
	gpusPerNode := nd.GpusPerNode
	mlnxPerNode := nd.MlnxPerNode
	if opts.GpusPerNode != nil {
		gpusPerNode = *opts.GpusPerNode
	}
	if opts.MlnxPerNode != nil {
		mlnxPerNode = *opts.MlnxPerNode
	}

	capableNodes, err := dropUnderCapacityNodes(archNodes, category, gpusPerNode)
	if err != nil {
		return "", err
	}

	nodesPerJob, err := resolveNodesPerJob(capableNodes, category, opts, entry, gpusPerNode, gpuArch)
	if err != nil {
		return "", err
	}

	enableMNNVL := derefBool(opts.EnableMNNVL)
	if opts.EnableMNNVL == nil {
		enableMNNVL = DefaultEnableMNNVL(gpuArch)
	}

	// --- 3. Render templates (entry.Build) ---
	workflowSpec, buildErr := entry.Build(certification.Spec.Target, catalog.BuildConfig{
		ImagePullSecrets:   opts.ImagePullSecrets,
		StorageClassName:   opts.StorageClassName,
		NodesPerJob:        nodesPerJob,
		GpusPerNode:        gpusPerNode,
		MlnxPerNode:        mlnxPerNode,
		Resources:          opts.Resources,
		EnableMNNVL:        enableMNNVL,
		EnableCheckpoint:   derefBool(opts.EnableCheckpoint),
		MaxSteps:           derefInt32(opts.MaxSteps),
		ExitDurationMins:   derefInt32(opts.ExitDurationMins),
		GPUArchitecture:    gpuArch,
		SaveInterval:       derefInt32(opts.SaveInterval),
		SaveRetainInterval: derefInt32(opts.SaveRetainInterval),
		SaveTopK:           derefInt32(opts.SaveTopK),
		StorageSize:        opts.StorageSize,
		TestScale:          opts.TestScale,
		MaxBytes:           opts.MaxBytes,
		NumIterations:      derefInt32(opts.NumIterations),
		NumCycles:          derefInt32(opts.NumCycles),
		Thresholds:         opts.Thresholds,
		MaxConcurrent:      derefInt32(opts.MaxConcurrent),
		MinGroupSize:       derefInt32(opts.MinGroupSize),
		RepeatCount:        derefInt32(opts.RepeatCount),
		MaxRestarts:        derefInt32(opts.MaxRestarts),
		TimeoutPerJob:      opts.TimeoutPerJob,
		MeasurementTimeout: opts.MeasurementTimeout,
	})
	if buildErr != nil {
		return "", fmt.Errorf("building workflow for %s/%s: %w", category.Domain, category.Variant, buildErr)
	}

	// --- 4. Apply overlays (best-effort, prune applied) ---
	orch := &nvcrev1alpha1.OrchestrationStatus{
		DetectedPlatform:        detectedPlatform,
		DetectedGPUArchitecture: gpuArch,
	}
	octx := BuildOverrideContext(&workflowSpec, orch, nodes)
	applied, overrideErr := ApplyOverridesWithTracking(&workflowSpec, octx)
	if overrideErr != nil {
		return "", fmt.Errorf("applying overrides for %s/%s: %w", category.Domain, category.Variant, overrideErr)
	}
	pruneAppliedOverrides(&workflowSpec, applied)
	pruneUnmatchableOverrides(&workflowSpec, octx)

	// Gang scheduling is applied last so it wins over anything the catalog entry
	// or a platform override put in schedulerName.
	if err := platform.ApplyGangSchedulerToDependencies(
		workflowSpec.Dependencies, certification.Spec.GangScheduler); err != nil {
		return "", fmt.Errorf("applying gang scheduler for %s/%s: %w", category.Domain, category.Variant, err)
	}

	if len(applied) > 0 || len(workflowSpec.Overrides) > 0 {
		log.Info("Resolved overlays",
			"domain", category.Domain, "variant", category.Variant,
			"applied", len(applied), "remaining", len(workflowSpec.Overrides))
	}

	// --- 5. Create Workflow ---
	workflowName := r.getWorkflowName(certification, category)
	workflow := &nvcrev1alpha1.Workflow{
		Name:      workflowName,
		Namespace: certification.Namespace,
		Labels: map[string]string{
			labelManagedBy:       managedByValue,
			labelCertification:   certification.Name,
			labelCategoryDomain:  category.Domain,
			labelCategoryVariant: category.Variant,
		},
		Spec: workflowSpec,
	}
	// Record what the operator asked for. The report otherwise infers the scale
	// from what was applied, and those differ: an entry whose template ignores
	// testScale still partitions one node per group, so an intra-rack request came
	// out as "intra-node" in the report.
	if opts.TestScale != "" {
		workflow.Annotations = map[string]string{
			annotationRequestedTestScale: opts.TestScale,
		}
	}

	if err := controllerutil.SetControllerReference(certification, workflow, r.Scheme); err != nil {
		return "", fmt.Errorf("failed to set owner reference on Workflow: %w", err)
	}

	log.Info("Creating Workflow", "name", workflowName, "domain", category.Domain, "variant", category.Variant)
	if err := r.Create(ctx, workflow); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Never adopt a pre-existing Workflow blindly. Fetch it and verify it
			// is the one this Certification created (a duplicate create caused by
			// cache lag or a crash-retry). A foreign Workflow with the generated
			// name is never recorded — the finalizer would otherwise delete it.
			existing := &nvcrev1alpha1.Workflow{}
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(workflow), existing); getErr != nil {
				return "", fmt.Errorf("failed to get existing Workflow %s: %w", workflowName, getErr)
			}
			if !metav1.IsControlledBy(existing, certification) {
				// A foreign holder that is already terminating (e.g. the child of
				// a same-named Certification that was just deleted) releases the
				// name shortly: retry with backoff instead of failing terminally.
				if !existing.DeletionTimestamp.IsZero() {
					return "", fmt.Errorf("existing Workflow %q in namespace %q is being deleted; retrying",
						workflowName, certification.Namespace)
				}
				return "", &nameCollisionError{
					Reason: ReasonWorkflowNameCollision,
					Message: fmt.Sprintf("Workflow %q already exists in namespace %q and is not controlled by Certification %q; refusing to adopt it",
						workflowName, certification.Namespace, certification.Name),
				}
			}
			log.Info("Workflow already exists and is controlled by this Certification, proceeding", "name", workflowName)
		} else {
			log.Error(err, "Failed to create Workflow", "name", workflowName)
			// One event per failed Create attempt. This branch is only reached
			// while a category is being started (never from the steady-state
			// polling path, which goes through checkActiveWorkflow), and the
			// setCertificationFailed below makes the Certification terminal, so
			// subsequent requeues short-circuit in reconcileWorkflows.
			r.warnf(certification, ReasonWorkflowCreationError,
				"Failed to create Workflow %s: %v", workflowName, err)
			if statusErr := r.setCertificationFailed(ctx, certification, ReasonWorkflowFailed,
				fmt.Sprintf("Failed to create Workflow %s: %v", workflowName, err)); statusErr != nil {
				log.Error(statusErr, "Failed to update Certification status after Workflow creation failure")
			}
			return "", fmt.Errorf("failed to create Workflow %s: %w", workflowName, err)
		}
	}

	return workflowName, nil
}

// ResolveOptions merges per-category overrides with global defaults.
// Returns a flat CategoryOptions with all values resolved.
func ResolveOptions(global *nvcrev1alpha1.CategoryOptions, override *nvcrev1alpha1.CategoryOptions) nvcrev1alpha1.CategoryOptions {
	resolved := *global
	if override == nil {
		return resolved
	}
	if override.NodesPerJob != nil {
		resolved.NodesPerJob = override.NodesPerJob
	}
	if override.EnableCheckpoint != nil {
		resolved.EnableCheckpoint = override.EnableCheckpoint
	}
	if override.MaxSteps != nil {
		resolved.MaxSteps = override.MaxSteps
	}
	if override.ExitDurationMins != nil {
		resolved.ExitDurationMins = override.ExitDurationMins
	}
	if override.GpusPerNode != nil {
		resolved.GpusPerNode = override.GpusPerNode
	}
	if override.MlnxPerNode != nil {
		resolved.MlnxPerNode = override.MlnxPerNode
	}
	if override.Resources != nil {
		resolved.Resources = override.Resources
	}
	if override.EnableMNNVL != nil {
		resolved.EnableMNNVL = override.EnableMNNVL
	}
	if len(override.ImagePullSecrets) > 0 {
		resolved.ImagePullSecrets = override.ImagePullSecrets
	}
	if override.StorageClassName != nil {
		resolved.StorageClassName = override.StorageClassName
	}
	if override.SaveInterval != nil {
		resolved.SaveInterval = override.SaveInterval
	}
	if override.SaveRetainInterval != nil {
		resolved.SaveRetainInterval = override.SaveRetainInterval
	}
	if override.SaveTopK != nil {
		resolved.SaveTopK = override.SaveTopK
	}
	if override.StorageSize != "" {
		resolved.StorageSize = override.StorageSize
	}
	if override.TestScale != "" {
		resolved.TestScale = override.TestScale
	}
	if override.MaxBytes != "" {
		resolved.MaxBytes = override.MaxBytes
	}
	if override.NumIterations != nil {
		resolved.NumIterations = override.NumIterations
	}
	if override.NumCycles != nil {
		resolved.NumCycles = override.NumCycles
	}
	if len(override.Thresholds) > 0 {
		resolved.Thresholds = override.Thresholds
	}
	if override.MaxConcurrent != nil {
		resolved.MaxConcurrent = override.MaxConcurrent
	}
	if override.MinGroupSize != nil {
		resolved.MinGroupSize = override.MinGroupSize
	}
	if override.RepeatCount != nil {
		resolved.RepeatCount = override.RepeatCount
	}
	if override.MaxRestarts != nil {
		resolved.MaxRestarts = override.MaxRestarts
	}
	if override.TimeoutPerJob != "" {
		resolved.TimeoutPerJob = override.TimeoutPerJob
	}
	if override.MeasurementTimeout != "" {
		resolved.MeasurementTimeout = override.MeasurementTimeout
	}
	return resolved
}

// dropUnderCapacityNodes removes nodes that cannot supply gpusPerNode before
// the job is sized. A node reporting fewer allocatable GPUs than the per-node
// request can never schedule the workload's pods, so counting it inflates
// nodesPerJob and the resulting groups hang Pending forever (issue #82). It
// runs after gpusPerNode is resolved, because that value derives from the GPU
// architecture of the arch-filter survivors plus any explicit option — it
// never depends on the node count, so filtering by it cannot loop back into
// its own inputs.
//
// When no node can supply the request this is a hard error naming the
// requirement and the best the fleet offers, never a quiet downsize: a job
// sized for nodes that do not exist would partition into groups that cannot
// schedule. The dropped names are discarded here for the same reason cordoned
// names are (see createWorkflowForCategory): the Workflow repeats the filter
// against the rendered spec and records them on its status, which is where the
// report reads coverage from.
//
// The two tiers deliberately read the requirement from different sources.
// This tier filters with the resolved catalog-default/option gpusPerNode; the
// Workflow filters with the post-override rendered request (see
// workloadGPUsPerNode), because overrides are only applied at that tier. An
// override that raises the request therefore surfaces as a workflow-tier
// PartitionError rather than this fail-fast — the Workflow's check is the
// authoritative one, this one exists so the job is sized against nodes that
// can run the common case.
func dropUnderCapacityNodes(nodes []corev1.Node, cat nvcrev1alpha1.CertificateCategory, gpusPerNode int32) ([]corev1.Node, error) {
	capableNodes, capacityExcluded := filterNodesByGPUCapacity(nodes, gpusPerNode)
	if len(capableNodes) == 0 {
		return nil, fmt.Errorf(
			"%s/%s: no node can supply the %d nvidia.com/gpu the workload requests per node;"+
				" best available is %d across %d matching node(s)."+
				" Lower gpusPerNode or target nodes with more GPUs",
			cat.Domain, cat.Variant, gpusPerNode,
			maxAllocatableGPUs(capacityExcluded), len(capacityExcluded))
	}
	return capableNodes, nil
}

// resolveNodesPerJob determines the nodesPerJob for a category.
// When explicitly set, clamps to the largest valid node count <= min(requested, available).
// Otherwise auto-selects the largest valid node count <= available nodes.
// "Valid" means satisfying the entry's constraints (minGPUs, TP×PP divisibility).
// When no constraints are defined, uses all available nodes.
func resolveNodesPerJob(nodes []corev1.Node, cat nvcrev1alpha1.CertificateCategory, opts nvcrev1alpha1.CategoryOptions, entry *catalog.Entry, gpusPerNode int32, gpuArch string) (int32, error) {
	n := int32(len(nodes))
	if n == 0 {
		return 0, fmt.Errorf("no matching nodes for %s/%s", cat.Domain, cat.Variant)
	}

	ceiling := n
	if opts.NodesPerJob != nil {
		ceiling = min(*opts.NodesPerJob, n)
	}

	// Use entry constraints to find the largest valid node count.
	if entry != nil && entry.MaxValidNodes != nil {
		best := entry.MaxValidNodes(ceiling, gpusPerNode, gpuArch)
		if best == 0 {
			return 0, fmt.Errorf("%s/%s: no valid node count <= %d satisfies model constraints (gpusPerNode=%d, arch=%s)",
				cat.Domain, cat.Variant, ceiling, gpusPerNode, gpuArch)
		}
		return best, nil
	}

	// No constraints — use all available nodes up to ceiling.
	return ceiling, nil
}

// pruneAppliedOverrides removes applied overlays from the spec, keeping
// only unresolved ones for the WorkflowController to handle.
func pruneAppliedOverrides(spec *nvcrev1alpha1.WorkflowSpec, applied []nvcrev1alpha1.AppliedOverride) {
	if len(applied) == 0 {
		return
	}
	appliedSet := make(map[int]bool, len(applied))
	for _, a := range applied {
		appliedSet[a.Index] = true
	}
	remaining := make([]nvcrev1alpha1.OverrideSpec, 0, len(spec.Overrides)-len(applied))
	for i, o := range spec.Overrides {
		if !appliedSet[i] {
			remaining = append(remaining, o)
		}
	}
	spec.Overrides = remaining
}

// pruneUnmatchableOverrides removes overrides whose when conditions cannot
// match the detected platform and GPU architecture. Since these are known at
// Certification build time, non-matching overrides are noise in the Workflow.
func pruneUnmatchableOverrides(spec *nvcrev1alpha1.WorkflowSpec, octx OverrideContext) {
	remaining := make([]nvcrev1alpha1.OverrideSpec, 0, len(spec.Overrides))
	for _, o := range spec.Overrides {
		matches, err := matchesWhen(o.When, octx)
		if err != nil || matches {
			remaining = append(remaining, o)
		}
	}
	spec.Overrides = remaining
}

// derefBool returns the value pointed to by p, or false if p is nil.
func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// setCertificationInProgress sets the Certification to InProgress state.
func (r *CertificationReconciler) setCertificationInProgress(ctx context.Context, certification *nvcrev1alpha1.Certification, reason, message string) error {
	return r.setExclusiveCondition(ctx, certification, nvcrev1alpha1.CertificationInProgress, reason, message)
}

// recoverIfWorkflowStillRunning checks child Workflows of a terminal Certification.
// If any Workflow is still running (e.g., restarted after a failed iteration with
// repeatCount), it resets the Certification and category status back to InProgress
// so reconciliation continues. Returns true if recovery occurred.
func (r *CertificationReconciler) recoverIfWorkflowStillRunning(ctx context.Context, certification *nvcrev1alpha1.Certification) bool {
	log := logf.FromContext(ctx)

	for i := range certification.Status.CategoryStatuses {
		catStatus := &certification.Status.CategoryStatuses[i]
		if catStatus.WorkflowRef == nil || catStatus.Status != categoryStatusFailed {
			continue
		}

		wf := &nvcrev1alpha1.Workflow{}
		ns := catStatus.WorkflowRef.Namespace
		if ns == "" {
			ns = certification.Namespace
		}
		if err := r.Get(ctx, client.ObjectKey{Name: catStatus.WorkflowRef.Name, Namespace: ns}, wf); err != nil {
			continue
		}

		// If the Workflow is not terminal (InProgress), the Certification's Failed
		// state is stale — the Workflow restarted a new iteration.
		wfFailed := meta.FindStatusCondition(wf.Status.Conditions, nvcrev1alpha1.WorkflowFailed)
		wfSucceeded := meta.FindStatusCondition(wf.Status.Conditions, nvcrev1alpha1.WorkflowSucceeded)
		wfTerminal := (wfFailed != nil && wfFailed.Status == metav1.ConditionTrue) ||
			(wfSucceeded != nil && wfSucceeded.Status == metav1.ConditionTrue)

		if !wfTerminal {
			log.Info("Recovering Certification from stale Failed state — Workflow is still running",
				"workflow", catStatus.WorkflowRef.Name, "category", catStatus.Domain+"/"+catStatus.Variant)
			catStatus.Status = categoryStatusInProgress
			if err := r.setCertificationInProgress(ctx, certification, ReasonWorkflowRunning,
				"Recovered: Workflow still running"); err != nil {
				log.Error(err, "Failed to recover Certification to InProgress")
				return false
			}
			return true
		}
	}
	return false
}

// isTerminal returns true if the Certification is in a terminal state
// (Succeeded or Failed). Matches the pattern used by Workflow and Job controllers.
func (r *CertificationReconciler) isTerminal(certification *nvcrev1alpha1.Certification) bool {
	if cond := meta.FindStatusCondition(certification.Status.Conditions, nvcrev1alpha1.CertificationSucceeded); cond != nil && cond.Status == metav1.ConditionTrue {
		return true
	}
	if cond := meta.FindStatusCondition(certification.Status.Conditions, nvcrev1alpha1.CertificationFailed); cond != nil && cond.Status == metav1.ConditionTrue {
		return true
	}
	return false
}

// setCertificationSucceeded sets the Certification to Succeeded state.
func (r *CertificationReconciler) setCertificationSucceeded(ctx context.Context, certification *nvcrev1alpha1.Certification, reason, message string) error {
	return r.setExclusiveCondition(ctx, certification, nvcrev1alpha1.CertificationSucceeded, reason, message)
}

// setCertificationFailed sets the Certification to Failed state.
func (r *CertificationReconciler) setCertificationFailed(ctx context.Context, certification *nvcrev1alpha1.Certification, reason, message string) error {
	return r.setExclusiveCondition(ctx, certification, nvcrev1alpha1.CertificationFailed, reason, message)
}

// setExclusiveCondition sets one condition True and all others False (mutually exclusive).
func (r *CertificationReconciler) setExclusiveCondition(ctx context.Context, certification *nvcrev1alpha1.Certification, conditionType, reason, message string) error {
	changed, err := setExclusiveStatusCondition(ctx, r.Client, certification,
		func(c *nvcrev1alpha1.Certification) *[]metav1.Condition { return &c.Status.Conditions },
		[]string{
			nvcrev1alpha1.CertificationInProgress,
			nvcrev1alpha1.CertificationSucceeded,
			nvcrev1alpha1.CertificationFailed,
		},
		conditionType, reason, message,
	)
	if err != nil {
		return err
	}
	if changed {
		logf.FromContext(ctx).Info("Certification status updated", "status", conditionType, "reason", reason)
	}
	return nil
}

// getWorkflowName returns the name for the Workflow created for a given category.
// When per-category options are set, a deterministic suffix is appended so that
// duplicate domain/variant entries with different options get unique Workflows.
func (r *CertificationReconciler) getWorkflowName(certification *nvcrev1alpha1.Certification, category nvcrev1alpha1.CertificateCategory) string {
	raw := fmt.Sprintf("%s-%s-%s", certification.Name, category.Domain, category.Variant)
	if opts := category.Options; opts != nil {
		raw += categoryOptionsSuffix(opts)
	}
	return naming.Truncate(raw, naming.MaxWorkflowNameLen)
}

// categoryOptionsSuffix returns a deterministic string from the category options
// that distinguishes duplicate domain/variant entries. Uses short human-readable
// tokens where possible; Truncate() hashes if the result is too long.
func categoryOptionsSuffix(opts *nvcrev1alpha1.CategoryOptions) string {
	var parts []string
	if opts.NodesPerJob != nil {
		parts = append(parts, fmt.Sprintf("n%d", *opts.NodesPerJob))
	}
	if opts.EnableMNNVL != nil {
		if *opts.EnableMNNVL {
			parts = append(parts, "mnnvl")
		} else {
			parts = append(parts, "nomnnvl")
		}
	}
	if opts.TestScale != "" {
		parts = append(parts, opts.TestScale)
	}
	if opts.EnableCheckpoint != nil && *opts.EnableCheckpoint {
		parts = append(parts, "ckpt")
	}
	if opts.MaxSteps != nil {
		parts = append(parts, fmt.Sprintf("s%d", *opts.MaxSteps))
	}
	if opts.ExitDurationMins != nil {
		parts = append(parts, fmt.Sprintf("e%d", *opts.ExitDurationMins))
	}
	if opts.GpusPerNode != nil {
		parts = append(parts, fmt.Sprintf("g%d", *opts.GpusPerNode))
	}
	if len(parts) == 0 {
		return ""
	}
	return "-" + strings.Join(parts, "-")
}

// getRequeueInterval returns the configured requeue interval or the default.
func (r *CertificationReconciler) getRequeueInterval() time.Duration {
	if r.WorkflowRequeueInterval > 0 {
		return r.WorkflowRequeueInterval
	}
	return defaultCertificationRequeueInterval
}

// handleDeletion handles the cleanup when a Certification is being deleted.
func (r *CertificationReconciler) handleDeletion(ctx context.Context, certification *nvcrev1alpha1.Certification) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(certification, certificationFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("Handling deletion of Certification")

	// Delete all owned Workflows
	for _, catStatus := range certification.Status.CategoryStatuses {
		if catStatus.WorkflowRef == nil {
			continue
		}
		ns := catStatus.WorkflowRef.Namespace
		if ns == "" {
			ns = certification.Namespace
		}
		workflow := &nvcrev1alpha1.Workflow{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: catStatus.WorkflowRef.Name}, workflow); err == nil {
			// Only delete Workflows this Certification actually created. A
			// same-named foreign Workflow (a name collision, or a stale ref)
			// must survive the finalizer.
			if !metav1.IsControlledBy(workflow, certification) {
				log.Info("Skipping Workflow not controlled by this Certification", "name", catStatus.WorkflowRef.Name)
				continue
			}
			log.Info("Deleting owned Workflow", "name", catStatus.WorkflowRef.Name)
			// The UID precondition closes the window between the ownership check
			// above and this delete: if the owned Workflow was replaced by a
			// same-named foreign one in between, the API server rejects the
			// delete with a conflict and we leave the newcomer alone.
			if err := r.Delete(ctx, workflow, client.Preconditions{UID: new(workflow.UID)}); err != nil &&
				!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete Workflow %s: %w", catStatus.WorkflowRef.Name, err)
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get Workflow %s for deletion: %w", catStatus.WorkflowRef.Name, err)
		}
	}

	log.Info("Removing finalizer from Certification")
	controllerutil.RemoveFinalizer(certification, certificationFinalizer)
	if err := r.Update(ctx, certification); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// derefInt32 returns the value pointed to by p, or 0 if p is nil.
func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// warnf emits a Warning event if the Recorder is configured. Every
// Certification-tier event is a warning; the Workflow reconciler's eventf
// takes an explicit type because it emits Normal events too.
//
// Safe to call when Recorder is nil (e.g. in unit tests, or any embedding that
// constructs CertificationReconciler directly).
func (r *CertificationReconciler) warnf(obj runtime.Object, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, reason, reason, messageFmt, args...)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CertificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nvcrev1alpha1.Certification{}).
		Owns(&nvcrev1alpha1.Workflow{}).
		Named("certification").
		WithOptions(controlleropts.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}
