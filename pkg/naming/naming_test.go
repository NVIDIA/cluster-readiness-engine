// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package naming

import (
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		wantLen  int // -1 means same as input length
		wantSame bool
	}{
		{
			name:     "short name passes through",
			input:    "my-workflow",
			maxLen:   35,
			wantSame: true,
		},
		{
			name:     "exact length passes through",
			input:    strings.Repeat("a", 35),
			maxLen:   35,
			wantSame: true,
		},
		{
			name:    "one over triggers truncation",
			input:   strings.Repeat("a", 36),
			maxLen:  35,
			wantLen: 35,
		},
		{
			name:    "nemotron5 workflow name",
			input:   "nemotron5-certification-training-nemotron5",
			maxLen:  MaxWorkflowNameLen,
			wantLen: MaxWorkflowNameLen,
		},
		{
			name:    "very small maxLen",
			input:   "very-long-name-that-exceeds",
			maxLen:  7,
			wantLen: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.input, tt.maxLen)

			if tt.wantSame {
				if got != tt.input {
					t.Errorf("expected passthrough, got %q", got)
				}
				return
			}

			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d (got %q)", len(got), tt.wantLen, got)
			}

			if got == tt.input {
				t.Errorf("expected truncation but got original")
			}
		})
	}
}

func TestTruncateDeterminism(t *testing.T) {
	input := "nemotron5-certification-training-nemotron5"
	a := Truncate(input, 35)
	b := Truncate(input, 35)
	if a != b {
		t.Errorf("non-deterministic: %q != %q", a, b)
	}
}

func TestTruncateAzureA100NcclWorkflowName(t *testing.T) {
	// Used by integration test certification-azure-a100-nccl (input_config.yaml collect name).
	input := "az-a100-nc-communication-nccl-all-reduce"
	got := Truncate(input, MaxWorkflowNameLen)
	want := "az-a100-nc-communication-18457"
	if got != want {
		t.Errorf("Truncate(%q, %d) = %q, want %q", input, MaxWorkflowNameLen, got, want)
	}
}

func TestTruncateAzureA100NcclAllGatherWorkflowName(t *testing.T) {
	// Used by integration test certification-azure-a100-nccl-all-gather (input_config.yaml collect name).
	input := "az-a100-ag-communication-nccl-all-gather"
	got := Truncate(input, MaxWorkflowNameLen)
	want := "az-a100-ag-communication-69833"
	if got != want {
		t.Errorf("Truncate(%q, %d) = %q, want %q", input, MaxWorkflowNameLen, got, want)
	}
}

func TestTruncateTrailingHyphen(t *testing.T) {
	// Input that would have a hyphen at the truncation boundary
	input := "foo-bar-baz-qux-quux-corge"
	got := Truncate(input, 15)
	if len(got) > 15 {
		t.Errorf("len = %d, want <= 15 (got %q)", len(got), got)
	}
	// Should not have double hyphens (from trailing hyphen + separator)
	if strings.Contains(got, "--") {
		t.Errorf("contains double hyphen: %q", got)
	}
}

func TestTruncateEndToEndChain(t *testing.T) {
	tests := []struct {
		name    string
		cert    string
		domain  string
		variant string
		// longest replicatedJob name that will be appended by the Trainer
		replicatedJob string
	}{
		{
			name:          "nemotron5 (torch, trainer replicatedJob)",
			cert:          "nemotron5-certification",
			domain:        "training",
			variant:       "nemotron5-8b",
			replicatedJob: "trainer",
		},
		{
			name:          "nccl-all-reduce (MPI, launcher replicatedJob)",
			cert:          "nccl-all-reduce-certification",
			domain:        "communication",
			variant:       "nccl-all-reduce",
			replicatedJob: "launcher",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflowName := Truncate(tt.cert+"-"+tt.domain+"-"+tt.variant, MaxWorkflowNameLen)
			if len(workflowName) > MaxWorkflowNameLen {
				t.Errorf("workflow name %q (%d chars) exceeds max %d", workflowName, len(workflowName), MaxWorkflowNameLen)
			}

			jobName := Truncate(workflowName+"-job", MaxJobNameLen)
			if len(jobName) > MaxJobNameLen {
				t.Errorf("job name %q (%d chars) exceeds max %d", jobName, len(jobName), MaxJobNameLen)
			}

			workloadName := Truncate(jobName+"-workload", MaxWorkloadNameLen)
			if len(workloadName) > MaxWorkloadNameLen {
				t.Errorf("workload name %q (%d chars) exceeds max %d", workloadName, len(workloadName), MaxWorkloadNameLen)
			}

			// Verify the full pod name stays within DNS-1123 label limit.
			// JobSet pod names: {workload}-{replicatedJob}-{replicaIdx}-{hash}
			podName := workloadName + "-" + tt.replicatedJob + "-0-abcde"
			if len(podName) > MaxK8sNameLen {
				t.Errorf("pod name %q (%d chars) exceeds DNS-1123 limit %d", podName, len(podName), MaxK8sNameLen)
			}

			t.Logf("Chain: %q → %q → %q → pod %q (%d chars)",
				workflowName, jobName, workloadName, podName, len(podName))
		})
	}
}

func TestTruncateDifferentInputsDifferentHashes(t *testing.T) {
	a := Truncate("nemotron5-certification-training-nemotron5", 35)
	b := Truncate("nemotron5-certification-training-nemotron5b", 35)
	if a == b {
		t.Errorf("different inputs produced same result: %q", a)
	}
}
