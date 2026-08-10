// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// isJobTimedOut returns false when Execution.TimeoutPerJob is nil, so a
// WorkloadRun that set no timeout could never time out. One run sat InProgress
// for 4h10m against a node with no allocatable GPUs.
func TestWorkloadRunTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workloadrun-timeout",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var spec burninv1alpha1.WorkloadRunSpec
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &spec); err != nil {
			return err
		}

		orch := buildWROrchestration(&spec)

		out := struct {
			TimeoutPerJob string `json:"timeoutPerJob"`
		}{}
		if orch.Execution.TimeoutPerJob != nil {
			out.TimeoutPerJob = orch.Execution.TimeoutPerJob.Duration.String()
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// A single-value assertion: the bound only matters if isJobTimedOut reads it.
func TestIsJobTimedOutUsesTheResolvedTimeout(t *testing.T) {
	r := &WorkflowReconciler{}
	orch := buildWROrchestration(&burninv1alpha1.WorkloadRunSpec{
		Orchestration: &burninv1alpha1.WorkloadOrchestration{TimeoutPerJob: "1s"},
	})
	wf := &burninv1alpha1.Workflow{Spec: burninv1alpha1.WorkflowSpec{Orchestration: *orch}}

	started := metav1.NewTime(time.Now().Add(-2 * time.Second))
	require.True(t, r.isJobTimedOut(wf, &burninv1alpha1.GroupStatus{StartTime: &started}))

	fresh := metav1.NewTime(time.Now())
	require.False(t, r.isJobTimedOut(wf, &burninv1alpha1.GroupStatus{StartTime: &fresh}))
}
