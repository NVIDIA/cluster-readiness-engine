// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleropts "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/naming"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor/cel"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/threshold"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

const (
	// jobFinalizer is the finalizer added to Job resources to ensure cleanup
	jobFinalizer = "nvcre.nvidia.com/finalizer"

	// workloadRequeueInterval is the safety-net requeue interval for workload status polling.
	// Primary status updates are event-driven via Owns(), but this ensures eventual consistency.
	workloadRequeueInterval = 15 * time.Second

	// defaultGoodputSampleInterval matches the default in the GoodputMeasurement controller.
	// Used as a stall detection buffer when the GM spec doesn't set sampleInterval.
	defaultGoodputSampleInterval = 60 * time.Second

	// defaultStartupStallTimeout is the time to wait after application start
	// for the first training step before declaring a startup stall.
	defaultStartupStallTimeout = 20 * time.Minute

	// Job tier reason constants are in helpers.go.

	// defaultMeasurementTimeout is how long after the Job succeeds to wait for
	// measurement data before failing threshold validation. Prevents indefinite
	// requeue when measurements never arrive (e.g., log parsing failure).
	defaultMeasurementTimeout = 5 * time.Minute

	// reasonMeasurementTimeout indicates threshold validation failed because
	// measurement data did not arrive within the allowed window.
	reasonMeasurementTimeout = "MeasurementTimeout"

	// reasonThresholdsMet indicates threshold validation passed explicitly.
	reasonThresholdsMet = "ThresholdsMet"
)

// JobReconciler reconciles a Job object
type JobReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Clientset      *kubernetes.Clientset
	NodeDiscoverer *nodemonitor.NodeDiscoverer
	Recorder       events.EventRecorder
	// MaxConcurrentReconciles bounds the number of Job objects reconciled concurrently.
	MaxConcurrentReconciles int

	// WorkloadRequeueInterval is how often to poll workload status when job is in progress.
	// If zero, defaults to workloadRequeueInterval (15s).
	WorkloadRequeueInterval time.Duration

	// MeasurementTimeout is how long after the Job succeeds to wait for measurement
	// data before failing threshold validation. If zero, defaults to defaultMeasurementTimeout (5m).
	MeasurementTimeout time.Duration

	// detectorCache caches compiled CEL detectors by expression string to avoid
	// recompiling on every reconcile. Protected by detectorCacheMu.
	detectorCache   map[string]*cel.Detector
	detectorCacheMu sync.Mutex
}

// getWorkloadRequeueInterval returns the effective requeue interval for workload status polling.
func (r *JobReconciler) getWorkloadRequeueInterval() time.Duration {
	if r.WorkloadRequeueInterval > 0 {
		return r.WorkloadRequeueInterval
	}
	return workloadRequeueInterval
}

// getMeasurementTimeout returns the effective timeout for waiting on measurement data
// after a Job has succeeded. Priority: Job.Spec > reconciler field > default (5m).
func (r *JobReconciler) getMeasurementTimeout(job *nvcrev1alpha1.Job) time.Duration {
	if job.Spec.MeasurementTimeout != nil && job.Spec.MeasurementTimeout.Duration > 0 {
		return job.Spec.MeasurementTimeout.Duration
	}
	if r.MeasurementTimeout > 0 {
		return r.MeasurementTimeout
	}
	return defaultMeasurementTimeout
}

// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=jobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=jobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=trainer.kubeflow.org,resources=trainjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=trainer.kubeflow.org,resources=trainjobs/status,verbs=get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *JobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	startTime := time.Now()
	log := logf.FromContext(ctx).WithValues("job", req.Name, "namespace", req.Namespace)
	ctx = logf.IntoContext(ctx, log)

	log.V(1).Info("Starting reconciliation")

	// Fetch the Job instance
	job := &nvcrev1alpha1.Job{}
	if err := r.Get(ctx, req.NamespacedName, job); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Job resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		recordReconcile(req.Namespace, req.Name, "", "error")
		return ctrl.Result{}, fmt.Errorf("failed to get Job: %w", err)
	}

	workflow := job.Labels["nvcre.nvidia.com/workflow"]

	// Defer metrics recording
	defer func() {
		duration := time.Since(startTime).Seconds()
		observeReconcileDuration(job.Namespace, job.Name, workflow, duration)
		log.V(1).Info("Reconciliation complete", "duration_seconds", duration)
	}()

	// Handle deletion
	if !job.DeletionTimestamp.IsZero() {
		log.Info("Job is being deleted, handling cleanup")
		result, err := r.handleDeletion(ctx, job)
		if err != nil {
			recordReconcile(job.Namespace, job.Name, workflow, "error")
		} else {
			recordReconcile(job.Namespace, job.Name, workflow, "success")
		}
		return result, err
	}

	// Add finalizer if not present. A successful add ends this reconcile; the
	// resulting watch event drives the next one.
	if added, err := ensureFinalizer(ctx, r.Client, job, jobFinalizer); err != nil || added {
		if err != nil {
			recordReconcile(job.Namespace, job.Name, workflow, "error")
			return ctrl.Result{}, err
		}
		recordReconcile(job.Namespace, job.Name, workflow, "success")
		return ctrl.Result{}, nil
	}

	// Reconcile the workload
	result, err := r.reconcileWorkload(ctx, job)
	if err != nil {
		recordReconcile(job.Namespace, job.Name, workflow, "error")
		return result, err
	}

	// Check for hardware failures if monitoring is configured
	// and job is not in a terminal state
	if job.Spec.NodeHealthMonitor != nil && !r.isTerminalState(job) {
		if hwResult, hwErr := r.checkNodeHealth(ctx, job); hwErr != nil {
			log.Error(hwErr, "Failed to check node health",
				"expression", job.Spec.NodeHealthMonitor.CEL.Expression)
			// Don't fail reconciliation, just log the error
		} else if hwResult.RequeueAfter > 0 {
			recordReconcile(job.Namespace, job.Name, workflow, "requeue")
			return hwResult, nil
		}
	}

	recordReconcile(job.Namespace, job.Name, workflow, "success")
	return result, nil
}

