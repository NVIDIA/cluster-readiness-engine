// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	syaml "sigs.k8s.io/yaml"
)

const helmTemplateTimeout = 30 * time.Second

func TestHelmTemplateRendersConcurrencyArgs(t *testing.T) {
	requireHelm(t)
	chartDir := chartDir(t)
	requireChartInputs(t, chartDir)

	p := testutil.TestCaseParser{
		Subdir:         "render-concurrency-args",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Set []string `yaml:"set"`
		}
		if err := syaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		rendered, err := helmTemplate(chartDir, input.Set)
		if err != nil {
			return err
		}
		args, err := managerArgs(rendered)
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(args, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI but not on PATH; the test job must install helm (azure/setup-helm)")
		}
		t.Skip("helm not available; skipping chart render test")
	}
}

func chartDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "helm", "cluster-readiness-engine"))
}

// requireChartInputs reads the chart in process. Go's test cache cannot observe
// files read only by the Helm subprocess.
func requireChartInputs(t *testing.T, chartDir string) {
	t.Helper()
	err := filepath.WalkDir(chartDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if _, err := os.ReadFile(path); err != nil {
			return fmt.Errorf("read chart input %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read chart inputs: %v", err)
	}
}

func helmTemplate(chartDir string, set []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), helmTemplateTimeout)
	defer cancel()

	args := make([]string, 0, 3+2*len(set))
	args = append(args, "template", "render-test", chartDir)
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

func managerArgs(rendered []byte) ([]string, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode helm template output: %w", err)
		}
		if obj.GetKind() != "Deployment" {
			continue
		}

		var dep appsv1.Deployment
		if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &dep); err != nil {
			return nil, fmt.Errorf("convert manager Deployment: %w", err)
		}
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == "manager" {
				return dep.Spec.Template.Spec.Containers[i].Args, nil
			}
		}
	}
	return nil, fmt.Errorf("rendered chart has no manager Deployment container")
}
