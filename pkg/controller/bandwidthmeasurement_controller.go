// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleropts "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nccl"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podutil"
)

const (
	defaultBandwidthMeasurementRequeueInterval = 15 * time.Second

	bandwidthMeasurementFinalizer = "nvcre.nvidia.com/bandwidthmeasurement-finalizer"

	reasonBandwidthJobRunning   = "JobRunning"
	reasonBandwidthJobSucceeded = "JobSucceeded"
	reasonBandwidthJobFailed    = "JobFailed"

	// reasonBandwidthLogProfileMissing marks a spec.logProfileRef that does not
	// resolve. The measurement keeps retrying, so this is not terminal.
	reasonBandwidthLogProfileMissing = "LogProfileNotFound"
	// reasonBandwidthNoData marks a measurement that ended without parsing
	// anything, so its result is absent rather than zero.
	reasonBandwidthNoData = "NoDataCollected"
	noDataMessage         = "Job succeeded but no bandwidth data was parsed; check spec.logProfileRef"
	// reasonBandwidthLogsUnavailable marks a measurement whose Job's complete
	// log could not be read, so it holds at most provisional samples, which
	// are not used for threshold evaluation.
	reasonBandwidthLogsUnavailable = "LogsUnavailable"
	// reasonBandwidthFinalReadPending marks a succeeded Job whose final log read
	// failed and is being retried within the grace period.
	reasonBandwidthFinalReadPending = "FinalReadPending"

	// defaultFinalReadGracePeriod is the least time a failed final log read is
	// retried; see finalReadGracePeriod.
	defaultFinalReadGracePeriod = 2 * time.Minute
)

// BandwidthMeasurementReconciler reconciles a BandwidthMeasurement object.
type BandwidthMeasurementReconciler struct {
	client.Client
	// APIReader is an uncached client used for pod discovery. Pod watch events
	// from the informer cache can lag behind the API server by several seconds,
	// causing the "pod not found" requeue loop to spin longer than necessary.
	// A direct API read guarantees the freshest view at the cost of one extra
	// API server call per reconcile cycle while waiting for pods.
	APIReader  client.Reader
	Scheme     *runtime.Scheme
	Clientset  *kubernetes.Clientset
	Recorder   events.EventRecorder
	LogFetcher podlogs.PodLogFetcher // if nil, defaults to Clientset-backed fetcher
	// MaxConcurrentReconciles bounds the number of BandwidthMeasurement objects reconciled concurrently.
	MaxConcurrentReconciles int

	// RequeueInterval is the interval between reconcile cycles when polling.
	// Tests set this to 1s; production uses 15s.
	RequeueInterval time.Duration

	// FinalReadGracePeriod is the least time a failed final log read is
	// retried before the measurement completes without final results; the
	// Job's measurement timeout extends it. Zero uses
	// defaultFinalReadGracePeriod; a negative value completes on the first
	// failed read.
	FinalReadGracePeriod time.Duration

	mu sync.Mutex
	// parsers caches compiled parsers by LogProfile name, invalidated by
	// resourceVersion so LogProfile edits are picked up without a restart.
	parsers      map[string]*cachedNCCLParser
	lastSample   map[string]time.Time // throttles sampling by sampleInterval
	lastLogFetch map[string]time.Time // advancing SinceTime anchor per measurement
}

// cachedNCCLParser pairs a compiled parser with the resourceVersion of the
// LogProfile it was compiled from, so a stale entry can be detected and replaced.
type cachedNCCLParser struct {
	resourceVersion string
	parser          *nccl.Parser
}

// podReader returns the APIReader when available, falling back to the cached
// client. The APIReader bypasses the informer cache so pod lookups reflect the
// current API server state rather than a potentially-stale watch snapshot.
func (r *BandwidthMeasurementReconciler) podReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *BandwidthMeasurementReconciler) getLogFetcher() podlogs.PodLogFetcher {
	if r.LogFetcher != nil {
		return r.LogFetcher
	}
	return podlogs.NewKubernetesLogFetcher(r.Clientset)
}

func (r *BandwidthMeasurementReconciler) getSampleInterval(m *nvcrev1alpha1.BandwidthMeasurement) time.Duration {
	if m.Spec.SampleInterval != nil {
		return m.Spec.SampleInterval.Duration
	}
	return defaultSampleInterval
}