// handleDeletion handles the cleanup when a Job is being deleted
func (r *JobReconciler) handleDeletion(ctx context.Context, job *nvcrev1alpha1.Job) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(job, jobFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("Handling deletion of Job")

	// Delete the associated workload if we have a reference to it
	if err := r.deleteWorkloadByRef(ctx, job); err != nil {
		return ctrl.Result{}, err
	}

	// Pod-drain barrier (issue #121): the workload delete above only starts
	// the teardown — pods keep running (Terminating) after the workload
	// object is gone. Waiters on Job deletion — the Workflow's handleDeletion
	// and its Job-NotFound group handling — treat "Job object gone" as the
	// signal that it is safe to delete the dependency resources
	// (ComputeDomain, ResourceClaimTemplate) whose DRA allocations those pods
	// still hold. Keep the finalizer until the pods are gone so that signal
	// is truthful; podDrainGracePeriod (measured from the Job's own
	// deletionTimestamp inside drainStart) bounds the wait so a pod stuck
	// Terminating cannot wedge Job deletion forever.
	if shouldWaitForPodDrain(ctx, r.Client, job) {
		log.Info("Waiting for workload pods to terminate before removing Job finalizer")
		return ctrl.Result{RequeueAfter: r.getWorkloadRequeueInterval()}, nil
	}

	// Clean up metrics for this job
	cleanupJobMetrics(job.Namespace, job.Name)

	// Remove finalizer
	log.Info("Removing finalizer from Job")
	patch := client.MergeFrom(job.DeepCopy())
	controllerutil.RemoveFinalizer(job, jobFinalizer)
	if err := r.Patch(ctx, job, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	log.Info("Job deletion complete")
	return ctrl.Result{}, nil
}

// deleteWorkloadByRef deletes the workload referenced by the Job's status.
// Returns nil if the workload is already gone or the adapter cannot be resolved.
func (r *JobReconciler) deleteWorkloadByRef(ctx context.Context, job *nvcrev1alpha1.Job) error {
	ref := job.Status.WorkloadRef
	if ref == nil {
		return nil
	}

	adapter, err := workload.ForSpec(&job.Spec.Workload)
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to resolve workload adapter")
		return nil // non-fatal: adapter mismatch shouldn't block cleanup
	}

	obj := adapter.NewObject()
	ns := ref.Namespace
	if ns == "" {
		ns = job.Namespace
	}

	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to get workload %s/%s: %w", ref.Kind, ref.Name, err)
	}

	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete workload %s/%s: %w", ref.Kind, ref.Name, err)
	}
	return nil
}

// reconcileWorkload ensures the workload exists and is up to date
func (r *JobReconciler) reconcileWorkload(ctx context.Context, job *nvcrev1alpha1.Job) (ctrl.Result, error) {
	// If the Job is already in a terminal state, preserve the workload and
	// its pods for log inspection (both success and failure). Cleanup
	// happens via owner reference cascade when the Certification is deleted.
	if r.isTerminalState(job) {
		if isJobAwaitingThresholdEvaluation(job) {
			if r.checkPerformanceThresholds(ctx, job) {
				return ctrl.Result{RequeueAfter: r.getWorkloadRequeueInterval()}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	// If we already have a workload reference in status, check its status
	if job.Status.WorkloadRef != nil {
		// These stay non-fatal so a measurement problem cannot block workload
		// status updates. Correctness is preserved by checkPerformanceThresholds,
		// which fails closed when a configured threshold has no measured value.
		// The event exists so a persistent creation failure is visible rather
		// than only appearing later as an opaque MeasurementTimeout.
		if err := r.ensureGoodputMeasurement(ctx, job); err != nil {
			logf.FromContext(ctx).Error(err, "Failed to ensure GoodputMeasurement")
			r.warnf(job, ReasonMeasurementCreationError,
				"Failed to ensure GoodputMeasurement: %v", err)
		}
		if err := r.ensureBandwidthMeasurement(ctx, job); err != nil {
			logf.FromContext(ctx).Error(err, "Failed to ensure BandwidthMeasurement")
			r.warnf(job, ReasonMeasurementCreationError,
				"Failed to ensure BandwidthMeasurement: %v", err)
		}
		return r.updateStatusFromWorkload(ctx, job)
	}

	// No workload reference yet — create it from spec
	return r.createWorkloadFromSpec(ctx, job)
}

// createWorkloadFromSpec creates a new workload resource from the Job's inline workload spec.
func (r *JobReconciler) createWorkloadFromSpec(ctx context.Context, job *nvcrev1alpha1.Job) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	adapter, err := workload.ForSpec(&job.Spec.Workload)
	if err != nil {
		if statusErr := r.setJobFailed(ctx, job, ReasonWorkloadCreationError,
			fmt.Sprintf("Unsupported workload type: %v", err)); statusErr != nil {
			log.Error(statusErr, "Failed to update Job status")
		}
		return ctrl.Result{}, fmt.Errorf("failed to resolve workload adapter: %w", err)
	}

	// Deep-copy the spec and inject the NVCRE pod label automatically
	specCopy := job.Spec.Workload.DeepCopy()
	adapter.InjectPodLabel(specCopy, "nvcre.nvidia.com/job", job.Name)

	workloadName := r.getWorkloadName(job)
	obj, err := adapter.Build(workloadName, job.Namespace, specCopy)
	if err != nil {
		if statusErr := r.setJobFailed(ctx, job, ReasonWorkloadCreationError,
			fmt.Sprintf("Failed to build workload: %v", err)); statusErr != nil {
			log.Error(statusErr, "Failed to update Job status")
		}
		return ctrl.Result{}, fmt.Errorf("failed to build workload: %w", err)
	}

	// Set labels for identification
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["app.kubernetes.io/managed-by"] = "nvcre"
	labels["nvcre.nvidia.com/job"] = job.Name
	obj.SetLabels(labels)

	// Set owner reference so the workload is garbage collected when the Job is deleted
	if err := controllerutil.SetControllerReference(job, obj, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner reference on workload: %w", err)
	}

	gvk := adapter.GVK()
	log.Info("Creating workload", "kind", gvk.Kind, "name", workloadName)
	if err := r.Create(ctx, obj); err != nil {
		// Return the error so controller-runtime applies exponential backoff.
		// Do not mark the Job as Failed — creation errors are transient
		// (e.g. webhook denial because TrainingRuntime isn't ready yet).
		log.Error(err, "Failed to create workload, will retry", "kind", gvk.Kind, "name", workloadName)
		r.warnf(job, ReasonWorkloadCreationError,
			"Failed to create %s/%s: %v", gvk.Kind, workloadName, err)
		return ctrl.Result{}, fmt.Errorf("failed to create workload: %w", err)
	}

	// Record workload creation metric
	recordWorkloadCreated(job.Namespace, job.Name, job.Labels["nvcre.nvidia.com/workflow"])
	log.Info("Workload created successfully", "kind", gvk.Kind, "name", workloadName)

	// Store workload reference in status
	job.Status.WorkloadRef = &nvcrev1alpha1.WorkloadReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       workloadName,
		Namespace:  job.Namespace,
	}

	// Set initial InProgress status
	if err := r.setJobInProgress(ctx, job, ReasonWorkloadCreated,
		fmt.Sprintf("Workload %s/%s created and is starting", gvk.Kind, workloadName)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update Job status: %w", err)
	}

	return ctrl.Result{RequeueAfter: r.getWorkloadRequeueInterval()}, nil
}

// updateStatusFromWorkload updates the Job status based on the workload status conditions
func (r *JobReconciler) updateStatusFromWorkload(ctx context.Context, job *nvcrev1alpha1.Job) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	ref := job.Status.WorkloadRef

	adapter, err := workload.ForSpec(&job.Spec.Workload)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to resolve workload adapter: %w", err)
	}

	obj := adapter.NewObject()
	ns := ref.Namespace
	if ns == "" {
		ns = job.Namespace
	}

	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			// Workload was deleted externally — treat as a failure.
			// Check if we should restart from checkpoint (same as WorkloadFailed).
			if r.shouldRestart(ctx, job) {
				log.Info("Workload deleted externally, restarting from checkpoint",
					"kind", ref.Kind, "name", ref.Name)
				return r.restartFromCheckpoint(ctx, job, ref, adapter)
			}
			if err := r.setJobFailed(ctx, job, ReasonWorkloadFailed,
				fmt.Sprintf("Workload %s/%s was deleted", ref.Kind, ref.Name)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update Job status: %w", err)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get workload %s/%s: %w", ref.Kind, ref.Name, err)
	}

	status, err := adapter.GetStatus(obj)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to read workload status: %w", err)
	}

	switch status.Phase {
	case workload.WorkloadFailed:
		// Check if we should restart from checkpoint
		if r.shouldRestart(ctx, job) {
			return r.restartFromCheckpoint(ctx, job, ref, adapter)
		}
		// No restart — mark Job as Failed
		message := fmt.Sprintf("Workload %s/%s has failed", ref.Kind, ref.Name)
		if status.Message != "" {
			message = fmt.Sprintf("Workload %s/%s failed: %s", ref.Kind, ref.Name, status.Message)
		}
		if err := r.setJobFailed(ctx, job, ReasonWorkloadFailed, message); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update Job status: %w", err)
		}
		return ctrl.Result{}, nil

	case workload.WorkloadSucceeded:
		message := fmt.Sprintf("Workload %s/%s completed successfully", ref.Kind, ref.Name)
		if status.Message != "" {
			message = fmt.Sprintf("Workload %s/%s completed: %s", ref.Kind, ref.Name, status.Message)
		}
		if err := r.setJobSucceeded(ctx, job, ReasonWorkloadCompleted, message); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update Job status: %w", err)
		}
		// Check performance thresholds (independent of execution state).
		// The Job is already Succeeded; this may additionally set the
		// ValidationFailed condition if thresholds are not met.
		// If measurements aren't ready yet, requeue to check later.
		if r.checkPerformanceThresholds(ctx, job) {
			return ctrl.Result{RequeueAfter: r.getWorkloadRequeueInterval()}, nil
		}
		return ctrl.Result{}, nil

	default:
		// Check for stall before marking as running.
		if stalled, stallMsg := r.checkStallTimeout(ctx, job); stalled {
			if r.shouldRestart(ctx, job) {
				return r.restartFromCheckpoint(ctx, job, ref, adapter)
			}
			if err := r.setJobFailed(ctx, job, ReasonWorkloadStalled, stallMsg); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update Job status: %w", err)
			}
			return ctrl.Result{}, nil
		}

		log.V(1).Info("Workload is running", "kind", ref.Kind, "name", ref.Name)
		if err := r.setJobInProgress(ctx, job, ReasonWorkloadRunning,
			fmt.Sprintf("Workload %s/%s is running", ref.Kind, ref.Name)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update Job status: %w", err)
		}
		return ctrl.Result{RequeueAfter: r.getWorkloadRequeueInterval()}, nil
	}
}

