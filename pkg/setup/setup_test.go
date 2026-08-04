// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestConfigFlags builds a ConfigFlags with kubeconfig/context set, for
// tests that exercise functions taking *kubeconfig.ConfigFlags.
func newTestConfigFlags(kubeconfigPath, kubeContext string) *kubeconfig.ConfigFlags {
	cf := kubeconfig.NewConfigFlags(true)
	*cf.KubeConfig = kubeconfigPath
	*cf.Context = kubeContext
	return cf
}

func TestPromptForConfirmation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{name: "yes", input: "yes\n", expected: true},
		{name: "yes with spaces", input: "  yes  \n", expected: true},
		{name: "no", input: "no\n", expected: false},
		{name: "empty", input: "\n", expected: false},
		{name: "y only", input: "y\n", expected: false},
		{name: "YES uppercase", input: "YES\n", expected: false},
		{name: "Yes mixed case", input: "Yes\n", expected: false},
		{name: "eof no input", input: "", expected: false},
		{name: "random text", input: "approve\n", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			result := promptForConfirmation(strings.NewReader(tt.input), &out)
			assert.Equal(t, tt.expected, result)
			assert.Contains(t, out.String(), "Enter a value:")
		})
	}
}

func TestGetClusterInfo(t *testing.T) {
	// Write a minimal kubeconfig to a temp file.
	kubeconfigYAML := `
apiVersion: v1
kind: Config
current-context: test-context
contexts:
  - name: test-context
    context:
      cluster: test-cluster
      user: test-user
  - name: other-context
    context:
      cluster: other-cluster
      user: test-user
clusters:
  - name: test-cluster
    cluster:
      server: https://10.0.1.100:6443
  - name: other-cluster
    cluster:
      server: https://10.0.2.200:6443
users:
  - name: test-user
    user:
      token: fake-token
`
	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfigPath, []byte(kubeconfigYAML), 0o600))

	t.Run("default context", func(t *testing.T) {
		ctxName, serverURL, err := getClusterInfo(newTestConfigFlags(kubeconfigPath, ""))
		require.NoError(t, err)
		assert.Equal(t, "test-context", ctxName)
		assert.Equal(t, "https://10.0.1.100:6443", serverURL)
	})

	t.Run("explicit context override", func(t *testing.T) {
		ctxName, serverURL, err := getClusterInfo(newTestConfigFlags(kubeconfigPath, "other-context"))
		require.NoError(t, err)
		assert.Equal(t, "other-context", ctxName)
		assert.Equal(t, "https://10.0.2.200:6443", serverURL)
	})

	t.Run("nonexistent context", func(t *testing.T) {
		_, _, err := getClusterInfo(newTestConfigFlags(kubeconfigPath, "nonexistent"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in kubeconfig")
	})

	t.Run("nonexistent kubeconfig file", func(t *testing.T) {
		_, _, err := getClusterInfo(newTestConfigFlags("/nonexistent/kubeconfig", ""))
		require.Error(t, err)
	})
}

// ---------------------------------------------------------------------------
// Tests for phase parsing
// ---------------------------------------------------------------------------

func TestParseSkipPhases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]bool
	}{
		{name: "empty", input: "", expected: map[string]bool{}},
		{name: "single", input: "deps", expected: map[string]bool{"deps": true}},
		{name: "multiple", input: "deps,crds", expected: map[string]bool{"deps": true, "crds": true}},
		{name: "with spaces", input: " deps , crds ", expected: map[string]bool{"deps": true, "crds": true}},
		{name: "all four", input: "deps,crds,controller,logprofiles",
			expected: map[string]bool{"deps": true, "crds": true, "controller": true, "logprofiles": true}},
		{name: "trailing comma", input: "deps,", expected: map[string]bool{"deps": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSkipPhases(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ---------------------------------------------------------------------------
// Tests for ncrectl setup init/reset helpers
// ---------------------------------------------------------------------------

func TestParseImage(t *testing.T) {
	tests := []struct {
		input    string
		wantName string
		wantTag  string
	}{
		{
			input:    "ghcr.io/nvidia/cluster-readiness-engine/manager:v1.0.0",
			wantName: "ghcr.io/nvidia/cluster-readiness-engine/manager",
			wantTag:  "v1.0.0",
		},
		{
			input:    "myregistry.io/cluster-readiness-engine:latest",
			wantName: "myregistry.io/cluster-readiness-engine",
			wantTag:  "latest",
		},
		{
			input:    "localhost:5000/cluster-readiness-engine:dev",
			wantName: "localhost:5000/cluster-readiness-engine",
			wantTag:  "dev",
		},
		{
			input:    "ghcr.io/nvidia/repo@sha256:abc123def456",
			wantName: "ghcr.io/nvidia/repo",
			wantTag:  "sha256:abc123def456",
		},
		{
			input:    "ghcr.io/nvidia/cluster-readiness-engine/manager",
			wantName: "ghcr.io/nvidia/cluster-readiness-engine/manager",
			wantTag:  "latest",
		},
		{
			input:    "cluster-readiness-engine:v2",
			wantName: "cluster-readiness-engine",
			wantTag:  "v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			name, tag := parseImage(tt.input)
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantTag, tag)
		})
	}
}

func TestDefaultImage(t *testing.T) {
	img := defaultImage("dev")
	assert.Equal(t, defaultImageRegistry+"/"+defaultImageRepository+":"+"dev", img)
	assert.Contains(t, img, "ghcr.io/nvidia/cluster-readiness-engine/manager:")
}

// ---------------------------------------------------------------------------
// Tests for YAML helpers (split, decode, patch)
// ---------------------------------------------------------------------------

func TestSplitYAMLDocuments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantKind string // kind of first doc, if any
	}{
		{
			name:    "empty",
			input:   "",
			wantLen: 0,
		},
		{
			name:    "single doc",
			input:   "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: test",
			wantLen: 1,
		},
		{
			name: "two docs",
			input: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: one\n" +
				"---\n" +
				"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: two",
			wantLen: 2,
		},
		{
			name: "empty docs between separators",
			input: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: one\n" +
				"---\n\n---\n" +
				"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: two",
			wantLen: 2,
		},
		{
			name:    "trailing separator",
			input:   "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: one\n---\n",
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := splitYAMLDocuments([]byte(tt.input))
			assert.Len(t, docs, tt.wantLen)
		})
	}
}

func TestDecodeUnstructured(t *testing.T) {
	t.Run("valid namespace", func(t *testing.T) {
		doc := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: test-ns")
		obj, err := decodeUnstructured(doc)
		require.NoError(t, err)
		assert.Equal(t, "v1", obj.GetAPIVersion())
		assert.Equal(t, "Namespace", obj.GetKind())
		assert.Equal(t, "test-ns", obj.GetName())
	})

	t.Run("valid deployment", func(t *testing.T) {
		doc := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: my-deploy\n  namespace: my-ns")
		obj, err := decodeUnstructured(doc)
		require.NoError(t, err)
		assert.Equal(t, "apps/v1", obj.GetAPIVersion())
		assert.Equal(t, "Deployment", obj.GetKind())
		assert.Equal(t, "my-deploy", obj.GetName())
		assert.Equal(t, "my-ns", obj.GetNamespace())
	})

	t.Run("invalid yaml", func(t *testing.T) {
		doc := []byte("not: [valid: yaml:")
		_, err := decodeUnstructured(doc)
		require.Error(t, err)
	})
}

func TestNsLabel(t *testing.T) {
	assert.Equal(t, "", nsLabel(""))
	assert.Equal(t, "(namespace: cluster-readiness-engine)", nsLabel("cluster-readiness-engine"))
}