// getRequeueInterval returns the configured requeue interval or the default.
func (r *BandwidthMeasurementReconciler) getRequeueInterval() time.Duration {
	if r.RequeueInterval > 0 {
		return r.RequeueInterval
	}
	return defaultBandwidthMeasurementRequeueInterval
}

// finalReadGracePeriod returns how long a failed final read of job's log is
// retried: FinalReadGracePeriod, extended to the Job's measurement timeout
// when that is longer. The Job waits that long for a result anyway, and a
// measurement that gives up sooner cannot be evaluated, so stopping early
// gains nothing. The floor still bounds the wait for a diagnose round, which
// has no timeout of its own.
func (r *BandwidthMeasurementReconciler) finalReadGracePeriod(job *nvcrev1alpha1.Job) time.Duration {
	if r.FinalReadGracePeriod < 0 {
		return 0
	}
	grace := r.FinalReadGracePeriod
	if grace == 0 {
		grace = defaultFinalReadGracePeriod
	}
	return max(grace, measurementTimeoutFor(job, 0))
}

// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=bandwidthmeasurements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=bandwidthmeasurements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvcre.nvidia.com,resources=bandwidthmeasurements/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop.
func (r *BandwidthMeasurementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	measurement := &nvcrev1alpha1.BandwidthMeasurement{}
	if err := r.Get(ctx, req.NamespacedName, measurement); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("BandwidthMeasurement resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get BandwidthMeasurement: %w", err)
	}

	// Handle deletion.
	if !measurement.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, measurement)
	}

	// Add finalizer if not present. A successful add ends this reconcile; the
	// resulting watch event drives the next one.
	if added, err := ensureFinalizer(ctx, r.Client, measurement, bandwidthMeasurementFinalizer); err != nil || added {
		return ctrl.Result{}, err
	}

	return r.reconcileMeasurement(ctx, measurement)
}

func (r *BandwidthMeasurementReconciler) reconcileMeasurement(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if measurement.Spec.JobRef.Name == "" {
		log.Info("No JobRef set, nothing to measure")
		return ctrl.Result{}, nil
	}

	// If already complete, nothing to do.
	if cond := meta.FindStatusCondition(measurement.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete); cond != nil && cond.Status == metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	// Fetch the referenced NVCRE Job.
	job := &nvcrev1alpha1.Job{}
	jobKey := types.NamespacedName{Name: measurement.Spec.JobRef.Name, Namespace: measurement.Namespace}
	if err := r.Get(ctx, jobKey, job); err != nil {
		if apierrors.IsNotFound(err) {
			return r.handleJobNotFound(ctx, measurement, jobKey)
		}
		return ctrl.Result{}, fmt.Errorf("failed to get referenced Job: %w", err)
	}
	if !measuresJob(measurement, job) {
		// The Job was replaced under the same name (a group retry): the one
		// this measurement was created for is gone.
		return r.finalizeJobGone(ctx, measurement)
	}

	// Determine Job phase from conditions.
	if meta.IsStatusConditionTrue(job.Status.Conditions, nvcrev1alpha1.JobSucceeded) {
		return r.handleJobSucceeded(ctx, measurement, job)
	}
	if meta.IsStatusConditionTrue(job.Status.Conditions, nvcrev1alpha1.JobFailed) {
		return r.handleJobFailed(ctx, measurement, job)
	}
	if cond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobInProgress); cond != nil && cond.Status == metav1.ConditionTrue {
		return r.handleRunning(ctx, measurement, job)
	}

	// Job hasn't started yet.
	return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
}

