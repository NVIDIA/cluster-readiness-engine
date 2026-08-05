// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package threshold provides CEL-based evaluation of performance thresholds.
// Threshold expressions use a single `value` variable (float64) and must
// return a boolean. Example: "value >= 900".
package threshold

import (
	"fmt"
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

// ValidateKeys returns any keys in thresholds that are not in the Registry.
func ValidateKeys(thresholds map[string]string) []string {
	known := knownKeys()
	var unknown []string
	for k := range thresholds {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	return unknown
}

// ValidateExpressions compiles all CEL expressions and returns errors
// for any that fail compilation. Keys with valid expressions are omitted.

// Violation describes a single threshold that was not met.
type Violation struct {
	Key        string
	Expression string
	Measured   float64
	Reason     string // "ThresholdViolated", "UnknownThresholdKey", "InvalidThresholdExpression"
	Message    string
}

// EvaluateAll checks all thresholds against measured values.
// Returns a list of violations (empty if all thresholds pass).
func EvaluateAll(thresholds map[string]string, measured map[string]float64) []Violation {
	if len(thresholds) == 0 {
		return nil
	}

	var violations []Violation

	// Validate keys first.
	for _, key := range ValidateKeys(thresholds) {
		violations = append(violations, Violation{
			Key:     key,
			Reason:  "UnknownThresholdKey",
			Message: fmt.Sprintf("Unknown threshold key %q", key),
		})
	}
	if len(violations) > 0 {
		return violations
	}

	// Evaluate each threshold.
	for key, expr := range thresholds {
		value, ok := measured[key]
		if !ok {
			continue // no measurement available — skip silently
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
	return violations
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
