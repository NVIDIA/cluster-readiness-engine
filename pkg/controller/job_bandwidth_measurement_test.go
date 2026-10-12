// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestBandwidthMeasurementWorkflowLabelMetrics(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "bandwidth-measurement-workflow-label",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			AlgBW            float64 `yaml:"algBW"`
			BusBW            float64 `yaml:"busBW"`
			MessageSizeBytes string  `yaml:"messageSizeBytes"`
			NCCLTest         string  `yaml:"ncclTest"`
			Workflow         string  `yaml:"workflow"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		jobName := "job-bandwidth-label-" + tc.Name
		measurementName := jobName + "-bandwidth"
		job := &nvcrev1alpha1.Job{
			Name:      jobName,
			Namespace: testNS,
			Labels:    map[string]string{labelWorkflowTracking: input.Workflow},
			Spec: nvcrev1alpha1.JobSpec{
				BandwidthMeasurement: &nvcrev1alpha1.BandwidthMeasurementConfig{
					LogProfileRef: "test-profile",
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := &JobReconciler{Client: c, Scheme: scheme}
		ctx := context.Background()
		if err := r.ensureBandwidthMeasurement(ctx, job); err != nil {
			return err
		}

		measurement := &nvcrev1alpha1.BandwidthMeasurement{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: measurementName}, measurement); err != nil {
			return err
		}

		collectors := []prometheus.Collector{
			ncclAlgBWGauge, ncclBusBWGauge,
			ncclAlgBWDeprecatedGauge, ncclBusBWDeprecatedGauge,
		}
		baseline := make([]int, len(collectors))
		for i, collector := range collectors {
			baseline[i] = promtest.CollectAndCount(collector)
		}
		workflow := measurement.Labels[labelWorkflowTracking]
		recordNCCLBandwidthMetrics(
			testNS, measurement.Name, job.Name, workflow,
			input.NCCLTest, input.MessageSizeBytes, input.AlgBW, input.BusBW,
		)
		defer cleanupNCCLBandwidthMetrics(
			testNS, measurement.Name, job.Name, workflow,
			input.NCCLTest, []string{input.MessageSizeBytes},
		)

		registry := prometheus.NewRegistry()
		for _, collector := range collectors {
			if err := registry.Register(collector); err != nil {
				return err
			}
		}
		families, err := registry.Gather()
		if err != nil {
			return err
		}
		type series struct {
			Metric   string `json:"metric"`
			Workflow string `json:"workflow"`
		}
		var recorded []series
		for _, family := range families {
			for _, metric := range family.GetMetric() {
				labels := make(map[string]string, len(metric.GetLabel()))
				for _, label := range metric.GetLabel() {
					labels[label.GetName()] = label.GetValue()
				}
				if labels[labelMeasurement] == measurement.Name {
					recorded = append(recorded, series{Metric: family.GetName(), Workflow: labels[labelWorkflow]})
				}
			}
		}
		sort.Slice(recorded, func(i, j int) bool { return recorded[i].Metric < recorded[j].Metric })

		cleanupNCCLBandwidthMetrics(
			testNS, measurement.Name, job.Name, workflow,
			input.NCCLTest, []string{input.MessageSizeBytes},
		)
		cleanupRemovedAllSeries := true
		for i, collector := range collectors {
			cleanupRemovedAllSeries = cleanupRemovedAllSeries && promtest.CollectAndCount(collector) == baseline[i]
		}

		data, err := json.MarshalIndent(struct {
			MeasurementWorkflow string   `json:"measurementWorkflow"`
			RecordedSeries      []series `json:"recordedSeries"`
			CleanupRemovedAll   bool     `json:"cleanupRemovedAllSeries"`
		}{measurement.Labels[labelWorkflowTracking], recorded, cleanupRemovedAllSeries}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
