// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

func TestRunCommandTimeoutHelp(t *testing.T) {
	usage := newRunCommand("dev").Flags().Lookup("timeout").Usage
	assert.Contains(t, usage, "ignored without --wait")
	assert.Contains(t, usage, "at least 1s")
}

// TestValidateWaitTimeout drives issue #409 policy through RunE: validate
// --timeout only when --wait is set, and reject values under 1s. Later
// pipeline errors (missing --category, no cluster) are treated as acceptance.
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
		err := executeDiscardingUsage(cmd, input.Args)
		if isWaitTimeoutValidationError(err) {
			return err
		}

		timeout, flagErr := cmd.Flags().GetDuration("timeout")
		if flagErr != nil {
			return flagErr
		}
		wait, flagErr := cmd.Flags().GetBool("wait")
		if flagErr != nil {
			return flagErr
		}

		data, err := json.MarshalIndent(map[string]any{
			"accepted": true,
			"timeout":  timeout.String(),
			"wait":     wait,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func executeDiscardingUsage(cmd *cobra.Command, args []string) error {
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func isWaitTimeoutValidationError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "--timeout must be at least")
}
