// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestBuildTolerations(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-tolerations",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Selectors []burninv1alpha1.TaintSelector `yaml:"selectors"`
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Node      corev1.Node                    `yaml:"node"`
			Selectors []burninv1alpha1.TaintSelector `yaml:"selectors"`
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Overflow  burninv1alpha1.GroupStatus   `yaml:"overflow"`
			AllGroups []burninv1alpha1.GroupStatus `yaml:"allGroups"`
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Candidate burninv1alpha1.GroupStatus   `yaml:"candidate"`
			AllGroups []burninv1alpha1.GroupStatus `yaml:"allGroups"`
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups []burninv1alpha1.GroupStatus `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		orch := &burninv1alpha1.OrchestrationStatus{Groups: input.Groups}
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups []burninv1alpha1.GroupStatus `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		r := &WorkflowReconciler{}
		orch := &burninv1alpha1.OrchestrationStatus{Groups: input.Groups}
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups []burninv1alpha1.GroupStatus `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		r := &WorkflowReconciler{}
		orch := &burninv1alpha1.OrchestrationStatus{Groups: input.Groups}
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
		ExpectedSuffix: ".json",
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

		workflow := &burninv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: input.WorkflowName},
			Spec: burninv1alpha1.WorkflowSpec{
				Orchestration: burninv1alpha1.OrchestrationSpec{
					Iterations: input.Iterations,
				},
			},
			Status: burninv1alpha1.WorkflowStatus{
				Orchestration: &burninv1alpha1.OrchestrationStatus{
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Iterations int `yaml:"iterations"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		orch := burninv1alpha1.OrchestrationSpec{Iterations: input.Iterations}
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
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Iterations int `yaml:"iterations"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		orch := burninv1alpha1.OrchestrationSpec{Iterations: input.Iterations}
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