func (r *BandwidthMeasurementReconciler) handleRunning(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement, job *nvcrev1alpha1.Job) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	key := measurement.Namespace + "/" + measurement.Name

	// Throttle: skip if sampled recently.
	r.mu.Lock()
	if r.lastSample == nil {
		r.lastSample = make(map[string]time.Time)
	}
	if last, ok := r.lastSample[key]; ok && time.Since(last) < r.getSampleInterval(measurement) {
		r.mu.Unlock()
		return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
	}
	r.mu.Unlock()

	// Fetch and compile parser from LogProfile.
	parser, profile, err := r.getOrCreateParser(ctx, measurement.Spec.LogProfileRef)
	if err != nil {
		log.Error(err, "Failed to get parser for LogProfile", "logProfile", measurement.Spec.LogProfileRef)
		r.noteLogProfileUnresolved(ctx, measurement, err)
		return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
	}

	// Determine which pod to read logs from.
	replicatedJobName := labelNode
	if profile.Spec.WorkerStrategy != nil && profile.Spec.WorkerStrategy.ReplicatedJobName != "" {
		replicatedJobName = profile.Spec.WorkerStrategy.ReplicatedJobName
	}

	// Get workload name from Job's workloadRef.
	workloadName := ""
	if job.Status.WorkloadRef != nil {
		workloadName = job.Status.WorkloadRef.Name
	}
	if workloadName == "" {
		log.Info("Job has no workloadRef yet, requeueing")
		return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
	}

	discoverer := podutil.NewWorkerDiscoverer(r.podReader())
	pod, err := discoverer.GetReplicatedJobPod(ctx, measurement.Namespace, workloadName, replicatedJobName)
	if err != nil {
		log.Info("Pod not found yet, requeueing", "replicatedJob", replicatedJobName, "error", err)
		return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
	}

	if !podutil.IsPodRunning(pod) && pod.Status.Phase != corev1.PodSucceeded {
		log.Info("Pod not running yet, requeueing", "pod", pod.Name, "phase", pod.Status.Phase)
		return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
	}

	// Skip reading logs if the container is currently in a waiting state
	// (e.g., CrashLoopBackOff). During SSH-related crash loops the logs
	// contain error messages rather than NCCL output.
	containerName := profile.Spec.ContainerName
	if restarts, waiting := podutil.ContainerRestartStatus(pod, containerName); waiting {
		log.Info("Container is waiting (CrashLoopBackOff), skipping log read",
			"pod", pod.Name, "restarts", restarts)
		return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
	}

	// Build log options with advancing SinceTime anchor.
	// After the first successful parse we read from the last log fetch time;
	// before that we read from the pod's start time.
	opts := podlogs.LogOptions{
		Container: containerName,
	}
	r.mu.Lock()
	if r.lastLogFetch == nil {
		r.lastLogFetch = make(map[string]time.Time)
	}
	var anchor time.Time
	if lastFetch, ok := r.lastLogFetch[key]; ok {
		anchor = lastFetch.Add(-time.Second)
	} else if pod.Status.StartTime != nil {
		anchor = pod.Status.StartTime.Time
	}
	r.mu.Unlock()

	// Clamp how far back the anchor may reach. When a container is crash-looping
	// the anchor is deliberately not advanced (see below), so it would otherwise
	// stay pinned to pod start and re-read the entire log on every sample for the
	// life of the run.
	if !anchor.IsZero() {
		if oldest := time.Now().Add(-maxLogLookback); anchor.Before(oldest) {
			log.V(1).Info("Clamping log read anchor to max lookback",
				"pod", pod.Name, "anchor", anchor, "maxLookback", maxLogLookback)
			anchor = oldest
		}
		sinceTime := metav1.NewTime(anchor)
		opts.SinceTime = &sinceTime
	}

	fetcher := r.getLogFetcher()
	lines, err := fetcher.FetchLogs(ctx, measurement.Namespace, pod.Name, opts)
	if err != nil {
		log.Error(err, "Failed to fetch logs", "pod", pod.Name)
		return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
	}

	// Parse bandwidth results and NCCL network names from new lines.
	dataPoints := parser.ParseBandwidthLogs(lines)
	transports := parser.ParseTransports(lines)
	if len(dataPoints) == 0 && len(transports) == 0 {
		restarts, _ := podutil.ContainerRestartStatus(pod, containerName)
		if restarts > 0 {
			// Container has restarted — don't advance the anchor. The empty
			// lines may be from a previous crash (SSH errors). The real NCCL
			// output will appear after recovery; we need to re-read from the
			// same point to catch it.
			log.Info("No bandwidth results found and container has restarts, not advancing anchor",
				"pod", pod.Name, "restarts", restarts, "lines", len(lines))
		} else {
			// No restarts — safe to advance the anchor past these empty lines.
			log.Info("No bandwidth results found in logs yet", "pod", pod.Name, "lines", len(lines))
			r.mu.Lock()
			r.lastLogFetch[key] = time.Now()
			r.mu.Unlock()
		}
		return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
	}

	// Merge new data points into existing results (running averages).
	if len(dataPoints) > 0 {
		measurement.Status.Results = mergeBandwidthResults(measurement.Status.Results, dataPoints)
	}
	if len(transports) > 0 {
		measurement.Status.Transport = mergeTransports(measurement.Status.Transport, transports)
	}

	// Set start time if not already set.
	if measurement.Status.StartTime == nil {
		now := metav1.Now()
		measurement.Status.StartTime = &now
	}

	// Set Measuring condition.
	meta.SetStatusCondition(&measurement.Status.Conditions, metav1.Condition{
		Type:               nvcrev1alpha1.BandwidthMeasurementMeasuring,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: measurement.Generation,
		Reason:             reasonBandwidthJobRunning,
		Message:            "Referenced Job is running, measurement in progress",
	})

	// Emit Prometheus metrics.
	jobName := measurement.Spec.JobRef.Name
	workflow := measurement.Labels["nvcre.nvidia.com/workflow"]
	ncclTest := measurement.Spec.TestType
	for _, r := range measurement.Status.Results {
		algBW, _ := strconv.ParseFloat(r.AlgBW, 64)
		busBW, _ := strconv.ParseFloat(r.BusBW, 64)
		recordNCCLBandwidthMetrics(measurement.Namespace, measurement.Name, jobName, workflow,
			ncclTest, strconv.FormatInt(r.SizeBytes, 10), algBW, busBW)
	}

	if err := r.Status().Update(ctx, measurement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update BandwidthMeasurement status: %w", err)
	}

	// Record sample time and advance the log fetch anchor.
	now := time.Now()
	r.mu.Lock()
	r.lastSample[key] = now
	r.lastLogFetch[key] = now
	r.mu.Unlock()

	return ctrl.Result{RequeueAfter: r.getSampleInterval(measurement)}, nil
}

