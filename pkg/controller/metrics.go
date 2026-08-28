// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metric labels
const (
	labelNamespace   = "namespace"
	labelJob         = "job"
	labelStatus      = "status"
	labelNode        = "node"
	labelMeasurement = "measurement"
	labelWorkflow    = "workflow"
)

var (
	// jobStatusGauge tracks the current status of NVCRE jobs.
	// Values: 1 for the current status, 0 for other statuses.
	// Status can be: "in_progress", "succeeded", "failed"
	jobStatusGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_job_status",
			Help: "Current status of NVCRE jobs (1 = current status, 0 = not current status)",
		},
		[]string{labelNamespace, labelJob, labelWorkflow, labelStatus},
	)

	// hardwareFailedJobsTotal counts the total number of jobs that detected hardware failures.
	hardwareFailedJobsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cre_hardware_failed_jobs_total",
			Help: "Total number of jobs that detected hardware failures",
		},
		[]string{labelNamespace, labelJob, labelWorkflow},
	)

	// failedNodesGauge tracks the number of failed nodes per job.
	failedNodesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_job_failed_nodes",
			Help: "Number of nodes with detected hardware failures per job",
		},
		[]string{labelNamespace, labelJob, labelWorkflow},
	)

	// nodeHealthCheckDuration tracks the duration of node health check operations.
	nodeHealthCheckDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cre_node_health_check_duration_seconds",
			Help:    "Duration of node health check operations in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~16s
		},
		[]string{labelNamespace, labelJob, labelWorkflow},
	)

	// nodesEvaluatedTotal counts the total number of node health evaluations performed.
	nodesEvaluatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cre_nodes_evaluated_total",
			Help: "Total number of node health evaluations performed",
		},
		[]string{labelNamespace, labelJob, labelWorkflow},
	)

	// hardwareFailuresDetectedTotal counts the total number of hardware failures detected.
	hardwareFailuresDetectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cre_hardware_failures_detected_total",
			Help: "Total number of hardware failures detected across all evaluations",
		},
		[]string{labelNamespace, labelJob, labelWorkflow, labelNode},
	)

	// reconcileTotal counts the total number of reconciliations.
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cre_reconcile_total",
			Help: "Total number of reconciliation attempts",
		},
		[]string{labelNamespace, labelJob, labelWorkflow, "result"},
	)

	// reconcileDuration tracks the duration of reconciliation operations.
	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cre_reconcile_duration_seconds",
			Help:    "Duration of reconciliation operations in seconds",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms to ~20s
		},
		[]string{labelNamespace, labelJob, labelWorkflow},
	)

	// workloadCreatedTotal counts the total number of workloads created.
	workloadCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cre_workload_created_total",
			Help: "Total number of workloads created",
		},
		[]string{labelNamespace, labelJob, labelWorkflow},
	)

	goodputLabels = []string{labelNamespace, labelMeasurement, labelJob, labelWorkflow}

	goodputRatioGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_ratio",
			Help: "Runtime goodput ratio (0.0 to 1.0)",
		},
		goodputLabels,
	)

	goodputAvgTFLOPSGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_avg_tflops_per_gpu",
			Help: "Average TFLOPS per GPU from goodput measurement",
		},
		goodputLabels,
	)

	goodputRescheduleTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_reschedule_time_seconds",
			Help: "Cumulative reschedule time in seconds",
		},
		goodputLabels,
	)

	goodputResumeTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_resume_time_seconds",
			Help: "Cumulative resume time in seconds",
		},
		goodputLabels,
	)

	goodputCheckpointSaveTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_checkpoint_save_time_seconds",
			Help: "Cumulative checkpoint save time in seconds",
		},
		goodputLabels,
	)

	goodputLostWorkTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_lost_work_time_seconds",
			Help: "Cumulative lost work time in seconds (work done after last checkpoint, lost on restart)",
		},
		goodputLabels,
	)

	goodputTrainingTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_training_time_seconds",
			Help: "Total training wall-clock time in seconds",
		},
		goodputLabels,
	)

	goodputWarmupTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_warmup_time_seconds",
			Help: "Total warmup step time in seconds",
		},
		goodputLabels,
	)

	goodputNonWarmupTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_non_warmup_time_seconds",
			Help: "Total non-warmup step time in seconds",
		},
		goodputLabels,
	)

	goodputAvgStepTimeGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_goodput_avg_step_time_seconds",
			Help: "Average time per training step in seconds",
		},
		goodputLabels,
	)

	topologyNodeLabels = []string{labelNamespace, labelWorkflow, "topology_key", "domain", "node"}

	// topologyValidatedNodesGauge tracks nodes that passed burn-in validation,
	// one time series per node with value 1.
	topologyValidatedNodesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_topology_validated_nodes",
			Help: "Nodes that passed burn-in validation per topology domain (1 per node)",
		},
		topologyNodeLabels,
	)

	// topologyFailedNodesGauge tracks nodes that failed burn-in validation,
	// one time series per node with value 1.
	topologyFailedNodesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_topology_failed_nodes",
			Help: "Nodes that failed burn-in validation per topology domain (1 per node)",
		},
		topologyNodeLabels,
	)

	ncclBandwidthLabels = []string{labelNamespace, labelMeasurement, labelJob, labelWorkflow, "nccl_test", "message_size_bytes"}

	ncclAlgBWGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_nccl_algbw_gbps",
			Help: "NCCL algorithmic bandwidth in GB/s per message size",
		},
		ncclBandwidthLabels,
	)

	ncclBusBWGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cre_nccl_busbw_gbps",
			Help: "NCCL bus bandwidth in GB/s per message size",
		},
		ncclBandwidthLabels,
	)
)

