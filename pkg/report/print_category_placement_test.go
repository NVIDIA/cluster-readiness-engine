// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestPrintCategoryPlacement records the scope lines of a category card
// together: Scale, Nodes/Job, Jobs and Placement.
//
// Their mutual consistency is what a reader actually checks, and no single line
// is wrong on its own. Under Pinned they reconcile without help, because Jobs
// times Nodes/Job equals the target node count. Unpinned breaks that identity
// on purpose: one job of 2 runs against a target of 18, and the other 16 nodes
// are untested by design. Nothing in the three older lines says so, which is
// why the Placement line carries both numbers rather than just the mode.
//
// Whole-card goldens rather than assertions on substrings, so a line that
// appears in the wrong order, loses its padding, or shows up when it should be
// absent is visible. The cards are fixed width and the padding is computed from
// the text, so a longer line is a layout change worth seeing.
func TestPrintCategoryPlacement(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "print-category",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cat CategoryReport
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cat); err != nil {
			return err
		}

		var buf bytes.Buffer
		printCategoryCard(&buf, &cat)
		tc.Actual = buf.String()
		return nil
	})
}