// noteLogProfileUnresolved records an unresolved spec.logProfileRef on the
// measurement, so the cause is visible in status and not only in the controller
// log. This is deliberately not terminal: a cluster-scoped LogProfile can be
// created after the run starts, and the next sample picks it up.
func (r *BandwidthMeasurementReconciler) noteLogProfileUnresolved(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement, cause error) {
	message := fmt.Sprintf("LogProfile %q could not be resolved: %v", measurement.Spec.LogProfileRef, cause)
	// One event per failed resolution attempt: this helper only runs when a
	// sampling pass actively tried and failed to fetch or compile the
	// LogProfile, never from observed state; a Complete measurement returns
	// before any fetch. Repeated failing attempts aggregate into one Event.
	r.warnf(measurement, reasonBandwidthLogProfileMissing, "%s", message)
	err := updateStatusWithRetry(ctx, r.Client, measurement, func(m *nvcrev1alpha1.BandwidthMeasurement) bool {
		return meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.BandwidthMeasurementMeasuring,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: m.Generation,
			Reason:             reasonBandwidthLogProfileMissing,
			Message:            message,
		})
	})
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to record unresolved LogProfile on status")
	}
}

// handleJobSucceeded finalizes the measurement from the launcher's complete
// log. The samples taken while the Job ran are provisional: they can stop
// short of the end of the sweep, where the largest message sizes and so the
// peak bandwidth are printed, and they can count a row twice when read windows
// overlap. The final read parses every row once and replaces them, so the
// result is a function of the log alone, whatever the sampling did.
//
// When the log cannot be read, the read is retried for finalReadGracePeriod
// and the measurement then completes as LogsUnavailable, with the read error.
// NoDataCollected is kept for a log that was read in full and held no rows.
// Threshold evaluation treats both as unmeasured, so a verdict never rests on
// provisional data.
func (r *BandwidthMeasurementReconciler) handleJobSucceeded(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement, job *nvcrev1alpha1.Job) (ctrl.Result, error) {
	anchor := terminalAnchor(job)
	final, readErr := r.readFinalResults(ctx, measurement, job)
	if readErr == nil {
		if len(final.results) == 0 {
			return r.finalizeTerminal(ctx, measurement, anchor, terminalOutcome{
				reason: reasonBandwidthNoData, message: noDataMessage,
				results: final.results, replace: true, transports: final.transports,
			})
		}
		return r.finalizeTerminal(ctx, measurement, anchor, terminalOutcome{
			reason: reasonBandwidthJobSucceeded, message: "Job succeeded",
			results: final.results, replace: true, transports: final.transports,
		})
	}

	// A log that cannot be paged fails the same way on every attempt.
	retryable := !errors.Is(readErr, podlogs.ErrLogUnpageable)
	pendingSince := finalReadPendingSince(measurement)
	if pendingSince.IsZero() {
		pendingSince = time.Now()
	}
	if wait := r.finalReadGracePeriod(job) - time.Since(pendingSince); retryable && wait > 0 {
		r.warnf(measurement, reasonBandwidthFinalReadPending, "Final log read failed, retrying: %v", readErr)
		if err := r.markFinalReadPending(ctx, measurement); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: min(wait, r.getRequeueInterval())}, nil
	}

	logf.FromContext(ctx).Info("Final log read failed, completing without final results", "error", readErr)
	return r.finalizeTerminal(ctx, measurement, anchor, terminalOutcome{
		reason:  reasonBandwidthLogsUnavailable,
		message: fmt.Sprintf("Job succeeded but its log could not be read in full (%v); %s", readErr, withoutFinalResults(measurement)),
	})
}

