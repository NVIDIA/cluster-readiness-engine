// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package numstr encodes and decodes the numeric-valued strings used in CRD
// status fields.
//
// Several status fields (goodput ratios, step times, bandwidths) are typed as
// strings rather than floats: the Kubernetes API rejects NaN and Inf in numeric
// fields, and float round-tripping through JSON is lossy in ways that produce
// noisy status diffs. Storing a fixed-precision decimal string avoids both.
//
// This package exists because the decode half was previously reimplemented in
// three packages with divergent behaviour — one used fmt.Sscanf, which silently
// accepts trailing garbage ("1.5abc" → 1.5) where the others returned 0. Since
// producer and consumer of these strings must agree, both halves live here.
package numstr

import (
	"strconv"
	"strings"
)

// storagePrecision is the number of decimal places Format emits. Six is enough
// to represent sub-millisecond step times and goodput ratios without the value
// changing on a parse/format round trip.
const storagePrecision = 6

// Parse decodes a numeric status string. It returns 0 for empty, malformed, or
// partially-numeric input, which is the appropriate zero value for every
// caller: these fields are absent until the controller has measured something,
// and an unset field must not read as a real measurement.
func Parse(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// Format encodes a float for storage in a status string field.
func Format(f float64) string {
	return strconv.FormatFloat(f, 'f', storagePrecision, 64)
}
