// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// workloadMetadataInput is the shape of each case's input.yaml. The three
// non-run fields are the values the CLI and the controller both compute before
// building, so a case pins them rather than re-deriving them.
type workloadMetadataInput struct {
	Run           nvcrev1alpha1.WorkloadRun `json:"run"`
	GpusPerNode   int32                     `json:"gpusPerNode"`
	MlnxPerNode   int32                     `json:"mlnxPerNode"`
	EnableMNNVL   bool                      `json:"enableMNNVL"`
	FrameworkType string                    `json:"frameworkType"`
	// Platform, when set, resolves the overrides before the post-override
	// check runs, which is what "nvcrectl workloadrun render --platform" and
	// the dry-run path both do. Left empty, the overrides stay conditional and
	// the output is a template: it keeps its intent and is not checked.
	Platform string `json:"platform,omitempty"`
	// GPUArchitecture picks the synthetic nodes the overrides match against.
	GPUArchitecture string `json:"gpuArchitecture,omitempty"`
}

// workloadMetadataResult records the resolved workload-object labels, the
// persisted intent, and which stage rejected the case. BuildError is the
// construction-time merge inside BuildWorkflowSpec; ValidateError is the
// post-override check. Keeping them apart is the point of several cases: the
// same conflicting queue is caught at construction when the user wrote it
// directly and after overrides when an override introduced it.
type workloadMetadataResult struct {
	BuildError    string                           `json:"buildError,omitempty"`
	ValidateError string                           `json:"validateError,omitempty"`
	Labels        map[string]string                `json:"labels"`
	GangScheduler *nvcrev1alpha1.GangSchedulerSpec `json:"gangScheduler"`
}

// TestWorkloadRunWorkloadMetadata drives the WorkloadRun construction path and
// the post-override check that follows it in the CLI.
//
// WorkloadRun differs from Certification here in a way worth pinning: its
// runtime dependency is built with the scheduler and queue already in place,
// so it never calls ApplyGangSchedulerToDependencies and instead validates its
// effective runtime after overrides. The construction-time merge is therefore
// provisional, and the persisted intent is what makes the later check
// authoritative — including for a rendered Workflow submitted on its own.
func TestWorkloadRunWorkloadMetadata(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "workload-metadata",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in workloadMetadataInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		result := workloadMetadataResult{}
		spec, err := BuildWorkflowSpec(&in.Run, in.GpusPerNode, in.MlnxPerNode,
			in.EnableMNNVL, in.FrameworkType)
		if err != nil {
			result.BuildError = err.Error()
			return marshalInto(tc, result)
		}

		if in.Platform != "" {
			nodes := loadSyntheticNodes(in.Platform, in.GPUArchitecture)
			orch := &nvcrev1alpha1.OrchestrationStatus{
				DetectedPlatform:        in.Platform,
				DetectedGPUArchitecture: in.GPUArchitecture,
			}
			octx := controller.BuildOverrideContext(spec, orch, nodes)
			if _, overrideErr := controller.ApplyOverridesWithTracking(spec, octx); overrideErr != nil {
				return overrideErr
			}
			spec.Overrides = nil

			if validateErr := platform.ValidateResolvedJobTemplate(
				&spec.JobTemplate.Spec, spec.Dependencies, spec.GangScheduler,
				platform.WorkloadRunWorkloadLabelsPath); validateErr != nil {
				result.ValidateError = validateErr.Error()
			}
		}

		if md := spec.JobTemplate.Spec.WorkloadMetadata; md != nil && len(md.Labels) > 0 {
			result.Labels = make(map[string]string, len(md.Labels))
			for key, value := range md.Labels {
				result.Labels[key] = string(value)
			}
		}
		result.GangScheduler = spec.GangScheduler

		return marshalInto(tc, result)
	})
}

func marshalInto(tc *testutil.TestCase, result workloadMetadataResult) error {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	tc.Actual = string(b) + "\n"
	return nil
}