// withoutFinalResults describes what a measurement completing without its
// final read is left with.
func withoutFinalResults(measurement *nvcrev1alpha1.BandwidthMeasurement) string {
	if len(measurement.Status.Results) == 0 {
		return "no bandwidth data was parsed"
	}
	return "results are provisional and are not used for threshold evaluation"
}

// handleJobFailed makes one best-effort final read without a grace period: the
// Workflow deletes a failed Job's workload at once, and a failed Job is never
// evaluated against thresholds. Failing that, the provisional results stay.
func (r *BandwidthMeasurementReconciler) handleJobFailed(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement, job *nvcrev1alpha1.Job) (ctrl.Result, error) {
	outcome := terminalOutcome{reason: reasonBandwidthJobFailed, message: "Job failed"}
	if final, err := r.readFinalResults(ctx, measurement, job); err == nil {
		outcome.transports = final.transports
		if len(final.results) > 0 {
			outcome.results, outcome.replace = final.results, true
		}
	}
	return r.finalizeTerminal(ctx, measurement, terminalAnchor(job), outcome)
}

// handleJobNotFound completes a measurement whose Job is gone, once the API
// server confirms it: an iteration's Jobs are deleted with their workloads, and
// without this the measurement would requeue for as long as it exists. A cache
// miss alone is not proof, so without an API reader the measurement waits.
func (r *BandwidthMeasurementReconciler) handleJobNotFound(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement, jobKey types.NamespacedName) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if r.APIReader == nil {
		log.Info("Referenced Job not found, requeueing", "job", jobKey)
		return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
	}
	if err := r.APIReader.Get(ctx, jobKey, &nvcrev1alpha1.Job{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to confirm referenced Job %s: %w", jobKey, err)
		}
		log.Info("Referenced Job not yet in cache, requeueing", "job", jobKey)
		return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
	}
	// A measurement can be created before its Job, for example by applying
	// both at once. Only one that has seen its Job knows it is gone.
	if !sawJob(measurement) {
		log.Info("Referenced Job does not exist yet, requeueing", "job", jobKey)
		return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
	}
	return r.finalizeJobGone(ctx, measurement)
}

// sawJob reports whether a measurement shows its Job existed: it was created
// for a Job instance, or it has measured.
func sawJob(measurement *nvcrev1alpha1.BandwidthMeasurement) bool {
	_, created := measurement.Annotations[annotationJobUID]
	return created || measurement.Status.StartTime != nil || len(measurement.Status.Results) > 0 ||
		meta.FindStatusCondition(measurement.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementMeasuring) != nil
}

// finalizeJobGone completes a measurement whose Job no longer exists.
func (r *BandwidthMeasurementReconciler) finalizeJobGone(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement) (ctrl.Result, error) {
	return r.finalizeTerminal(ctx, measurement, time.Now(), terminalOutcome{
		reason:  reasonBandwidthLogsUnavailable,
		message: "Referenced Job no longer exists; " + withoutFinalResults(measurement),
	})
}

// finalRead is what the final log read collects: every bandwidth row,
// aggregated, and the distinct NCCL network names the log reports.
type finalRead struct {
	results    []nvcrev1alpha1.BandwidthResult
	transports []string
}