// shouldRestart checks whether a failed Job should be restarted from checkpoint.
// It returns true only if the Job has a Checkpoint config, restarts remain, and a checkpoint exists.
func (r *JobReconciler) shouldRestart(ctx context.Context, job *nvcrev1alpha1.Job) bool {
	if job.Spec.Checkpoint == nil {
		return false
	}
	maxRestarts := int32(0)
	if job.Spec.Checkpoint.MaxRestarts != nil {
		maxRestarts = *job.Spec.Checkpoint.MaxRestarts
	}
	if job.Status.RestartCount >= maxRestarts {
		return false
	}
	return r.hasCheckpoint(ctx, job)
}

// hasCheckpoint returns true if there is a GoodputMeasurement for this Job that has recorded
// at least one checkpoint step (LastCheckpointStep > 0).
func (r *JobReconciler) hasCheckpoint(ctx context.Context, job *nvcrev1alpha1.Job) bool {
	log := logf.FromContext(ctx)
	var measurements nvcrev1alpha1.GoodputMeasurementList
	if err := r.List(ctx, &measurements, matchingJobRef(job.Namespace, job.Name)...); err != nil {
		log.Error(err, "Failed to list GoodputMeasurements")
		return false
	}
	for _, m := range measurements.Items {
		if m.Status.LastCheckpointStep > 0 {
			return true
		}
	}
	return false
}

// checkStallTimeout checks if the workload has stalled based on the configured
// stallMultiplier and step timing from the associated GoodputMeasurement.
// Returns (true, message) if the workload is stalled, (false, "") otherwise.
// Does NOT update Job status.
//
// Two stall modes:
//  1. Startup stall: applicationStartTime is set but no training step has arrived
//     within the startup timeout.
//  2. Training stall: steps were arriving but stopped for longer than
//     stallMultiplier * avgStepTime * logInterval.
//
// The threshold includes the GoodputMeasurement's sample interval to account
// for log parsing lag: LastStepTimestamp reflects the last step the GM parsed,
// not the last step the workload produced. Without this buffer, the stall
// detector fires false positives when the GM hasn't read logs recently.
func (r *JobReconciler) checkStallTimeout(ctx context.Context, job *nvcrev1alpha1.Job) (bool, string) {
	if job.Spec.StallMultiplier == nil {
		return false, ""
	}

	gm := findJobGoodputMeasurement(ctx, r.Client, job)
	if gm == nil {
		return false, ""
	}

	if gm.Status.LastStepTimestamp == nil || gm.Status.AvgStepTimeSec == "" {
		// No training steps yet. Measure elapsed from application start if
		// available, otherwise from measurement start. This catches both
		// post-init stalls (app started but no steps) and pre-init stalls
		// (NCCL hang, crash loop — app never started).
		var since time.Time
		if gm.Status.ApplicationStartTime != nil {
			since = gm.Status.ApplicationStartTime.Time
		} else if gm.Status.StartTime != nil {
			since = gm.Status.StartTime.Time
		} else {
			return false, ""
		}
		timeout := defaultStartupStallTimeout
		if job.Spec.StartupStallTimeoutSeconds != nil {
			timeout = time.Duration(*job.Spec.StartupStallTimeoutSeconds) * time.Second
		}
		// Add 2x sample interval buffer (same rationale as training stall).
		if gm.Spec.SampleInterval != nil {
			timeout += time.Duration(gm.Spec.SampleInterval.Seconds()) * 2 * time.Second
		} else {
			timeout += 2 * defaultGoodputSampleInterval
		}
		elapsed := time.Since(since)
		if elapsed > timeout {
			logf.FromContext(ctx).Info("Startup stall detected",
				"elapsed", elapsed.Seconds(),
				"timeout", timeout.Seconds(),
				"hasApplicationStart", gm.Status.ApplicationStartTime != nil)
			return true, "Workload stalled: no training step observed within startup timeout"
		}
		return false, ""
	}

	avgStepTime := parseStallFloat(gm.Status.AvgStepTimeSec)
	if avgStepTime <= 0 {
		return false, ""
	}

	logInterval := max(gm.Status.LogInterval, 1)
	stallThreshold := float64(*job.Spec.StallMultiplier) * avgStepTime * float64(logInterval)

	// Add 2x the GM's sample interval as a buffer. LastStepTimestamp is only
	// updated when the GM reconciles, so elapsed time grows with wall clock
	// even while the workload is progressing. The 2x factor accounts for the
	// sample interval itself plus scheduling jitter and API server latency
	// that may delay the GM's reconcile beyond its nominal interval.
	if gm.Spec.SampleInterval != nil {
		stallThreshold += 2 * gm.Spec.SampleInterval.Seconds()
	} else {
		stallThreshold += 2 * defaultGoodputSampleInterval.Seconds()
	}

	elapsed := time.Since(gm.Status.LastStepTimestamp.Time).Seconds()

	if elapsed > stallThreshold {
		logf.FromContext(ctx).Info("Workload stall detected",
			"elapsed", elapsed,
			"threshold", stallThreshold,
			"stallMultiplier", *job.Spec.StallMultiplier,
			"avgStepTime", avgStepTime,
			"logInterval", logInterval)
		return true, "Workload stalled: no training step within the configured threshold"
	}

	return false, ""
}