func init() {
	// Register custom metrics with the controller-runtime metrics registry
	metrics.Registry.MustRegister(
		jobStatusGauge,
		hardwareFailedJobsTotal,
		failedNodesGauge,
		nodeHealthCheckDuration,
		nodesEvaluatedTotal,
		hardwareFailuresDetectedTotal,
		reconcileTotal,
		reconcileDuration,
		workloadCreatedTotal,
		goodputRatioGauge,
		goodputAvgTFLOPSGauge,
		goodputRescheduleTimeGauge,
		goodputResumeTimeGauge,
		goodputCheckpointSaveTimeGauge,
		goodputLostWorkTimeGauge,
		goodputTrainingTimeGauge,
		goodputWarmupTimeGauge,
		goodputNonWarmupTimeGauge,
		goodputAvgStepTimeGauge,
		topologyValidatedNodesGauge,
		topologyFailedNodesGauge,
		ncclAlgBWGauge,
		ncclBusBWGauge,
	)
}

// recordJobStatus updates the job status gauge for the given job.
// It sets the current status to 1 and all other statuses to 0.
func recordJobStatus(namespace, jobName, workflow, status string) {
	statuses := []string{"in_progress", "succeeded", "failed"}
	for _, s := range statuses {
		value := float64(0)
		if s == status {
			value = 1
		}
		jobStatusGauge.WithLabelValues(namespace, jobName, workflow, s).Set(value)
	}
}

// recordHardwareFailure increments the hardware failure counters and updates the failed nodes gauge.
func recordHardwareFailure(namespace, jobName, workflow string, failedNodes []string) {
	failedNodesGauge.WithLabelValues(namespace, jobName, workflow).Set(float64(len(failedNodes)))
	for _, node := range failedNodes {
		hardwareFailuresDetectedTotal.WithLabelValues(namespace, jobName, workflow, node).Inc()
	}
}

// recordFirstHardwareFailure increments the counter for jobs that detected hardware failures.
// Should only be called once per job when first hardware failure is detected.
func recordFirstHardwareFailure(namespace, jobName, workflow string) {
	hardwareFailedJobsTotal.WithLabelValues(namespace, jobName, workflow).Inc()
}

// observeNodeHealthCheckDuration records the duration of a node health check operation.
func observeNodeHealthCheckDuration(namespace, jobName, workflow string, durationSeconds float64) {
	nodeHealthCheckDuration.WithLabelValues(namespace, jobName, workflow).Observe(durationSeconds)
}

// recordNodesEvaluated increments the counter for nodes evaluated.
func recordNodesEvaluated(namespace, jobName, workflow string, count int) {
	nodesEvaluatedTotal.WithLabelValues(namespace, jobName, workflow).Add(float64(count))
}

// recordReconcile records a reconciliation attempt with its result.
func recordReconcile(namespace, jobName, workflow, result string) {
	reconcileTotal.WithLabelValues(namespace, jobName, workflow, result).Inc()
}

