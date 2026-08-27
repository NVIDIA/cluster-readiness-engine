// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const testAPIGroup = "cre.nvidia.com"

func TestHumanSize(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "human-size",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Bytes int64 `yaml:"bytes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := humanSize(in.Bytes)

		b, err := json.MarshalIndent(struct {
			Bytes     int64  `json:"bytes"`
			HumanSize string `json:"humanSize"`
		}{in.Bytes, got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestParseFloat(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse-float",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Input string `yaml:"input"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := parseFloat(in.Input)

		b, err := json.MarshalIndent(struct {
			Input  string  `json:"input"`
			Parsed float64 `json:"parsed"`
		}{in.Input, got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestAvg(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "avg",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Vals []float64 `yaml:"vals"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := avg(in.Vals)

		b, err := json.MarshalIndent(struct {
			Vals []float64 `json:"vals"`
			Avg  float64   `json:"avg"`
		}{in.Vals, got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestFmtPercent(t *testing.T) {
	assert.Equal(t, "0.92 (92%)", fmtPercent(0.92))
	assert.Equal(t, "1.00 (100%)", fmtPercent(1.0))
	assert.Equal(t, "0.50 (50%)", fmtPercent(0.5))
}

func TestFmtDuration(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "fmt-duration",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Secs float64 `yaml:"secs"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := fmtDuration(in.Secs)

		b, err := json.MarshalIndent(struct {
			Secs      float64 `json:"secs"`
			Formatted string  `json:"formatted"`
		}{in.Secs, got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestFmtAvg(t *testing.T) {
	t.Run("empty returns empty", func(t *testing.T) {
		assert.Equal(t, "", fmtAvg(nil, fmtPercent))
	})

	t.Run("non-empty formats", func(t *testing.T) {
		result := fmtAvg([]float64{0.9, 0.95}, fmtPercent)
		assert.Contains(t, result, "92")
	})

	t.Run("all zeros returns empty", func(t *testing.T) {
		assert.Equal(t, "", fmtAvg([]float64{0, 0}, fmtPercent))
	})
}

func TestHasCondition(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: "Succeeded", Status: metav1.ConditionTrue},
		{Type: "Failed", Status: metav1.ConditionFalse},
	}

	t.Run("match true", func(t *testing.T) {
		assert.True(t, controller.CondIsTrue(conditions, "Succeeded"))
	})

	t.Run("exists but false", func(t *testing.T) {
		assert.False(t, controller.CondIsTrue(conditions, "Failed"))
	})

	t.Run("no match", func(t *testing.T) {
		assert.False(t, controller.CondIsTrue(conditions, "InProgress"))
	})

	t.Run("empty list", func(t *testing.T) {
		assert.False(t, controller.CondIsTrue(nil, "Succeeded"))
	})
}

func TestBuildDomainReports(t *testing.T) {
	apiGroup := testAPIGroup

	t.Run("multi-domain grouping", func(t *testing.T) {
		orch := &crev1alpha1.OrchestrationStatus{
			Groups: []crev1alpha1.GroupStatus{
				{
					Name:    "group-0",
					Nodes:   []string{"node-0", "node-1"},
					Domains: []string{"clique-0"},
					JobRef:  &crev1alpha1.WorkloadReference{Name: "job-0"},
				},
				{
					Name:    "group-1",
					Nodes:   []string{"node-2", "node-3"},
					Domains: []string{"clique-1"},
					JobRef:  &crev1alpha1.WorkloadReference{Name: "job-1"},
				},
			},
		}

		measurements := []crev1alpha1.GoodputMeasurement{
			{
				Spec: crev1alpha1.GoodputMeasurementSpec{
					JobRef: corev1.TypedLocalObjectReference{
						APIGroup: &apiGroup,
						Kind:     "Job",
						Name:     "job-0",
					},
				},
				Status: crev1alpha1.GoodputMeasurementStatus{
					Result:          "0.95",
					AvgTFLOPSPerGPU: "800.5",
					TrainingTimeSec: "120",
					AvgStepTimeSec:  "1.25",
				},
			},
			{
				Spec: crev1alpha1.GoodputMeasurementSpec{
					JobRef: corev1.TypedLocalObjectReference{
						APIGroup: &apiGroup,
						Kind:     "Job",
						Name:     "job-1",
					},
				},
				Status: crev1alpha1.GoodputMeasurementStatus{
					Result:          "0.90",
					AvgTFLOPSPerGPU: "750.0",
					TrainingTimeSec: "130",
					AvgStepTimeSec:  "1.30",
				},
			},
		}

		reports := buildDomainReports(orch, measurements)
		require.Len(t, reports, 2)

		byDomain := map[string]DomainReport{}
		for _, r := range reports {
			byDomain[r.Name] = r
		}

		clique0 := byDomain["clique-0"]
		assert.Equal(t, 2, clique0.NodeCount)
		assert.Contains(t, clique0.Goodput, "0.95")
		assert.Contains(t, clique0.TFLOPs, "800.5")

		clique1 := byDomain["clique-1"]
		assert.Equal(t, 2, clique1.NodeCount)
		assert.Contains(t, clique1.Goodput, "0.90")
	})

	t.Run("no-domain fallback", func(t *testing.T) {
		measurements := []crev1alpha1.GoodputMeasurement{
			{
				Spec: crev1alpha1.GoodputMeasurementSpec{
					JobRef: corev1.TypedLocalObjectReference{
						APIGroup: &apiGroup,
						Kind:     "Job",
						Name:     "job-x",
					},
				},
				Status: crev1alpha1.GoodputMeasurementStatus{
					Result:          "0.88",
					AvgTFLOPSPerGPU: "600",
				},
			},
		}

		reports := buildDomainReports(nil, measurements)
		require.Len(t, reports, 1)
		assert.Equal(t, "", reports[0].Name)
		assert.Contains(t, reports[0].Goodput, "0.88")
	})

	t.Run("empty measurements", func(t *testing.T) {
		orch := &crev1alpha1.OrchestrationStatus{
			Groups: []crev1alpha1.GroupStatus{
				{
					Name:    "group-0",
					Domains: []string{"clique-0"},
					JobRef:  &crev1alpha1.WorkloadReference{Name: "job-0"},
				},
			},
		}
		reports := buildDomainReports(orch, nil)
		assert.Empty(t, reports)
	})
}

func TestPrintReport(t *testing.T) {
	report := &CertReport{
		Name:       "test-cert",
		Platform:   "gcp",
		GPU:        "H100",
		TotalNodes: 8,
		Categories: []CategoryReport{
			{
				Domain:  "training",
				Variant: "nemotron",
				Status:  "Succeeded",
				Runtime: "2m 30s",
				Domains: []DomainReport{
					{
						Name:      "clique-0",
						NodeCount: 4,
						Goodput:   "0.95 (95%)",
						TFLOPs:    "800.5",

						StepTime: "1.25s",
					},
				},
			},
			{
				Domain:  "communication",
				Variant: "nccl-all-reduce",
				Status:  "Succeeded",
				Bandwidth: []BandwidthRow{
					{Size: "1 MB", AlgBW: "10.5 GB/s", BusBW: "9.8 GB/s", Samples: 100},
				},
			},
		},
		Result: "PASSED",
	}

	var buf bytes.Buffer
	Print(&buf, report)
	output := buf.String()

	// Check header box.
	assert.Contains(t, output, "Certification Report")
	assert.Contains(t, output, "╔")
	assert.Contains(t, output, "╗")

	// Check metadata.
	assert.Contains(t, output, "test-cert")
	assert.Contains(t, output, "gcp")
	assert.Contains(t, output, "H100")
	assert.Contains(t, output, "8")

	// Check category cards.
	assert.Contains(t, output, "training/nemotron")
	assert.Contains(t, output, "communication/nccl-all-reduce")
	assert.Contains(t, output, "Succeeded")

	// Check training metrics.
	assert.Contains(t, output, "Avg Runtime Goodput")
	assert.Contains(t, output, "0.95 (95%)")
	assert.Contains(t, output, "clique-0")

	// Check bandwidth table.
	assert.Contains(t, output, "Bandwidth:")
	assert.Contains(t, output, "1 MB")
	assert.Contains(t, output, "10.5 GB/s")

	// Check summary.
	assert.Contains(t, output, "Summary")
	assert.Contains(t, output, "2/2 passed")
	assert.Contains(t, output, "PASSED")
}

func TestPrintReportFailed(t *testing.T) {
	report := &CertReport{
		Name: "fail-cert",
		Categories: []CategoryReport{
			{Domain: "training", Variant: "v1", Status: "Failed"},
		},
		FailedNodes: []string{"node-1", "node-2"},
		Result:      "FAILED",
	}

	var buf bytes.Buffer
	Print(&buf, report)
	output := buf.String()

	assert.Contains(t, output, "FAILED")
	assert.Contains(t, output, "- node-1")
	assert.Contains(t, output, "- node-2")
	assert.Contains(t, output, "0/1 passed")
}

func TestPrintCategoryCardTraining(t *testing.T) {
	cat := &CategoryReport{
		Domain:  "training",
		Variant: "nemotron",
		Status:  "Succeeded",
		Runtime: "5m 0s",
		Domains: []DomainReport{
			{
				Name:      "clique-0",
				NodeCount: 4,
				Goodput:   "0.92 (92%)",
				TFLOPs:    "750.0",
				StepTime:  "1.10s",
			},
			{
				Name:      "clique-1",
				NodeCount: 4,
				Goodput:   "0.91 (91%)",
				TFLOPs:    "745.0",
				StepTime:  "1.12s",
			},
		},
	}

	var buf bytes.Buffer
	printCategoryCard(&buf, cat)
	output := buf.String()

	// Card structure.
	assert.Contains(t, output, "training/nemotron")
	assert.Contains(t, output, "Status:    Succeeded")
	assert.Contains(t, output, "Runtime:   5m 0s")

	// Domain sub-boxes.
	assert.Contains(t, output, "clique-0 (4 nodes)")
	assert.Contains(t, output, "clique-1 (4 nodes)")
	assert.Contains(t, output, "Avg Runtime Goodput")
	assert.Contains(t, output, "Avg TFLOPs/GPU")
	assert.NotContains(t, output, "Avg Train Time")
	assert.Contains(t, output, "Avg Step Time")
	assert.Contains(t, output, "┌")
	assert.Contains(t, output, "└")
}

func TestPrintCategoryCardCommunication(t *testing.T) {
	cat := &CategoryReport{
		Domain:  "communication",
		Variant: "nccl-all-reduce",
		Status:  "Succeeded",
		Bandwidth: []BandwidthRow{
			{Size: "1 KB", AlgBW: "0.5 GB/s", BusBW: "0.4 GB/s", Samples: 50},
			{Size: "1 MB", AlgBW: "10.5 GB/s", BusBW: "9.8 GB/s", Samples: 100},
		},
	}

	var buf bytes.Buffer
	printCategoryCard(&buf, cat)
	output := buf.String()

	assert.Contains(t, output, "communication/nccl-all-reduce")
	assert.Contains(t, output, "Bandwidth:")
	assert.Contains(t, output, "Size")
	assert.Contains(t, output, "AlgBW")
	assert.Contains(t, output, "BusBW")
	assert.Contains(t, output, "Samples")
	assert.Contains(t, output, "1 KB")
	assert.Contains(t, output, "1 MB")
}

func TestPrintCategoryCardNoTopology(t *testing.T) {
	cat := &CategoryReport{
		Domain:  "training",
		Variant: "v1",
		Status:  "Succeeded",
		Domains: []DomainReport{
			{
				Name:    "", // no topology
				Goodput: "0.90 (90%)",
				TFLOPs:  "700.0",
			},
		},
	}

	var buf bytes.Buffer
	printCategoryCard(&buf, cat)
	output := buf.String()

	// Flat metrics (no domain sub-box).
	assert.Contains(t, output, "Avg Runtime Goodput")
	assert.Contains(t, output, "0.90 (90%)")
	assert.NotContains(t, output, "┌ ")
}

// ---------------------------------------------------------------------------
// Tests for formatting helpers
// ---------------------------------------------------------------------------

func TestFmtFloat1(t *testing.T) {
	assert.Equal(t, "3.1", fmtFloat1(3.14))
	assert.Equal(t, "0.0", fmtFloat1(0.0))
	assert.Equal(t, "100.0", fmtFloat1(100.0))
	assert.Equal(t, "99.9", fmtFloat1(99.94))
}

func TestFmtFloat2(t *testing.T) {
	assert.Equal(t, "1.25s", fmtFloat2(1.25))
	assert.Equal(t, "0.00s", fmtFloat2(0.0))
	assert.Equal(t, "10.50s", fmtFloat2(10.5))
}

func TestCountDigits(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "count-digits",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			N int `yaml:"n"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := countDigits(in.N)

		b, err := json.MarshalIndent(struct {
			N      int `json:"n"`
			Digits int `json:"digits"`
		}{in.N, got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestPad(t *testing.T) {
	assert.Equal(t, "", pad(0))
	assert.Equal(t, "", pad(-1))
	assert.Equal(t, " ", pad(1))
	assert.Equal(t, "   ", pad(3))
}

func TestFailureReason(t *testing.T) {
	t.Run("has failed condition", func(t *testing.T) {
		conditions := []metav1.Condition{
			{Type: "InProgress", Status: metav1.ConditionFalse},
			{Type: "Failed", Status: metav1.ConditionTrue, Message: "workload timeout"},
		}
		assert.Equal(t, "workload timeout", failureReasonFromConditions(conditions))
	})

	t.Run("no failed condition", func(t *testing.T) {
		conditions := []metav1.Condition{
			{Type: "Succeeded", Status: metav1.ConditionTrue},
		}
		assert.Equal(t, "", failureReasonFromConditions(conditions))
	})

	t.Run("failed condition false", func(t *testing.T) {
		conditions := []metav1.Condition{
			{Type: "Failed", Status: metav1.ConditionFalse, Message: "old error"},
		}
		assert.Equal(t, "", failureReasonFromConditions(conditions))
	})

	t.Run("empty conditions", func(t *testing.T) {
		assert.Equal(t, "", failureReasonFromConditions(nil))
	})
}

// ---------------------------------------------------------------------------
// Tests for print helpers
// ---------------------------------------------------------------------------

func TestPrintDomainBox(t *testing.T) {
	d := &DomainReport{
		Name:      "clique-0",
		NodeCount: 4,
		Goodput:   "0.95 (95%)",
		TFLOPs:    "800.5",
		StepTime:  "1.25s",
	}

	var buf bytes.Buffer
	printDomainBox(&buf, "clique-0 (4 nodes)", d)
	output := buf.String()

	assert.Contains(t, output, "clique-0 (4 nodes)")
	assert.Contains(t, output, "Avg Runtime Goodput")
	assert.Contains(t, output, "0.95 (95%)")
	assert.Contains(t, output, "Avg TFLOPs/GPU")
	assert.Contains(t, output, "800.5")
	assert.NotContains(t, output, "Avg Train Time")
	assert.Contains(t, output, "Avg Step Time")
	assert.Contains(t, output, "1.25s")
	// Sub-box borders.
	assert.Contains(t, output, "┌")
	assert.Contains(t, output, "└")
}

func TestPrintDomainBoxEmptyMetrics(t *testing.T) {
	d := &DomainReport{
		Name: "clique-0",
		// All metrics empty — lines should be omitted.
	}

	var buf bytes.Buffer
	printDomainBox(&buf, "clique-0", d)
	output := buf.String()

	assert.Contains(t, output, "clique-0")
	assert.NotContains(t, output, "Avg Runtime Goodput")
	assert.NotContains(t, output, "Avg TFLOPs/GPU")
}

func TestPrintMetricsFlat(t *testing.T) {
	d := &DomainReport{
		Goodput: "0.90 (90%)",
		TFLOPs:  "700.0",
		// StepTime empty — omitted.
	}

	var buf bytes.Buffer
	printMetricsFlat(&buf, d)
	output := buf.String()

	assert.Contains(t, output, "Avg Runtime Goodput")
	assert.Contains(t, output, "0.90 (90%)")
	assert.Contains(t, output, "Avg TFLOPs/GPU")
	assert.Contains(t, output, "700.0")
	assert.NotContains(t, output, "Avg Train Time")
	assert.NotContains(t, output, "Avg Step Time")
}

func TestPrintMetricLine(t *testing.T) {
	t.Run("non-empty value", func(t *testing.T) {
		var buf bytes.Buffer
		printMetricLine(&buf, "Goodput", "0.95")
		output := buf.String()
		assert.Contains(t, output, "Goodput")
		assert.Contains(t, output, "0.95")
		assert.Contains(t, output, "│")
	})

	t.Run("empty value is omitted", func(t *testing.T) {
		var buf bytes.Buffer
		printMetricLine(&buf, "Goodput", "")
		assert.Empty(t, buf.String())
	})
}

func TestPrintCategoryCardWithFailureReason(t *testing.T) {
	cat := &CategoryReport{
		Domain:        "training",
		Variant:       "nemotron",
		Status:        "Failed",
		FailureReason: "workload timed out after 30m",
		Runtime:       "30m 0s",
	}

	var buf bytes.Buffer
	printCategoryCard(&buf, cat)
	output := buf.String()

	assert.Contains(t, output, "training/nemotron")
	assert.Contains(t, output, "Status:    Failed")
	assert.Contains(t, output, "Reason:    workload timed out after 30m")
	assert.Contains(t, output, "Runtime:   30m 0s")
}

func TestPrintCategoryCardLongReasonTruncated(t *testing.T) {
	longReason := strings.Repeat("x", 100)
	cat := &CategoryReport{
		Domain:        "training",
		Variant:       "v1",
		Status:        "Failed",
		FailureReason: longReason,
	}

	var buf bytes.Buffer
	printCategoryCard(&buf, cat)
	output := buf.String()

	// The long reason should be truncated with "..."
	assert.Contains(t, output, "...")
	// The reason line specifically should not contain the full 100-char string.
	assert.NotContains(t, output, longReason)
}

func TestPrintReportMinimal(t *testing.T) {
	// No platform, GPU, or nodes — just the bare minimum.
	report := &CertReport{
		Name: "minimal-cert",
		Categories: []CategoryReport{
			{Domain: "communication", Variant: "nccl-loopback", Status: "Succeeded"},
		},
		Result: "PASSED",
	}

	var buf bytes.Buffer
	Print(&buf, report)
	output := buf.String()

	assert.Contains(t, output, "Certification Report")
	assert.Contains(t, output, "minimal-cert")
	// Metadata lines should be absent when fields are empty.
	assert.NotContains(t, output, "Platform:")
	assert.NotContains(t, output, "GPU:")
	// "Nodes:" as a metadata header (not "Failed Nodes:" in summary).
	assert.NotContains(t, output, "  Nodes:")
	assert.Contains(t, output, "communication/nccl-loopback")
	assert.Contains(t, output, "1/1 passed")
	assert.Contains(t, output, "PASSED")
}

func TestPrintReportMultipleCategoriesMixed(t *testing.T) {
	report := &CertReport{
		Name:       "mixed-cert",
		Platform:   "aws",
		GPU:        "gb200",
		TotalNodes: 16,
		Categories: []CategoryReport{
			{Domain: "communication", Variant: "nccl-all-reduce", Status: "Succeeded"},
			{Domain: "training", Variant: "nemotron5-8b", Status: "Failed",
				FailureReason: "hardware failure detected"},
			{Domain: "communication", Variant: "nccl-loopback", Status: "Succeeded"},
		},
		FailedNodes: []string{"node-5"},
		Result:      "FAILED",
	}

	var buf bytes.Buffer
	Print(&buf, report)
	output := buf.String()

	assert.Contains(t, output, "mixed-cert")
	assert.Contains(t, output, "aws")
	assert.Contains(t, output, "gb200")
	assert.Contains(t, output, "16")
	assert.Contains(t, output, "communication/nccl-all-reduce")
	assert.Contains(t, output, "training/nemotron5-8b")
	assert.Contains(t, output, "communication/nccl-loopback")
	assert.Contains(t, output, "hardware failure detected")
	assert.Contains(t, output, "2/3 passed")
	assert.Contains(t, output, "node-5")
	assert.Contains(t, output, "FAILED")
}

// ---------------------------------------------------------------------------
// Box drawing helper tests
// ---------------------------------------------------------------------------

func TestBoxDrawingHelpers(t *testing.T) {
	t.Run("printBoxTop", func(t *testing.T) {
		var buf bytes.Buffer
		printBoxTop(&buf)
		assert.Contains(t, buf.String(), "╔")
		assert.Contains(t, buf.String(), "╗")
	})

	t.Run("printBoxBottom", func(t *testing.T) {
		var buf bytes.Buffer
		printBoxBottom(&buf)
		assert.Contains(t, buf.String(), "╚")
		assert.Contains(t, buf.String(), "╝")
	})

	t.Run("printBoxCenter", func(t *testing.T) {
		var buf bytes.Buffer
		printBoxCenter(&buf, "Test")
		output := buf.String()
		assert.Contains(t, output, "║")
		assert.Contains(t, output, "Test")
	})

	t.Run("printCardTop", func(t *testing.T) {
		var buf bytes.Buffer
		printCardTop(&buf)
		assert.Contains(t, buf.String(), "┌")
		assert.Contains(t, buf.String(), "┐")
	})

	t.Run("printCardBottom", func(t *testing.T) {
		var buf bytes.Buffer
		printCardBottom(&buf)
		assert.Contains(t, buf.String(), "└")
		assert.Contains(t, buf.String(), "┘")
	})

	t.Run("printCardSep", func(t *testing.T) {
		var buf bytes.Buffer
		printCardSep(&buf)
		assert.Contains(t, buf.String(), "├")
		assert.Contains(t, buf.String(), "┤")
	})

	t.Run("printCardTitle", func(t *testing.T) {
		var buf bytes.Buffer
		printCardTitle(&buf, "MyTitle")
		output := buf.String()
		assert.Contains(t, output, "MyTitle")
		assert.Contains(t, output, "│")
	})
}

func TestBuildDomainReportsPartialMetrics(t *testing.T) {
	apiGroup := testAPIGroup

	// Only goodput set, other metrics are empty.
	measurements := []crev1alpha1.GoodputMeasurement{
		{
			Spec: crev1alpha1.GoodputMeasurementSpec{
				JobRef: corev1.TypedLocalObjectReference{
					APIGroup: &apiGroup,
					Kind:     "Job",
					Name:     "job-0",
				},
			},
			Status: crev1alpha1.GoodputMeasurementStatus{
				Result: "0.85",
				// All other fields empty.
			},
		},
	}

	reports := buildDomainReports(nil, measurements)
	require.Len(t, reports, 1)
	assert.Contains(t, reports[0].Goodput, "0.85")
	assert.Equal(t, "", reports[0].TFLOPs)
	assert.Equal(t, "", reports[0].StepTime)
}

func TestBuildCliqueReport(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-clique-report",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Groups []struct {
				Name             string         `yaml:"name"`
				Nodes            []string       `yaml:"nodes"`
				Domains          []string       `yaml:"domains"`
				DomainNodeCounts map[string]int `yaml:"domainNodeCounts"`
			} `yaml:"groups"`
			FailedNodes      []string `yaml:"failedNodes"`
			NilOrchestration bool     `yaml:"nilOrchestration"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		wf := &crev1alpha1.Workflow{}
		if !input.NilOrchestration {
			orch := &crev1alpha1.OrchestrationStatus{}
			for _, g := range input.Groups {
				orch.Groups = append(orch.Groups, crev1alpha1.GroupStatus{
					Name:             g.Name,
					Nodes:            g.Nodes,
					Domains:          g.Domains,
					DomainNodeCounts: g.DomainNodeCounts,
				})
			}
			wf.Status.Orchestration = orch
		}
		var failedNodes []crev1alpha1.FailedNode
		for _, n := range input.FailedNodes {
			failedNodes = append(failedNodes,
				crev1alpha1.FailedNode{Name: n, Reason: "WorkloadFailed"})
		}

		reports := buildCliqueReport(wf, failedNodes)
		if reports == nil {
			reports = []CliqueReport{}
		}

		data, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDetectTestScale(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-test-scale",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Topology *struct {
				TopologyKey  string `yaml:"topologyKey"`
				StrictDomain bool   `yaml:"strictDomain"`
			} `yaml:"topology"`
			NodesPerJob        int    `yaml:"nodesPerJob"`
			RequestedTestScale string `yaml:"requestedTestScale"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		wf := &crev1alpha1.Workflow{}
		if input.RequestedTestScale != "" {
			wf.Annotations = map[string]string{
				annotationRequestedTestScale: input.RequestedTestScale,
			}
		}
		if input.Topology != nil {
			wf.Spec.Orchestration.Topology = &crev1alpha1.TopologySpec{
				TopologyKey:  input.Topology.TopologyKey,
				StrictDomain: input.Topology.StrictDomain,
			}
		}
		if input.NodesPerJob > 0 {
			wf.Status.Orchestration = &crev1alpha1.OrchestrationStatus{
				NodesPerJob: input.NodesPerJob,
			}
		}

		result := detectTestScale(wf)

		data, err := json.MarshalIndent(struct {
			TestScale string `json:"testScale"`
		}{TestScale: result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestBuildDomainReportsMultipleMeasurementsSameDomain(t *testing.T) {
	apiGroup := testAPIGroup

	orch := &crev1alpha1.OrchestrationStatus{
		Groups: []crev1alpha1.GroupStatus{
			{
				Name:    "group-0",
				Nodes:   []string{"node-0", "node-1"},
				Domains: []string{"rack-a"},
				JobRef:  &crev1alpha1.WorkloadReference{Name: "job-0"},
			},
			{
				Name:    "group-1",
				Nodes:   []string{"node-2", "node-3"},
				Domains: []string{"rack-a"}, // Same domain.
				JobRef:  &crev1alpha1.WorkloadReference{Name: "job-1"},
			},
		},
	}

	measurements := []crev1alpha1.GoodputMeasurement{
		{
			Spec: crev1alpha1.GoodputMeasurementSpec{
				JobRef: corev1.TypedLocalObjectReference{
					APIGroup: &apiGroup, Kind: "Job", Name: "job-0",
				},
			},
			Status: crev1alpha1.GoodputMeasurementStatus{
				Result: "0.90", AvgTFLOPSPerGPU: "800",
			},
		},
		{
			Spec: crev1alpha1.GoodputMeasurementSpec{
				JobRef: corev1.TypedLocalObjectReference{
					APIGroup: &apiGroup, Kind: "Job", Name: "job-1",
				},
			},
			Status: crev1alpha1.GoodputMeasurementStatus{
				Result: "0.80", AvgTFLOPSPerGPU: "700",
			},
		},
	}

	reports := buildDomainReports(orch, measurements)
	// Both measurements map to "rack-a" → one report with averaged values.
	require.Len(t, reports, 1)
	assert.Equal(t, "rack-a", reports[0].Name)
	// Avg goodput: (0.90+0.80)/2 = 0.85.
	assert.Contains(t, reports[0].Goodput, "0.85")
	// Avg TFLOPs: (800+700)/2 = 750.
	assert.Contains(t, reports[0].TFLOPs, "750")
}

func TestPeakBandwidthResult(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		assert.Nil(t, peakBandwidthResult(nil))
		assert.Nil(t, peakBandwidthResult([]crev1alpha1.BandwidthResult{}))
	})

	t.Run("ascending — last is peak", func(t *testing.T) {
		results := []crev1alpha1.BandwidthResult{
			{SizeBytes: 1024, BusBW: "1.0"},
			{SizeBytes: 1048576, BusBW: "100.0"},
			{SizeBytes: 17179869184, BusBW: "350.0"},
		}
		peak := peakBandwidthResult(results)
		require.NotNil(t, peak)
		assert.Equal(t, int64(17179869184), peak.SizeBytes)
		assert.Equal(t, "350.0", peak.BusBW)
	})

	t.Run("unsorted — picks largest size, not last", func(t *testing.T) {
		results := []crev1alpha1.BandwidthResult{
			{SizeBytes: 17179869184, BusBW: "350.0"},
			{SizeBytes: 1024, BusBW: "1.0"},
			{SizeBytes: 1048576, BusBW: "100.0"},
		}
		peak := peakBandwidthResult(results)
		require.NotNil(t, peak)
		assert.Equal(t, int64(17179869184), peak.SizeBytes)
		assert.Equal(t, "350.0", peak.BusBW)
	})

	t.Run("single entry", func(t *testing.T) {
		results := []crev1alpha1.BandwidthResult{{SizeBytes: 8, BusBW: "0.1"}}
		peak := peakBandwidthResult(results)
		require.NotNil(t, peak)
		assert.Equal(t, int64(8), peak.SizeBytes)
	})
}

func TestBuildGroupBandwidthRowsUnsorted(t *testing.T) {
	apiGroup := testAPIGroup
	orch := &crev1alpha1.OrchestrationStatus{
		Groups: []crev1alpha1.GroupStatus{
			{
				Name:    "group-0",
				Nodes:   []string{"node-0", "node-1"},
				Domains: []string{"clique-0"},
				JobRef:  &crev1alpha1.WorkloadReference{Name: "job-0"},
			},
		},
	}
	measurements := []crev1alpha1.BandwidthMeasurement{
		{
			Spec: crev1alpha1.BandwidthMeasurementSpec{
				JobRef: corev1.TypedLocalObjectReference{
					APIGroup: &apiGroup, Kind: "Job", Name: "job-0",
				},
			},
			Status: crev1alpha1.BandwidthMeasurementStatus{
				Results: []crev1alpha1.BandwidthResult{
					// Largest size first — last entry is NOT the peak.
					{SizeBytes: 17179869184, BusBW: "350.0"},
					{SizeBytes: 1024, BusBW: "1.0"},
				},
			},
		},
	}

	rows := buildGroupBandwidthRows(orch, measurements, "")
	require.Len(t, rows, 1)
	assert.Equal(t, "350.0 GB/s", rows[0].BusBW)
}

// fmtDuration formats seconds as "Xm Xs" or "Xs".
func fmtDuration(v float64) string {
	secs := int64(v)
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm %ds", secs/60, secs%60)
}
