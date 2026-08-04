// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestDetectPlatform(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-platform",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []corev1.Node `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := detectPlatform(input.Nodes)

		data, err := json.MarshalIndent(struct {
			Platform string `json:"platform"`
		}{Platform: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDetectPlatformConsistent(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-platform-consistent",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []corev1.Node `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		platform, err := detectPlatformConsistent(input.Nodes)

		type result struct {
			Platform string `json:"platform"`
			Error    string `json:"error,omitempty"`
		}
		r := result{Platform: platform}
		if err != nil {
			r.Error = err.Error()
		}
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDetectGPUArchConsistent(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-gpu-arch-consistent",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []corev1.Node `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		arch, filtered := detectGPUArchConsistent(input.Nodes)

		var nodeNames []string
		for _, n := range filtered {
			nodeNames = append(nodeNames, n.Name)
		}

		data, err := json.MarshalIndent(struct {
			GPUArchitecture string   `json:"gpuArchitecture"`
			FilteredNodes   []string `json:"filteredNodes"`
		}{GPUArchitecture: arch, FilteredNodes: nodeNames}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDetectGPUArchitecture(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-gpu-architecture",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []corev1.Node `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := detectGPUArchitecture(input.Nodes)

		data, err := json.MarshalIndent(struct {
			GPUArchitecture string `json:"gpuArchitecture"`
		}{GPUArchitecture: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestMatchesWhen(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "matches-when",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			When            burninv1alpha1.WhenSpec `yaml:"when"`
			Platform        string                  `yaml:"platform"`
			GPUArchitecture string                  `yaml:"gpuArchitecture"`
			WorkloadKind    string                  `yaml:"workloadKind"`
			TopologyMode    string                  `yaml:"topologyMode"`
			DomainCount     int                     `yaml:"domainCount"`
			Config          *apiextensionsv1.JSON   `yaml:"config"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		octx := OverrideContext{
			Platform:        input.Platform,
			GPUArchitecture: input.GPUArchitecture,
			WorkloadKind:    input.WorkloadKind,
			TopologyMode:    input.TopologyMode,
			DomainCount:     input.DomainCount,
			Config:          input.Config,
		}
		result, err := matchesWhen(input.When, octx)
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(struct {
			Matches bool `json:"matches"`
		}{Matches: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestApplyOverrides(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-overrides",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Spec            burninv1alpha1.WorkflowSpec `yaml:"spec"`
			Platform        string                      `yaml:"platform"`
			GPUArchitecture string                      `yaml:"gpuArchitecture"`
			WorkloadKind    string                      `yaml:"workloadKind"`
			TopologyMode    string                      `yaml:"topologyMode"`
			DomainCount     int                         `yaml:"domainCount"`
			Config          *apiextensionsv1.JSON       `yaml:"config"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		octx := OverrideContext{
			Platform:        input.Platform,
			GPUArchitecture: input.GPUArchitecture,
			WorkloadKind:    input.WorkloadKind,
			TopologyMode:    input.TopologyMode,
			DomainCount:     input.DomainCount,
			Config:          input.Config,
		}
		if err := applyOverrides(&input.Spec, octx); err != nil {
			return err
		}

		data, err := json.MarshalIndent(input.Spec, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDetectWorkloadKind(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-workload-kind",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Workload burninv1alpha1.WorkloadSpec `yaml:"workload"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := detectWorkloadKind(&input.Workload)

		data, err := json.MarshalIndent(struct {
			WorkloadKind string `json:"workloadKind"`
		}{WorkloadKind: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestMatchesIntSpec(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "matches-int-spec",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Spec  burninv1alpha1.IntMatchSpec `yaml:"spec"`
			Value int                         `yaml:"value"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := matchesIntSpec(input.Spec, input.Value)

		data, err := json.MarshalIndent(struct {
			Matches bool `json:"matches"`
		}{Matches: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestSummarizeWhen(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "summarize-when",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			When burninv1alpha1.WhenSpec `yaml:"when"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := summarizeWhen(input.When)

		data, err := json.MarshalIndent(struct {
			Summary string `json:"summary"`
		}{Summary: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestApplyOverridesWithTracking(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-overrides-tracking",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Spec            burninv1alpha1.WorkflowSpec `yaml:"spec"`
			Platform        string                      `yaml:"platform"`
			GPUArchitecture string                      `yaml:"gpuArchitecture"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		octx := OverrideContext{
			Platform:        input.Platform,
			GPUArchitecture: input.GPUArchitecture,
		}
		applied, err := applyOverridesWithTracking(&input.Spec, octx)
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(struct {
			Applied []burninv1alpha1.AppliedOverride `json:"applied"`
		}{Applied: applied}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCountDomains(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "count-domains",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes       []corev1.Node `yaml:"nodes"`
			TopologyKey string        `yaml:"topologyKey"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := countDomains(input.Nodes, input.TopologyKey)

		data, err := json.MarshalIndent(struct {
			DomainCount int `json:"domainCount"`
		}{DomainCount: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
