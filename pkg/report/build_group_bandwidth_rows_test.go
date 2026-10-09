// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestBuildGroupBandwidthRows pins which measurements become group rows and
// how they render. A measurement with a transport but no bandwidth rows keeps
// its row, with an empty BusBW, so the group output does not lose it.
func TestBuildGroupBandwidthRows(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-group-bandwidth-rows",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Groups              []nvcrev1alpha1.GroupStatus          `json:"groups"`
			Measurements        []nvcrev1alpha1.BandwidthMeasurement `json:"measurements"`
			MinBusBandwidthGBps string                               `json:"minBusBandwidthGBps"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		orch := &nvcrev1alpha1.OrchestrationStatus{Groups: in.Groups}
		rows := buildGroupBandwidthRows(orch, in.Measurements, in.MinBusBandwidthGBps)

		var buf bytes.Buffer
		printGroupBandwidth(&buf, rows)
		buf.WriteString("--- rows ---\n")
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteString("\n")
		tc.Actual = buf.String()
		return nil
	})
}
