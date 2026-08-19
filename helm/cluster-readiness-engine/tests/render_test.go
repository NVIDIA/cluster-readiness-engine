// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const helmTemplateTimeout = 30 * time.Second

func TestHelmTemplateRendersConcurrencyArgs(t *testing.T) {
	requireHelm(t)
	chartDir := chartDir(t)

	tests := []struct {
		name string
		set  []string
		want []string
	}{
		{
			name: "defaults",
			want: []string{
				"--max-concurrent-reconciles=10",
				"--measurement-max-concurrent-reconciles=5",
			},
		},
		{
			name: "overrides",
			set: []string{
				"manager.maxConcurrentReconciles=20",
				"manager.measurementMaxConcurrentReconciles=7",
			},
			want: []string{
				"--max-concurrent-reconciles=20",
				"--measurement-max-concurrent-reconciles=7",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := managerArgs(t, helmTemplate(t, chartDir, tt.set))
			for _, flag := range tt.want {
				assert.Truef(t, slices.Contains(args, flag), "manager args %q missing %q", args, flag)
			}
		})
	}
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
	require.True(t, ok, "runtime.Caller")
	return filepath.Clean(filepath.Join(filepath.Dir(filename), ".."))
}

func helmTemplate(t *testing.T, chartDir string, set []string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), helmTemplateTimeout)
	defer cancel()

	args := make([]string, 0, 3+2*len(set))
	args = append(args, "template", "render-test", chartDir)
	for _, value := range set {
		args = append(args, "--set", value)
	}

	cmd := exec.CommandContext(ctx, "helm", args...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "helm template failed:\n%s", out)
	return out
}

func managerArgs(t *testing.T, rendered []byte) []string {
	t.Helper()
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj)
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "decode helm template output")
		if obj.GetKind() != "Deployment" {
			continue
		}

		var dep appsv1.Deployment
		require.NoError(t, kruntime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &dep))
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == "manager" {
				return dep.Spec.Template.Spec.Containers[i].Args
			}
		}
	}
	t.Fatal("rendered chart has no manager Deployment container")
	return nil
}
