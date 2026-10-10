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

// TestAggregateBandwidthRow pins the category Bandwidth row: the lowest BusBW
// at the largest message size, so list order cannot pick a healthy group to
// stand in for a slow one, and a provisional result is marked, not hidden.
func TestAggregateBandwidthRow(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "aggregate-bandwidth-row",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Measurements []nvcrev1alpha1.BandwidthMeasurement `json:"measurements"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		var rows []BandwidthRow
		if row := aggregateBandwidthRow(in.Measurements); row != nil {
			rows = append(rows, *row)
		}

		var buf bytes.Buffer
		printTransportAndBandwidth(&buf, nil, rows)
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

// TestPrintDiagnoseResults pins how diagnose test rows render, including a
// test whose Job passed on provisional bandwidth.
func TestPrintDiagnoseResults(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "print-diagnose-results",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var d DiagnoseReport
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &d); err != nil {
			return err
		}
		computeDiagnoseMinMax(&d)

		var buf bytes.Buffer
		printDiagnoseResults(&buf, &d)
		tc.Actual = buf.String()
		return nil
	})
}
