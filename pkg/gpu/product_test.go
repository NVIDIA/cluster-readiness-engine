// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

func TestParseProduct(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse-product",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Input string `yaml:"input"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got := ParseProduct(in.Input)

		b, err := json.MarshalIndent(struct {
			Input string `json:"input"`
			Want  string `json:"want"`
		}{in.Input, got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
