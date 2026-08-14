// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nccl"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podutil"
)

const (
	defaultBandwidthMeasurementRequeueInterval = 15 * time.Second

	bandwidthMeasurementFinalizer = "cre.nvidia.com/bandwidthmeasurement-finalizer"

	reasonBandwidthJobRunning   = "JobRunning"
	reasonBandwidthJobSucceeded = "JobSucceeded"
	reasonBandwidthJobFailed    = "JobFailed"

	// reasonBandwidthLogProfileMissing marks a spec.logProfileRef that does not
	// resolve. The measurement keeps retrying, so this is not terminal.
	reasonBandwidthLogProfileMissing = "LogProfileNotFound"
	// reasonBandwidthNoData marks a measurement that ended without parsing
	// anything, so its result is absent rather than zero.
	reasonBandwidthNoData = "NoDataCollected"
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
	LogFetcher podlogs.PodLogFetcher // if nil, defaults to Clientset-backed fetcher

	// RequeueInterval is the interval between reconcile cycles when polling.
	// Tests set this to 1s; production uses 15s.
	RequeueInterval time.Duration

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

func (r *BandwidthMeasurementReconciler) getSampleInterval(m *burninv1alpha1.BandwidthMeasurement) time.Duration {
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

// +kubebuilder:rbac:groups=cre.nvidia.com,resources=bandwidthmeasurements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=bandwidthmeasurements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=bandwidthmeasurements/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop.
func (r *BandwidthMeasurementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	measurement := &burninv1alpha1.BandwidthMeasurement{}
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

func (r *BandwidthMeasurementReconciler) reconcileMeasurement(ctx context.Context, measurement *burninv1alpha1.BandwidthMeasurement) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if measurement.Spec.JobRef.Name == "" {
		log.Info("No JobRef set, nothing to measure")
		return ctrl.Result{}, nil
	}

	// If already complete, nothing to do.
	if cond := meta.FindStatusCondition(measurement.Status.Conditions, burninv1alpha1.BandwidthMeasurementComplete); cond != nil && cond.Status == metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	// Fetch the referenced burnin Job.
	job := &burninv1alpha1.Job{}
	jobKey := types.NamespacedName{Name: measurement.Spec.JobRef.Name, Namespace: measurement.Namespace}
	if err := r.Get(ctx, jobKey, job); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Referenced Job not found, requeueing", "job", jobKey)
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get referenced Job: %w", err)
	}

	// Determine Job phase from conditions.
	if cond := meta.FindStatusCondition(job.Status.Conditions, burninv1alpha1.JobSucceeded); cond != nil && cond.Status == metav1.ConditionTrue {
		// Do a final log parse if we don't have results yet.
		if len(measurement.Status.Results) == 0 {
			if _, err := r.handleRunning(ctx, measurement, job); err != nil {
				log.Error(err, "Failed final log parse on Job success")
			}
		}
		return r.handleTerminal(ctx, measurement, reasonBandwidthJobSucceeded, "Job succeeded")
	}
	if cond := meta.FindStatusCondition(job.Status.Conditions, burninv1alpha1.JobFailed); cond != nil && cond.Status == metav1.ConditionTrue {
		// Attempt final log parse — failed jobs may still have partial bandwidth data.
		if len(measurement.Status.Results) == 0 {
			if _, err := r.handleRunning(ctx, measurement, job); err != nil {
				log.Error(err, "Failed final log parse on Job failure")
			}
		}
		return r.handleTerminal(ctx, measurement, reasonBandwidthJobFailed, "Job failed")
	}
	if cond := meta.FindStatusCondition(job.Status.Conditions, burninv1alpha1.JobInProgress); cond != nil && cond.Status == metav1.ConditionTrue {
		return r.handleRunning(ctx, measurement, job)
	}

	// Job hasn't started yet.
	return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
}

func (r *BandwidthMeasurementReconciler) handleRunning(ctx context.Context, measurement *burninv1alpha1.BandwidthMeasurement, job *burninv1alpha1.Job) (ctrl.Result, error) {
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
	replicatedJobName := "node"
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

	// Parse bandwidth results from new lines.
	dataPoints := parser.ParseBandwidthLogs(lines)
	if len(dataPoints) == 0 {
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
	measurement.Status.Results = mergeBandwidthResults(measurement.Status.Results, dataPoints)

	// Set start time if not already set.
	if measurement.Status.StartTime == nil {
		now := metav1.Now()
		measurement.Status.StartTime = &now
	}

	// Set Measuring condition.
	meta.SetStatusCondition(&measurement.Status.Conditions, metav1.Condition{
		Type:               burninv1alpha1.BandwidthMeasurementMeasuring,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: measurement.Generation,
		Reason:             reasonBandwidthJobRunning,
		Message:            "Referenced Job is running, measurement in progress",
	})

	// Emit Prometheus metrics.
	jobName := measurement.Spec.JobRef.Name
	workflow := measurement.Labels["cre.nvidia.com/workflow"]
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
func (r *BandwidthMeasurementReconciler) noteLogProfileUnresolved(ctx context.Context, measurement *burninv1alpha1.BandwidthMeasurement, cause error) {
	message := fmt.Sprintf("LogProfile %q could not be resolved: %v", measurement.Spec.LogProfileRef, cause)
	err := updateStatusWithRetry(ctx, r.Client, measurement, func(m *burninv1alpha1.BandwidthMeasurement) bool {
		return meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type:               burninv1alpha1.BandwidthMeasurementMeasuring,
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

func (r *BandwidthMeasurementReconciler) handleTerminal(ctx context.Context, measurement *burninv1alpha1.BandwidthMeasurement, reason, message string) (ctrl.Result, error) {
	now := metav1.Now()
	measurement.Status.CompletionTime = &now

	// A measurement that parsed nothing did not measure anything, whatever the
	// Job did. Reporting JobSucceeded here reads as a successful measurement.
	if reason == reasonBandwidthJobSucceeded && len(measurement.Status.Results) == 0 {
		reason = reasonBandwidthNoData
		message = "Job succeeded but no bandwidth data was parsed; check spec.logProfileRef"
	}

	meta.SetStatusCondition(&measurement.Status.Conditions, metav1.Condition{
		Type:               burninv1alpha1.BandwidthMeasurementMeasuring,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: measurement.Generation,
		Reason:             reason,
		Message:            message,
	})
	meta.SetStatusCondition(&measurement.Status.Conditions, metav1.Condition{
		Type:               burninv1alpha1.BandwidthMeasurementComplete,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: measurement.Generation,
		Reason:             reason,
		Message:            message,
	})

	if err := r.Status().Update(ctx, measurement); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update BandwidthMeasurement status: %w", err)
	}

	// Clean up NCCL bandwidth gauges so stale values don't persist in Prometheus.
	jobName := measurement.Spec.JobRef.Name
	workflow := measurement.Labels["cre.nvidia.com/workflow"]
	sizes := make([]string, 0, len(measurement.Status.Results))
	for _, r := range measurement.Status.Results {
		sizes = append(sizes, strconv.FormatInt(r.SizeBytes, 10))
	}
	cleanupNCCLBandwidthMetrics(measurement.Namespace, measurement.Name, jobName, workflow, measurement.Spec.TestType, sizes)

	return ctrl.Result{}, nil
}

func (r *BandwidthMeasurementReconciler) handleDeletion(ctx context.Context, measurement *burninv1alpha1.BandwidthMeasurement) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(measurement, bandwidthMeasurementFinalizer) {
		// Cleanup Prometheus metrics.
		jobName := measurement.Spec.JobRef.Name
		workflow := measurement.Labels["cre.nvidia.com/workflow"]
		sizes := make([]string, 0, len(measurement.Status.Results))
		for _, r := range measurement.Status.Results {
			sizes = append(sizes, strconv.FormatInt(r.SizeBytes, 10))
		}
		cleanupNCCLBandwidthMetrics(measurement.Namespace, measurement.Name, jobName, workflow, measurement.Spec.TestType, sizes)

		// Clean up in-memory state for this measurement.
		key := measurement.Namespace + "/" + measurement.Name
		r.mu.Lock()
		delete(r.lastSample, key)
		delete(r.lastLogFetch, key)
		r.mu.Unlock()

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
func (r *BandwidthMeasurementReconciler) getOrCreateParser(ctx context.Context, logProfileName string) (*nccl.Parser, *burninv1alpha1.LogProfile, error) {
	// The profile is always needed (workerStrategy, containerName) and this Get is
	// served from the informer cache, so fetch it first and use its resourceVersion
	// to decide whether the cached parser is still valid.
	profile := &burninv1alpha1.LogProfile{}
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
func mergeBandwidthResults(existing []burninv1alpha1.BandwidthResult, dataPoints []nccl.BandwidthDataPoint) []burninv1alpha1.BandwidthResult {
	// Index existing results by size for O(1) lookup.
	idx := make(map[int64]int, len(existing))
	for i, r := range existing {
		idx[r.SizeBytes] = i
	}

	// Accumulate new data points per size.
	type accumulator struct {
		sumAlgBW float64
		sumBusBW float64
		count    int
	}
	var newOrder []int64
	accum := make(map[int64]*accumulator)
	for _, dp := range dataPoints {
		a, ok := accum[dp.SizeBytes]
		if !ok {
			a = &accumulator{}
			accum[dp.SizeBytes] = a
			newOrder = append(newOrder, dp.SizeBytes)
		}
		a.sumAlgBW += dp.AlgBW
		a.sumBusBW += dp.BusBW
		a.count++
	}

	// Deep-copy existing results so we don't mutate the caller's slice.
	results := make([]burninv1alpha1.BandwidthResult, len(existing))
	copy(results, existing)

	for _, size := range newOrder {
		a := accum[size]
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
			results = append(results, burninv1alpha1.BandwidthResult{
				SizeBytes: size,
				AlgBW:     strconv.FormatFloat(a.sumAlgBW/float64(a.count), 'f', 2, 64),
				BusBW:     strconv.FormatFloat(a.sumBusBW/float64(a.count), 'f', 2, 64),
				Samples:   a.count,
			})
		}
	}

	return results
}

// SetupWithManager sets up the controller with the Manager.
func (r *BandwidthMeasurementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&burninv1alpha1.BandwidthMeasurement{}).
		Named("bandwidthmeasurement").
		Complete(r)
}
