// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// applyRunOverrides merges the command line flags into the spec that was read
// from a file. A flag can contradict the file, so each case records the four
// fields the flags can change, rather than only the flag that was set.
func TestApplyRunOverrides(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-run-overrides",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Run            nvcrev1alpha1.WorkloadRun `yaml:"run"`
			NameOverride   string                    `yaml:"nameOverride"`
			NodeList       string                    `yaml:"nodeList"`
			TopologyDomain string                    `yaml:"topologyDomain"`
			TopologyKey    string                    `yaml:"topologyKey"`
			TestScale      string                    `yaml:"testScale"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		run := input.Run
		applyRunOverrides(&run, input.NameOverride, input.NodeList,
			input.TopologyDomain, input.TopologyKey, input.TestScale)

		out := struct {
			Name     string                               `json:"name"`
			NumNodes int32                                `json:"numNodes"`
			Target   *nvcrev1alpha1.TargetSpec            `json:"target"`
			Orch     *nvcrev1alpha1.WorkloadOrchestration `json:"orchestration"`
		}{run.Name, run.Spec.NumNodes, run.Spec.Target, run.Spec.Orchestration}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// The repository holds two copies of resolveWRTimeout. One is here, on the
// command line render path, and the other is in
// pkg/controller/workloadrun_controller.go. PR #64 changed both copies but
// added a test for the controller copy only.
//
// The cases below cover the copy in this package. They do not detect drift
// between the two copies on their own, because each test calls one copy. The
// way to remove the risk is to delete this copy and call the controller copy,
// which is possible because pkg/workloadrun already imports pkg/controller and
// pkg/controller does not import pkg/workloadrun.
func TestResolveWRTimeoutUsesTheDefaultWhenTheValueIsUnusable(t *testing.T) {
	// A value the user wrote is used as written.
	require.Equal(t, "45m0s", resolveWRTimeout("45m").Duration.String())

	// An empty value returns the WorkloadRun default instead of nil. A nil
	// value is what left a run with no time limit, because isJobTimedOut
	// returns false when the limit is nil.
	require.Equal(t, "24h0m0s", resolveWRTimeout("").Duration.String())

	// A value that does not parse also returns the default. The first report of
	// T2 missed this case. It is worse than leaving the field out, because the
	// YAML looks correct to the person who wrote it.
	require.Equal(t, "24h0m0s", resolveWRTimeout("2 hours").Duration.String())
}
