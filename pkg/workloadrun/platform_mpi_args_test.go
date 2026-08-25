// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// applyPlatformMPIArgs is the render-preview half of the platform mpirun-args
// mechanism: the controller bakes override mpiArgs into trainer.args at
// reconcile time via applyWRPreTemplateOverrides, and "nvcrectl workloadrun
// render" calls this function so the preview shows the same args (ADR-070).
// Each case records spec.framework.mpi.mpiArgs after the call — the platform
// pins must land ahead of the user's own args, and non-matching platforms or
// non-MPI frameworks must leave the spec alone.
//
// The tags below are json, not yaml: sigs.k8s.io/yaml converts YAML to JSON
// and encoding/json ignores yaml tags, silently zeroing mistyped keys.
func TestApplyPlatformMPIArgs(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-platform-mpi-args",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Run           crev1alpha1.WorkloadRun `json:"run"`
			Platform      string                  `json:"platform"`
			GPUArch       string                  `json:"gpuArch"`
			GpusPerNode   int32                   `json:"gpusPerNode"`
			MlnxPerNode   int32                   `json:"mlnxPerNode"`
			EnableMNNVL   bool                    `json:"enableMNNVL"`
			FrameworkType string                  `json:"frameworkType"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		run := input.Run
		applyPlatformMPIArgs(&run, input.Platform, input.GPUArch,
			input.GpusPerNode, input.MlnxPerNode, input.EnableMNNVL, input.FrameworkType)

		out := struct {
			MPIArgs []string `json:"mpiArgs"`
		}{}
		if run.Spec.Framework.MPI != nil {
			out.MPIArgs = run.Spec.Framework.MPI.MpiArgs
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
