// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// isJobTimedOut returns false when Execution.TimeoutPerJob is nil, so a
// WorkloadRun that set no timeout could never time out. One such run sat
// InProgress for 4h10m against a node with no allocatable GPUs.
func TestWorkloadRunAlwaysGetsATimeout(t *testing.T) {
	tests := []struct {
		name string
		spec *burninv1alpha1.WorkloadRunSpec
		want time.Duration
	}{
		{
			name: "no orchestration block at all",
			spec: &burninv1alpha1.WorkloadRunSpec{},
			want: 24 * time.Hour,
		},
		{
			name: "orchestration set but no timeout",
			spec: &burninv1alpha1.WorkloadRunSpec{
				Orchestration: &burninv1alpha1.WorkloadOrchestration{},
			},
			want: 24 * time.Hour,
		},
		{
			name: "an unparseable timeout falls back rather than leaving it unbounded",
			spec: &burninv1alpha1.WorkloadRunSpec{
				Orchestration: &burninv1alpha1.WorkloadOrchestration{
					TimeoutPerJob: "not-a-duration",
				},
			},
			want: 24 * time.Hour,
		},
		{
			name: "the user's own value wins",
			spec: &burninv1alpha1.WorkloadRunSpec{
				Orchestration: &burninv1alpha1.WorkloadOrchestration{
					TimeoutPerJob: "45m",
				},
			},
			want: 45 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orch := buildWROrchestration(tt.spec)
			require.NotNil(t, orch.Execution.TimeoutPerJob,
				"a Job with no timeout can never time out")
			require.Equal(t, tt.want, orch.Execution.TimeoutPerJob.Duration)
		})
	}
}

// The bound only does anything if isJobTimedOut can read it.
func TestIsJobTimedOutUsesTheResolvedTimeout(t *testing.T) {
	r := &WorkflowReconciler{}
	orch := buildWROrchestration(&burninv1alpha1.WorkloadRunSpec{
		Orchestration: &burninv1alpha1.WorkloadOrchestration{TimeoutPerJob: "1s"},
	})
	wf := &burninv1alpha1.Workflow{
		Spec: burninv1alpha1.WorkflowSpec{Orchestration: *orch},
	}

	started := metav1.NewTime(time.Now().Add(-2 * time.Second))
	g := &burninv1alpha1.GroupStatus{StartTime: &started}
	require.True(t, r.isJobTimedOut(wf, g), "2s elapsed against a 1s timeout")

	fresh := metav1.NewTime(time.Now())
	g2 := &burninv1alpha1.GroupStatus{StartTime: &fresh}
	require.False(t, r.isJobTimedOut(wf, g2))
}