// observeReconcileDuration records the duration of a reconciliation operation.
func observeReconcileDuration(namespace, jobName, workflow string, durationSeconds float64) {
	reconcileDuration.WithLabelValues(namespace, jobName, workflow).Observe(durationSeconds)
}

// recordWorkloadCreated increments the counter for workloads created.
func recordWorkloadCreated(namespace, jobName, workflow string) {
	workloadCreatedTotal.WithLabelValues(namespace, jobName, workflow).Inc()
}

// goodputMetricValues holds the values for all goodput gauge metrics.
type goodputMetricValues struct {
	GoodputRatio       float64 `json:"goodputRatio"`
	AvgTFLOPS          float64 `json:"avgTFLOPS"`
	AvgStepTime        float64 `json:"avgStepTime"`
	RescheduleTime     float64 `json:"rescheduleTime"`
	ResumeTime         float64 `json:"resumeTime"`
	CheckpointSaveTime float64 `json:"checkpointSaveTime"`
	LostWorkTime       float64 `json:"lostWorkTime"`
	TrainingTime       float64 `json:"trainingTime"`
	WarmupTime         float64 `json:"warmupTime"`
	NonWarmupTime      float64 `json:"nonWarmupTime"`
}

// recordGoodputMetrics sets all goodput gauge metrics.
func recordGoodputMetrics(namespace, measurement, job, workflow string, v goodputMetricValues) {
	labels := []string{namespace, measurement, job, workflow}
	goodputRatioGauge.WithLabelValues(labels...).Set(v.GoodputRatio)
	goodputAvgTFLOPSGauge.WithLabelValues(labels...).Set(v.AvgTFLOPS)
	goodputRescheduleTimeGauge.WithLabelValues(labels...).Set(v.RescheduleTime)
	goodputResumeTimeGauge.WithLabelValues(labels...).Set(v.ResumeTime)
	goodputCheckpointSaveTimeGauge.WithLabelValues(labels...).Set(v.CheckpointSaveTime)
	goodputLostWorkTimeGauge.WithLabelValues(labels...).Set(v.LostWorkTime)
	goodputTrainingTimeGauge.WithLabelValues(labels...).Set(v.TrainingTime)
	goodputWarmupTimeGauge.WithLabelValues(labels...).Set(v.WarmupTime)
	goodputNonWarmupTimeGauge.WithLabelValues(labels...).Set(v.NonWarmupTime)
	goodputAvgStepTimeGauge.WithLabelValues(labels...).Set(v.AvgStepTime)
}

// cleanupGoodputMetrics deletes all goodput gauge label sets for the given measurement.
func cleanupGoodputMetrics(namespace, measurement, job, workflow string) {
	labels := []string{namespace, measurement, job, workflow}
	goodputRatioGauge.DeleteLabelValues(labels...)
	cleanupOperationalGoodputMetrics(namespace, measurement, job, workflow)
}

// cleanupOperationalGoodputMetrics deletes operational goodput gauges (TFLOPs, step time,
// training time, etc.) but preserves the goodput ratio which reports the outcome.
// Called when a measurement completes (success or failure) so stale measurement
// values don't persist in Prometheus.
func cleanupOperationalGoodputMetrics(namespace, measurement, job, workflow string) {
	labels := []string{namespace, measurement, job, workflow}
	goodputAvgTFLOPSGauge.DeleteLabelValues(labels...)
	goodputAvgStepTimeGauge.DeleteLabelValues(labels...)
	goodputRescheduleTimeGauge.DeleteLabelValues(labels...)
	goodputResumeTimeGauge.DeleteLabelValues(labels...)
	goodputCheckpointSaveTimeGauge.DeleteLabelValues(labels...)
	goodputLostWorkTimeGauge.DeleteLabelValues(labels...)
	goodputTrainingTimeGauge.DeleteLabelValues(labels...)
	goodputWarmupTimeGauge.DeleteLabelValues(labels...)
	goodputNonWarmupTimeGauge.DeleteLabelValues(labels...)
}

