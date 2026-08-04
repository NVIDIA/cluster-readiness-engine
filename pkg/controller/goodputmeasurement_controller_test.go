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
		ExpectedSuffix: ".json",
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
		ExpectedSuffix: ".json",
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
		ExpectedSuffix: ".json",
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
