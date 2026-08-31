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

// TestNodesPerJobForScale pins the testScale sizing rule: intra-node yields
// one node per Job; every other value (and no orchestration block at all)
// keeps the requested count. Each case feeds a WorkloadRunSpec fragment and
// records the resulting nodesPerJob.
func TestNodesPerJobForScale(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workloadrun-scale",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var spec nvcrev1alpha1.WorkloadRunSpec
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &spec); err != nil {
			return err
		}

		out := struct {
			NodesPerJob int32 `json:"nodesPerJob"`
		}{
			NodesPerJob: NodesPerJobForScale(spec.Orchestration, spec.NumNodes),
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