// parseStallFloat parses a string to float64 for stall detection, returning 0 on error.
func parseStallFloat(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	_, _ = fmt.Sscanf(s, "%f", &f)
	return f
}

// maxBusBandwidth returns the maximum bus bandwidth across all message sizes in the results.
// Returns 0 if no results are present.
func maxBusBandwidth(results []nvcrev1alpha1.BandwidthResult) float64 {
	var max float64
	for _, r := range results {
		bw := parseStallFloat(r.BusBW)
		if bw > max {
			max = bw
		}
	}
	return max
}

// checkPerformanceThresholds evaluates CEL-based thresholds after workload success.
// Thresholds are on Job.Spec.Thresholds (map[string]string). Measured values are
// collected from BandwidthMeasurement and GoodputMeasurement status fields.
// If any threshold is violated, the ValidationFailed condition is set on the Job.
// Returns true if measurements are pending and the caller should requeue.
func (r *JobReconciler) checkPerformanceThresholds(ctx context.Context, job *nvcrev1alpha1.Job) bool {
	if len(job.Spec.Thresholds) == 0 {
		return false
	}
	log := logf.FromContext(ctx)
	measured := collectJobMeasuredValues(ctx, r.Client, job)

	if missing := missingJobThresholdKeys(job.Spec.Thresholds, measured); len(missing) > 0 {
		if r.measurementTimedOut(job) {
			log.Info("Measurement timeout exceeded, failing threshold validation",
				"missingKeys", missing, "timeout", r.getMeasurementTimeout(job))
			if err := r.setJobValidationStatus(ctx, job, metav1.ConditionTrue, reasonMeasurementTimeout,
				fmt.Sprintf("Measurement data not available within %s for keys: %v",
					r.getMeasurementTimeout(job), missing)); err != nil {
				log.Error(err, "Failed to set ValidationFailed for measurement timeout")
			}
			return false
		}
		log.V(1).Info("Threshold configured but measurement data not yet available, requeueing",
			"missingKeys", missing)
		return true
	}

	if violations := threshold.EvaluateAll(job.Spec.Thresholds, measured); len(violations) > 0 {
		v := violations[0]
		log.Info("Threshold check failed", "key", v.Key, "reason", v.Reason, "message", v.Message)
		if err := r.setJobValidationStatus(ctx, job, metav1.ConditionTrue, v.Reason, v.Message); err != nil {
			log.Error(err, "Failed to set ValidationFailed")
		}
		return false
	}

	if err := r.setJobValidationStatus(ctx, job, metav1.ConditionFalse, reasonThresholdsMet, "All performance thresholds satisfied"); err != nil {
		log.Error(err, "Failed to set ValidationFailed=False for threshold pass")
	}
	return false
}

// measurementTimedOut returns true if the Job has been in Succeeded state longer
// than the measurement timeout, meaning we've waited long enough for data.
func (r *JobReconciler) measurementTimedOut(job *nvcrev1alpha1.Job) bool {
	succeededCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobSucceeded)
	if succeededCond == nil || succeededCond.Status != metav1.ConditionTrue {
		return false
	}
	return time.Since(succeededCond.LastTransitionTime.Time) > r.getMeasurementTimeout(job)
}

// maxAlgBandwidth returns the maximum algorithm bandwidth across all message sizes.
func maxAlgBandwidth(results []nvcrev1alpha1.BandwidthResult) float64 {
	var m float64
	for _, r := range results {
		bw := parseStallFloat(r.AlgBW)
		if bw > m {
			m = bw
		}
	}
	return m
}

// setJobValidationFailed sets the ValidationFailed condition independently of execution state.
// This mirrors setJobHardwareFailed — the condition is additive, not exclusive.
func (r *JobReconciler) setJobValidationStatus(ctx context.Context, job *nvcrev1alpha1.Job, status metav1.ConditionStatus, reason, message string) error {
	changed := false
	err := updateStatusWithRetry(ctx, r.Client, job, func(j *nvcrev1alpha1.Job) bool {
		if status == metav1.ConditionTrue && len(j.Status.FailedNodes) == 0 {
			j.Status.FailedNodes = noderesults.NodesWithFailureDetails(groupNodeNames(j), ReasonThresholdViolation, message)
		}
		c := meta.SetStatusCondition(&j.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.JobValidationFailed,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: j.Generation,
		})
		changed = changed || c
		return c
	})
	if err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}
	if changed {
		logf.FromContext(ctx).Info("ValidationFailed condition set", "reason", reason)
	}
	return nil
}

