// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestBuildTolerations(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-tolerations",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Selectors []nvcrev1alpha1.TaintSelector `yaml:"selectors"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := buildTolerations(input.Selectors)

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestNodeMatchesTaints(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "node-match-taints",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Node      corev1.Node                   `yaml:"node"`
			Selectors []nvcrev1alpha1.TaintSelector `yaml:"selectors"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := nodeMatchesTaints(input.Node, input.Selectors)

		data, err := json.MarshalIndent(struct {
			Result bool `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCanLaunchOverflow(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "can-launch-overflow",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Overflow  nvcrev1alpha1.GroupStatus   `yaml:"overflow"`
			AllGroups []nvcrev1alpha1.GroupStatus `yaml:"allGroups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := !hasNodeOverlap(&input.Overflow, input.AllGroups)

		data, err := json.MarshalIndent(struct {
			Result bool `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestHasNodeOverlap(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "has-node-overlap",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Candidate nvcrev1alpha1.GroupStatus   `yaml:"candidate"`
			AllGroups []nvcrev1alpha1.GroupStatus `yaml:"allGroups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := hasNodeOverlap(&input.Candidate, input.AllGroups)

		data, err := json.MarshalIndent(struct {
			Result bool `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCountRunningGroups(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "count-running-groups",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups []nvcrev1alpha1.GroupStatus `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		orch := &nvcrev1alpha1.OrchestrationStatus{Groups: input.Groups}
		result := countRunningGroups(orch)

		data, err := json.MarshalIndent(struct {
			Result int `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestAllGroupsTerminal(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "all-groups-terminal",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups []nvcrev1alpha1.GroupStatus `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		r := &WorkflowReconciler{}
		orch := &nvcrev1alpha1.OrchestrationStatus{Groups: input.Groups}
		result := r.allGroupsTerminal(orch)

		data, err := json.MarshalIndent(struct {
			Result bool `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestHasRunningGroups(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "has-running-groups",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups []nvcrev1alpha1.GroupStatus `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		r := &WorkflowReconciler{}
		orch := &nvcrev1alpha1.OrchestrationStatus{Groups: input.Groups}
		result := r.hasRunningGroups(orch)

		data, err := json.MarshalIndent(struct {
			Result bool `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestGetGroupJobName(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "get-group-job-name",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			WorkflowName string `yaml:"workflowName"`
			GroupName    string `yaml:"groupName"`
			Iteration    int    `yaml:"iteration"`
			Iterations   int    `yaml:"iterations"`
			TotalGroups  int    `yaml:"totalGroups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		workflow := &nvcrev1alpha1.Workflow{
			Name: input.WorkflowName,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{
					Iterations: input.Iterations,
				},
			},
			Status: nvcrev1alpha1.WorkflowStatus{
				Orchestration: &nvcrev1alpha1.OrchestrationStatus{
					TotalGroups: input.TotalGroups,
				},
			},
		}

		r := &WorkflowReconciler{}
		result := r.getGroupJobName(workflow, input.GroupName, input.Iteration)

		data, err := json.MarshalIndent(struct {
			Result string `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestEffectiveIterations(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "effective-iterations",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Iterations int `yaml:"iterations"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		orch := nvcrev1alpha1.OrchestrationSpec{Iterations: input.Iterations}
		result := effectiveIterations(orch)

		data, err := json.MarshalIndent(struct {
			Result int `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestHasMultipleIterations(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "has-multiple-iterations",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Iterations int `yaml:"iterations"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		orch := nvcrev1alpha1.OrchestrationSpec{Iterations: input.Iterations}
		result := hasMultipleIterations(orch)

		data, err := json.MarshalIndent(struct {
			Result bool `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