// cleanupInstantaneousGoodputMetrics deletes instantaneous goodput gauges (TFLOPs, avg step time)
// that should not survive job restarts. These are point-in-time measurements that
// become stale when training restarts from checkpoint.
func cleanupInstantaneousGoodputMetrics(namespace, measurement, job, workflow string) {
	labels := []string{namespace, measurement, job, workflow}
	goodputAvgTFLOPSGauge.DeleteLabelValues(labels...)
	goodputAvgStepTimeGauge.DeleteLabelValues(labels...)
}

// cleanupJobMetrics removes every metric series associated with a deleted job.
//
// Jobs are ephemeral and numerous — a Workflow creates one per group per
// iteration, and diagnose bisection multiplies that again — so any series keyed
// by job name must be removed when the Job goes away. Retaining them (previously
// done for the counters, on the reasoning that cumulative totals aid
// observability) grows the process heap and the Prometheus TSDB without bound
// for the lifetime of the controller. Aggregate totals belong in recording rules
// over the live series, not in per-object series that outlive their object.
//
// Matching is on namespace+job alone: DeletePartialMatch handles metrics that
// carry extra labels (result, node), and it also catches series recorded before
// the workflow label was known — the early-error path in Reconcile records with
// an empty workflow, which an exact-match delete would miss.
func cleanupJobMetrics(namespace, jobName string) {
	jobLabels := prometheus.Labels{labelNamespace: namespace, labelJob: jobName}

	jobStatusGauge.DeletePartialMatch(jobLabels)
	failedNodesGauge.DeletePartialMatch(jobLabels)

	// Histograms are the most expensive to leak: each label set expands to one
	// series per bucket plus _sum and _count.
	reconcileDuration.DeletePartialMatch(jobLabels)
	nodeHealthCheckDuration.DeletePartialMatch(jobLabels)

	reconcileTotal.DeletePartialMatch(jobLabels)
	nodesEvaluatedTotal.DeletePartialMatch(jobLabels)
	workloadCreatedTotal.DeletePartialMatch(jobLabels)
	hardwareFailedJobsTotal.DeletePartialMatch(jobLabels)
	hardwareFailuresDetectedTotal.DeletePartialMatch(jobLabels)
}

// recordTopologyValidatedNodes sets gauge=1 for each node in the domain.
func recordTopologyValidatedNodes(namespace, workflow, topologyKey string, domainNodes map[string][]string) {
	for domain, nodes := range domainNodes {
		for _, node := range nodes {
			topologyValidatedNodesGauge.WithLabelValues(namespace, workflow, topologyKey, domain, node).Set(1)
		}
	}
}

// recordTopologyFailedNodes sets gauge=1 for each node in the domain.
func recordTopologyFailedNodes(namespace, workflow, topologyKey string, domainNodes map[string][]string) {
	for domain, nodes := range domainNodes {
		for _, node := range nodes {
			topologyFailedNodesGauge.WithLabelValues(namespace, workflow, topologyKey, domain, node).Set(1)
		}
	}
}

// cleanupTopologyMetrics deletes all topology gauge entries matching the given namespace/workflow/topologyKey.
func cleanupTopologyMetrics(namespace, workflow, topologyKey string, domainNodes map[string][]string) {
	for domain, nodes := range domainNodes {
		for _, node := range nodes {
			topologyValidatedNodesGauge.DeleteLabelValues(namespace, workflow, topologyKey, domain, node)
			topologyFailedNodesGauge.DeleteLabelValues(namespace, workflow, topologyKey, domain, node)
		}
	}
}

// recordNCCLBandwidthMetrics sets the NCCL bandwidth gauges for a given message size.
func recordNCCLBandwidthMetrics(namespace, measurement, job, workflow, ncclTest, messageSizeBytes string, algBW, busBW float64) {
	labels := []string{namespace, measurement, job, workflow, ncclTest, messageSizeBytes}
	ncclAlgBWGauge.WithLabelValues(labels...).Set(algBW)
	ncclBusBWGauge.WithLabelValues(labels...).Set(busBW)
}

// cleanupNCCLBandwidthMetrics deletes all NCCL bandwidth gauge label sets for the given measurement.
func cleanupNCCLBandwidthMetrics(namespace, measurement, job, workflow, ncclTest string, messageSizes []string) {
	for _, size := range messageSizes {
		labels := []string{namespace, measurement, job, workflow, ncclTest, size}
		ncclAlgBWGauge.DeleteLabelValues(labels...)
		ncclBusBWGauge.DeleteLabelValues(labels...)
	}
}