// readFinalResults reads the launcher's current log in full and aggregates
// every bandwidth row in it, each exactly once. It also collects the NCCL
// network names, so a Job that finishes before a running sample sees its
// "Using network" line still records the transport.
func (r *BandwidthMeasurementReconciler) readFinalResults(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement, job *nvcrev1alpha1.Job) (finalRead, error) {
	if job.Status.WorkloadRef == nil || job.Status.WorkloadRef.Name == "" {
		return finalRead{}, fmt.Errorf("job %s has no workload reference", job.Name)
	}
	parser, profile, err := r.getOrCreateParser(ctx, measurement.Spec.LogProfileRef)
	if err != nil {
		return finalRead{}, err
	}
	replicatedJobName := labelNode
	if profile.Spec.WorkerStrategy != nil && profile.Spec.WorkerStrategy.ReplicatedJobName != "" {
		replicatedJobName = profile.Spec.WorkerStrategy.ReplicatedJobName
	}
	pod, err := podutil.NewWorkerDiscoverer(r.podReader()).GetReplicatedJobPod(
		ctx, measurement.Namespace, job.Status.WorkloadRef.Name, replicatedJobName)
	if err != nil {
		return finalRead{}, err
	}
	// Only a finished pod's log is final; a pod still running may yet write
	// the rows the read exists to capture.
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return finalRead{}, fmt.Errorf("pod %s has not finished (%s)", pod.Name, pod.Status.Phase)
	}

	// Rows are summed per size as they are read, so memory is bounded by the
	// number of message sizes rather than the length of the log, and each
	// average is rounded once, whatever the page boundaries. Network names
	// are kept as a set for the same reason.
	var agg bandwidthAggregate
	seen := make(map[string]struct{})
	var transports []string
	err = podlogs.ReadAll(ctx, r.getLogFetcher(), measurement.Namespace, pod.Name,
		podlogs.ReadAllOptions{Container: profile.Spec.ContainerName},
		func(line string) {
			if dp, ok := parser.ParseBandwidthLine(line); ok {
				agg.add(dp)
				return
			}
			if name, ok := parser.ParseTransportLine(line); ok {
				if _, dup := seen[name]; !dup {
					seen[name] = struct{}{}
					transports = append(transports, name)
				}
			}
		})
	if err != nil {
		return finalRead{}, fmt.Errorf("reading log of pod %s: %w", pod.Name, err)
	}
	return finalRead{results: agg.results(), transports: mergeTransports(nil, transports)}, nil
}

// terminalOutcome is what a measurement completes with. results replace the
// provisional ones only when replace is set. transports are unioned into the
// recorded set, never replace it.
type terminalOutcome struct {
	reason     string
	message    string
	results    []nvcrev1alpha1.BandwidthResult
	replace    bool
	transports []string
}

// finalizeTerminal writes the terminal status in one update: results,
// completionTime anchored to the Job's terminal transition, Measuring=False and
// Complete=True. It is a no-op on a measurement that is already Complete, so a
// replay from a stale cache cannot overwrite the first terminal write.
func (r *BandwidthMeasurementReconciler) finalizeTerminal(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement, anchor time.Time, outcome terminalOutcome) (ctrl.Result, error) {
	provisional := measurement.Status.Results
	completion := metav1.NewTime(anchor)
	err := updateStatusWithRetry(ctx, r.Client, measurement, func(m *nvcrev1alpha1.BandwidthMeasurement) bool {
		if meta.IsStatusConditionTrue(m.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete) {
			return false
		}
		if outcome.replace {
			m.Status.Results = outcome.results
		}
		m.Status.Transport = mergeTransports(m.Status.Transport, outcome.transports)
		m.Status.CompletionTime = &completion
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.BandwidthMeasurementMeasuring,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: m.Generation,
			Reason:             outcome.reason,
			Message:            outcome.message,
		})
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.BandwidthMeasurementComplete,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: m.Generation,
			Reason:             outcome.reason,
			Message:            outcome.message,
		})
		return true
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update BandwidthMeasurement status: %w", err)
	}

	// Clean up NCCL bandwidth gauges so stale values don't persist in
	// Prometheus. Provisional samples can carry sizes the final read does not,
	// so both sets are cleared.
	sizes := make([]string, 0, len(provisional)+len(measurement.Status.Results))
	for _, set := range [][]nvcrev1alpha1.BandwidthResult{provisional, measurement.Status.Results} {
		for _, res := range set {
			sizes = append(sizes, strconv.FormatInt(res.SizeBytes, 10))
		}
	}
	cleanupNCCLBandwidthMetrics(measurement.Namespace, measurement.Name, measurement.Spec.JobRef.Name,
		measurement.Labels["nvcre.nvidia.com/workflow"], measurement.Spec.TestType, sizes)
	r.forgetSampling(measurement.Namespace + "/" + measurement.Name)

	return ctrl.Result{}, nil
}

