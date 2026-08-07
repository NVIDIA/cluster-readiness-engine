// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A run that excluded nodes still reports PASSED. The report has to say so,
// otherwise an operator reads PASSED and never learns part of the fleet was
// never tested.
func TestReportShowsExcludedNodes(t *testing.T) {
	r := CertReport{
		Name:            "cert",
		Platform:        "onprem",
		GPU:             "h100",
		TotalNodes:      1,
		ExcludedNodes:   []string{"node002"},
		ExclusionReason: "target set has more than one GPU architecture; certified h100 only",
		Result:          "PASSED",
	}

	var buf bytes.Buffer
	Print(&buf, &r)
	out := buf.String()

	require.Contains(t, out, "Nodes:     1")
	require.Contains(t, out, "Excluded:  1 (node002)")
	require.Contains(t, out, "more than one GPU architecture")
	require.Contains(t, out, "PASSED", "the verdict is deliberately unchanged")
}

func TestReportOmitsExclusionWhenNothingExcluded(t *testing.T) {
	r := CertReport{Name: "cert", GPU: "a100", TotalNodes: 2, Result: "PASSED"}

	var buf bytes.Buffer
	Print(&buf, &r)
	require.NotContains(t, buf.String(), "Excluded:")

	b, err := json.Marshal(r)
	require.NoError(t, err)
	require.NotContains(t, string(b), "excludedNodes",
		"omitempty keeps the JSON unchanged for the normal case")
}

func TestExcludedNodesReachTheJSON(t *testing.T) {
	r := CertReport{
		Name: "cert", TotalNodes: 1,
		ExcludedNodes:   []string{"node002"},
		ExclusionReason: "reason",
	}
	b, err := json.Marshal(r)
	require.NoError(t, err)
	require.Contains(t, string(b), `"excludedNodes":["node002"]`)
	require.Contains(t, string(b), `"exclusionReason":"reason"`)
}
