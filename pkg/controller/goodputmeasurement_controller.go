// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/goodput"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/numstr"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/podlogs"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/podutil"
)

const (
	goodputMeasurementFinalizer = "cre.nvidia.com/goodputmeasurement-finalizer"
	defaultSampleInterval       = 60 * time.Second

	// reasonGoodputLogProfileMissing marks a spec.logProfileRef that does not
	// resolve. The measurement keeps retrying, so this is not terminal.
	reasonGoodputLogProfileMissing = "LogProfileNotFound"
	// reasonGoodputNoData marks a measurement that ended without computing a
	// goodput ratio, so its result is absent rather than zero.
	reasonGoodputNoData = "NoDataCollected"
)

// cachedProfileParser pairs a compiled parser with the resourceVersion of the
// LogProfile it was compiled from, so a stale entry can be detected and replaced.
type cachedProfileParser struct {
	resourceVersion string
	parser          *goodput.ProfileParser
}

// GoodputMeasurementReconciler reconciles a GoodputMeasurement object.
type GoodputMeasurementReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Clientset  *kubernetes.Clientset
	LogFetcher podlogs.PodLogFetcher // if nil, defaults to Clientset-backed fetcher

	mu        sync.Mutex
	jobStates map[string]*goodput.JobState // key: namespace/name of GoodputMeasurement

	// parsers caches compiled ProfileParsers by LogProfile name, invalidated by
	// resourceVersion so LogProfile edits are picked up without a restart.
	parsers map[string]*cachedProfileParser

	// lastSample tracks when each measurement was last sampled.
	lastSample map[string]time.Time
}

// getLogFetcher returns the configured LogFetcher or falls back to a Clientset-backed one.
func (r *GoodputMeasurementReconciler) getLogFetcher() podlogs.PodLogFetcher {
	if r.LogFetcher != nil {
		return r.LogFetcher
	}
	return podlogs.NewKubernetesLogFetcher(r.Clientset)
}

// +kubebuilder:rbac:groups=cre.nvidia.com,resources=goodputmeasurements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=goodputmeasurements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=goodputmeasurements/finalizers,verbs=update
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=logprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop.
func (r *GoodputMeasurementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	measurement := &crev1alpha1.GoodputMeasurement{}
	if err := r.Get(ctx, req.NamespacedName, measurement); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("GoodputMeasurement resource not found, likely deleted")
			r.cleanupState(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get GoodputMeasurement: %w", err)
	}

	// Handle deletion.
	if !measurement.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, measurement)
	}

	// Add finalizer if not present. A successful add ends this reconcile; the
	// resulting watch event drives the next one.
	if added, err := ensureFinalizer(ctx, r.Client, measurement, goodputMeasurementFinalizer); err != nil || added {
		return ctrl.Result{}, err
	}

	return r.reconcileMeasurement(ctx, measurement)
}

// reconcileMeasurement watches the referenced Job and drives goodput computation.
func (r *GoodputMeasurementReconciler) reconcileMeasurement(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If no JobRef is set, nothing to measure.
	if measurement.Spec.JobRef.Name == "" {
		log.Info("No JobRef set, nothing to measure")
		return ctrl.Result{}, nil
	}

	// If measurement is already complete, nothing to do.
	if cond := meta.FindStatusCondition(measurement.Status.Conditions, crev1alpha1.GoodputMeasurementComplete); cond != nil && cond.Status == metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	// Fetch the referenced CRE Job.
	job := &crev1alpha1.Job{}
	jobKey := types.NamespacedName{
		Name:      measurement.Spec.JobRef.Name,
		Namespace: measurement.Namespace,
	}
	if err := r.Get(ctx, jobKey, job); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Referenced Job not found, requeueing", "job", jobKey)
			return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get referenced Job: %w", err)
	}

	// Determine Job phase from conditions.
	if cond := meta.FindStatusCondition(job.Status.Conditions, crev1alpha1.JobSucceeded); cond != nil && cond.Status == metav1.ConditionTrue {
		r.finalSample(ctx, measurement, job)
		return r.handleSucceeded(ctx, measurement, job)
	}
	if cond := meta.FindStatusCondition(job.Status.Conditions, crev1alpha1.JobFailed); cond != nil && cond.Status == metav1.ConditionTrue {
		r.finalSample(ctx, measurement, job)
		return r.handleFailed(ctx, measurement, job)
	}
	if cond := meta.FindStatusCondition(job.Status.Conditions, crev1alpha1.JobInProgress); cond != nil && cond.Status == metav1.ConditionTrue {
		return r.handleRunning(ctx, measurement, job)
	}

	// Job exists but has no conditions yet — requeue.
	log.Info("Referenced Job has no conditions yet, requeueing", "job", jobKey)
	return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
}

// finalSample reads the logs one last time before a measurement goes terminal.
//
// handleSucceeded and handleFailed both build from the stored status and never
// read logs again, so everything written in the last sampling window was lost —
// up to sampleInterval, 60s by default. That matters most on failure, where the
// last lines before a crash are the interesting ones. Observed on hardware: a
// script that logged to iteration 100 was recorded as reaching step 90.
//
// The throttle in handleRunning exists to stop status updates re-triggering a
// read, and would usually skip this one, so the sample time is cleared first.
// Best effort: a failure here leaves the previous status in place, which is
// what would have happened anyway.
func (r *GoodputMeasurementReconciler) finalSample(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement, job *crev1alpha1.Job) {
	key := fmt.Sprintf("%s/%s", measurement.Namespace, measurement.Name)

	r.mu.Lock()
	delete(r.lastSample, key)
	r.mu.Unlock()

	if _, err := r.handleRunning(ctx, measurement, job); err != nil {
		logf.FromContext(ctx).Error(err, "Final log read before completion failed",
			"measurement", key)
	}
}