// finalReadPendingSince returns when the final read first failed, or zero if
// it has not. It is read from status so the grace period survives a
// controller restart.
func finalReadPendingSince(measurement *nvcrev1alpha1.BandwidthMeasurement) time.Time {
	cond := meta.FindStatusCondition(measurement.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementMeasuring)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonBandwidthFinalReadPending {
		return time.Time{}
	}
	return cond.LastTransitionTime.Time
}

// markFinalReadPending records the first failed final read. The condition is
// replaced rather than updated so its transition time is when the wait began,
// even if Measuring was already False for another reason.
func (r *BandwidthMeasurementReconciler) markFinalReadPending(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement) error {
	return updateStatusWithRetry(ctx, r.Client, measurement, func(m *nvcrev1alpha1.BandwidthMeasurement) bool {
		if !finalReadPendingSince(m).IsZero() {
			return false
		}
		meta.RemoveStatusCondition(&m.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementMeasuring)
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.BandwidthMeasurementMeasuring,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: m.Generation,
			Reason:             reasonBandwidthFinalReadPending,
			Message:            "Job succeeded; waiting to read its complete log",
		})
		return true
	})
}

// forgetSampling drops the in-memory sampling state of a measurement.
func (r *BandwidthMeasurementReconciler) forgetSampling(key string) {
	r.mu.Lock()
	delete(r.lastSample, key)
	delete(r.lastLogFetch, key)
	r.mu.Unlock()
}

func (r *BandwidthMeasurementReconciler) handleDeletion(ctx context.Context, measurement *nvcrev1alpha1.BandwidthMeasurement) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(measurement, bandwidthMeasurementFinalizer) {
		// Cleanup Prometheus metrics.
		jobName := measurement.Spec.JobRef.Name
		workflow := measurement.Labels["nvcre.nvidia.com/workflow"]
		sizes := make([]string, 0, len(measurement.Status.Results))
		for _, r := range measurement.Status.Results {
			sizes = append(sizes, strconv.FormatInt(r.SizeBytes, 10))
		}
		cleanupNCCLBandwidthMetrics(measurement.Namespace, measurement.Name, jobName, workflow, measurement.Spec.TestType, sizes)
		r.forgetSampling(measurement.Namespace + "/" + measurement.Name)

		controllerutil.RemoveFinalizer(measurement, bandwidthMeasurementFinalizer)
		if err := r.Update(ctx, measurement); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
		}
		log.Info("Removed finalizer from BandwidthMeasurement")
	}

	return ctrl.Result{}, nil
}

// getOrCreateParser returns a cached NCCL parser or creates one from the LogProfile.
//
// The cache is keyed by LogProfile name but validated against its resourceVersion,
// so editing a LogProfile's patterns takes effect on the next reconcile rather
// than requiring a controller restart. Only one entry per profile is retained.
func (r *BandwidthMeasurementReconciler) getOrCreateParser(ctx context.Context, logProfileName string) (*nccl.Parser, *nvcrev1alpha1.LogProfile, error) {
	// The profile is always needed (workerStrategy, containerName) and this Get is
	// served from the informer cache, so fetch it first and use its resourceVersion
	// to decide whether the cached parser is still valid.
	profile := &nvcrev1alpha1.LogProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: logProfileName}, profile); err != nil {
		return nil, nil, fmt.Errorf("getting LogProfile %s: %w", logProfileName, err)
	}

	r.mu.Lock()
	if r.parsers == nil {
		r.parsers = make(map[string]*cachedNCCLParser)
	}
	if c, ok := r.parsers[logProfileName]; ok && c.resourceVersion == profile.ResourceVersion {
		r.mu.Unlock()
		return c.parser, profile, nil
	}
	r.mu.Unlock()

	parser, err := nccl.NewParser(profile)
	if err != nil {
		return nil, nil, fmt.Errorf("creating parser from LogProfile %s: %w", logProfileName, err)
	}

	r.mu.Lock()
	r.parsers[logProfileName] = &cachedNCCLParser{
		resourceVersion: profile.ResourceVersion,
		parser:          parser,
	}
	r.mu.Unlock()

	return parser, profile, nil
}