// restartFromCheckpoint deletes the failed workload, increments the restart count,
// clears the workload reference, and requeues so that reconcileWorkload() will
// create a fresh workload from spec on the next loop.
func (r *JobReconciler) restartFromCheckpoint(ctx context.Context, job *nvcrev1alpha1.Job, ref *nvcrev1alpha1.WorkloadReference, adapter workload.Adapter) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Delete the failed workload
	obj := adapter.NewObject()
	ns := ref.Namespace
	if ns == "" {
		ns = job.Namespace
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); err == nil {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete workload for restart: %w", err)
		}
	}

	// Clear workload reference and increment restart count
	job.Status.WorkloadRef = nil
	job.Status.RestartCount++

	// Reset stall detection: clear step state and reset StartTime so
	// the startup stall timeout starts fresh from the restart.
	if gm := findJobGoodputMeasurement(ctx, r.Client, job); gm != nil {
		gm.Status.LastStepTimestamp = nil
		gm.Status.AvgStepTimeSec = ""
		gm.Status.ApplicationStartTime = nil
		now := metav1.Now()
		gm.Status.StartTime = &now
		if err := r.Status().Update(ctx, gm); err != nil {
			log.Error(err, "Failed to reset stall detection state on GoodputMeasurement")
		}
	}

	maxRestarts := int32(0)
	if job.Spec.Checkpoint.MaxRestarts != nil {
		maxRestarts = *job.Spec.Checkpoint.MaxRestarts
	}

	log.Info("Restarting workload from checkpoint",
		"restartCount", job.Status.RestartCount,
		"maxRestarts", maxRestarts)

	// Stay InProgress with restart reason
	if err := r.setJobInProgress(ctx, job, "WorkloadRestarting",
		fmt.Sprintf("Restarting workload from checkpoint (restart %d/%d)",
			job.Status.RestartCount, maxRestarts)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update Job status: %w", err)
	}

	// Delay before creating the new workload so the GoodputMeasurement
	// controller has time to read final training logs from the terminating
	// pods. Use 2× the goodput sampleInterval as the grace period.
	grace := 30 * time.Second
	if job.Spec.GoodputMeasurement != nil && job.Spec.GoodputMeasurement.SampleInterval != nil {
		grace = 2 * job.Spec.GoodputMeasurement.SampleInterval.Duration
	}

	// Next reconcile: reconcileWorkload() → no workloadRef → createWorkloadFromSpec()
	return ctrl.Result{RequeueAfter: grace}, nil
}

// setJobInProgress sets the Job status to InProgress
func (r *JobReconciler) setJobInProgress(ctx context.Context, job *nvcrev1alpha1.Job, reason, message string) error {
	return r.setExclusiveCondition(ctx, job, nvcrev1alpha1.JobInProgress, reason, message)
}

// setJobSucceeded sets the Job status to Succeeded
func (r *JobReconciler) setJobSucceeded(ctx context.Context, job *nvcrev1alpha1.Job, reason, message string) error {
	return r.setExclusiveCondition(ctx, job, nvcrev1alpha1.JobSucceeded, reason, message)
}

// setJobFailed sets the Job status to Failed and captures failure logs.
//
// Both captureFailureLog and FailedNodes are set inside the updateStatusWithRetry
// closure (via the extra function passed to setExclusiveStatusCondition) so that
// on a 409 conflict the retry re-applies them to the freshly-fetched object.
// Setting them on the stale object before the closure would cause them to be
// silently discarded whenever the API server returns a conflict.
func (r *JobReconciler) setJobFailed(ctx context.Context, job *nvcrev1alpha1.Job, reason, message string) error {
	changed, err := setExclusiveStatusCondition(ctx, r.Client, job,
		func(j *nvcrev1alpha1.Job) *[]metav1.Condition { return &j.Status.Conditions },
		[]string{
			nvcrev1alpha1.JobInProgress,
			nvcrev1alpha1.JobSucceeded,
			nvcrev1alpha1.JobFailed,
		},
		nvcrev1alpha1.JobFailed, reason, message,
		func(j *nvcrev1alpha1.Job) bool {
			c := false
			if j.Status.FailureLog == nil {
				r.captureFailureLog(ctx, j)
				c = true
			}
			if len(j.Status.FailedNodes) == 0 {
				j.Status.FailedNodes = noderesults.NodesWithFailureDetails(groupNodeNames(j), ReasonWorkloadFailed, message)
				c = true
			}
			return c
		},
	)
	if err != nil {
		return err
	}
	if changed {
		recordJobStatus(job.Namespace, job.Name, job.Labels["nvcre.nvidia.com/workflow"], "failed")
		logf.FromContext(ctx).Info("Job status updated", "status", nvcrev1alpha1.JobFailed, "reason", reason)
	}
	return nil
}

// failureLogTailLines is the number of log lines requested from a failed pod.
//
// This was 30, which is right for a workload whose last line is the error, and
// wrong for one that prints a structured document. dcgmi diag --json ends with
// closing braces and a metadata block, so 30 lines of a failed run stored 392
// bytes ending in "status" : "Pass" and named no failing test. Measured against
// dcgm 4.5.2 on an A100.
const failureLogTailLines = 800

// failureLogMaxBytes caps what is stored in Job.status.failureLog.tail. The
// requested lines are usually well inside this; the cap only guards a workload
// that prints very long lines.
const failureLogMaxBytes = 32 * 1024

// readLogTail returns the last max bytes of r.
//
// The byte cap has to trim from the front, keeping the end of the stream.
// Reading the first max bytes instead would drop the most recent output, which
// is the part a failure record needs.
func readLogTail(r io.Reader, max int) (string, error) {
	buf := make([]byte, 0, max)
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > max {
				buf = append(buf[:0], buf[len(buf)-max:]...)
			}
		}
		if readErr == io.EOF {
			return string(buf), nil
		}
		if readErr != nil {
			return string(buf), readErr
		}
	}
}