// handleRunning processes a running job: reads logs, parses them, computes goodput.
func (r *GoodputMeasurementReconciler) handleRunning(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement, job *crev1alpha1.Job) (ctrl.Result, error) { //nolint:gocyclo
	log := logf.FromContext(ctx)
	key := fmt.Sprintf("%s/%s", measurement.Namespace, measurement.Name)
	interval := r.getSampleInterval(measurement)

	// Throttle: skip if we sampled recently (status updates trigger re-reconcile).
	r.mu.Lock()
	if r.lastSample == nil {
		r.lastSample = make(map[string]time.Time)
	}
	if last, ok := r.lastSample[key]; ok && time.Since(last) < interval {
		r.mu.Unlock()
		return ctrl.Result{RequeueAfter: interval - time.Since(last)}, nil
	}
	r.mu.Unlock()

	// Fetch the LogProfile.
	profile, err := r.getLogProfile(ctx, measurement.Spec.LogProfileRef)
	if err != nil {
		log.Error(err, "Failed to fetch LogProfile", "logProfile", measurement.Spec.LogProfileRef)
		r.noteLogProfileUnresolved(ctx, measurement, err)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	// Get or create compiled parser.
	parser, err := r.getOrCreateParser(profile)
	if err != nil {
		log.Error(err, "Failed to compile LogProfile patterns", "logProfile", profile.Name)
		r.noteLogProfileUnresolved(ctx, measurement, err)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	// Take the per-measurement state and hold its lock for the rest of this
	// reconcile. Everything below is a read-modify-write cycle over JobState, so
	// it must be serialized against a concurrent reconcile of the same
	// measurement. Helpers invoked below (recordInterruptionFromRestart,
	// completeInterruption) assume the caller already holds this lock.
	state := r.getOrCreateState(key, measurement)
	state.Lock()
	defer state.Unlock()

	// Determine the workload kind and name from Job's workloadRef.
	if job.Status.WorkloadRef == nil {
		// Detect workload restart: if training had started and no pending interruption
		// exists yet, the Job is being restarted from checkpoint.
		if state.TrainingStarted && state.PendingInterruption == nil {
			workflow := job.Labels["cre.nvidia.com/workflow"]
			r.recordInterruptionFromRestart(ctx, state, measurement, workflow)
			if err := r.Status().Update(ctx, measurement); err != nil {
				log.Error(err, "Failed to persist interruption to status")
			}
		}
		log.Info("Job workloadRef not set yet, requeueing")
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	workloadKind := job.Status.WorkloadRef.Kind
	workloadName := job.Status.WorkloadRef.Name

	// Discover worker pods.
	discoverer := podutil.NewWorkerDiscoverer(r.Client)
	worker0, lastWorker, err := discoverer.GetWorkerPods(ctx, measurement.Namespace, workloadName, workloadKind)
	if err != nil {
		log.V(1).Info("Worker pods not found yet", "error", err)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	if !podutil.IsPodRunning(worker0) {
		log.V(1).Info("Worker-0 pod not running yet", "phase", worker0.Status.Phase)
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	// Build log options.
	// We use SinceTime-based reading so that NCCL noise between checkpoint
	// save/done lines cannot cause important lines to be missed. After the
	// first successful parse we read from the last log timestamp; before that
	// we read from the pod's start time to capture applicationStart.
	//
	// Detect restart via RestartCount — handles the race where workloadRef
	// was nil only briefly and the GM controller missed the transition.
	if job.Status.RestartCount > int32(measurement.Status.InterruptionCount) &&
		state.TrainingStarted && state.PendingInterruption == nil {
		workflow := job.Labels["cre.nvidia.com/workflow"]
		r.recordInterruptionFromRestart(ctx, state, measurement, workflow)
	}

	opts := podlogs.LogOptions{
		Container: profile.Spec.ContainerName,
	}
	var anchor time.Time
	if !state.LastLogFetch.IsZero() {
		anchor = state.LastLogFetch.Add(-time.Second)
	} else if worker0.Status.StartTime != nil {
		anchor = worker0.Status.StartTime.Time
	}
	// Bound the lookback. LastLogFetch is reset to zero on a detected restart, so
	// a long-running job that restarts late would otherwise re-read from the
	// replacement pod's start across every remaining sample.
	if !anchor.IsZero() {
		if oldest := time.Now().Add(-maxLogLookback); anchor.Before(oldest) {
			log.V(1).Info("Clamping log read anchor to max lookback",
				"pod", worker0.Name, "anchor", anchor, "maxLookback", maxLogLookback)
			anchor = oldest
		}
		sinceTime := metav1.NewTime(anchor)
		opts.SinceTime = &sinceTime
	}

	// Create log reader.
	reader := goodput.NewLogReader(r.getLogFetcher(), parser)

	// Read and parse logs.
	var result *goodput.ParseResult
	workerStrategy := getWorkerStrategy(profile)
	if workerStrategy == "Multi" && lastWorker != nil && lastWorker.Name != worker0.Name && podutil.IsPodRunning(lastWorker) {
		result, err = reader.ReadMultiWorkerLogs(ctx, measurement.Namespace, worker0.Name, lastWorker.Name, opts)
	} else {
		result, err = reader.ReadLogs(ctx, measurement.Namespace, worker0.Name, opts)
	}
	if err != nil {
		log.Error(err, "Failed to read logs")
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	// Track the last log timestamp so subsequent reads use SinceTime.
	// ApplicationStopTime uses wall-clock time (not the log-embedded timestamp)
	// because the app continues running between logged steps. For models that
	// log infrequently (e.g., NeMo 6 logs every 10 steps), the log timestamp
	// can lag the actual crash time by a full log interval.
	if !result.LastLogTimestamp.IsZero() {
		state.LastLogFetch = result.LastLogTimestamp
		now := time.Now()
		state.ApplicationStopTime = now
		t := metav1.NewTime(now)
		measurement.Status.ApplicationStopTime = &t
	}

	// Get cumulative metrics from the GoodputMeasurement's in-memory state.
	cm := r.buildCumulativeFromStatus(measurement)

	// Fill in checkpoint save duration from persisted pending save when the
	// save line was outside the tail window but the done line is visible.
	if state.PendingCheckpointSave != nil {
		for _, ckpt := range result.Checkpoints {
			if ckpt.SaveDuration == 0 && !ckpt.Timestamp.IsZero() &&
				ckpt.Step == state.PendingCheckpointSave.Step {
				ckpt.SaveDuration = ckpt.Timestamp.Sub(state.PendingCheckpointSave.Timestamp).Seconds()
			}
		}
	}

	// Persist or clear pending checkpoint save state.
	if result.PendingSave != nil {
		state.PendingCheckpointSave = result.PendingSave
	} else if result.LastCheckpoint != nil {
		// Done line was seen in this window, clear pending state.
		state.PendingCheckpointSave = nil
	}

	// Update state from parsed results.
	if result.LastStep != nil {
		state.LastKnownStep = result.LastStep.GlobalStep
		if result.LastStep.GlobalStep > state.HighestStep {
			state.HighestStep = result.LastStep.GlobalStep
		}
		log.V(1).Info("Detected training step",
			"globalStep", result.LastStep.GlobalStep,
			"iteration", result.LastStep.Iteration)
	}
	if result.LastCheckpoint != nil {
		if result.LastCheckpoint.Step != state.LastCheckpointStep {
			log.V(1).Info("Detected new checkpoint",
				"step", result.LastCheckpoint.Step,
				"saveDuration", result.LastCheckpoint.SaveDuration)
		}
		state.LastCheckpointStep = result.LastCheckpoint.Step
		state.LastCheckpointTime = result.LastCheckpoint.Timestamp
	}

	// Persist ApplicationStartTime from early reconciles (during init, before training).
	// Once training starts, stop updating — mid-training matches are not the real startup.
	if !state.TrainingStarted && !result.ApplicationStartTime.IsZero() && state.ApplicationStartTime.IsZero() {
		state.ApplicationStartTime = result.ApplicationStartTime
	}

	// Training starts at first step.
	if !state.TrainingStarted && result.FirstStep != nil {
		state.TrainingStarted = true
		state.StartTime = result.FirstStep.Timestamp
		if cm.TrainingStartTime.IsZero() {
			cm.TrainingStartTime = result.FirstStep.Timestamp
		}

		// Record warmup base step for persisted warmup detection.
		state.WarmupBaseStep = result.FirstStep.GlobalStep
		wbs := result.FirstStep.GlobalStep
		measurement.Status.WarmupBaseStep = &wbs

		// On a fresh start (no pending interruption), capture the startup time
		// (applicationStart → firstStep) as resume time. This covers initialization,
		// checkpoint loading, and warmup step execution.
		// Uses state.ApplicationStartTime (persisted from early reconciles) because the
		// startup log line may scroll out of the tail window by the time training begins.
		if state.PendingInterruption == nil && !state.ApplicationStartTime.IsZero() {
			startupTime := result.FirstStep.Timestamp.Sub(state.ApplicationStartTime).Seconds()
			if startupTime > 0 {
				cm.ResumeTime += startupTime
				log.Info("Recorded initial startup time as resume time",
					"startupTime", startupTime)
			}
		}

		// Capture first step's timing as warmup.
		if result.FirstStep.StepTiming > 0 && profile.Spec.WarmupSteps != nil && *profile.Spec.WarmupSteps > 0 {
			measurement.Status.WarmupTimeSec = formatFloat(
				parseFloat(measurement.Status.PriorWarmupTimeSec) + result.FirstStep.StepTiming)
		}

		log.Info("Training started (first step seen)",
			"startTime", state.StartTime,
			"globalStep", result.FirstStep.GlobalStep)
	}

	// Complete pending interruption if we have first training step (t_resumed).
	if state.PendingInterruption != nil && result.FirstStep != nil {
		// If the restart is not from a checkpoint, discard prior accumulated
		// warmup/nonWarmup since training starts from scratch.
		if state.PendingInterruption.CheckpointStep == 0 {
			measurement.Status.PriorWarmupTimeSec = ""
			measurement.Status.PriorNonWarmupTimeSec = ""
		}
		r.completeInterruption(state, cm, result)

		// Reset warmup base step for the new run.
		state.WarmupBaseStep = result.FirstStep.GlobalStep
		wbs := result.FirstStep.GlobalStep
		measurement.Status.WarmupBaseStep = &wbs

		// Reset incremental NonWarmupTime accumulation for the new run.
		// Start from PriorNonWarmupTimeSec (carried from previous run).
		measurement.Status.NonWarmupTimeSec = measurement.Status.PriorNonWarmupTimeSec
		state.LastNonWarmupStep = 0
		measurement.Status.LastNonWarmupStep = 0

		// Capture first step after restore as warmup.
		if result.FirstStep.StepTiming > 0 && profile.Spec.WarmupSteps != nil && *profile.Spec.WarmupSteps > 0 {
			measurement.Status.WarmupTimeSec = formatFloat(
				parseFloat(measurement.Status.PriorWarmupTimeSec) + result.FirstStep.StepTiming)
		}
	}

	// Scenario B: controller restarted after training started but before startup
	// time was captured. Use persisted ApplicationStartTime and TrainingStartTime.
	if state.TrainingStarted && cm.ResumeTime == 0 && state.PendingInterruption == nil &&
		!state.ApplicationStartTime.IsZero() && !cm.TrainingStartTime.IsZero() {
		startupTime := cm.TrainingStartTime.Sub(state.ApplicationStartTime).Seconds()
		if startupTime > 0 {
			cm.ResumeTime += startupTime
			log.Info("Recovered startup resume time after restart",
				"startupTime", startupTime)
		}
	}

	// Update cumulative metrics.
	cm.SetStatus("Training")
	cm.UpdateTrainingProgress(state.LastKnownStep, state.LastCheckpointStep, state.LastCheckpointTime)

	// Track checkpoint save time (only count new checkpoints).
	if result.LastCheckpoint != nil && result.LastCheckpoint.Step > state.LastCountedCheckpointStep {
		if result.LastCheckpoint.SaveDuration > 0 {
			cm.CheckpointSaveTime += result.LastCheckpoint.SaveDuration
			cm.CheckpointCount++
			cm.LastCheckpointDuration = result.LastCheckpoint.SaveDuration
			cm.AvgCheckpointDuration = cm.CheckpointSaveTime / float64(cm.CheckpointCount)
			log.V(1).Info("Added checkpoint save time",
				"step", result.LastCheckpoint.Step,
				"saveDuration", result.LastCheckpoint.SaveDuration,
				"totalSaveTime", cm.CheckpointSaveTime)
		}
		state.LastCountedCheckpointStep = result.LastCheckpoint.Step
	}

	// Calculate training time (t_w) and goodput.
	if state.TrainingStarted && !cm.TrainingStartTime.IsZero() {
		cm.UpdateTrainingTime(time.Since(cm.TrainingStartTime).Seconds())
	}

	// Clear pending interruption from status after completion.
	if state.PendingInterruption == nil {
		cm.PendingInterruption = nil
	}

	// Set measuring condition and record start time.
	if measurement.Status.StartTime == nil {
		now := metav1.Now()
		measurement.Status.StartTime = &now
	}
	meta.SetStatusCondition(&measurement.Status.Conditions, metav1.Condition{
		Type:               crev1alpha1.GoodputMeasurementMeasuring,
		Status:             metav1.ConditionTrue,
		Reason:             "JobRunning",
		Message:            "Referenced Job is running, measurement in progress",
		ObservedGeneration: measurement.Generation,
	})

	// Write cumulative metrics to status.
	r.writeStatusFromCumulative(measurement, cm)

	// Persist ApplicationStartTime so it survives controller restarts.
	if !state.ApplicationStartTime.IsZero() {
		t := metav1.NewTime(state.ApplicationStartTime)
		measurement.Status.ApplicationStartTime = &t
	} else {
		measurement.Status.ApplicationStartTime = nil
	}

	// Persist LastCheckpointTime so lost work time survives controller restarts.
	if !state.LastCheckpointTime.IsZero() {
		t := metav1.NewTime(state.LastCheckpointTime)
		measurement.Status.LastCheckpointTime = &t
	} else {
		measurement.Status.LastCheckpointTime = nil
	}

	// Re-apply warmup flags from persisted state when applicationStart
	// has dropped out of the SinceTime log window on subsequent reads.
	if profile.Spec.WarmupSteps != nil && *profile.Spec.WarmupSteps > 0 && state.WarmupBaseStep >= 0 {
		for _, s := range result.Steps {
			if s.GlobalStep-state.WarmupBaseStep < *profile.Spec.WarmupSteps {
				s.IsWarmup = true
			}
		}
	}

	// Persist logInterval from LogProfile so the Job controller can scale
	// the stall detection threshold by the number of iterations between logs.
	if profile.Spec.LogInterval != nil {
		measurement.Status.LogInterval = *profile.Spec.LogInterval
	}

	// Only use non-warmup steps for stall detection timestamp.
	// Warmup step timestamps are inflated and should not anchor stall detection.
	// Stall detection won't activate until the first non-warmup step.
	if result.LastStep != nil && !result.LastStep.Timestamp.IsZero() && !result.LastStep.IsWarmup {
		t := metav1.NewTime(result.LastStep.Timestamp)
		measurement.Status.LastStepTimestamp = &t
	}
	if avg := computeAvgStepTime(result); avg > 0 {
		measurement.Status.AvgStepTimeSec = formatFloat(avg)
	}
	if avgTFLOPS := computeAvgTFLOPS(result); avgTFLOPS > 0 {
		measurement.Status.AvgTFLOPSPerGPU = formatFloat(avgTFLOPS)
	}
	// WarmupTimeSec is captured once at first-step detection (above).
	// NonWarmupTime is accumulated incrementally: only steps above the watermark are summed.
	delta := computeNonWarmupTimeDelta(result, state.LastNonWarmupStep)
	if delta > 0 {
		nonWarmupTime := parseFloat(measurement.Status.NonWarmupTimeSec) + delta
		measurement.Status.NonWarmupTimeSec = formatFloat(nonWarmupTime)
	}
	// Advance watermark to the highest step in this window.
	if result.LastStep != nil && result.LastStep.GlobalStep > state.LastNonWarmupStep {
		state.LastNonWarmupStep = result.LastStep.GlobalStep
		measurement.Status.LastNonWarmupStep = state.LastNonWarmupStep
	}

	workflow := job.Labels["cre.nvidia.com/workflow"]
	recordGoodputMetrics(
		measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name, workflow,
		goodputMetricValues{
			GoodputRatio:       cm.Goodput,
			AvgTFLOPS:          parseFloat(measurement.Status.AvgTFLOPSPerGPU),
			AvgStepTime:        parseFloat(measurement.Status.AvgStepTimeSec),
			RescheduleTime:     cm.RescheduleTime,
			ResumeTime:         cm.ResumeTime,
			CheckpointSaveTime: cm.CheckpointSaveTime,
			LostWorkTime:       cm.LostWorkTime,
			TrainingTime:       cm.TrainingTime,
			WarmupTime:         parseFloat(measurement.Status.WarmupTimeSec),
			NonWarmupTime:      parseFloat(measurement.Status.NonWarmupTimeSec),
		},
	)

	if err := r.Status().Update(ctx, measurement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update measurement status: %w", err)
	}

	// Record sample time so we throttle the next status-triggered reconcile.
	r.mu.Lock()
	r.lastSample[key] = time.Now()
	r.mu.Unlock()

	return ctrl.Result{RequeueAfter: interval}, nil
}

// handleSucceeded processes a succeeded job.
func (r *GoodputMeasurementReconciler) handleSucceeded(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement, job *crev1alpha1.Job) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	key := fmt.Sprintf("%s/%s", measurement.Namespace, measurement.Name)

	cm := r.buildCumulativeFromStatus(measurement)

	if state := r.getState(key); state != nil {
		state.Lock()
		trainingStarted := state.TrainingStarted
		state.Unlock()
		if trainingStarted && !cm.TrainingStartTime.IsZero() {
			cm.UpdateTrainingTime(time.Since(cm.TrainingStartTime).Seconds())
		}
	}
	cm.SetStatus("Succeeded")

	r.writeStatusFromCumulative(measurement, cm)
	log.Info("Job succeeded", "goodput", cm.Goodput, "trainingTime", cm.TrainingTime)

	workflow := job.Labels["cre.nvidia.com/workflow"]
	recordGoodputMetrics(
		measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name, workflow,
		goodputMetricValues{
			GoodputRatio:       cm.Goodput,
			AvgTFLOPS:          parseFloat(measurement.Status.AvgTFLOPSPerGPU),
			AvgStepTime:        parseFloat(measurement.Status.AvgStepTimeSec),
			RescheduleTime:     cm.RescheduleTime,
			ResumeTime:         cm.ResumeTime,
			CheckpointSaveTime: cm.CheckpointSaveTime,
			LostWorkTime:       cm.LostWorkTime,
			TrainingTime:       cm.TrainingTime,
			WarmupTime:         parseFloat(measurement.Status.WarmupTimeSec),
			NonWarmupTime:      parseFloat(measurement.Status.NonWarmupTimeSec),
		},
	)
	cleanupOperationalGoodputMetrics(measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name, workflow)

	// A measurement that computed no goodput did not measure anything, whatever
	// the Job did. Reporting JobSucceeded here reads as a successful measurement.
	// The ratio has to be checked as well as the empty string. writeStatusFromCumulative
	// stores it through formatFloat, and formatFloat(0) is "0.000000", so a measurement
	// that sampled but parsed nothing ends up with a result that is present and zero,
	// never absent. Observed on hardware: a run whose logs missed the trainingStep
	// pattern reported Complete/JobSucceeded with result 0.000000 and no steps.
	if measurement.Status.Result == "" || parseFloat(measurement.Status.Result) == 0 {
		return ctrl.Result{}, r.setComplete(ctx, measurement, reasonGoodputNoData,
			"Job succeeded but no goodput was computed; check spec.logProfileRef")
	}

	return ctrl.Result{}, r.setComplete(ctx, measurement, "JobSucceeded", "Referenced Job completed successfully")
}

// handleFailed processes a failed job by recording an interruption event.
func (r *GoodputMeasurementReconciler) handleFailed(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement, job *crev1alpha1.Job) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	key := fmt.Sprintf("%s/%s", measurement.Namespace, measurement.Name)

	// Hold the state lock for the whole handler: the interruption event below is
	// built from several JobState fields that must be read consistently.
	state := r.getOrCreateState(key, measurement)
	state.Lock()
	defer state.Unlock()

	cm := r.buildCumulativeFromStatus(measurement)

	// Determine termination time. Prefer ApplicationStopTime (last log timestamp)
	// as it reflects the last known application activity — more accurate than
	// container termination time for stalls and crashes.
	var terminationTime time.Time
	if !state.ApplicationStopTime.IsZero() {
		terminationTime = state.ApplicationStopTime
	} else if job.Status.WorkloadRef != nil {
		// Fallback: try to get the termination time from the worker pod.
		discoverer := podutil.NewWorkerDiscoverer(r.Client)
		worker0, _, _ := discoverer.GetWorkerPods(ctx, measurement.Namespace, job.Status.WorkloadRef.Name, job.Status.WorkloadRef.Kind)
		if worker0 != nil {
			containerName := ""
			profile, err := r.getLogProfile(ctx, measurement.Spec.LogProfileRef)
			if err == nil {
				containerName = profile.Spec.ContainerName
			}
			if termTime := podutil.GetContainerTerminationTime(worker0, containerName); termTime != nil {
				terminationTime = termTime.Time
			}
		}
	}
	if terminationTime.IsZero() {
		terminationTime = time.Now()
	}

	// Build interruption event.
	event := goodput.InterruptionEvent{
		TCheckpoint:    state.LastCheckpointTime,
		TInterrupt:     terminationTime,
		Reason:         "JobFailed",
		CheckpointStep: state.LastCheckpointStep,
		LastStep:       state.LastKnownStep,
	}
	if !state.LastCheckpointTime.IsZero() {
		event.TCh = terminationTime.Sub(state.LastCheckpointTime).Seconds()
	}

	log.Info("Job failed", "t_ch", event.TCh)

	// Store as pending interruption (for when the Job restarts).
	cm.PendingInterruption = &event
	cm.LostWorkTime += event.TCh
	cm.InterruptionCount++

	if state.TrainingStarted && !cm.TrainingStartTime.IsZero() {
		cm.TrainingTime = terminationTime.Sub(cm.TrainingStartTime).Seconds()
		cm.Goodput = goodput.CalculateGoodput(cm.TrainingTime, cm.LostWorkTime, cm.RescheduleTime, cm.ResumeTime, cm.CheckpointSaveTime)
	}

	cm.SetStatus("Failed")

	// Snapshot accumulated warmup/nonWarmup so the next run can accumulate on top.
	// The decision to use or discard these is made at restart time in handleRunning.
	measurement.Status.PriorWarmupTimeSec = measurement.Status.WarmupTimeSec
	measurement.Status.PriorNonWarmupTimeSec = measurement.Status.NonWarmupTimeSec

	// Reset training started for the next run.
	state.TrainingStarted = false
	state.ApplicationStartTime = time.Time{}
	state.PendingInterruption = &event

	r.writeStatusFromCumulative(measurement, cm)
	measurement.Status.ApplicationStartTime = nil

	workflow := job.Labels["cre.nvidia.com/workflow"]
	recordGoodputMetrics(
		measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name, workflow,
		goodputMetricValues{
			GoodputRatio:       cm.Goodput,
			AvgTFLOPS:          parseFloat(measurement.Status.AvgTFLOPSPerGPU),
			AvgStepTime:        parseFloat(measurement.Status.AvgStepTimeSec),
			RescheduleTime:     cm.RescheduleTime,
			ResumeTime:         cm.ResumeTime,
			CheckpointSaveTime: cm.CheckpointSaveTime,
			LostWorkTime:       cm.LostWorkTime,
			TrainingTime:       cm.TrainingTime,
			WarmupTime:         parseFloat(measurement.Status.WarmupTimeSec),
			NonWarmupTime:      parseFloat(measurement.Status.NonWarmupTimeSec),
		},
	)
	cleanupOperationalGoodputMetrics(measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name, workflow)

	return ctrl.Result{}, r.setComplete(ctx, measurement, "JobFailed", "Referenced Job failed")
}

// completeInterruption completes a pending interruption event after job restart.
//
// The caller must hold state's lock.
func (r *GoodputMeasurementReconciler) completeInterruption(state *goodput.JobState, cm *goodput.CumulativeMetrics, result *goodput.ParseResult) {
	event := state.PendingInterruption
	if event == nil || result.FirstStep == nil {
		return
	}

	// t_scheduled = when the app started (or pod start time as fallback).
	// Prefer result (current window), then state (persisted from earlier reconciles).
	if !result.ApplicationStartTime.IsZero() {
		event.TScheduled = result.ApplicationStartTime
	} else if !state.ApplicationStartTime.IsZero() {
		event.TScheduled = state.ApplicationStartTime
	} else {
		// Use the K8s first log timestamp as a rough approximation.
		event.TScheduled = result.FirstStep.Timestamp
	}

	// t_resumed = when first training step was logged.
	event.TResumed = result.FirstStep.Timestamp

	// Calculate t_re and t_rm. Clamp to zero — a negative value can occur
	// when the replacement pod is scheduled before the controller records
	// the interruption (race between kubelet restart and controller reconcile).
	event.TRe = max(0, event.TScheduled.Sub(event.TInterrupt).Seconds())
	event.TRm = max(0, event.TResumed.Sub(event.TScheduled).Seconds())

	logf.Log.Info("Completed interruption tracking",
		"t_ch", event.TCh, "t_re", event.TRe, "t_rm", event.TRm)

	// Add only the newly calculated reschedule and resume times.
	// LostWorkTime and InterruptionCount were already accounted for when the
	// interruption was first recorded (in handleFailed or recordInterruptionFromRestart).
	cm.RescheduleTime += event.TRe
	cm.ResumeTime += event.TRm
	cm.Interruptions = append(cm.Interruptions, *event)
	state.PendingInterruption = nil
}

// recordInterruptionFromRestart records an interruption event when a workload restart
// is detected (workloadRef is nil but training had started). This persists the
// interruption to Status so it survives controller restarts.
//
// The caller must hold state's lock.
func (r *GoodputMeasurementReconciler) recordInterruptionFromRestart(ctx context.Context, state *goodput.JobState, measurement *crev1alpha1.GoodputMeasurement, workflow string) {
	log := logf.FromContext(ctx)

	interruptTime := state.ApplicationStopTime
	if interruptTime.IsZero() {
		interruptTime = time.Now()
	}
	event := goodput.InterruptionEvent{
		TCheckpoint:    state.LastCheckpointTime,
		TInterrupt:     interruptTime,
		Reason:         "WorkloadRestarted",
		CheckpointStep: state.LastCheckpointStep,
		LastStep:       state.LastKnownStep,
	}
	if !state.LastCheckpointTime.IsZero() {
		event.TCh = interruptTime.Sub(state.LastCheckpointTime).Seconds()
	}

	state.PendingInterruption = &event
	state.TrainingStarted = false
	state.ApplicationStartTime = time.Time{}
	state.LastLogFetch = time.Time{} // Reset so next log read captures full history from new pod

	// Clear stall detection inputs so stall detection won't fire on stale data
	// if the Job controller's GM status update was missed.
	measurement.Status.LastStepTimestamp = nil
	measurement.Status.AvgStepTimeSec = ""

	// Persist to status so it survives controller restarts.
	cm := r.buildCumulativeFromStatus(measurement)
	cm.PendingInterruption = &event
	cm.LostWorkTime += event.TCh
	cm.InterruptionCount++
	r.writeStatusFromCumulative(measurement, cm)

	// Clean up instantaneous metrics that become stale on restart.
	cleanupInstantaneousGoodputMetrics(measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name, workflow)

	log.Info("Recorded interruption from workload restart",
		"checkpointStep", event.CheckpointStep,
		"lastStep", event.LastStep,
		"tCh", event.TCh)
}

// getLogProfile fetches a cluster-scoped LogProfile by name.
func (r *GoodputMeasurementReconciler) getLogProfile(ctx context.Context, name string) (*crev1alpha1.LogProfile, error) {
	profile := &crev1alpha1.LogProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, profile); err != nil {
		return nil, fmt.Errorf("failed to get LogProfile %s: %w", name, err)
	}
	return profile, nil
}

// getOrCreateParser returns a cached compiled parser for the LogProfile, or creates one.
//
// The cache is keyed by LogProfile name but validated against its resourceVersion,
// so editing a LogProfile's patterns takes effect on the next reconcile rather
// than requiring a controller restart. Only one entry per profile is retained.
func (r *GoodputMeasurementReconciler) getOrCreateParser(profile *crev1alpha1.LogProfile) (*goodput.ProfileParser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.parsers == nil {
		r.parsers = make(map[string]*cachedProfileParser)
	}

	if c, ok := r.parsers[profile.Name]; ok && c.resourceVersion == profile.ResourceVersion {
		return c.parser, nil
	}

	p, err := goodput.NewProfileParser(profile)
	if err != nil {
		return nil, err
	}
	r.parsers[profile.Name] = &cachedProfileParser{
		resourceVersion: profile.ResourceVersion,
		parser:          p,
	}
	return p, nil
}

// getOrCreateState returns the in-memory job state, creating it if needed.
// When creating a new state, it recovers from the measurement's persisted Status
// so that state survives controller restarts.
func (r *GoodputMeasurementReconciler) getOrCreateState(key string, measurement *crev1alpha1.GoodputMeasurement) *goodput.JobState {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.jobStates == nil {
		r.jobStates = make(map[string]*goodput.JobState)
	}

	if state, ok := r.jobStates[key]; ok {
		return state
	}
	state := &goodput.JobState{
		JobName:   measurement.Name,
		Namespace: measurement.Namespace,
	}

	// Recover state from Status (survives controller restarts).
	if measurement.Status.StartTime != nil {
		state.TrainingStarted = true
	}
	state.HighestStep = measurement.Status.HighestStep
	state.LastKnownStep = measurement.Status.CurrentStep
	state.LastCheckpointStep = measurement.Status.LastCheckpointStep
	state.LastNonWarmupStep = measurement.Status.LastNonWarmupStep
	if measurement.Status.LastCheckpointTime != nil {
		state.LastCheckpointTime = measurement.Status.LastCheckpointTime.Time
	}

	if measurement.Status.WarmupBaseStep != nil {
		state.WarmupBaseStep = *measurement.Status.WarmupBaseStep
	} else {
		state.WarmupBaseStep = -1
	}

	if measurement.Status.ApplicationStartTime != nil {
		state.ApplicationStartTime = measurement.Status.ApplicationStartTime.Time
	}
	if measurement.Status.ApplicationStopTime != nil {
		state.ApplicationStopTime = measurement.Status.ApplicationStopTime.Time
	}

	if measurement.Status.PendingInterruption != nil {
		pi := measurement.Status.PendingInterruption
		event := goodput.InterruptionEvent{
			Reason:         pi.Reason,
			CheckpointStep: pi.CheckpointStep,
			LastStep:       pi.LastStep,
			TCh:            parseFloat(pi.TCh),
		}
		if pi.TCheckpoint != nil {
			event.TCheckpoint = pi.TCheckpoint.Time
			state.LastCheckpointTime = pi.TCheckpoint.Time
		}
		if pi.TInterrupt != nil {
			event.TInterrupt = pi.TInterrupt.Time
		}
		state.PendingInterruption = &event
	}

	r.jobStates[key] = state
	return state
}

// getState returns the in-memory job state if it exists.
func (r *GoodputMeasurementReconciler) getState(key string) *goodput.JobState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobStates == nil {
		return nil
	}
	return r.jobStates[key]
}

// cleanupState removes in-memory state for a deleted measurement.
func (r *GoodputMeasurementReconciler) cleanupState(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobStates != nil {
		delete(r.jobStates, key)
	}
	if r.lastSample != nil {
		delete(r.lastSample, key)
	}
	// Don't clean up parsers — they may be shared across measurements.
}

// buildCumulativeFromStatus reconstructs CumulativeMetrics from the measurement status.
func (r *GoodputMeasurementReconciler) buildCumulativeFromStatus(measurement *crev1alpha1.GoodputMeasurement) *goodput.CumulativeMetrics {
	cm := goodput.NewCumulativeMetrics()

	cm.CurrentStep = measurement.Status.CurrentStep
	cm.HighestStep = measurement.Status.HighestStep
	cm.LastCheckpointStep = measurement.Status.LastCheckpointStep
	cm.InterruptionCount = measurement.Status.InterruptionCount
	cm.TrainingTime = parseFloat(measurement.Status.TrainingTimeSec)
	cm.LostWorkTime = parseFloat(measurement.Status.LostWorkTimeSec)
	cm.RescheduleTime = parseFloat(measurement.Status.RescheduleTimeSec)
	cm.ResumeTime = parseFloat(measurement.Status.ResumeTimeSec)
	cm.CheckpointSaveTime = parseFloat(measurement.Status.CheckpointSaveTimeSec)
	cm.Goodput = parseFloat(measurement.Status.Result)

	if measurement.Status.StartTime != nil {
		cm.TrainingStartTime = measurement.Status.StartTime.Time
	}

	// Recover pending interruption from persisted status.
	if measurement.Status.PendingInterruption != nil {
		pi := measurement.Status.PendingInterruption
		event := &goodput.InterruptionEvent{
			Reason:         pi.Reason,
			CheckpointStep: pi.CheckpointStep,
			LastStep:       pi.LastStep,
			TCh:            parseFloat(pi.TCh),
		}
		if pi.TCheckpoint != nil {
			event.TCheckpoint = pi.TCheckpoint.Time
		}
		if pi.TInterrupt != nil {
			event.TInterrupt = pi.TInterrupt.Time
		}
		cm.PendingInterruption = event
	}

	return cm
}

// writeStatusFromCumulative writes CumulativeMetrics back to the measurement status.
func (r *GoodputMeasurementReconciler) writeStatusFromCumulative(measurement *crev1alpha1.GoodputMeasurement, cm *goodput.CumulativeMetrics) {
	measurement.Status.Result = formatFloat(cm.Goodput)
	measurement.Status.CurrentStep = cm.CurrentStep
	measurement.Status.HighestStep = cm.HighestStep
	measurement.Status.LastCheckpointStep = cm.LastCheckpointStep
	measurement.Status.InterruptionCount = cm.InterruptionCount
	measurement.Status.TrainingTimeSec = formatFloat(cm.TrainingTime)
	measurement.Status.LostWorkTimeSec = formatFloat(cm.LostWorkTime)
	measurement.Status.RescheduleTimeSec = formatFloat(cm.RescheduleTime)
	measurement.Status.ResumeTimeSec = formatFloat(cm.ResumeTime)
	measurement.Status.CheckpointSaveTimeSec = formatFloat(cm.CheckpointSaveTime)

	// Persist pending interruption so it survives controller restarts.
	if cm.PendingInterruption != nil {
		pi := cm.PendingInterruption
		pending := &crev1alpha1.PendingInterruptionStatus{
			CheckpointStep: pi.CheckpointStep,
			LastStep:       pi.LastStep,
			TCh:            formatFloat(pi.TCh),
			Reason:         pi.Reason,
		}
		if !pi.TCheckpoint.IsZero() {
			t := metav1.NewTime(pi.TCheckpoint)
			pending.TCheckpoint = &t
		}
		if !pi.TInterrupt.IsZero() {
			t := metav1.NewTime(pi.TInterrupt)
			pending.TInterrupt = &t
		}
		measurement.Status.PendingInterruption = pending
	} else {
		measurement.Status.PendingInterruption = nil
	}
}

// noteLogProfileUnresolved records an unresolved spec.logProfileRef on the
// measurement, so the cause is visible in status and not only in the controller
// log. This is deliberately not terminal: a cluster-scoped LogProfile can be
// created after the run starts, and the next sample picks it up.
func (r *GoodputMeasurementReconciler) noteLogProfileUnresolved(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement, cause error) {
	message := fmt.Sprintf("LogProfile %q could not be resolved: %v", measurement.Spec.LogProfileRef, cause)
	err := updateStatusWithRetry(ctx, r.Client, measurement, func(m *crev1alpha1.GoodputMeasurement) bool {
		return meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               crev1alpha1.GoodputMeasurementMeasuring,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: m.Generation,
			Reason:             reasonGoodputLogProfileMissing,
			Message:            message,
		})
	})
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to record unresolved LogProfile on status")
	}
}

