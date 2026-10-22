// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestBuildWorkflowSpecPlacement covers placement on the offline render path,
// which TestBuildWorkflowSpec cannot: that harness returns the error to the
// parser and a case whose whole point is the rejection would fail rather than
// record. Here the error is the recorded value, so accept and reject cases sit
// in the same golden format and can be read against each other.
//
// BuildWorkflowSpec runs the same controller.ValidateWRPlacement the Workflow
// controller runs, so an offline render refuses exactly the specs the cluster
// would. A preview that accepted what the cluster rejects is the failure mode
// worth pinning, and it is invisible until someone submits.
func TestBuildWorkflowSpecPlacement(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "placement",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		// json tags, not yaml: sigs.k8s.io/yaml converts to JSON and then calls
		// encoding/json, which would silently leave a yaml-tagged field at its
		// zero value. Same reasoning as TestBuildWorkflowSpec.
		var input struct {
			Run           nvcrev1alpha1.WorkloadRun `json:"run"`
			GpusPerNode   int32                     `json:"gpusPerNode"`
			MlnxPerNode   int32                     `json:"mlnxPerNode"`
			EnableMNNVL   bool                      `json:"enableMNNVL"`
			FrameworkType string                    `json:"frameworkType"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		spec, err := BuildWorkflowSpec(&input.Run, input.GpusPerNode, input.MlnxPerNode,
			input.EnableMNNVL, input.FrameworkType)

		var out struct {
			Error string `json:"error,omitempty"`
			// Placement is what the render carries into the Workflow, and
			// numNodes is the size that travels with it. On an accepted Unpinned
			// run the size must be the one the user asked for: it is both the
			// trainer's numNodes and, on GB200/GB300, what the ComputeDomain
			// channel is templated from.
			Placement *string `json:"placement,omitempty"`
			NumNodes  *int32  `json:"numNodes,omitempty"`
			// TotalGroups is deliberately absent. Partitioning happens in the
			// controller, not here; this path only renders the spec that decides
			// it. cmd/integration/testdata/reconcile/workloadrun-unpinned covers
			// the one-job outcome itself.
		}
		switch {
		case err != nil:
			out.Error = err.Error()
		default:
			out.Placement = &spec.Orchestration.Placement
			if tj := spec.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
				out.NumNodes = tj.Trainer.NumNodes
			}
		}

		b, marshalErr := json.MarshalIndent(out, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
