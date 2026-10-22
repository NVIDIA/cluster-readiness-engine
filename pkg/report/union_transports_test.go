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

func TestUnionTransports(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "union-transports",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Measurements []struct {
				Transport []string `yaml:"transport"`
			} `yaml:"measurements"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		measurements := make([]nvcrev1alpha1.BandwidthMeasurement, 0, len(in.Measurements))
		for _, m := range in.Measurements {
			measurements = append(measurements, nvcrev1alpha1.BandwidthMeasurement{
				Status: nvcrev1alpha1.BandwidthMeasurementStatus{Transport: m.Transport},
			})
		}

		got := unionTransports(measurements)
		b, err := json.MarshalIndent(struct {
			Transport []string `json:"transport"`
		}{Transport: got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestFormatTransportLine(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "format-transport-line",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Values []string `yaml:"values"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		b, err := json.MarshalIndent(struct {
			Line string `json:"line"`
		}{Line: formatTransportLine(in.Values)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestPrintCliques(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "print-cliques",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Cliques []CliqueReport      `yaml:"cliques"`
			Groups  []GroupBandwidthRow `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		var buf bytes.Buffer
		printCliques(&buf, in.Cliques, in.Groups)
		tc.Actual = buf.String()
		return nil
	})
}
