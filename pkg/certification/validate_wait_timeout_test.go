// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestValidateWaitTimeout covers issue #409: --timeout must be positive when
// --wait is set or when the flag is passed explicitly. Cases parse flags
// through the real cobra command so DurationVar's 0 / negative acceptance is
// in the fixture, not invented in the test.
func TestValidateWaitTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "validate-wait-timeout",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Args []string `json:"args"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		cmd := newRunCommand("dev")
		if err := cmd.ParseFlags(input.Args); err != nil {
			return err
		}
		timeout, err := cmd.Flags().GetDuration("timeout")
		if err != nil {
			return err
		}
		wait, err := cmd.Flags().GetBool("wait")
		if err != nil {
			return err
		}

		if !strings.Contains(cmd.Flags().Lookup("timeout").Usage, "positive") {
			tc.T.Errorf("--timeout help %q does not say the value must be positive",
				cmd.Flags().Lookup("timeout").Usage)
		}

		if err := validateWaitTimeout(timeout, wait, cmd.Flags().Changed("timeout")); err != nil {
			return err
		}

		data, err := json.MarshalIndent(map[string]any{
			"timeout":  timeout.String(),
			"wait":     wait,
			"explicit": cmd.Flags().Changed("timeout"),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestNewRunCommandRejectsNonPositiveTimeout(t *testing.T) {
	t.Run("zero timeout with wait", func(t *testing.T) {
		cmd := newRunCommand("dev")
		cmd.SetArgs([]string{
			testCategoryFlag, testCategoryNCCLAllReduce,
			"--wait", "--timeout=0",
		})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, "--timeout must be positive, got 0s", err.Error())
	})

	t.Run("negative timeout with wait", func(t *testing.T) {
		cmd := newRunCommand("dev")
		cmd.SetArgs([]string{
			testCategoryFlag, testCategoryNCCLAllReduce,
			"--wait", "--timeout=-1s",
		})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, "--timeout must be positive, got -1s", err.Error())
	})

	t.Run("explicit zero timeout without wait", func(t *testing.T) {
		cmd := newRunCommand("dev")
		cmd.SetArgs([]string{
			testCategoryFlag, testCategoryNCCLAllReduce,
			"--timeout=0",
		})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, "--timeout must be positive, got 0s", err.Error())
	})
}