// mergeBandwidthResults merges new data points into existing results using running averages.
// For sizes already in existing, the average is updated: newAvg = (oldAvg*oldN + sum(new)) / (oldN + newN).
// New sizes are appended in the order they appear in the data points.
func mergeBandwidthResults(existing []nvcrev1alpha1.BandwidthResult, dataPoints []nccl.BandwidthDataPoint) []nvcrev1alpha1.BandwidthResult {
	var agg bandwidthAggregate
	for _, dp := range dataPoints {
		agg.add(dp)
	}
	return mergeAggregate(existing, &agg)
}

// mergeAggregate merges summed rows into existing results; see
// mergeBandwidthResults.
func mergeAggregate(existing []nvcrev1alpha1.BandwidthResult, agg *bandwidthAggregate) []nvcrev1alpha1.BandwidthResult {
	// Index existing results by size for O(1) lookup.
	idx := make(map[int64]int, len(existing))
	for i, r := range existing {
		idx[r.SizeBytes] = i
	}

	// Deep-copy existing results so we don't mutate the caller's slice.
	results := make([]nvcrev1alpha1.BandwidthResult, len(existing))
	copy(results, existing)

	for _, size := range agg.order {
		a := agg.sums[size]
		if i, ok := idx[size]; ok {
			// Merge into existing entry.
			oldAlg, _ := strconv.ParseFloat(results[i].AlgBW, 64)
			oldBus, _ := strconv.ParseFloat(results[i].BusBW, 64)
			oldN := results[i].Samples
			totalN := oldN + a.count
			results[i].AlgBW = strconv.FormatFloat((oldAlg*float64(oldN)+a.sumAlgBW)/float64(totalN), 'f', 2, 64)
			results[i].BusBW = strconv.FormatFloat((oldBus*float64(oldN)+a.sumBusBW)/float64(totalN), 'f', 2, 64)
			results[i].Samples = totalN
		} else {
			// New size — append.
			results = append(results, nvcrev1alpha1.BandwidthResult{
				SizeBytes: size,
				AlgBW:     strconv.FormatFloat(a.sumAlgBW/float64(a.count), 'f', 2, 64),
				BusBW:     strconv.FormatFloat(a.sumBusBW/float64(a.count), 'f', 2, 64),
				Samples:   a.count,
			})
		}
	}

	return results
}

// bandwidthAggregate sums bandwidth rows per message size, keeping sizes in
// the order they first appear.
type bandwidthAggregate struct {
	order []int64
	sums  map[int64]*bandwidthSum
}

type bandwidthSum struct {
	sumAlgBW float64
	sumBusBW float64
	count    int
}

func (a *bandwidthAggregate) add(dp nccl.BandwidthDataPoint) {
	if a.sums == nil {
		a.sums = make(map[int64]*bandwidthSum)
	}
	s, ok := a.sums[dp.SizeBytes]
	if !ok {
		s = &bandwidthSum{}
		a.sums[dp.SizeBytes] = s
		a.order = append(a.order, dp.SizeBytes)
	}
	s.sumAlgBW += dp.AlgBW
	s.sumBusBW += dp.BusBW
	s.count++
}

// results averages each size over its rows, rounding once.
func (a *bandwidthAggregate) results() []nvcrev1alpha1.BandwidthResult {
	return mergeAggregate(nil, a)
}

// mergeTransports unions newly observed NCCL network names into the existing
// status set. The result is sorted for stable CRD and golden-file output.
func mergeTransports(existing, observed []string) []string {
	if len(observed) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(observed))
	var out []string
	add := func(vals []string) {
		for _, t := range vals {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	add(existing)
	add(observed)
	slices.Sort(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// warnf emits a Warning event if the Recorder is configured. Every
// BandwidthMeasurement-tier event is a warning; the Workflow reconciler's
// eventf takes an explicit type because it emits Normal events too.
//
// Safe to call when Recorder is nil (e.g. in unit tests, or any embedding that
// constructs BandwidthMeasurementReconciler directly).
func (r *BandwidthMeasurementReconciler) warnf(obj runtime.Object, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, reason, reason, "%s", formatEventNote(messageFmt, args...))
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *BandwidthMeasurementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nvcrev1alpha1.BandwidthMeasurement{}).
		Watches(
			&nvcrev1alpha1.Job{},
			handler.EnqueueRequestsFromMapFunc(r.jobToBandwidthMeasurements),
			builder.WithPredicates(jobPhaseChangePredicate()),
		).
		Named("bandwidthmeasurement").
		WithOptions(controlleropts.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}
