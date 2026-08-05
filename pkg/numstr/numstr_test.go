// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package numstr

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{name: "plain decimal", in: "0.85", want: 0.85},
		{name: "integer", in: "42", want: 42},
		{name: "negative", in: "-1.5", want: -1.5},
		{name: "surrounding whitespace is tolerated", in: "  2.5  ", want: 2.5},
		{name: "scientific notation", in: "1e3", want: 1000},
		{name: "storage precision round trip", in: "922.940000", want: 922.94},
		{name: "empty is zero", in: "", want: 0},
		{name: "non-numeric is zero", in: "abc", want: 0},

		// The previous fmt.Sscanf implementation returned 1.5 here. Nothing in
		// the codebase produces such a value — every writer goes through Format
		// or strconv.FormatFloat — so accepting it could only mask corruption.
		{name: "trailing garbage is rejected, not truncated", in: "1.5abc", want: 0},
		{name: "units are rejected", in: "922.94 GB/s", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.InDelta(t, tt.want, Parse(tt.in), 1e-9)
		})
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   float64
	}{
		{name: "goodput ratio", in: 0.857142},
		{name: "zero", in: 0},
		{name: "bandwidth", in: 922.94},
		{name: "negative", in: -3.5},
		{name: "sub-millisecond step time", in: 0.000125},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.InDelta(t, tt.in, Parse(Format(tt.in)), 1e-6,
				"Format must round trip through Parse within storage precision")
		})
	}
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
