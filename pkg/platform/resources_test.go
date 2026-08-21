// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

type workerResourcesInput struct {
	GpusPerNode int32 `yaml:"gpusPerNode"`
	// OmitResources models a WorkloadRun that does not set spec.resources.
	OmitResources bool                         `yaml:"omitResources"`
	Resources     *corev1.ResourceRequirements `yaml:"resources"`
}

// A WorkloadRun had no safe way to ask for a GPU off a DGX. Omitting
// spec.resources added memory: 800Gi and the pod never scheduled; setting it
// dropped nvidia.com/gpu and the pod ran with no GPU, failing inside CUDA.
// Each case renders the resources block one of those two paths produces.
func TestWorkerResources(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "worker-resources",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in workerResourcesInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		var out any
		if in.OmitResources {
			out = defaultWorkerResources(in.GpusPerNode)
		} else {
			out = withGPURequest(in.Resources, in.GpusPerNode)
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// Two properties cannot be expressed as input-in, JSON-out: that nil passes
// through, and that the caller's block is not edited in place. They stay as
// plain Go tests.
func TestWithGPURequestDoesNotMutateTheCaller(t *testing.T) {
	in := &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{"memory": resource.MustParse("8Gi")},
	}
	_ = withGPURequest(in, 2)

	_, ok := in.Limits[gpuResourceName]
	require.False(t, ok, "withGPURequest must copy, not edit in place")
}

func TestWithGPURequestPassesNilThrough(t *testing.T) {
	require.Nil(t, withGPURequest(nil, 8))
}
