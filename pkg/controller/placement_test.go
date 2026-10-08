// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/orchestration"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestBuildNodeAffinity exercises the whole placement seam at once: the target
// terms, the GPU product term, and the hostname term the two placements decide
// between. It builds the affinity through PinnedHostnames rather than passing a
// hostname list directly, so a case reads the way the controller runs.
func TestBuildNodeAffinity(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "target-affinity",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Placement   string                    `yaml:"placement"`
			GroupNodes  []string                  `yaml:"groupNodes"`
			Target      *nvcrev1alpha1.TargetSpec `yaml:"target"`
			GPUProducts []string                  `yaml:"gpuProducts"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		hostnames := PinnedHostnames(input.Placement, input.GroupNodes, input.Target)
		affinity := BuildNodeAffinity(hostnames, input.Target, input.GPUProducts)

		data, err := json.MarshalIndent(struct {
			Hostnames []string             `json:"hostnames"`
			Affinity  *corev1.NodeAffinity `json:"affinity"`
		}{Hostnames: hostnames, Affinity: affinity}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestValidatePlacement pins which orchestration settings Unpinned rejects and,
// just as importantly, which it tolerates. The topology-key case is the one
// that matters: the GB200 catalog override injects that field, so rejecting it
// would fail the Nemotron run this mode was built for.
func TestValidatePlacement(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "validate-placement",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var orch nvcrev1alpha1.OrchestrationSpec
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &orch); err != nil {
			return err
		}

		result := struct {
			Error              string `json:"error"`
			IgnoredTopologyKey string `json:"ignoredTopologyKey"`
		}{IgnoredTopologyKey: IgnoredTopologyKey(&orch)}
		if err := ValidatePlacement(&orch); err != nil {
			result.Error = err.Error()
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestValidateWRPlacement is the WorkloadRun half, where testScale has not yet
// been lowered into an OrchestrationSpec and so is still nameable in a message.
func TestValidateWRPlacement(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "validate-wr-placement",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var orch nvcrev1alpha1.WorkloadOrchestration
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &orch); err != nil {
			return err
		}

		result := struct {
			Error string `json:"error"`
		}{}
		if err := ValidateWRPlacement(&orch); err != nil {
			result.Error = err.Error()
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestBuildGroupStatuses covers the Unpinned shape that reaches this function:
// one group with no nodes and no domains, which must still produce a usable
// Pending status rather than a nil entry or a panic.
func TestBuildGroupStatuses(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-group-statuses",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups    []orchestration.Group       `yaml:"groups"`
			NodeInfos []orchestration.NodeInfo    `yaml:"nodeInfos"`
			Topology  *nvcrev1alpha1.TopologySpec `yaml:"topology"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := buildGroupStatuses(input.Groups, input.NodeInfos, input.Topology)

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestDistinctGPUProducts pins the raw label values carried onto the pods.
// DetectedGPUArchitecture is normalized and lossy, so these cannot be derived
// from it; a wrong value here is an affinity term that matches no node.
func TestDistinctGPUProducts(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "distinct-gpu-products",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []corev1.Node `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		data, err := json.MarshalIndent(DistinctGPUProducts(input.Nodes), "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
