// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestResolveWaitTimeout covers the --wait timeout derivation (issue #183):
// an explicit --timeout always wins; otherwise the timeout is derived from the
// selected categories' catalog timeoutPerJob budgets (max across categories,
// scaled by the queue-time margin), floored at the 30m flag default.
func TestResolveWaitTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve-wait-timeout",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// ExplicitTimeout simulates cmd.Flags().Changed("timeout").
			ExplicitTimeout bool `json:"explicitTimeout"`
			// FlagTimeout is the --timeout flag value (its default when
			// ExplicitTimeout is false).
			FlagTimeout   string                      `json:"flagTimeout"`
			Certification nvcrev1alpha1.Certification `json:"certification"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		flagValue, err := time.ParseDuration(input.FlagTimeout)
		if err != nil {
			return err
		}

		timeout, derived := resolveWaitTimeout(&input.Certification, flagValue, input.ExplicitTimeout)

		data, err := json.MarshalIndent(map[string]any{
			"timeout": timeout.String(),
			"derived": derived,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
