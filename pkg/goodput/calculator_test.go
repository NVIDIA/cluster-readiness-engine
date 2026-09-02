// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package goodput

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"
)

// ──────────────────────────────────────────────────────────────────────────────
// Golden-file tests using testutil
// ──────────────────────────────────────────────────────────────────────────────

type calculatorInput struct {
	TW   float64 `yaml:"tW"`
	TCh  float64 `yaml:"tCh"`
	TRe  float64 `yaml:"tRe"`
	TRm  float64 `yaml:"tRm"`
	TSav float64 `yaml:"tSav"`
}

func TestCalculateGoodput(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "calculate-goodput",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input calculatorInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		got := CalculateGoodput(input.TW, input.TCh, input.TRe, input.TRm, input.TSav)
		b, err := json.MarshalIndent(map[string]any{"goodput": roundTo(got, 6)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

type progressInput struct {
	Operations []struct {
		CurrentStep    int       `yaml:"currentStep"`
		CheckpointStep int       `yaml:"checkpointStep"`
		CheckpointTime time.Time `yaml:"checkpointTime"`
	} `yaml:"operations"`
}

func TestCumulativeMetricsUpdateProgress(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "cumulative-update-progress",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input progressInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		cm := NewCumulativeMetrics()
		for _, op := range input.Operations {
			cm.UpdateTrainingProgress(op.CurrentStep, op.CheckpointStep, op.CheckpointTime)
		}
		b, err := json.MarshalIndent(map[string]any{
			"currentStep":        cm.CurrentStep,
			"highestStep":        cm.HighestStep,
			"lastCheckpointStep": cm.LastCheckpointStep,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}
