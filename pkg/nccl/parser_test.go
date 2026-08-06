// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nccl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestParseBandwidthLogs(t *testing.T) {
	profile := &v1alpha1.LogProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "nccl-all-reduce"},
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: "2006-01-02T15:04:05.999999999Z"},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: `^\s*(?P<size>\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`,
				},
			},
		},
	}

	parser, err := NewParser(profile)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	lines := []string{
		// K8s timestamp prefix + NCCL INFO line (should be skipped)
		"2026-02-16T10:00:00.000000000Z nccl-node-0:1295:1295 [0] NCCL INFO Bootstrap: Using eth0",
		// Header lines (should be skipped)
		"2026-02-16T10:00:01.000000000Z #       size         count      type   redop    root     time   algbw   busbw  #wrong",
		"2026-02-16T10:00:01.000000000Z #        (B)    (elements)                               (us)  (GB/s)  (GB/s)",
		// Data lines with K8s timestamp prefix
		"2026-02-16T10:00:02.000000000Z            8             2     float     sum      -1    58.31    0.00    0.00       0    58.40    0.00    0.00       0",
		"2026-02-16T10:00:03.000000000Z        65536         16384     float     sum      -1    58.16    1.13    2.18       0    58.12    1.13    2.18       0",
		"2026-02-16T10:00:04.000000000Z   8589934592    2147483648     float     sum      -1  18551.0  463.04  897.15       0  18510.2  464.07  899.13       0",
		// Data lines without K8s timestamp prefix (e.g., raw output)
		"  17179869184    4294967296     float     sum      -1  36065.1  476.36  922.94       0  36071.8  476.27  922.77       0",
	}

	results := parser.ParseBandwidthLogs(lines)

	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}

	tests := []struct {
		name      string
		idx       int
		wantSize  int64
		wantAlgBW float64
		wantBusBW float64
	}{
		{"8 bytes", 0, 8, 0.00, 0.00},
		{"64KB", 1, 65536, 1.13, 2.18},
		{"8GB", 2, 8589934592, 463.04, 897.15},
		{"16GB", 3, 17179869184, 476.36, 922.94},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := results[tt.idx]
			if r.SizeBytes != tt.wantSize {
				t.Errorf("SizeBytes = %d, want %d", r.SizeBytes, tt.wantSize)
			}
			if r.AlgBW != tt.wantAlgBW {
				t.Errorf("AlgBW = %f, want %f", r.AlgBW, tt.wantAlgBW)
			}
			if r.BusBW != tt.wantBusBW {
				t.Errorf("BusBW = %f, want %f", r.BusBW, tt.wantBusBW)
			}
		})
	}
}

// TestParseBandwidthLogsNonUTCNode covers log lines from a node that is not
// set to UTC. Kubelet then writes an offset such as "-07:00" instead of "Z",
// which makes the timestamp prefix longer. These lines come from a real
// A100 run on a node in PDT.
func TestParseBandwidthLogsNonUTCNode(t *testing.T) {
	profile := &v1alpha1.LogProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "nccl-loopback"},
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: "2006-01-02T15:04:05.999999999Z"},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: `^\s*(?P<size>\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`,
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
		ObjectMeta: metav1.ObjectMeta{Name: "nccl-loopback"},
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: "2006-01-02T15:04:05.999999999Z"},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: `^\s*(?P<size>\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`,
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
		ObjectMeta: metav1.ObjectMeta{Name: "nccl-bandwidth"},
		Spec: v1alpha1.LogProfileSpec{
			Timestamp: v1alpha1.TimestampSpec{Layout: "2006-01-02T15:04:05.999999999Z"},
			Patterns: v1alpha1.LogPatternSet{
				BandwidthResult: &v1alpha1.EventPattern{
					Regex: `^\s*(?P<size>\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`,
				},
			},
		},
	}

	parser, err := NewParser(profile)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	results := parser.ParseBandwidthLogs(strings.Split(string(raw), "\n"))

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
}

func TestNewParserMissingPattern(t *testing.T) {
	profile := &v1alpha1.LogProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "empty"},
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

func TestStripK8sTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"with nanosecond timestamp",
			"2026-02-16T10:00:02.000000000Z            8             2     float",
			"           8             2     float",
		},
		{
			"with second timestamp",
			"2026-02-16T10:00:02Z            8             2     float",
			"           8             2     float",
		},
		{
			"without timestamp",
			"           8             2     float     sum      -1",
			"           8             2     float     sum      -1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripK8sTimestamp(tt.input)
			if got != tt.want {
				t.Errorf("stripK8sTimestamp(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
