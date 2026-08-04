// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package threshold

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name       string
		measured   float64
		expression string
		want       bool
		wantErr    bool
	}{
		// >= operator
		{"gte pass", 900.0, "value >= 900", true, false},
		{"gte exact", 900.0, "value >= 900.0", true, false},
		{"gte fail", 899.9, "value >= 900", false, false},

		// > operator
		{"gt pass", 900.1, "value > 900", true, false},
		{"gt exact fail", 900.0, "value > 900", false, false},

		// <= operator
		{"lte pass", 3.0, "value <= 3.0", true, false},
		{"lte fail", 3.1, "value <= 3.0", false, false},

		// < operator
		{"lt pass", 2.9, "value < 3.0", true, false},
		{"lt exact fail", 3.0, "value < 3.0", false, false},

		// Range
		{"range pass", 950.0, "value >= 900 && value <= 1200", true, false},
		{"range fail low", 800.0, "value >= 900 && value <= 1200", false, false},
		{"range fail high", 1300.0, "value >= 900 && value <= 1200", false, false},

		// Ratio
		{"ratio pass", 0.92, "value >= 0.85", true, false},
		{"ratio fail", 0.80, "value >= 0.85", false, false},

		// Errors
		{"invalid expr", 0, "not_a_variable >= 1", false, true},
		{"non-bool return", 0, "value + 1.0", false, true},
		{"syntax error", 0, "value >=", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Evaluate(tt.measured, tt.expression)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
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
