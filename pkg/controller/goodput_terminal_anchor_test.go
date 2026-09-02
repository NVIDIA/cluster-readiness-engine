// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Terminal goodput computation is anchored to the Job's terminal condition
// timestamp so that replaying the terminal path produces the same values
// (ADR-072). These cases pin the anchor source per terminal condition and the
// wall-clock last resort for a terminal condition without a timestamp.
func TestTerminalAnchor(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "goodput-terminal-anchor",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Conditions []metav1.Condition `yaml:"conditions"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		job := &nvcrev1alpha1.Job{
			Status: nvcrev1alpha1.JobStatus{Conditions: in.Conditions},
		}

		before := time.Now()
		anchor := terminalAnchor(job)
		wallClockFallback := !anchor.Before(before)

		out := struct {
			Anchor                string `json:"anchor"`
			UsedWallClockFallback bool   `json:"usedWallClockFallback"`
		}{UsedWallClockFallback: wallClockFallback}
		if !wallClockFallback {
			out.Anchor = anchor.UTC().Format(time.RFC3339)
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
