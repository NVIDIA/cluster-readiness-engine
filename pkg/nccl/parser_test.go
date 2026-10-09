// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nccl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// testTimestampLayout is the Kubernetes log timestamp layout shared by the
// fixtures below.
const testTimestampLayout = "2006-01-02T15:04:05.999999999Z"

// testBusBWRegex is the NCCL bandwidth table row regex shared by the
// fixtures below.
const testBusBWRegex = `^\s*(?P<size>\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`

// testTransportRegex is the shipped nccl-bandwidth networkTransport pattern.
const testTransportRegex = `NCCL INFO Using network (?P<transport>.+)`

const testProfileNCCLBandwidth = "nccl-bandwidth"

// The lines below cover an NCCL INFO line, a two-line table header, data
// lines with a K8s timestamp prefix (both "Z" and offset-free variants), and
// a raw data line with no timestamp prefix at all, to prove the parser skips
// what it should and extracts size/algBW/busBW from what it shouldn't.
func TestParseBandwidthLogs(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse-bandwidth-logs",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Regex           string `yaml:"regex"`
			TimestampLayout string `yaml:"timestampLayout"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		profile := &v1alpha1.LogProfile{
			Name: "nccl-all-reduce",
			Spec: v1alpha1.LogProfileSpec{
				Timestamp: v1alpha1.TimestampSpec{Layout: in.TimestampLayout},
				Patterns: v1alpha1.LogPatternSet{
					BandwidthResult: &v1alpha1.EventPattern{
						Regex: in.Regex,
					},
				},
			},
		}

		parser, err := NewParser(profile)
		if err != nil {
			return fmt.Errorf("NewParser: %w", err)
		}

		lines := strings.Split(strings.TrimRight(tc.Inputs["input_log.txt"], "\n"), "\n")
		results := parser.ParseBandwidthLogs(lines)

		type point struct {
			SizeBytes int64   `json:"sizeBytes"`
			AlgBW     float64 `json:"algBW"`
			BusBW     float64 `json:"busBW"`
		}
		out := struct {
			Count   int     `json:"count"`
			Results []point `json:"results"`
		}{Count: len(results)}
		for _, r := range results {
			out.Results = append(out.Results, point(r))
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestParseTransports(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse-transports",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Regex           string `yaml:"regex"`
			TransportRegex  string `yaml:"transportRegex"`
			TimestampLayout string `yaml:"timestampLayout"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		profile := &v1alpha1.LogProfile{
			Name: testProfileNCCLBandwidth,
			Spec: v1alpha1.LogProfileSpec{
				Timestamp: v1alpha1.TimestampSpec{Layout: in.TimestampLayout},
				Patterns: v1alpha1.LogPatternSet{
					BandwidthResult: &v1alpha1.EventPattern{
						Regex: in.Regex,
					},
				},
			},
		}
		if in.TransportRegex != "" {
			profile.Spec.Patterns.NetworkTransport = &v1alpha1.EventPattern{
				Regex: in.TransportRegex,
			}
		}

		parser, err := NewParser(profile)
		if err != nil {
			return fmt.Errorf("NewParser: %w", err)
		}

		lines := strings.Split(strings.TrimRight(tc.Inputs["input_log.txt"], "\n"), "\n")
		got := parser.ParseTransports(lines)

		b, err := json.MarshalIndent(struct {
			Transport []string `json:"transport"`
		}{Transport: got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// TestParseBandwidthLogsNonUTCNode covers log lines from a node that is not
// set to UTC. Kubelet then writes an offset such as "-07:00" instead of "Z",
// which makes the timestamp prefix longer. These lines come from a real
// A100 run on a node in PDT.
func TestParseBandwidthLogsNonUTCNode(t *testing.T) {
	profile := &v1alpha1.LogProfile{
		Name: "nccl-loopback",
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: testTimestampLayout},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: testBusBWRegex,
				},
			},
		},
	}

	parser, err := NewParser(profile)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	lines := []string{
		"2026-08-06T09:30:29.569516911-07:00    536870912     134217728     float     sum      -1   814.58  659.07    0.00       0     1.52  352278    0.00       0",
		"2026-08-06T09:30:31.128034512-07:00   1073741824     268435456     float     sum      -1  1558.51  688.95    0.00       0     0.18   6e+06    0.00       0",
		// A positive offset must work too.
		"2026-08-06T16:30:31.128034512+05:30       65536         16384     float     sum      -1    58.16    1.13    2.18       0    58.12    1.13    2.18       0",
	}

	results := parser.ParseBandwidthLogs(lines)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[1].SizeBytes != 1073741824 || results[1].AlgBW != 688.95 {
		t.Errorf("got size %d algBW %f, want 1073741824 and 688.95",
			results[1].SizeBytes, results[1].AlgBW)
	}
	if results[2].BusBW != 2.18 {
		t.Errorf("BusBW = %f, want 2.18", results[2].BusBW)
	}
}

// TestParseRealA100LoopbackLog parses the captured output of an
// nccl-loopback run on an A100 node whose clock is set to PDT. The file is
// the pod log exactly as kubectl returns it, including the NCCL banner, the
// table header, and the footer.
func TestParseRealA100LoopbackLog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "real_a100_loopback_pdt.log"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	profile := &v1alpha1.LogProfile{
		Name: "nccl-loopback",
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: testTimestampLayout},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: testBusBWRegex,
				},
			},
		},
	}

	parser, err := NewParser(profile)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	results := parser.ParseBandwidthLogs(strings.Split(string(raw), "\n"))

	// The run measured 28 message sizes, from 8 bytes to 1 GB, over two
	// cycles, so the table holds 56 rows.
	if len(results) != 56 {
		t.Fatalf("expected 56 results, got %d", len(results))
	}

	first, last := results[0], results[len(results)-1]
	if first.SizeBytes != 8 {
		t.Errorf("first size = %d, want 8", first.SizeBytes)
	}
	if last.SizeBytes != 1073741824 {
		t.Errorf("last size = %d, want 1073741824", last.SizeBytes)
	}
	if last.AlgBW != 688.95 {
		t.Errorf("last algBW = %f, want 688.95", last.AlgBW)
	}
	// A single rank gives a bus bandwidth of 0, because the formula scales by
	// 2*(n-1)/n. The parser must still record the row.
	if last.BusBW != 0 {
		t.Errorf("last busBW = %f, want 0", last.BusBW)
	}
}

