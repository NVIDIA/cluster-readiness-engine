// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"
)

func TestRender(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `yaml:"platform"`
			GPUArch  string `yaml:"gpuArch"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_config.yaml"]), &cfg); err != nil {
			return err
		}

		workflowPath := filepath.Join(t.TempDir(), "workflow.yaml")
		if err := os.WriteFile(workflowPath, []byte(tc.Inputs["input_workflow.yaml"]), 0o644); err != nil {
			return err
		}

		var nodesPath string
		if nodesData, ok := tc.Inputs["input_nodes.yaml"]; ok {
			nodesPath = filepath.Join(t.TempDir(), "nodes.yaml")
			if err := os.WriteFile(nodesPath, []byte(nodesData), 0o644); err != nil {
				return err
			}
		}

		workflow, meta, err := render(workflowPath, cfg.Platform, cfg.GPUArch, nodesPath)
		if err != nil {
			return err
		}

		// Add render metadata as annotations (same as CLI output).
		SetRenderAnnotations(workflow, meta)

		data, err := json.MarshalIndent(workflow, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestRenderErrors(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render-errors",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform     string `yaml:"platform"`
			GPUArch      string `yaml:"gpuArch"`
			WorkflowFile string `yaml:"workflowFile"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_config.yaml"]), &cfg); err != nil {
			return err
		}

		workflowPath := cfg.WorkflowFile
		if workflowData, ok := tc.Inputs["input_workflow.yaml"]; ok {
			workflowPath = filepath.Join(t.TempDir(), "workflow.yaml")
			if err := os.WriteFile(workflowPath, []byte(workflowData), 0o644); err != nil {
				return err
			}
		}

		_, _, err := render(workflowPath, cfg.Platform, cfg.GPUArch, "")

		type result struct {
			Error string `json:"error"`
		}
		var r result
		if err != nil {
			r.Error = err.Error()
		}

		data, marshalErr := json.MarshalIndent(r, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestValidateFlags(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "validate-flags",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			DryRun    bool   `yaml:"dryRun"`
			NodesFile string `yaml:"nodesFile"`
			Platform  string `yaml:"platform"`
			GPUArch   string `yaml:"gpuArch"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_config.yaml"]), &cfg); err != nil {
			return err
		}

		err := validateFlags(cfg.DryRun, cfg.NodesFile, cfg.Platform, cfg.GPUArch)

		type result struct {
			Error string `json:"error"`
		}
		var r result
		if err != nil {
			r.Error = err.Error()
		}

		data, marshalErr := json.MarshalIndent(r, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestListAvailable(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "list-available",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		avail := listAvailable()

		data, err := json.MarshalIndent(avail, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
