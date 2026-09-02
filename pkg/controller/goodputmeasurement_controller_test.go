// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/goodput"
)

func TestComputeAvgTFLOPS(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "compute-avg-tflops",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Steps []*goodput.TrainingStepInfo `yaml:"steps"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := computeAvgTFLOPS(&goodput.ParseResult{Steps: input.Steps})

		data, err := json.MarshalIndent(struct {
			Result float64 `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestComputeWarmupTime(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "compute-warmup-time",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Steps       []*goodput.TrainingStepInfo `yaml:"steps"`
			LogInterval int                         `yaml:"logInterval"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := computeWarmupTime(&goodput.ParseResult{Steps: input.Steps, LogInterval: input.LogInterval})

		data, err := json.MarshalIndent(struct {
			Result float64 `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestComputeNonWarmupTime(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "compute-non-warmup-time",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Steps       []*goodput.TrainingStepInfo `yaml:"steps"`
			LogInterval int                         `yaml:"logInterval"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		result := computeNonWarmupTimeDelta(&goodput.ParseResult{Steps: input.Steps, LogInterval: input.LogInterval}, 0)

		data, err := json.MarshalIndent(struct {
			Result float64 `json:"result"`
		}{Result: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// computeWarmupTime returns the total time spent on warmup steps in seconds.
// Only used by tests — lives here to avoid polluting the production controller file.
func computeWarmupTime(result *goodput.ParseResult) float64 {
	var sum float64
	var prevStep int
	for _, s := range result.Steps {
		step := s.GlobalStep
		if !s.IsWarmup {
			prevStep = step
			continue
		}
		if s.StepTiming > 0 {
			delta := step - prevStep
			if prevStep == 0 || delta < 1 {
				delta = max(result.LogInterval, 1)
			}
			sum += s.StepTiming * float64(delta)
		}
		prevStep = step
	}
	if sum > 0 {
		return sum
	}

	// Fall back to timestamp gaps between consecutive warmup steps.
	var prev *goodput.TrainingStepInfo
	for _, s := range result.Steps {
		if !s.IsWarmup {
			continue
		}
		if prev != nil && !prev.Timestamp.IsZero() && !s.Timestamp.IsZero() {
			gap := s.Timestamp.Sub(prev.Timestamp).Seconds()
			if gap > 0 {
				sum += gap
			}
		}
		prev = s
	}
	return sum
}
