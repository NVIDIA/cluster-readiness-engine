// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package threshold

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Covers the >=, >, <=, < operators, ranges, ratio-typed thresholds, and the
// three ways an expression can fail (undeclared variable, non-bool result,
// syntax error).
func TestEvaluate(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "evaluate",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Measured   float64 `yaml:"measured"`
			Expression string  `yaml:"expression"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got, err := Evaluate(in.Measured, in.Expression)
		if err != nil {
			return err
		}

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Measured   float64 `json:"measured"`
			Expression string  `json:"expression"`
			Pass       bool    `json:"pass"`
		}{in.Measured, in.Expression, got}); err != nil {
			return err
		}
		tc.Actual = buf.String()
		return nil
	})
}

// TestEvaluateAll covers the pass, violation, invalid-expression, and
// missing-measurement paths, plus the unknown-key error (issue #52): a key
// that is not in the Registry makes EvaluateAll return an error naming the
// key and listing the valid keys, instead of silently skipping validation.
func TestEvaluateAll(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "evaluate-all",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Thresholds map[string]string  `json:"thresholds"`
			Measured   map[string]float64 `json:"measured"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		violations, err := EvaluateAll(in.Thresholds, in.Measured)
		if err != nil {
			return err
		}

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Violations []Violation `json:"violations"`
		}{violations}); err != nil {
			return err
		}
		tc.Actual = buf.String()
		return nil
	})
}

func TestValidateKeys(t *testing.T) {
	t.Run("all known", func(t *testing.T) {
		unknown := ValidateKeys(map[string]string{
			"busBandwidthGBps": "value >= 900",
			"goodputRatio":     "value >= 0.85",
		})
		assert.Empty(t, unknown)
	})

	t.Run("unknown key", func(t *testing.T) {
		unknown := ValidateKeys(map[string]string{
			"busBandwidthGBps": "value >= 900",
			"fooBar":           "value > 0",
		})
		assert.Equal(t, []string{"fooBar"}, unknown)
	})

	t.Run("empty map", func(t *testing.T) {
		unknown := ValidateKeys(map[string]string{})
		assert.Empty(t, unknown)
	})
}

func TestValidateExpressions(t *testing.T) {
	t.Run("all valid", func(t *testing.T) {
		errs := ValidateExpressions(map[string]string{
			"busBandwidthGBps": "value >= 900",
			"avgStepTimeSec":   "value <= 3.0",
		})
		assert.Empty(t, errs)
	})

	t.Run("invalid expression", func(t *testing.T) {
		errs := ValidateExpressions(map[string]string{
			"busBandwidthGBps": "value >= 900",
			"goodputRatio":     "invalid expr !!",
		})
		assert.Len(t, errs, 1)
		assert.Contains(t, errs, "goodputRatio")
	})

	t.Run("non-bool expression", func(t *testing.T) {
		errs := ValidateExpressions(map[string]string{
			"busBandwidthGBps": "value + 1.0",
		})
		assert.Len(t, errs, 1)
	})
}

// ValidateExpressions pre-validates a map of CEL threshold expressions.
func ValidateExpressions(thresholds map[string]string) map[string]error {
	errs := make(map[string]error)
	for k, expr := range thresholds {
		if _, err := compile(expr); err != nil {
			errs[k] = err
		}
	}
	return errs
}
