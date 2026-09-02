// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package numstr

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Each case parses tc.Inputs["input.yaml"]'s "in" string and formats the
// result to 9 decimal places, matching the precision of the require.InDelta
// this test used before conversion (1e-9).
func TestParse(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			In string `yaml:"in"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := Parse(in.In)

		b, err := json.MarshalIndent(struct {
			Got string `json:"got"`
		}{fmt.Sprintf("%.9f", got)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// Each case round trips tc.Inputs["input.yaml"]'s "in" float through Format
// then Parse, and formats the result to 9 decimal places — finer than the
// require.InDelta precision (1e-6) this test used before conversion, so the
// golden still pins the value to at least that tolerance.
func TestFormatParseRoundTrip(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "format-parse-round-trip",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			In float64 `yaml:"in"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := Parse(Format(in.In))

		b, err := json.MarshalIndent(struct {
			RoundTripped string `json:"roundTripped"`
		}{fmt.Sprintf("%.9f", got)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// Non-finite values round trip intact: strconv.ParseFloat accepts "NaN" and
// "+Inf" without error, so the codec neither rejects nor sanitises them. This
// matches all three implementations that preceded this package, and is pinned
// here so any future change is a deliberate one rather than a silent shift.
//
// The codec is the wrong layer to sanitise these — a NaN reaching here means a
// caller divided by zero, and squashing it to 0 would hide that while still
// reporting a plausible-looking measurement. Guard the division instead; see
// CalculateGoodput, which returns 0 when the denominator is non-positive.
func TestNonFiniteRoundTripsIntact(t *testing.T) {
	require.True(t, math.IsNaN(Parse(Format(math.NaN()))), "NaN survives the round trip")
	require.True(t, math.IsInf(Parse(Format(math.Inf(1))), 1), "+Inf survives the round trip")
	require.True(t, math.IsNaN(Parse("NaN")), "NaN is parsed, not treated as malformed")
}
