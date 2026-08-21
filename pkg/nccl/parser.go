// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nccl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	v1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
)

// Parser parses NCCL bandwidth test output using a compiled regex from a LogProfile.
type Parser struct {
	regex    *regexp.Regexp
	sizeIdx  int
	algBWIdx int
	busBWIdx int
}

// NewParser creates a Parser from a LogProfile's bandwidthResult pattern.
// Returns an error if the pattern is nil or the regex is invalid.
func NewParser(profile *v1alpha1.LogProfile) (*Parser, error) {
	if profile.Spec.Patterns.BandwidthResult == nil {
		return nil, fmt.Errorf("LogProfile %s has no bandwidthResult pattern", profile.Name)
	}

	re, err := regexp.Compile(profile.Spec.Patterns.BandwidthResult.Regex)
	if err != nil {
		return nil, fmt.Errorf("compiling bandwidthResult regex: %w", err)
	}

	sizeIdx := namedGroupIndex(re, "size")
	algBWIdx := namedGroupIndex(re, "algBW")
	busBWIdx := namedGroupIndex(re, "busBW")

	if sizeIdx < 0 || algBWIdx < 0 || busBWIdx < 0 {
		return nil, fmt.Errorf("bandwidthResult regex must have named groups: size, algBW, busBW")
	}

	return &Parser{
		regex:    re,
		sizeIdx:  sizeIdx,
		algBWIdx: algBWIdx,
		busBWIdx: busBWIdx,
	}, nil
}

// ParseBandwidthLogs parses log lines and returns all matched bandwidth data points.
// Lines that don't match the pattern are silently skipped.
func (p *Parser) ParseBandwidthLogs(lines []string) []BandwidthDataPoint {
	results := make([]BandwidthDataPoint, 0, len(lines)/4) // most lines are non-data

	for _, line := range lines {
		// Strip the Kubernetes RFC3339 timestamp prefix if present.
		// Format: "2026-02-05T15:30:00.123456Z <content>"
		content := stripK8sTimestamp(line)

		matches := p.regex.FindStringSubmatch(content)
		if matches == nil {
			continue
		}

		size, err := strconv.ParseInt(matches[p.sizeIdx], 10, 64)
		if err != nil {
			continue
		}

		algBW, err := strconv.ParseFloat(matches[p.algBWIdx], 64)
		if err != nil {
			continue
		}

		busBW, err := strconv.ParseFloat(matches[p.busBWIdx], 64)
		if err != nil {
			continue
		}

		results = append(results, BandwidthDataPoint{
			SizeBytes: size,
			AlgBW:     algBW,
			BusBW:     busBW,
		})
	}

	return results
}

// namedGroupIndex returns the index of a named capture group in a compiled regex.
// Returns -1 if the group is not found.
func namedGroupIndex(re *regexp.Regexp, name string) int {
	for i, n := range re.SubexpNames() {
		if n == name {
			return i
		}
	}
	return -1
}

// stripK8sTimestamp removes the Kubernetes RFC3339 timestamp prefix from a log line.
// Kubernetes log lines are prefixed with "2006-01-02T15:04:05.999999999Z " when
// Timestamps: true is set in the PodLogOptions.
func stripK8sTimestamp(line string) string {
	// The prefix is RFC3339Nano. It ends in "Z" on a node set to UTC and in an
	// offset such as "-07:00" on any other node, so its length varies. Cut at
	// the first space instead of assuming a maximum length.
	if len(line) > 20 && line[4] == '-' && line[10] == 'T' {
		if _, rest, found := strings.Cut(line, " "); found {
			return rest
		}
	}
	return line
}