// TestParseRealA100AllReduce2NodeLog parses the captured launcher output of an
// nccl-all-reduce run across two A100 nodes whose clocks are set to PDT. Two
// ranks give a bus bandwidth above zero, which the single rank loopback
// fixture cannot show.
func TestParseRealA100AllReduce2NodeLog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "real_a100_allreduce_2node_pdt.log"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	profile := &v1alpha1.LogProfile{
		Name: testProfileNCCLBandwidth,
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: testTimestampLayout},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: testBusBWRegex,
				},
				NetworkTransport: &v1alpha1.EventPattern{
					Regex: testTransportRegex,
				},
			},
		},
	}

	parser, err := NewParser(profile)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	lines := strings.Split(string(raw), "\n")
	results := parser.ParseBandwidthLogs(lines)

	// The run measured 24 message sizes, from 8 bytes to 64 MiB.
	if len(results) != 24 {
		t.Fatalf("expected 24 results, got %d", len(results))
	}

	last := results[len(results)-1]
	if last.SizeBytes != 67108864 {
		t.Errorf("last size = %d, want 67108864", last.SizeBytes)
	}
	// Two ranks give a bus bandwidth above zero. The old parser recorded
	// nothing at all, so this row proves the offset timestamp is handled.
	if last.BusBW <= 0 {
		t.Errorf("last busBW = %f, want a value above 0", last.BusBW)
	}
	if last.AlgBW <= 0 {
		t.Errorf("last algBW = %f, want a value above 0", last.AlgBW)
	}

	transports := parser.ParseTransports(lines)
	if len(transports) != 1 || transports[0] != "Socket" {
		t.Errorf("transport = %v, want [Socket]", transports)
	}
}

func TestNewParserMissingPattern(t *testing.T) {
	profile := &v1alpha1.LogProfile{
		Name: "empty",
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: "2006-01-02T15:04:05Z"},
			Patterns:  v1alpha1.LogPatternSet{},
		},
	}

	_, err := NewParser(profile)
	if err == nil {
		t.Fatal("expected error for missing bandwidthResult pattern")
	}
}

func TestNewParserMissingTransportGroup(t *testing.T) {
	profile := &v1alpha1.LogProfile{
		Name: testProfileNCCLBandwidth,
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: testTimestampLayout},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: testBusBWRegex,
				},
				NetworkTransport: &v1alpha1.EventPattern{
					Regex: `NCCL INFO Using network (.+)`,
				},
			},
		},
	}

	_, err := NewParser(profile)
	if err == nil {
		t.Fatal("expected error for missing transport named group")
	}
}

func TestParseTransportsWithoutPattern(t *testing.T) {
	profile := &v1alpha1.LogProfile{
		Name: testProfileNCCLBandwidth,
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: testTimestampLayout},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: testBusBWRegex,
				},
			},
		},
	}

	parser, err := NewParser(profile)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	if got := parser.ParseTransports([]string{"NCCL INFO Using network Socket"}); got != nil {
		t.Errorf("ParseTransports without pattern = %v, want nil", got)
	}
}

func TestStripK8sTimestamp(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "strip-k8s-timestamp",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Input string `yaml:"input"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := stripK8sTimestamp(in.Input)

		b, err := json.MarshalIndent(struct {
			Got string `json:"got"`
		}{Got: got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// TestParseTransportLine covers the per-line parse the controller's final log
// read uses, so it records the same names ParseTransports does.
func TestParseTransportLine(t *testing.T) {
	newParser := func(withTransport bool) *Parser {
		profile := &v1alpha1.LogProfile{
			Name: testProfileNCCLBandwidth,
			Spec: v1alpha1.LogProfileSpec{
				Timestamp: v1alpha1.TimestampSpec{Layout: testTimestampLayout},
				Patterns: v1alpha1.LogPatternSet{
					BandwidthResult: &v1alpha1.EventPattern{Regex: testBusBWRegex},
				},
			},
		}
		if withTransport {
			profile.Spec.Patterns.NetworkTransport = &v1alpha1.EventPattern{Regex: testTransportRegex}
		}
		parser, err := NewParser(profile)
		if err != nil {
			t.Fatalf("NewParser: %v", err)
		}
		return parser
	}
	withPattern := newParser(true)

	cases := []struct {
		name   string
		parser *Parser
		line   string
		want   string
		wantOK bool
	}{
		{"timestamped", withPattern, "2026-09-30T10:01:00.1Z host:1:1 [0] NCCL INFO Using network IB", "IB", true},
		{"untimestamped", withPattern, "NCCL INFO Using network Socket", "Socket", true},
		{"trailing space trimmed", withPattern, "NCCL INFO Using network IB  ", "IB", true},
		{"blank name", withPattern, "NCCL INFO Using network  ", "", false},
		{"no match", withPattern, "NCCL INFO Bootstrap : Using eth0", "", false},
		{"no pattern", newParser(false), "NCCL INFO Using network IB", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.parser.ParseTransportLine(tc.line)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("ParseTransportLine(%q) = (%q, %v), want (%q, %v)", tc.line, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
