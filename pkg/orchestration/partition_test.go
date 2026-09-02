// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"
)

func TestPartitionNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "partition",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input PartitionInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		groups, err := PartitionNodes(input)
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(groups, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestPartitionNodesErrors(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "partition-errors",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input PartitionInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		_, err := PartitionNodes(input)

		result := struct {
			Error string `json:"error"`
		}{}
		if err != nil {
			result.Error = err.Error()
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