// captureFailureLog finds the pod that caused the failure and stores its last
// N log lines in job.Status.FailureLog. Best-effort: errors are logged, not returned.
func (r *JobReconciler) captureFailureLog(ctx context.Context, job *nvcrev1alpha1.Job) {
	log := logf.FromContext(ctx)
	if r.Clientset == nil {
		return
	}

	// List pods for this Job using the field index for O(1) lookup.
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(job.Namespace),
		client.MatchingFields{nodemonitor.PodNVCREJobIndexField: job.Name},
	); err != nil {
		log.V(1).Info("Failed to list pods for failure log capture", "error", err)
		return
	}

	// Find the pod that caused the failure: first pod with a non-zero exit code,
	// preferring OOMKilled (root cause) over other failures.
	var failedPod *corev1.Pod
	var failedContainer *corev1.ContainerStatus
	for i := range podList.Items {
		pod := &podList.Items[i]
		for j := range pod.Status.ContainerStatuses {
			cs := &pod.Status.ContainerStatuses[j]
			if cs.State.Terminated == nil || cs.State.Terminated.ExitCode == 0 {
				continue
			}
			if failedPod == nil || cs.State.Terminated.Reason == "OOMKilled" {
				failedPod = pod
				failedContainer = cs
			}
		}
	}
	// Record why there is no log rather than leaving the field unset. A Job whose
	// pod was reaped before the failure was recorded (backoff limit exceeded, for
	// example) would otherwise carry no diagnostics at all, and the operator would
	// see a failed Job with nothing to read.
	if failedPod == nil {
		reason := "NoTerminatedContainer"
		tail := "pods for this Job were found, but none had a container terminated with a non-zero exit code"
		if len(podList.Items) == 0 {
			reason = "PodNotFound"
			tail = "no pods for this Job were found when the failure was recorded; the pod was most likely deleted before its logs could be read"
		}
		job.Status.FailureLog = &nvcrev1alpha1.FailureLog{Reason: reason, Tail: tail}
		log.Info("No failed pod available for failure log capture", "reason", reason)
		return
	}

	fl := &nvcrev1alpha1.FailureLog{
		PodName:  failedPod.Name,
		NodeName: failedPod.Spec.NodeName,
		ExitCode: failedContainer.State.Terminated.ExitCode,
		Reason:   failedContainer.State.Terminated.Reason,
	}

	// Fetch tail of logs from the failed container.
	tailLines := int64(failureLogTailLines)
	logOpts := &corev1.PodLogOptions{
		Container: failedContainer.Name,
		TailLines: &tailLines,
	}
	req := r.Clientset.CoreV1().Pods(failedPod.Namespace).GetLogs(failedPod.Name, logOpts)
	stream, err := podlogs.OpenStream(ctx, req)
	if err != nil {
		log.V(1).Info("Failed to stream pod logs for failure capture", "pod", failedPod.Name, "error", err)
		job.Status.FailureLog = fl
		return
	}
	defer stream.Close() //nolint:errcheck

	tail, err := readLogTail(stream, failureLogMaxBytes)
	if err != nil {
		log.V(1).Info("Failed to read pod logs", "pod", failedPod.Name, "error", err)
	}
	fl.Tail = tail
	job.Status.FailureLog = fl
	log.Info("Captured failure log", "pod", failedPod.Name, "node", failedPod.Spec.NodeName,
		"exitCode", fl.ExitCode, "bytes", len(fl.Tail))
}

// setJobHardwareFailed sets the HardwareFailed condition and records the failed nodes.
// This condition is independent of job execution state (InProgress/Succeeded/Failed).
func (r *JobReconciler) setJobHardwareFailed(ctx context.Context, job *nvcrev1alpha1.Job, reason, message string, failedNodes []nvcrev1alpha1.FailedNode) error {
	log := logf.FromContext(ctx)

	// Whether this is the first hardware failure is evaluated against the state
	// this attempt observed, so it is recomputed inside the retry rather than
	// captured from a possibly-stale read.
	var isFirstFailure, changed bool
	err := updateStatusWithRetry(ctx, r.Client, job, func(j *nvcrev1alpha1.Job) bool {
		isFirstFailure = len(j.Status.FailedNodes) == 0 && len(failedNodes) > 0
		j.Status.FailedNodes = failedNodes

		// Set HardwareFailed independently — it is not exclusive with the
		// InProgress/Succeeded/Failed set.
		c := meta.SetStatusCondition(&j.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.JobHardwareFailed,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: j.Generation,
		})
		changed = changed || c
		return c
	})
	if err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	if changed {
		// Record metrics
		workflow := job.Labels["nvcre.nvidia.com/workflow"]
		nodeNames := noderesults.FailedNodeNames(failedNodes)
		if isFirstFailure {
			recordFirstHardwareFailure(job.Namespace, job.Name, workflow)
		}
		recordHardwareFailure(job.Namespace, job.Name, workflow, nodeNames)

		log.Info("Hardware failure condition set",
			"failedNodes", nodeNames,
			"totalFailedNodes", len(failedNodes))
	}
	return nil
}

// isTerminalState checks if the job is in a terminal state (Succeeded or Failed).
// Note: HardwareFailed is NOT a terminal state - job execution continues regardless of hardware failures.
func (r *JobReconciler) isTerminalState(job *nvcrev1alpha1.Job) bool {
	succeededCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobSucceeded)
	if succeededCond != nil && succeededCond.Status == metav1.ConditionTrue {
		return true
	}

	failedCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobFailed)
	if failedCond != nil && failedCond.Status == metav1.ConditionTrue {
		return true
	}

	return false
}

