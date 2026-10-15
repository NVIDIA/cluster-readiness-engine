// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestRunningStatusDetail pins which Workflow InProgress reasons the report
// relays for a Running category: a scheduling stall (ADR-083) and a wait for
// the controller's namespace access (ADR-092). Ordinary progress says nothing.
func TestRunningStatusDetail(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "running-status-detail", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Conditions []metav1.Condition `json:"conditions"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		data, err := json.MarshalIndent(map[string]string{
			"statusDetail": runningStatusDetail(input.Conditions),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
