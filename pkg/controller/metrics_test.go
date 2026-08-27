// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/yaml"
)

func TestRecordGoodputMetrics(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "record-goodput-metrics",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input goodputMetricValues
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		// Use case name in labels to isolate from other cases.
		const testNS = "default"
		ns, meas, job, wf := testNS, "gm-"+tc.Name, "job-"+tc.Name, "wf-"+tc.Name
		labels := []string{ns, meas, job, wf}

		recordGoodputMetrics(ns, meas, job, wf, input)
		defer cleanupGoodputMetrics(ns, meas, job, wf)

		result := map[string]float64{
			"avgStepTime":        promtest.ToFloat64(goodputAvgStepTimeGauge.WithLabelValues(labels...)),
			"avgTFLOPS":          promtest.ToFloat64(goodputAvgTFLOPSGauge.WithLabelValues(labels...)),
			"checkpointSaveTime": promtest.ToFloat64(goodputCheckpointSaveTimeGauge.WithLabelValues(labels...)),
			"goodputRatio":       promtest.ToFloat64(goodputRatioGauge.WithLabelValues(labels...)),
			"lostWorkTime":       promtest.ToFloat64(goodputLostWorkTimeGauge.WithLabelValues(labels...)),
			"nonWarmupTime":      promtest.ToFloat64(goodputNonWarmupTimeGauge.WithLabelValues(labels...)),
			"rescheduleTime":     promtest.ToFloat64(goodputRescheduleTimeGauge.WithLabelValues(labels...)),
			"resumeTime":         promtest.ToFloat64(goodputResumeTimeGauge.WithLabelValues(labels...)),
			"trainingTime":       promtest.ToFloat64(goodputTrainingTimeGauge.WithLabelValues(labels...)),
			"warmupTime":         promtest.ToFloat64(goodputWarmupTimeGauge.WithLabelValues(labels...)),
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestRecordGoodputMetricsSetsAllGauges(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "record-goodput-all-gauges",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input goodputMetricValues
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const testNS = "default"
		ns, meas, job, wf := testNS, "gm-allgauges-"+tc.Name, "job-allgauges-"+tc.Name, "wf-allgauges-"+tc.Name
		labels := []string{ns, meas, job, wf}

		recordGoodputMetrics(ns, meas, job, wf, input)
		defer cleanupGoodputMetrics(ns, meas, job, wf)

		gaugeValues := map[string]float64{
			"avgStepTime":        promtest.ToFloat64(goodputAvgStepTimeGauge.WithLabelValues(labels...)),
			"avgTFLOPS":          promtest.ToFloat64(goodputAvgTFLOPSGauge.WithLabelValues(labels...)),
			"checkpointSaveTime": promtest.ToFloat64(goodputCheckpointSaveTimeGauge.WithLabelValues(labels...)),
			"goodputRatio":       promtest.ToFloat64(goodputRatioGauge.WithLabelValues(labels...)),
			"lostWorkTime":       promtest.ToFloat64(goodputLostWorkTimeGauge.WithLabelValues(labels...)),
			"nonWarmupTime":      promtest.ToFloat64(goodputNonWarmupTimeGauge.WithLabelValues(labels...)),
			"rescheduleTime":     promtest.ToFloat64(goodputRescheduleTimeGauge.WithLabelValues(labels...)),
			"resumeTime":         promtest.ToFloat64(goodputResumeTimeGauge.WithLabelValues(labels...)),
			"trainingTime":       promtest.ToFloat64(goodputTrainingTimeGauge.WithLabelValues(labels...)),
			"warmupTime":         promtest.ToFloat64(goodputWarmupTimeGauge.WithLabelValues(labels...)),
		}

		// Check that all gauges are non-zero; build sorted result
		type gaugeEntry struct {
			Name  string  `json:"name"`
			Value float64 `json:"value"`
			IsSet bool    `json:"isSet"`
		}
		var entries []gaugeEntry
		for name, val := range gaugeValues {
			entries = append(entries, gaugeEntry{Name: name, Value: val, IsSet: val != 0})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

		data, err := json.MarshalIndent(struct {
			Gauges []gaugeEntry `json:"gauges"`
		}{Gauges: entries}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCleanupGoodputMetrics(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "cleanup-goodput-metrics",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input goodputMetricValues
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const testNS = "default"
		ns, meas, job, wf := testNS, "gm-cleanup-"+tc.Name, "job-cleanup-"+tc.Name, "wf-cleanup-"+tc.Name

		recordGoodputMetrics(ns, meas, job, wf, input)
		cleanupGoodputMetrics(ns, meas, job, wf)

		count := promtest.CollectAndCount(goodputAvgTFLOPSGauge)

		data, err := json.MarshalIndent(struct {
			GaugeCount int `json:"gaugeCount"`
		}{GaugeCount: count}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestRecordJobStatus(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "record-job-status",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Status   string `yaml:"status"`
			Workflow string `yaml:"workflow"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		// Use case name in labels to isolate from other cases.
		const testNS = "default"
		ns, job, wf := testNS, "job-"+tc.Name, input.Workflow

		recordJobStatus(ns, job, wf, input.Status)
		defer cleanupJobMetrics(ns, job)

		result := map[string]float64{
			"in_progress": promtest.ToFloat64(jobStatusGauge.WithLabelValues(ns, job, wf, "in_progress")),
			"succeeded":   promtest.ToFloat64(jobStatusGauge.WithLabelValues(ns, job, wf, "succeeded")),
			"failed":      promtest.ToFloat64(jobStatusGauge.WithLabelValues(ns, job, wf, "failed")),
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCleanupJobMetrics(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "cleanup-job-metrics",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Status string `yaml:"status"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const testNS = "default"
		ns, job, wf := testNS, "job-cleanup-"+tc.Name, "wf-cleanup-"+tc.Name

		recordJobStatus(ns, job, wf, input.Status)
		cleanupJobMetrics(ns, job)

		count := promtest.CollectAndCount(jobStatusGauge)

		data, err := json.MarshalIndent(struct {
			GaugeCount int `json:"gaugeCount"`
		}{GaugeCount: count}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestRecordTopologyValidatedNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "record-topology-validated-nodes",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			TopologyKey string              `yaml:"topologyKey"`
			DomainNodes map[string][]string `yaml:"domainNodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const testNS = "default"
		ns, wf := testNS, "wf-topo-"+tc.Name
		recordTopologyValidatedNodes(ns, wf, input.TopologyKey, input.DomainNodes)
		defer cleanupTopologyMetrics(ns, wf, input.TopologyKey, input.DomainNodes)

		// Build sorted result: domain -> sorted node list.
		type domainResult struct {
			Domain string   `json:"domain"`
			Nodes  []string `json:"nodes"`
		}
		var results []domainResult
		for domain, nodes := range input.DomainNodes {
			// Verify each node has gauge=1.
			var verified []string
			for _, node := range nodes {
				val := promtest.ToFloat64(topologyValidatedNodesGauge.WithLabelValues(ns, wf, input.TopologyKey, domain, node))
				if val == 1 {
					verified = append(verified, node)
				}
			}
			sort.Strings(verified)
			results = append(results, domainResult{Domain: domain, Nodes: verified})
		}
		sort.Slice(results, func(i, j int) bool { return results[i].Domain < results[j].Domain })

		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCleanupTopologyMetrics(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "cleanup-topology-metrics",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			TopologyKey string              `yaml:"topologyKey"`
			DomainNodes map[string][]string `yaml:"domainNodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const testNS = "default"
		ns, wf := testNS, "wf-topo-cleanup-"+tc.Name

		recordTopologyValidatedNodes(ns, wf, input.TopologyKey, input.DomainNodes)
		recordTopologyFailedNodes(ns, wf, input.TopologyKey, input.DomainNodes)
		cleanupTopologyMetrics(ns, wf, input.TopologyKey, input.DomainNodes)

		validatedCount := promtest.CollectAndCount(topologyValidatedNodesGauge)
		failedCount := promtest.CollectAndCount(topologyFailedNodesGauge)
		count := validatedCount + failedCount

		data, err := json.MarshalIndent(struct {
			GaugeCount int `json:"gaugeCount"`
		}{GaugeCount: count}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