// checkNodeHealth evaluates node health for the job using the configured detector.
// Node health evaluation is parallelized for performance with large node counts.
// The failed nodes list is additive - nodes that recover are NOT removed from the list.
func (r *JobReconciler) checkNodeHealth(ctx context.Context, job *nvcrev1alpha1.Job) (ctrl.Result, error) { //nolint:unparam
	startTime := time.Now()
	log := logf.FromContext(ctx)

	if job.Spec.NodeHealthMonitor == nil || job.Spec.NodeHealthMonitor.CEL == nil {
		return ctrl.Result{}, nil
	}

	// Get or compile the CEL detector — cache by expression string to avoid
	// recompiling on every reconcile (cel.NewEnv + compile + link is expensive).
	expr := job.Spec.NodeHealthMonitor.CEL.Expression
	r.detectorCacheMu.Lock()
	if r.detectorCache == nil {
		r.detectorCache = make(map[string]*cel.Detector)
	}
	detector, ok := r.detectorCache[expr]
	if !ok {
		var err error
		detector, err = cel.NewDetector(expr)
		if err != nil {
			r.detectorCacheMu.Unlock()
			return ctrl.Result{}, fmt.Errorf("failed to create CEL detector: %w", err)
		}
		r.detectorCache[expr] = detector
	}
	r.detectorCacheMu.Unlock()

	// Discover nodes running the job's workload via nvcre.nvidia.com/job label
	nodeNames, err := r.NodeDiscoverer.DiscoverNodesForJob(ctx, job.Namespace, job.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to discover nodes: %w", err)
	}

	if len(nodeNames) == 0 {
		log.V(1).Info("No nodes found running workload pods", "job", job.Name)
		return ctrl.Result{}, nil
	}

	log.V(1).Info("Starting node health evaluation", "nodeCount", len(nodeNames))

	// Fetch all nodes in parallel
	nodes, errs := r.NodeDiscoverer.GetNodes(ctx, nodeNames)
	for _, fetchErr := range errs {
		log.Error(fetchErr, "Failed to get node")
	}

	// Evaluate node health in parallel using goroutines
	// CEL evaluation is deterministic and CPU-bound, so parallelization improves performance
	type nodeResult struct {
		nodeName string
		failed   bool
		reason   string
		message  string
	}

	var (
		resultsCh = make(chan nodeResult, len(nodes))
		wg        sync.WaitGroup
	)

	for _, node := range nodes {
		wg.Add(1)
		go func(n *corev1.Node) {
			defer wg.Done()

			result, evalErr := detector.Detect(ctx, n)
			if evalErr != nil {
				log.Error(evalErr, "Failed to evaluate node health", "node", n.Name)
				return
			}

			if result.Failed {
				resultsCh <- nodeResult{
					nodeName: n.Name,
					failed:   true,
					reason:   result.Reason,
					message:  result.Message,
				}
			}
		}(node)
	}

	// Close channel when all goroutines complete
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	// Collect newly detected failed nodes
	var newFailedNodes []string
	for result := range resultsCh {
		if result.failed {
			log.Info("Hardware failure detected on node",
				"node", result.nodeName,
				"reason", result.reason,
				"message", result.message)
			newFailedNodes = append(newFailedNodes, result.nodeName)
		}
	}

	// Record metrics for node health check
	duration := time.Since(startTime).Seconds()
	observeNodeHealthCheckDuration(job.Namespace, job.Name, job.Labels["nvcre.nvidia.com/workflow"], duration)
	recordNodesEvaluated(job.Namespace, job.Name, job.Labels["nvcre.nvidia.com/workflow"], len(nodes))

	log.V(1).Info("Node health evaluation complete",
		"nodesEvaluated", len(nodes),
		"newFailuresDetected", len(newFailedNodes),
		"duration_seconds", duration)

	// Merge with existing failed nodes (additive - nodes that recover stay in the list)
	// This ensures flappy nodes only appear once and recovered nodes aren't removed.
	// CEL-detected failures are attributed to ReasonHardwareFailureDetected.
	allFailedNodes, hasNewFailures := mergeFailedNodes(job.Status.FailedNodes, newFailedNodes,
		ReasonHardwareFailureDetected, "Hardware failure detected by node health check")

	// Only update if there are actual new failures (deep comparison, not just length)
	if hasNewFailures {
		message := fmt.Sprintf("Hardware failure detected on node(s): %v", noderesults.FailedNodeNames(allFailedNodes))
		if err := r.setJobHardwareFailed(ctx, job, ReasonHardwareFailureDetected, message, allFailedNodes); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set HardwareFailed condition: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// mergeFailedNodes merges newly detected node names (all attributed to the given
// reason and message) into the existing FailedNode list, deduplicating by the
// (name, reason) pair and sorting the result. Existing entries are preserved
// (nodes are never removed from the list). Returns the merged list and a boolean
// indicating whether any new (name, reason) attribution was added — so a new
// reason for an already-listed node still counts as a new failure.
func mergeFailedNodes(existing []nvcrev1alpha1.FailedNode, newNames []string, reason nvcrev1alpha1.NodeFailureReason, message string) ([]nvcrev1alpha1.FailedNode, bool) {
	existingSet := make(map[[2]string]struct{}, len(existing))
	for _, fn := range existing {
		existingSet[[2]string{fn.Name, string(fn.Reason)}] = struct{}{}
	}

	hasNewFailures := false
	for _, n := range newNames {
		if n == "" {
			continue
		}
		if _, exists := existingSet[[2]string{n, string(reason)}]; !exists {
			hasNewFailures = true
		}
	}

	merged := sortMergedFailedNodes(existing, noderesults.NodesWithFailureDetails(newNames, reason, message))
	return merged, hasNewFailures
}

// groupNodeNames reads the group-nodes Job annotation and returns the deduped,
// sorted node names.
func groupNodeNames(job *nvcrev1alpha1.Job) []string {
	raw := job.Annotations["nvcre.nvidia.com/group-nodes"]
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	var nodes []string
	for _, p := range parts {
		n := strings.TrimSpace(p)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return nodes
}

// setExclusiveCondition sets a job execution condition to True and all other execution conditions to False.
// This ensures only one of InProgress/Succeeded/Failed is True at any given time.
// Note: HardwareFailed is NOT part of this exclusive set - it's independent.
func (r *JobReconciler) setExclusiveCondition(ctx context.Context, job *nvcrev1alpha1.Job, conditionType, reason, message string) error {
	// Only job execution states are mutually exclusive; HardwareFailed is set
	// independently and must not be cleared here.
	changed, err := setExclusiveStatusCondition(ctx, r.Client, job,
		func(j *nvcrev1alpha1.Job) *[]metav1.Condition { return &j.Status.Conditions },
		[]string{
			nvcrev1alpha1.JobInProgress,
			nvcrev1alpha1.JobSucceeded,
			nvcrev1alpha1.JobFailed,
		},
		conditionType, reason, message,
	)
	if err != nil {
		return err
	}

	if changed {
		// Record job status metric
		var metricStatus string
		switch conditionType {
		case nvcrev1alpha1.JobInProgress:
			metricStatus = "in_progress"
		case nvcrev1alpha1.JobSucceeded:
			metricStatus = "succeeded"
		case nvcrev1alpha1.JobFailed:
			metricStatus = "failed"
		}
		recordJobStatus(job.Namespace, job.Name, job.Labels["nvcre.nvidia.com/workflow"], metricStatus)
		logf.FromContext(ctx).Info("Job status updated", "status", conditionType, "reason", reason)
	}
	return nil
}

// getWorkloadName returns the name of the workload for a given Job
func (r *JobReconciler) getWorkloadName(job *nvcrev1alpha1.Job) string {
	return naming.Truncate(job.Name+"-workload", naming.MaxWorkloadNameLen)
}

// getGoodputMeasurementName returns the name of the auto-created GoodputMeasurement for a given Job
func (r *JobReconciler) getGoodputMeasurementName(job *nvcrev1alpha1.Job) string {
	return naming.Truncate(job.Name+"-goodput", naming.MaxK8sNameLen)
}

// getOwnerWorkflow returns the parent Workflow for a Job by looking up its OwnerReference.
// Measurements are owned by the Workflow (not the Job) so they survive iteration restarts.
func (r *JobReconciler) getOwnerWorkflow(ctx context.Context, job *nvcrev1alpha1.Job) *nvcrev1alpha1.Workflow {
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "Workflow" {
			wf := &nvcrev1alpha1.Workflow{}
			if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: job.Namespace}, wf); err == nil {
				return wf
			}
		}
	}
	return nil
}

// ensureGoodputMeasurement creates a GoodputMeasurement child resource if the Job has
// spec.goodputMeasurement configured and the resource doesn't already exist.
func (r *JobReconciler) ensureGoodputMeasurement(ctx context.Context, job *nvcrev1alpha1.Job) error {
	if job.Spec.GoodputMeasurement == nil {
		return nil
	}

	log := logf.FromContext(ctx)
	gmName := r.getGoodputMeasurementName(job)

	// Check if already exists
	existing := &nvcrev1alpha1.GoodputMeasurement{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: gmName}, existing); err == nil {
		return nil // already exists
	}

	apiGroup := "nvcre.nvidia.com"
	gm := &nvcrev1alpha1.GoodputMeasurement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gmName,
			Namespace: job.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "nvcre",
				"nvcre.nvidia.com/job":         job.Name,
			},
		},
		Spec: nvcrev1alpha1.GoodputMeasurementSpec{
			JobRef: corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "Job",
				Name:     job.Name,
			},
			LogProfileRef:  job.Spec.GoodputMeasurement.LogProfileRef,
			SampleInterval: job.Spec.GoodputMeasurement.SampleInterval,
		},
	}

	// Own by Workflow so measurements survive iteration restarts (Job deletion).
	// Fall back to Job ownership if the Workflow is not found.
	if wf := r.getOwnerWorkflow(ctx, job); wf != nil {
		if err := controllerutil.SetControllerReference(wf, gm, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on GoodputMeasurement: %w", err)
		}
	} else {
		if err := controllerutil.SetControllerReference(job, gm, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on GoodputMeasurement: %w", err)
		}
	}

	log.Info("Creating GoodputMeasurement", "name", gmName)
	if err := r.Create(ctx, gm); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create GoodputMeasurement: %w", err)
	}

	return nil
}

