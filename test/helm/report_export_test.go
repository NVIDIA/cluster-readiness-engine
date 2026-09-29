// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	syaml "sigs.k8s.io/yaml"
)

// TestHelmReportExport pins the independent deployment and permission boundary:
// Secret reads are namespaced GETs, source access is read-only, and enabling
// registration connects the manager to the reporting namespace explicitly.
func TestHelmReportExport(t *testing.T) {
	const exporterChart = "exporter"
	requireHelm(t)
	coreDir := chartDir(t)
	exporterDir := filepath.Join(filepath.Dir(coreDir), "report-exporter")
	requireChartInputs(t, coreDir)
	requireChartInputs(t, exporterDir)

	p := testutil.TestCaseParser{
		Subdir:         "report-export",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Chart     string   `yaml:"chart"`
			Namespace string   `yaml:"namespace"`
			Set       []string `yaml:"set"`
		}
		if err := syaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		dir := coreDir
		if input.Chart == exporterChart {
			dir = exporterDir
		}
		rendered, err := reportExportTemplate(dir, input.Namespace, input.Set)
		if err != nil {
			return err
		}
		var result struct {
			ManagerArgs []string         `json:"managerArgs,omitempty"`
			Resources   []map[string]any `json:"resources"`
		}
		result.Resources = []map[string]any{}
		if input.Chart != exporterChart {
			result.ManagerArgs, err = managerArgs(rendered)
			if err != nil {
				return err
			}
		}
		dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
		for {
			var obj unstructured.Unstructured
			if err := dec.Decode(&obj); err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("decode report export chart: %w", err)
			}
			if obj.GetKind() == "" || input.Chart != exporterChart && !strings.HasSuffix(obj.GetName(), "-report-registration") {
				continue
			}
			// Labels change with packaging; keep ownership-relevant names,
			// namespaces, RBAC and the complete Deployment pod specification.
			unstructured.RemoveNestedField(obj.Object, "metadata", "labels")
			unstructured.RemoveNestedField(obj.Object, "spec", "template", "metadata", "labels", "helm.sh/chart")
			result.Resources = append(result.Resources, obj.Object)
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func reportExportTemplate(dir, namespace string, set []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), helmTemplateTimeout)
	defer cancel()
	args := make([]string, 0, 5+2*len(set))
	args = append(args, "template", "report-test", dir, "--namespace", namespace)
	for _, value := range set {
		args = append(args, "--set", value)
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("helm template failed: %w: %s", err, stderr.String())
	}
	return out, nil
}
