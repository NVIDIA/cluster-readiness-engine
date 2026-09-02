// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// timeoutPerJob must be measured from when the workload was first observed
// running, not from Job creation: a TrainJob suspended by Kueue sits queued
// with no pods, and counting that time failed healthy nodes (issue #213).
// Inputs use durations relative to now ("ago") so the wall clock cannot make
// a case flaky: "timed out" cases sit hours past a 1s timeout, "not timed
// out" cases sit seconds inside a 1h one.
func TestIsJobTimedOut(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "job-timed-out",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			TimeoutPerJob    string `yaml:"timeoutPerJob"`
			GroupStartAgo    string `yaml:"groupStartAgo"`
			WorkloadStartAgo string `yaml:"workloadStartAgo"`
			HasWorkloadRef   bool   `yaml:"hasWorkloadRef"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		wf := &nvcrev1alpha1.Workflow{}
		if input.TimeoutPerJob != "" {
			d, err := time.ParseDuration(input.TimeoutPerJob)
			if err != nil {
				return err
			}
			wf.Spec.Orchestration.Execution.TimeoutPerJob = &metav1.Duration{Duration: d}
		}

		g := &nvcrev1alpha1.GroupStatus{}
		if input.GroupStartAgo != "" {
			d, err := time.ParseDuration(input.GroupStartAgo)
			if err != nil {
				return err
			}
			t := metav1.NewTime(time.Now().Add(-d))
			g.StartTime = &t
		}

		job := &nvcrev1alpha1.Job{}
		if input.HasWorkloadRef {
			job.Status.WorkloadRef = &nvcrev1alpha1.WorkloadReference{
				APIVersion: "trainer.kubeflow.org/v1alpha1",
				Kind:       "TrainJob",
				Name:       "test-workload",
			}
		}
		if input.WorkloadStartAgo != "" {
			d, err := time.ParseDuration(input.WorkloadStartAgo)
			if err != nil {
				return err
			}
			t := metav1.NewTime(time.Now().Add(-d))
			job.Status.WorkloadStartTime = &t
		}

		r := &WorkflowReconciler{}
		b, err := json.MarshalIndent(map[string]any{
			"timedOut": r.isJobTimedOut(wf, g, job),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
