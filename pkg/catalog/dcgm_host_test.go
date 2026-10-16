// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"fmt"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestDCGMHost verifies the `dcgmi diag --host` value dcgm-level4 renders:
// the GPU Operator's DCGM Service when CategoryOptions.dcgmHost is unset,
// and the user's address when it is set (issue #428). It also records which
// platform overrides still carry a jobTemplatePatch, because the Nscale
// override repoints --host with a test-then-replace patch that must be left
// out once the user has chosen the address.
func TestDCGMHost(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "dcgm-host",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Target      nvcrev1alpha1.TargetSpec `json:"target"`
			GpusPerNode int32                    `json:"gpusPerNode"`
			DCGMHost    string                   `json:"dcgmHost"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		entry := Lookup("diagnostics", "dcgm-level4")
		if entry == nil {
			return fmt.Errorf("category diagnostics/dcgm-level4 not registered")
		}
		spec, err := entry.Build(input.Target, BuildConfig{
			NodesPerJob:     1,
			GpusPerNode:     input.GpusPerNode,
			GPUArchitecture: GPUArchFromNodeSelector(input.Target.NodeSelector),
			DCGMHost:        input.DCGMHost,
		})
		if err != nil {
			return err
		}

		out := struct {
			TrainerArgs               []string `json:"trainerArgs"`
			JobTemplatePatchPlatforms []string `json:"jobTemplatePatchPlatforms"`
		}{
			TrainerArgs:               spec.JobTemplate.Spec.Workload.TrainJob.Trainer.Args,
			JobTemplatePatchPlatforms: []string{},
		}
		for _, o := range spec.Overrides {
			if o.JobTemplatePatch != nil && o.When.Platform != nil {
				out.JobTemplatePatchPlatforms = append(out.JobTemplatePatchPlatforms, o.When.Platform.Equals)
			}
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
