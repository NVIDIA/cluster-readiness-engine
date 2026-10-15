// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat_mokka

package mokka

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClusterInfo checks that NVCRE's detection reads what the real producers
// wrote: Kind's providerID resolves to onprem, and the architecture and GPU
// count come from GFD's labels and the device plugin's allocatable, not from
// fixtures.
func TestClusterInfo(t *testing.T) {
	want := map[string]struct {
		arch        string
		gpusPerNode int
	}{
		"h100":  {"h100", 8},
		"b200":  {"b200", 8},
		"gb200": {"gb200", 4},
		"gb300": {"gb300", 4},
	}
	exp, ok := want[profile]
	if !ok {
		t.Skipf("no expectation for profile %s", profile)
	}

	var info struct {
		Platform        string `json:"platform"`
		GPUArchitecture string `json:"gpuArchitecture"`
		GPUsPerNode     int    `json:"gpusPerNode"`
		TotalNodes      int    `json:"totalNodes"`
		TotalGPUs       int    `json:"totalGPUs"`
	}
	out := nvcrectl(context.Background(), t, "cluster", "info", "-o", "json")
	require.NoError(t, json.Unmarshal(out, &info), "cluster info output: %s", out)

	require.Equal(t, "onprem", info.Platform)
	require.Equal(t, exp.arch, info.GPUArchitecture)
	require.Equal(t, exp.gpusPerNode, info.GPUsPerNode)
	require.Equal(t, 2, info.TotalNodes, "both simulated GPU workers")
	require.Equal(t, 2*exp.gpusPerNode, info.TotalGPUs)
}
