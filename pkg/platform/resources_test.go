// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func rl(pairs map[string]string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for k, v := range pairs {
		out[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return out
}

// A WorkloadRun that sets spec.resources used to lose its GPU request, because
// the block was taken exactly as written. The pod then landed on a node it
// could not use and failed inside CUDA.
func TestWithGPURequest(t *testing.T) {
	tests := []struct {
		name        string
		res         *corev1.ResourceRequirements
		gpusPerNode int32
		wantGPU     string // "" means the resource must be absent
		wantMemory  string
	}{
		{
			name:        "memory only gets the GPU filled in",
			res:         &corev1.ResourceRequirements{Limits: rl(map[string]string{"memory": "64Gi"})},
			gpusPerNode: 1,
			wantGPU:     "1",
			wantMemory:  "64Gi",
		},
		{
			name:        "an explicit GPU count is kept",
			res:         &corev1.ResourceRequirements{Limits: rl(map[string]string{"nvidia.com/gpu": "2"})},
			gpusPerNode: 8,
			wantGPU:     "2",
		},
		{
			name:        "an explicit zero is kept, so asking for no GPU still works",
			res:         &corev1.ResourceRequirements{Limits: rl(map[string]string{"nvidia.com/gpu": "0"})},
			gpusPerNode: 8,
			wantGPU:     "0",
		},
		{
			name: "naming it in requests only is left alone",
			res: &corev1.ResourceRequirements{
				Requests: rl(map[string]string{"nvidia.com/gpu": "3"}),
			},
			gpusPerNode: 8,
			wantGPU:     "", // absent from limits; requests keeps the user's 3
		},
		{
			name:        "an empty block still gets a GPU",
			res:         &corev1.ResourceRequirements{},
			gpusPerNode: 4,
			wantGPU:     "4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withGPURequest(tt.res, tt.gpusPerNode)
			require.NotNil(t, got)

			q, ok := got.Limits[gpuResourceName]
			if tt.wantGPU == "" {
				require.False(t, ok, "expected no GPU entry in limits")
			} else {
				require.True(t, ok, "expected a GPU entry in limits")
				require.Equal(t, tt.wantGPU, q.String())
			}
			if tt.wantMemory != "" {
				m := got.Limits[corev1.ResourceName("memory")]
				require.Equal(t, tt.wantMemory, m.String(), "the user's own values survive")
			}
		})
	}

	t.Run("nil is passed through", func(t *testing.T) {
		require.Nil(t, withGPURequest(nil, 8))
	})

	t.Run("the caller's block is not mutated", func(t *testing.T) {
		in := &corev1.ResourceRequirements{Limits: rl(map[string]string{"memory": "8Gi"})}
		_ = withGPURequest(in, 2)
		_, ok := in.Limits[gpuResourceName]
		require.False(t, ok, "withGPURequest must copy, not edit in place")
	})
}

// Omitting spec.resources used to add memory: 800Gi, which only a DGX-sized
// node can satisfy, so the pod stayed Pending everywhere else.
func TestDefaultWorkerResourcesAsksOnlyForGPUs(t *testing.T) {
	got := defaultWorkerResources(8)

	b, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(b), "memory",
		"CRE cannot know an arbitrary workload's memory needs, so it must not guess")
	require.Contains(t, string(b), `"nvidia.com/gpu":"8"`)

	limits, ok := got["limits"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "8", limits["nvidia.com/gpu"])
	require.Len(t, limits, 1, "GPUs are the only thing CRE knows to ask for")
}

// The two halves together: whichever branch a user takes, the pod ends up
// asking for a GPU.
func TestBothBranchesRequestAGPU(t *testing.T) {
	t.Run("resources omitted", func(t *testing.T) {
		b, _ := json.Marshal(defaultWorkerResources(1))
		require.Contains(t, string(b), "nvidia.com/gpu")
	})
	t.Run("resources set without a GPU", func(t *testing.T) {
		res := withGPURequest(
			&corev1.ResourceRequirements{Limits: rl(map[string]string{"memory": "64Gi"})}, 1)
		_, ok := res.Limits[gpuResourceName]
		require.True(t, ok)
	})
}
