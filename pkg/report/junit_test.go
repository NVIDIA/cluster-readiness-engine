// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

func TestWriteJUnit(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "junit", ExpectedSuffix: ".xml"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var reports []*CertReport
		if err := json.Unmarshal([]byte(tc.Inputs["input.json"]), &reports); err != nil {
			return err
		}
		path := filepath.Join(tc.T.TempDir(), "results.xml")
		if err := WriteJUnit(path, reports); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tc.Actual = string(data)
		return nil
	})
}