// setComplete sets the Complete condition, records CompletionTime, and stores the result.
func (r *GoodputMeasurementReconciler) setComplete(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement, reason, message string) error {
	// The computed status payload is carried over on retry: a conflicting write
	// only means the object moved on, not that this measurement's result changed.
	status := measurement.Status.DeepCopy()
	now := metav1.Now()

	err := updateStatusWithRetry(ctx, r.Client, measurement, func(m *crev1alpha1.GoodputMeasurement) bool {
		conditions := m.Status.Conditions
		status.DeepCopyInto(&m.Status)
		m.Status.Conditions = conditions
		m.Status.CompletionTime = &now

		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               crev1alpha1.GoodputMeasurementComplete,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: m.Generation,
		})
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               crev1alpha1.GoodputMeasurementMeasuring,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "Measurement completed",
			ObservedGeneration: m.Generation,
		})
		return true
	})
	if err != nil {
		return fmt.Errorf("failed to update measurement status: %w", err)
	}

	logf.FromContext(ctx).Info("Measurement completed",
		"result", measurement.Status.Result, "reason", reason)
	return nil
}

// handleDeletion handles the cleanup when a GoodputMeasurement is being deleted.
func (r *GoodputMeasurementReconciler) handleDeletion(ctx context.Context, measurement *crev1alpha1.GoodputMeasurement) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(measurement, goodputMeasurementFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("Handling deletion of GoodputMeasurement")
	r.cleanupState(fmt.Sprintf("%s/%s", measurement.Namespace, measurement.Name))

	// Fetch the Job to read the workflow label for metric cleanup.
	var workflow string
	if measurement.Spec.JobRef.Name != "" {
		job := &crev1alpha1.Job{}
		jobKey := types.NamespacedName{Name: measurement.Spec.JobRef.Name, Namespace: measurement.Namespace}
		if err := r.Get(ctx, jobKey, job); err == nil {
			workflow = job.Labels["cre.nvidia.com/workflow"]
		}
	}
	cleanupGoodputMetrics(measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name, workflow)

	controllerutil.RemoveFinalizer(measurement, goodputMeasurementFinalizer)
	if err := r.Update(ctx, measurement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// getSampleInterval returns the configured sample interval or the default.
func (r *GoodputMeasurementReconciler) getSampleInterval(measurement *crev1alpha1.GoodputMeasurement) time.Duration {
	if measurement.Spec.SampleInterval != nil {
		return measurement.Spec.SampleInterval.Duration
	}
	return defaultSampleInterval
}

// getWorkerStrategy returns the worker strategy type from the profile.
func getWorkerStrategy(profile *crev1alpha1.LogProfile) string {
	if profile.Spec.WorkerStrategy != nil {
		return profile.Spec.WorkerStrategy.Type
	}
	return "Single"
}

// computeAvgStepTime computes the average step time excluding warmup steps.
func computeAvgStepTime(result *goodput.ParseResult) float64 {
	return computeAvgStepTimeFiltered(result, false)
}

// computeAvgStepTimeFiltered computes the average step time from parsed results.
// If includeWarmup is true, all steps are included; otherwise only non-warmup steps.
// It prefers explicit StepTiming values; if none are available, it falls back to timestamp gaps.
func computeAvgStepTimeFiltered(result *goodput.ParseResult, includeWarmup bool) float64 {
	var steps []*goodput.TrainingStepInfo
	for _, s := range result.Steps {
		if includeWarmup || !s.IsWarmup {
			steps = append(steps, s)
		}
	}
	if len(steps) == 0 {
		return 0
	}

	// Try explicit StepTiming values first.
	var sum float64
	var count int
	for _, s := range steps {
		if s.StepTiming > 0 {
			sum += s.StepTiming
			count++
		}
	}
	if count > 0 {
		return sum / float64(count)
	}

	// Fall back to timestamp gaps between consecutive steps.
	// When log-interval > 1, each gap covers multiple iterations,
	// so divide by the step delta to get the per-iteration average.
	if len(steps) < 2 {
		return 0
	}
	var gapSum float64
	var gapCount int
	for i := 1; i < len(steps); i++ {
		if !steps[i-1].Timestamp.IsZero() && !steps[i].Timestamp.IsZero() {
			gap := steps[i].Timestamp.Sub(steps[i-1].Timestamp).Seconds()
			if gap > 0 {
				stepDelta := max(steps[i].GlobalStep-steps[i-1].GlobalStep, 1)
				gapSum += gap / float64(stepDelta)
				gapCount++
			}
		}
	}
	if gapCount > 0 {
		return gapSum / float64(gapCount)
	}
	return 0
}

// computeNonWarmupTimeDelta returns the total training time for non-warmup steps
// whose step is above the given watermark. When log-interval > 1
// (e.g. --log-interval 10), each log line's StepTiming is the per-iteration average,
// so it is multiplied by the step delta to get the total time for that interval.
//
// prevStep tracks the last step seen across ALL log lines (including warmup and
// watermarked ones) so the delta to the first new step is always accurate.
// When no prior step exists in the window (prevStep == 0), delta defaults to
// result.LogInterval (if set), otherwise 1.
func computeNonWarmupTimeDelta(result *goodput.ParseResult, lastCountedStep int) float64 {
	var sum float64
	var prevStep int
	for _, s := range result.Steps {
		step := s.GlobalStep
		if s.IsWarmup || step <= lastCountedStep {
			prevStep = step
			continue
		}
		if s.StepTiming > 0 {
			delta := step - prevStep
			if prevStep == 0 || delta < 1 {
				delta = max(result.LogInterval, 1)
			}
			sum += s.StepTiming * float64(delta)
		}
		prevStep = step
	}
	return sum
}

// computeAvgTFLOPS computes the average TFLOPS per GPU including warmup steps.
func computeAvgTFLOPS(result *goodput.ParseResult) float64 {
	var sum float64
	var count int
	for _, s := range result.Steps {
		if s.TFLOPS > 0 {
			sum += s.TFLOPS
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// parseFloat parses a string to float64, returning 0 on error.
func parseFloat(s string) float64 {
	return numstr.Parse(s)
}

// formatFloat formats a float64 as a string with up to 6 decimal places.
func formatFloat(f float64) string {
	return numstr.Format(f)
}

// SetupWithManager sets up the controller with the Manager.
func (r *GoodputMeasurementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&crev1alpha1.GoodputMeasurement{}).
		Named("goodputmeasurement").
		Complete(r)
}
