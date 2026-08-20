// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package naming

import (
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

func TestTruncate(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "truncate",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Input  string `yaml:"input"`
			MaxLen int    `yaml:"maxLen"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := Truncate(in.Input, in.MaxLen)

		b, err := json.MarshalIndent(struct {
			Truncated string `json:"truncated"`
			Length    int    `json:"length"`
		}{got, len(got)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
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
	p := testutil.TestCaseParser{
		Subdir:         "truncate-end-to-end-chain",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Cert    string `yaml:"cert"`
			Domain  string `yaml:"domain"`
			Variant string `yaml:"variant"`
			// longest replicatedJob name that will be appended by the Trainer
			ReplicatedJob string `yaml:"replicatedJob"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		workflowName := Truncate(in.Cert+"-"+in.Domain+"-"+in.Variant, MaxWorkflowNameLen)
		if len(workflowName) > MaxWorkflowNameLen {
			tc.T.Errorf("workflow name %q (%d chars) exceeds max %d", workflowName, len(workflowName), MaxWorkflowNameLen)
		}

		jobName := Truncate(workflowName+"-job", MaxJobNameLen)
		if len(jobName) > MaxJobNameLen {
			tc.T.Errorf("job name %q (%d chars) exceeds max %d", jobName, len(jobName), MaxJobNameLen)
		}

		workloadName := Truncate(jobName+"-workload", MaxWorkloadNameLen)
		if len(workloadName) > MaxWorkloadNameLen {
			tc.T.Errorf("workload name %q (%d chars) exceeds max %d", workloadName, len(workloadName), MaxWorkloadNameLen)
		}

		// Verify the full pod name stays within DNS-1123 label limit.
		// JobSet pod names: {workload}-{replicatedJob}-{replicaIdx}-{hash}
		podName := workloadName + "-" + in.ReplicatedJob + "-0-abcde"
		if len(podName) > MaxK8sNameLen {
			tc.T.Errorf("pod name %q (%d chars) exceeds DNS-1123 limit %d", podName, len(podName), MaxK8sNameLen)
		}

		b, err := json.MarshalIndent(struct {
			WorkflowName string `json:"workflowName"`
			WorkflowLen  int    `json:"workflowLen"`
			JobName      string `json:"jobName"`
			JobLen       int    `json:"jobLen"`
			WorkloadName string `json:"workloadName"`
			WorkloadLen  int    `json:"workloadLen"`
			PodName      string `json:"podName"`
			PodLen       int    `json:"podLen"`
		}{
			workflowName, len(workflowName),
			jobName, len(jobName),
			workloadName, len(workloadName),
			podName, len(podName),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestTruncateDifferentInputsDifferentHashes(t *testing.T) {
	a := Truncate("nemotron5-certification-training-nemotron5", 35)
	b := Truncate("nemotron5-certification-training-nemotron5b", 35)
	if a == b {
		t.Errorf("different inputs produced same result: %q", a)
	}
}
