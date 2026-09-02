// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// ──────────────────────────────────────────────────────────────────────────────
// Golden-file tests using testutil
// ──────────────────────────────────────────────────────────────────────────────

func TestDetect(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "detect",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		expression := strings.TrimSpace(tc.Inputs["input_expression.txt"])
		detector, err := NewDetector(expression)
		if err != nil {
			return err
		}
		var node corev1.Node
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_node.yaml"]), &node); err != nil {
			return err
		}
		result, err := detector.Detect(context.Background(), &node)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(map[string]any{
			"failed":   result.Failed,
			"nodeName": result.NodeName,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestDetectorName(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "detector-name",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		expression := strings.TrimSpace(tc.Inputs["input_expression.txt"])
		detector, err := NewDetector(expression)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(map[string]any{"name": detector.Name()}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}
