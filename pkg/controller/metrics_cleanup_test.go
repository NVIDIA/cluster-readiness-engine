// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestCleanupJobMetricsRemovesEveryJobScopedSeries pins the cardinality
// contract: a deleted Job must leave nothing behind. Jobs are created per group
// per iteration (and again per diagnose bisection round), so any series that
// survives its Job accumulates for the lifetime of the controller process.
//
// Counters and histograms were previously exempt from cleanup, which is what
// this guards against.
func TestCleanupJobMetricsRemovesEveryJobScopedSeries(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "cleanup-job-metrics-cardinality",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			MetricNames []string `yaml:"metricNames"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		const (
			ns  = "cleanup-test-ns"
			job = "cleanup-test-job"
			wf  = "cleanup-test-workflow"
		)

		// Every metric keyed by job, with the collector and the number of series one
		// label set expands to. Histograms expand to buckets + _sum + _count, which
		// is why leaking them is the most expensive.
		jobScoped := []struct {
			name    string
			collect prometheus.Collector
			record  func()
		}{
			{"cre_job_status", jobStatusGauge, func() { recordJobStatus(ns, job, wf, "in_progress") }},
			{"cre_job_failed_nodes", failedNodesGauge, func() { recordHardwareFailure(ns, job, wf, []string{"node-a"}) }},
			{"cre_hardware_failures_detected_total", hardwareFailuresDetectedTotal, func() { recordHardwareFailure(ns, job, wf, []string{"node-a"}) }},
			{"cre_hardware_failed_jobs_total", hardwareFailedJobsTotal, func() { recordFirstHardwareFailure(ns, job, wf) }},
			{"cre_nodes_evaluated_total", nodesEvaluatedTotal, func() { recordNodesEvaluated(ns, job, wf, 8) }},
			{"cre_workload_created_total", workloadCreatedTotal, func() { recordWorkloadCreated(ns, job, wf) }},
			{"cre_reconcile_total", reconcileTotal, func() { recordReconcile(ns, job, wf, "success") }},
			{"cre_reconcile_duration_seconds", reconcileDuration, func() { observeReconcileDuration(ns, job, wf, 0.25) }},
			{"cre_node_health_check_duration_seconds", nodeHealthCheckDuration, func() { observeNodeHealthCheckDuration(ns, job, wf, 0.1) }},
		}

		// The golden file pins the exact set of job-scoped metric names this test
		// covers. If jobScoped and input.yaml drift apart (a metric added to one
		// but not the other), fail loudly instead of silently under-testing.
		if len(in.MetricNames) != len(jobScoped) {
			return fmt.Errorf("input.yaml lists %d metric names, jobScoped has %d entries", len(in.MetricNames), len(jobScoped))
		}
		for i, m := range jobScoped {
			if in.MetricNames[i] != m.name {
				return fmt.Errorf("input.yaml[%d] = %q, want %q", i, in.MetricNames[i], m.name)
			}
		}

		baseline := make(map[string]int, len(jobScoped))
		for _, m := range jobScoped {
			baseline[m.name] = promtest.CollectAndCount(m.collect)
		}

		// Also record on the early-error path, which fires before the workflow label
		// is known. An exact-match delete would miss this series.
		recordReconcile(ns, job, "", "error")

		for _, m := range jobScoped {
			m.record()
		}

		type seriesResult struct {
			Name                        string `json:"name"`
			GrewAfterRecording          bool   `json:"grewAfterRecording"`
			RestoredToBaselineOnCleanup bool   `json:"restoredToBaselineOnCleanup"`
		}

		results := make([]seriesResult, len(jobScoped))
		for i, m := range jobScoped {
			results[i].Name = m.name
			// Expected the test to have created at least one series.
			results[i].GrewAfterRecording = promtest.CollectAndCount(m.collect) > baseline[m.name]
		}

		cleanupJobMetrics(ns, job)

		for i, m := range jobScoped {
			// Expected no series to have survived cleanupJobMetrics.
			results[i].RestoredToBaselineOnCleanup = promtest.CollectAndCount(m.collect) == baseline[m.name]
		}

		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
