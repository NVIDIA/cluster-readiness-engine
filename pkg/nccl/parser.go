// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nccl

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	v1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// Parser parses NCCL bandwidth test output using a compiled regex from a LogProfile.
type Parser struct {
	regex    *regexp.Regexp
	sizeIdx  int
	algBWIdx int
	busBWIdx int

	// transportRegex is optional. LogProfiles that predate networkTransport
	// still compile; ParseTransports then returns nothing.
	transportRegex *regexp.Regexp
	transportIdx   int
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

	parser := &Parser{
		regex:    re,
		sizeIdx:  sizeIdx,
		algBWIdx: algBWIdx,
		busBWIdx: busBWIdx,
	}

	if profile.Spec.Patterns.NetworkTransport != nil {
		tr, err := regexp.Compile(profile.Spec.Patterns.NetworkTransport.Regex)
		if err != nil {
			return nil, fmt.Errorf("compiling networkTransport regex: %w", err)
		}
		transportIdx := namedGroupIndex(tr, "transport")
		if transportIdx < 0 {
			return nil, fmt.Errorf("networkTransport regex must have named group: transport")
		}
		parser.transportRegex = tr
		parser.transportIdx = transportIdx
	}

	return parser, nil
}

// ParseBandwidthLogs parses log lines and returns all matched bandwidth data points.
// Lines that don't match the pattern are silently skipped.
func (p *Parser) ParseBandwidthLogs(lines []string) []BandwidthDataPoint {
	results := make([]BandwidthDataPoint, 0, len(lines)/4) // most lines are non-data

	for _, line := range lines {
		if dp, ok := p.ParseBandwidthLine(line); ok {
			results = append(results, dp)
		}
	}

	return results
}

// ParseBandwidthLine parses one log line, reporting false if it is not a
// bandwidth result row.
func (p *Parser) ParseBandwidthLine(line string) (BandwidthDataPoint, bool) {
	// Strip the Kubernetes RFC3339 timestamp prefix if present.
	// Format: "2026-02-05T15:30:00.123456Z <content>"
	content := stripK8sTimestamp(line)

	matches := p.regex.FindStringSubmatch(content)
	if matches == nil {
		return BandwidthDataPoint{}, false
	}

	size, err := strconv.ParseInt(matches[p.sizeIdx], 10, 64)
	if err != nil {
		return BandwidthDataPoint{}, false
	}

	algBW, err := strconv.ParseFloat(matches[p.algBWIdx], 64)
	if err != nil {
		return BandwidthDataPoint{}, false
	}

	busBW, err := strconv.ParseFloat(matches[p.busBWIdx], 64)
	if err != nil {
		return BandwidthDataPoint{}, false
	}

	return BandwidthDataPoint{
		SizeBytes: size,
		AlgBW:     algBW,
		BusBW:     busBW,
	}, true
}

// ParseTransports returns the distinct, sorted NCCL network names captured
// from "Using network ..." log lines. Returns nil when the LogProfile has no
// networkTransport pattern or no line matched.
func (p *Parser) ParseTransports(lines []string) []string {
	if p.transportRegex == nil {
		return nil
	}

	seen := make(map[string]struct{})
	var out []string
	for _, line := range lines {
		name, ok := p.ParseTransportLine(line)
		if !ok {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	slices.Sort(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseTransportLine returns the NCCL network name from one "Using network
// ..." log line. It reports false when the LogProfile has no networkTransport
// pattern, the line does not match, or the captured name is blank.
func (p *Parser) ParseTransportLine(line string) (string, bool) {
	if p.transportRegex == nil {
		return "", false
	}
	matches := p.transportRegex.FindStringSubmatch(stripK8sTimestamp(line))
	if matches == nil {
		return "", false
	}
	name := strings.TrimSpace(matches[p.transportIdx])
	if name == "" {
		return "", false
	}
	return name, true
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
