// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package goodput

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
)

// ──────────────────────────────────────────────────────────────────────────────
// Golden-file tests using testutil
// ──────────────────────────────────────────────────────────────────────────────

func TestParseLogs(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "parse-logs",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var profile v1alpha1.LogProfile
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_profile.yaml"]), &profile); err != nil {
			return err
		}
		parser, err := NewProfileParser(&profile)
		if err != nil {
			return err
		}
		raw := strings.TrimRight(tc.Inputs["input_logs.txt"], "\n")
		var lines []string
		if raw != "" {
			lines = strings.Split(raw, "\n")
		}
		result := parser.ParseLogs(lines)

		output := toTestOutput(result)
		b, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestExtractK8sTimestamp(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "extract-k8s-timestamp",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		line := strings.TrimSpace(tc.Inputs["input.txt"])
		got := extractK8sTimestamp(line)
		ts := ""
		if !got.IsZero() {
			ts = got.UTC().Format(time.RFC3339Nano)
		}
		b, err := json.MarshalIndent(map[string]any{"timestamp": ts}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestNormalizeUnit(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "normalize-unit",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Field string            `yaml:"field"`
			Value float64           `yaml:"value"`
			Units map[string]string `yaml:"units"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		parser := &ProfileParser{}
		cp := &compiledPattern{units: input.Units}
		got := parser.normalizeUnit(cp, input.Field, input.Value)
		b, err := json.MarshalIndent(map[string]any{"result": roundTo(got, 6)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// toTestOutput converts a ParseResult to a deterministic, JSON-serializable map.
// Zero-value timestamps and nil fields are omitted for cleaner golden files.
func toTestOutput(r *ParseResult) map[string]any {
	result := map[string]any{}
	if !r.ApplicationStartTime.IsZero() {
		result["applicationStartTime"] = r.ApplicationStartTime.UTC().Format(time.RFC3339Nano)
	}
	if !r.LastLogTimestamp.IsZero() {
		result["lastLogTimestamp"] = r.LastLogTimestamp.UTC().Format(time.RFC3339Nano)
	}
	if r.FirstStep != nil {
		result["firstStep"] = stepToMap(r.FirstStep)
	}
	if r.LastStep != nil {
		result["lastStep"] = stepToMap(r.LastStep)
	}
	if len(r.Steps) > 0 {
		steps := make([]map[string]any, len(r.Steps))
		for i, s := range r.Steps {
			steps[i] = stepToMap(s)
		}
		result["steps"] = steps
	}
	if r.LastCheckpoint != nil {
		result["lastCheckpoint"] = checkpointToMap(r.LastCheckpoint)
	}
	if len(r.Checkpoints) > 0 {
		ckpts := make([]map[string]any, len(r.Checkpoints))
		for i, c := range r.Checkpoints {
			ckpts[i] = checkpointToMap(c)
		}
		result["checkpoints"] = ckpts
	}
	if r.CheckpointRestore != nil {
		m := map[string]any{
			"step": r.CheckpointRestore.Step,
		}
		if r.CheckpointRestore.Path != "" {
			m["path"] = r.CheckpointRestore.Path
		}
		if !r.CheckpointRestore.Timestamp.IsZero() {
			m["timestamp"] = r.CheckpointRestore.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		result["checkpointRestore"] = m
	}
	if r.LogInterval > 0 {
		result["logInterval"] = r.LogInterval
	}
	if r.PendingSave != nil {
		ps := map[string]any{
			"step": r.PendingSave.Step,
		}
		if !r.PendingSave.Timestamp.IsZero() {
			ps["timestamp"] = r.PendingSave.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		if r.PendingSave.Path != "" {
			ps["path"] = r.PendingSave.Path
		}
		result["pendingSave"] = ps
	}
	return result
}

func stepToMap(s *TrainingStepInfo) map[string]any {
	m := map[string]any{
		"globalStep": s.GlobalStep,
		"epoch":      s.Epoch,
		"iteration":  s.Iteration,
	}
	if !s.Timestamp.IsZero() {
		m["timestamp"] = s.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	if s.StepTiming != 0 {
		m["stepTiming"] = s.StepTiming
	}
	if s.Loss != 0 {
		m["loss"] = s.Loss
	}
	if s.TFLOPS != 0 {
		m["tflops"] = s.TFLOPS
	}
	if s.ElapsedTime != 0 {
		m["elapsedTime"] = s.ElapsedTime
	}
	if s.IsWarmup {
		m["isWarmup"] = true
	}
	return m
}

func checkpointToMap(c *CheckpointInfo) map[string]any {
	m := map[string]any{
		"step": c.Step,
	}
	if !c.Timestamp.IsZero() {
		m["timestamp"] = c.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	if c.Path != "" {
		m["path"] = c.Path
	}
	if c.SaveDuration != 0 {
		m["saveDuration"] = c.SaveDuration
	}
	return m
}

// roundTo rounds a float64 to n decimal places.
func roundTo(f float64, n int) float64 {
	pow := math.Pow(10, float64(n))
	return math.Round(f*pow) / pow
}
