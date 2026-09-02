// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// The GPU capacity filter has to use the exact per-node GPU count the pods
// will request, and that value lives in different places depending on how the
// spec was built: catalog entries and WorkloadRuns put it in the
// TrainingRuntime dependency that templates the worker pods, hand-written
// Workflows in trainer.resourcesPerNode. Re-deriving it from architecture
// defaults would disagree with an operator's gpusPerNode override, so it is
// read back from the rendered spec instead. These cases pin each location, the
// largest-wins rule, and that resource names in labels or nodeSelectors are
// never misread as requests.
func TestWorkloadGPUsPerNode(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workload-gpus-per-node",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var spec nvcrev1alpha1.WorkflowSpec
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &spec); err != nil {
			return err
		}

		got := workloadGPUsPerNode(&spec)

		b, err := json.MarshalIndent(struct {
			GpusPerNode int32 `json:"gpusPerNode"`
		}{GpusPerNode: got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