func (r *JobReconciler) ensureBandwidthMeasurement(ctx context.Context, job *nvcrev1alpha1.Job) error {
	if job.Spec.BandwidthMeasurement == nil {
		return nil
	}

	log := logf.FromContext(ctx)
	bmName := naming.Truncate(job.Name+"-bandwidth", naming.MaxK8sNameLen)

	// Check if already exists
	existing := &nvcrev1alpha1.BandwidthMeasurement{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: bmName}, existing); err == nil {
		return nil // already exists
	}

	apiGroup := "nvcre.nvidia.com"
	bm := &nvcrev1alpha1.BandwidthMeasurement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bmName,
			Namespace: job.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "nvcre",
				"nvcre.nvidia.com/job":         job.Name,
			},
		},
		Spec: nvcrev1alpha1.BandwidthMeasurementSpec{
			JobRef: corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "Job",
				Name:     job.Name,
			},
			LogProfileRef:  job.Spec.BandwidthMeasurement.LogProfileRef,
			SampleInterval: job.Spec.BandwidthMeasurement.SampleInterval,
			TestType:       job.Spec.BandwidthMeasurement.TestType,
		},
	}

	// Own by Workflow so measurements survive iteration restarts (Job deletion).
	if wf := r.getOwnerWorkflow(ctx, job); wf != nil {
		if err := controllerutil.SetControllerReference(wf, bm, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on BandwidthMeasurement: %w", err)
		}
	} else {
		if err := controllerutil.SetControllerReference(job, bm, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on BandwidthMeasurement: %w", err)
		}
	}

	log.Info("Creating BandwidthMeasurement", "name", bmName)
	if err := r.Create(ctx, bm); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create BandwidthMeasurement: %w", err)
	}

	return nil
}

// warnf emits a Warning event if the Recorder is configured. Every Job-tier
// event is a warning; the Workflow reconciler's eventf takes an explicit type
// because it emits Normal events too.
//
// Safe to call when Recorder is nil (e.g. in unit tests, or any embedding that
// constructs JobReconciler directly).
func (r *JobReconciler) warnf(obj runtime.Object, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, reason, reason, messageFmt, args...)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *JobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize node discoverer if not set
	if r.NodeDiscoverer == nil {
		r.NodeDiscoverer = nodemonitor.NewNodeDiscoverer(mgr.GetClient())
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&nvcrev1alpha1.Job{}).
		// Watch owned workload resources for status changes (event-driven reconciliation)
		Owns(&trainerv1alpha1.TrainJob{}).
		Owns(&nvcrev1alpha1.GoodputMeasurement{})

	return b.
		// Watch Nodes for health changes - maps node events to jobs via pod lookups
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToJobRequests),
			builder.WithPredicates(r.nodeHealthChangePredicate()),
		).
		Named("job").
		WithOptions(controlleropts.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}

// nodeToJobRequests maps a Node event to Job reconcile requests for all Jobs with pods on that node
func (r *JobReconciler) nodeToJobRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}

	// Find all pods running on this node that have the NVCRE job label.
	//
	// This runs in the watch path for every Node event in the cluster, so it must
	// stay a keyed lookup. The index is registered unconditionally at manager
	// startup; if the read fails it is a transient cache error, and falling back
	// to an unfiltered cluster-wide Pod list would turn that into a list storm.
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.MatchingFields{podNodeNameIndexField: node.Name}); err != nil {
		logf.FromContext(ctx).V(1).Info("Failed to list pods for node, skipping enqueue",
			"node", node.Name, "error", err)
		return nil
	}

	requests := make(map[string]reconcile.Request)
	for _, pod := range podList.Items {
		// The index already constrains this, but a pod can be rescheduled between
		// the indexed read and here; re-check rather than enqueue the wrong Job.
		if pod.Spec.NodeName != node.Name {
			continue
		}

		if jobName, ok := pod.Labels["nvcre.nvidia.com/job"]; ok {
			key := pod.Namespace + "/" + jobName
			requests[key] = reconcile.Request{
				NamespacedName: client.ObjectKey{
					Namespace: pod.Namespace,
					Name:      jobName,
				},
			}
		}
	}

	result := make([]reconcile.Request, 0, len(requests))
	for _, req := range requests {
		result = append(result, req)
	}
	return result
}

// nodeHealthChangePredicate filters node events to only health-relevant changes
func (r *JobReconciler) nodeHealthChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return false
			}

			// Trigger on condition changes
			if !nodeConditionsEqual(oldNode.Status.Conditions, newNode.Status.Conditions) {
				return true
			}

			// Trigger on taint changes
			if !nodeTaintsEqual(oldNode.Spec.Taints, newNode.Spec.Taints) {
				return true
			}

			// Trigger on label changes (for GPUd-style health labels)
			if !mapsEqual(oldNode.Labels, newNode.Labels) {
				return true
			}

			// Trigger on cordon/uncordon
			if oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable {
				return true
			}

			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false // Node deletion handled differently
		},
	}
}

// nodeConditionsEqual compares two slices of node conditions for equality
func nodeConditionsEqual(a, b []corev1.NodeCondition) bool {
	if len(a) != len(b) {
		return false
	}
	condMap := make(map[corev1.NodeConditionType]corev1.ConditionStatus)
	for _, c := range a {
		condMap[c.Type] = c.Status
	}
	for _, c := range b {
		if condMap[c.Type] != c.Status {
			return false
		}
	}
	return true
}

// nodeTaintsEqual compares two slices of taints for equality
func nodeTaintsEqual(a, b []corev1.Taint) bool {
	if len(a) != len(b) {
		return false
	}
	taintMap := make(map[string]corev1.Taint)
	for _, t := range a {
		taintMap[t.Key] = t
	}
	for _, t := range b {
		if existing, ok := taintMap[t.Key]; !ok || existing.Value != t.Value || existing.Effect != t.Effect {
			return false
		}
	}
	return true
}

// mapsEqual compares two string maps for equality
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Field indexes used here are defined and registered in indexes.go.
