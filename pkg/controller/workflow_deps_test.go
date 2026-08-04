// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestClassifyDependencies(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "classify-dependencies",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			JobSpec string `yaml:"jobSpec"`
			Deps    []struct {
				Raw string `yaml:"raw"`
			} `yaml:"deps"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		var deps []burninv1alpha1.DependencySpec
		for _, d := range input.Deps {
			deps = append(deps, burninv1alpha1.DependencySpec{
				RawExtension: runtime.RawExtension{Raw: []byte(d.Raw)},
			})
		}

		workflowDeps, jobDeps := classifyDependencies(deps, []byte(input.JobSpec))

		var workflowNames, jobNames []string
		for _, dep := range workflowDeps {
			workflowNames = append(workflowNames, extractMetadataName(dep.Raw))
		}
		for _, dep := range jobDeps {
			jobNames = append(jobNames, extractMetadataName(dep.Raw))
		}
		if workflowNames == nil {
			workflowNames = []string{}
		}
		if jobNames == nil {
			jobNames = []string{}
		}

		data, err := json.MarshalIndent(struct {
			WorkflowScoped []string `json:"workflowScoped"`
			JobScoped      []string `json:"jobScoped"`
		}{WorkflowScoped: workflowNames, JobScoped: jobNames}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestOrderDependencies(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "order-dependencies",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Deps []struct {
				Raw string `yaml:"raw"`
			} `yaml:"deps"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		var deps []burninv1alpha1.DependencySpec
		for _, d := range input.Deps {
			deps = append(deps, burninv1alpha1.DependencySpec{
				RawExtension: runtime.RawExtension{Raw: []byte(d.Raw)},
			})
		}

		ordered := orderDependencies(deps)
		var names []string
		for _, dep := range ordered {
			names = append(names, extractMetadataName(dep.Raw))
		}
		if names == nil {
			names = []string{}
		}

		data, err := json.MarshalIndent(struct {
			Order []string `json:"order"`
		}{Order: names}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDetectCrossRefs(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-cross-refs",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Deps []struct {
				Raw string `yaml:"raw"`
			} `yaml:"deps"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		var deps []burninv1alpha1.DependencySpec
		for _, d := range input.Deps {
			deps = append(deps, burninv1alpha1.DependencySpec{
				RawExtension: runtime.RawExtension{Raw: []byte(d.Raw)},
			})
		}

		crossRefs := detectCrossRefs(deps)
		var refs []string
		for ref := range crossRefs {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		if refs == nil {
			refs = []string{}
		}

		data, err := json.MarshalIndent(struct {
			CrossRefs []string `json:"crossRefs"`
		}{CrossRefs: refs}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDepJobSuffix(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "dep-job-suffix",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups       int    `yaml:"groups"`
			MultiIter    bool   `yaml:"multiIter"`
			GroupName    string `yaml:"groupName"`
			Iteration    int    `yaml:"iteration"`
			WorkflowName string `yaml:"workflowName"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		if input.WorkflowName == "" {
			input.WorkflowName = "test-workflow"
		}

		result := depJobSuffix(input.Groups, input.MultiIter, input.GroupName, input.Iteration, input.WorkflowName)

		data, err := json.MarshalIndent(struct {
			Suffix string `json:"suffix"`
		}{Suffix: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestBuildReplacementMap(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-replacement-map",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Suffix  string `yaml:"suffix"`
			JobSpec string `yaml:"jobSpec"`
			Deps    []struct {
				Raw string `yaml:"raw"`
			} `yaml:"deps"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		var deps []burninv1alpha1.DependencySpec
		for _, d := range input.Deps {
			dep := burninv1alpha1.DependencySpec{
				RawExtension: runtime.RawExtension{Raw: []byte(d.Raw)},
			}
			deps = append(deps, dep)
		}

		got := buildReplacementMap(deps, input.Suffix)

		type kv struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		var sorted []kv
		for k, v := range got {
			sorted = append(sorted, kv{k, v})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
		if sorted == nil {
			sorted = []kv{}
		}

		data, err := json.MarshalIndent(struct {
			Replacements []kv `json:"replacements"`
		}{Replacements: sorted}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestSuffixDependencyObject(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "suffix-dependency-object",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Raw          string            `yaml:"raw"`
			Replacements map[string]string `yaml:"replacements"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		obj, err := suffixDependencyObject([]byte(input.Raw), input.Replacements)
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(obj.Object, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestSuffixJobSpec(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "suffix-job-spec",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Spec         string            `yaml:"spec"`
			Replacements map[string]string `yaml:"replacements"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		spec := &burninv1alpha1.JobSpec{}
		if err := json.Unmarshal([]byte(input.Spec), spec); err != nil {
			return err
		}

		result, err := suffixJobSpec(spec, input.Replacements)
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestIsResourceName(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "is-resource-name",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Input string `yaml:"input"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := isResourceName(input.Input)

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

func TestExtractMetadataName(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "extract-metadata-name",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Raw string `yaml:"raw"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := extractMetadataName([]byte(input.Raw))

		data, err := json.MarshalIndent(struct {
			Name string `json:"name"`
		}{Name: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestReverseDependencyRefs(t *testing.T) {
	tests := []struct {
		name     string
		input    []burninv1alpha1.DependencyResourceRef
		expected []string // expected names in order
	}{
		{
			name:     "empty",
			input:    nil,
			expected: nil,
		},
		{
			name: "single",
			input: []burninv1alpha1.DependencyResourceRef{
				{Name: "a"},
			},
			expected: []string{"a"},
		},
		{
			name: "multiple reversed",
			input: []burninv1alpha1.DependencyResourceRef{
				{Name: "compute-domain", Kind: "ComputeDomain"},
				{Name: "training-runtime", Kind: "TrainingRuntime"},
				{Name: "config-map", Kind: "ConfigMap"},
			},
			expected: []string{"config-map", "training-runtime", "compute-domain"},
		},
		{
			name: "preserves all fields",
			input: []burninv1alpha1.DependencyResourceRef{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-a", Namespace: "ns", Scope: "workflow"},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-b", Namespace: "ns", Scope: "job", GroupName: "g0", Iteration: 1},
			},
			expected: []string{"cm-b", "cm-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reverseDependencyRefs(tt.input)
			var names []string
			for _, ref := range got {
				names = append(names, ref.Name)
			}
			if len(tt.expected) == 0 && len(names) == 0 {
				return
			}
			if len(names) != len(tt.expected) {
				t.Fatalf("got %v, want %v", names, tt.expected)
			}
			for i, name := range names {
				if name != tt.expected[i] {
					t.Errorf("index %d: got %q, want %q", i, name, tt.expected[i])
				}
			}
			// Verify fields are preserved (not just names)
			if len(tt.input) > 1 {
				reversed := reverseDependencyRefs(tt.input)
				if reversed[0].Kind != tt.input[len(tt.input)-1].Kind {
					t.Errorf("field preservation: got Kind %q, want %q", reversed[0].Kind, tt.input[len(tt.input)-1].Kind)
				}
			}
		})
	}
}

func TestCollectAllStrings(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "collect-all-strings",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Raw string `yaml:"raw"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		got := collectAllStrings([]byte(input.Raw))
		if got == nil {
			got = []string{}
		}
		sort.Strings(got)

		data, err := json.MarshalIndent(struct {
			Strings []string `json:"strings"`
		}{Strings: got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
