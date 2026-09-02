// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package threshold provides CEL-based evaluation of performance thresholds.
// Threshold expressions use a single `value` variable (float64) and must
// return a boolean. Example: "value >= 900".
package threshold

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
)

// Definition describes a known threshold key.
type Definition struct {
	// Key is the camelCase metric name with unit suffix.
	Key string
	// Unit is the human-readable unit for display (e.g., "GB/s", "seconds").
	Unit string
	// Description is a short help text for the metric.
	Description string
}

// Registry lists all known threshold keys. Unknown keys are rejected
// by ValidateKeys.
var Registry = []Definition{
	{Key: "busBandwidthGBps", Unit: "GB/s", Description: "Bus bandwidth (from BandwidthMeasurement)"},
	{Key: "algBandwidthGBps", Unit: "GB/s", Description: "Algorithm bandwidth (from BandwidthMeasurement)"},
	{Key: "goodputRatio", Unit: "ratio (0-1)", Description: "Goodput ratio (from GoodputMeasurement)"},
	{Key: "avgTFLOPsPerGPU", Unit: "TFLOPS", Description: "Average TFLOPS per GPU (from GoodputMeasurement)"},
	{Key: "avgStepTimeSec", Unit: "seconds", Description: "Average step time (from GoodputMeasurement)"},
}

var registryKeys map[string]bool
var registryOnce sync.Once

func knownKeys() map[string]bool {
	registryOnce.Do(func() {
		registryKeys = make(map[string]bool, len(Registry))
		for _, d := range Registry {
			registryKeys[d.Key] = true
		}
	})
	return registryKeys
}

// Keys returns all known threshold keys in Registry order.
func Keys() []string {
	keys := make([]string, 0, len(Registry))
	for _, d := range Registry {
		keys = append(keys, d.Key)
	}
	return keys
}

// ValidateKeys returns any keys in thresholds that are not in the Registry,
// sorted for deterministic messages.
func ValidateKeys(thresholds map[string]string) []string {
	known := knownKeys()
	var unknown []string
	for k := range thresholds {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// ValidateKeysError returns an error when thresholds contains keys that are
// not in the Registry. The error names each offending key and lists the valid
// keys, so a typo is diagnosed instead of silently disabling validation.
// Returns nil when every key is known.
func ValidateKeysError(thresholds map[string]string) error {
	unknown := ValidateKeys(thresholds)
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("unknown threshold key(s) [%s]; valid keys: [%s]",
		strings.Join(unknown, ", "), strings.Join(Keys(), ", "))
}

// ValidateExpressions compiles all CEL expressions and returns errors
// for any that fail compilation. Keys with valid expressions are omitted.

// Violation describes a single threshold that was not met.
type Violation struct {
	Key        string
	Expression string
	Measured   float64
	Reason     string // "ThresholdViolated", "InvalidThresholdExpression"
	Message    string
}

// EvaluateAll checks all thresholds against measured values.
// Returns a list of violations (empty if all thresholds pass), sorted by key.
// Returns an error when thresholds contains a key that is not in the Registry:
// an unknown key can never be measured, so evaluating around it would silently
// skip validation. The error names the offending keys and lists the valid ones.
func EvaluateAll(thresholds map[string]string, measured map[string]float64) ([]Violation, error) {
	if len(thresholds) == 0 {
		return nil, nil
	}

	// Reject unknown keys first: a typo in one key must fail loudly rather
	// than disable threshold validation.
	if err := ValidateKeysError(thresholds); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(thresholds))
	for key := range thresholds {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Evaluate each threshold in sorted key order for deterministic output.
	var violations []Violation
	for _, key := range keys {
		expr := thresholds[key]
		value, ok := measured[key]
		if !ok {
			// No measurement available — the caller's missing-key handling
			// (requeue then MeasurementTimeout) is responsible for this case.
			continue
		}
		pass, err := Evaluate(value, expr)
		if err != nil {
			violations = append(violations, Violation{
				Key:        key,
				Expression: expr,
				Measured:   value,
				Reason:     "InvalidThresholdExpression",
				Message:    fmt.Sprintf("Threshold %q: %v", key, err),
			})
			continue
		}
		if !pass {
			violations = append(violations, Violation{
				Key:        key,
				Expression: expr,
				Measured:   value,
				Reason:     "ThresholdViolated",
				Message:    fmt.Sprintf("Threshold %q violated: measured %.4f, expression: %s", key, value, expr),
			})
		}
	}
	return violations, nil
}

// Evaluate compiles and evaluates a CEL expression with the measured value.
// Returns true if the threshold is satisfied (expression returns true),
// false if violated (expression returns false).
// Returns an error if the expression fails to compile or evaluate.
func Evaluate(measured float64, expression string) (bool, error) {
	prg, err := compile(expression)
	if err != nil {
		return false, fmt.Errorf("compile threshold expression %q: %w", expression, err)
	}

	out, _, err := prg.Eval(map[string]any{
		"value": measured,
	})
	if err != nil {
		return false, fmt.Errorf("evaluate threshold expression %q with value=%f: %w", expression, measured, err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("threshold expression %q returned %T, expected bool", expression, out.Value())
	}
	return result, nil
}

// compile creates a CEL program from a threshold expression.
// The expression has access to a single `value` variable of type double.
func compile(expression string) (cel.Program, error) {
	env, err := cel.NewEnv(
		cel.Variable("value", cel.DoubleType),
		cel.CrossTypeNumericComparisons(true),
	)
	if err != nil {
		return nil, fmt.Errorf("create CEL environment: %w", err)
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile: %w", issues.Err())
	}

	// Ensure the expression returns a boolean.
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("expression must return bool, got %s", ast.OutputType())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	return prg, nil
}
