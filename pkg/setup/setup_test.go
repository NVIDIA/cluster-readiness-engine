// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
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
	p := testutil.TestCaseParser{
		Subdir:         "prompt-for-confirmation",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		input := tc.Inputs["input.txt"]

		var out bytes.Buffer
		result := promptForConfirmation(strings.NewReader(input), &out)
		if !strings.Contains(out.String(), "Enter a value:") {
			return fmt.Errorf("prompt output missing %q: got %q", "Enter a value:", out.String())
		}

		b, err := json.MarshalIndent(struct {
			Input     string `json:"input"`
			Confirmed bool   `json:"confirmed"`
		}{input, result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
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

// TestPhaseSummaryMatchesGatedPhases pins the confirmation summary against
// each command's accepted --skip-phases set. A summary listing a phase the
// command does not gate is how `--skip-phases=helm` became a silent no-op
// that changed only the printed output, and nothing caught it.
func TestPhaseSummaryMatchesGatedPhases(t *testing.T) {
	summaryNames := func(phases []phaseInfo) []string {
		names := make([]string, 0, len(phases))
		for _, phase := range phases {
			names = append(names, phase.name)
			assert.NotEmpty(t, phase.description, "phase %s has no description", phase.name)
		}
		sort.Strings(names)
		return names
	}
	sorted := func(in []string) []string {
		out := append([]string{}, in...)
		sort.Strings(out)
		return out
	}

	assert.Equal(t, sorted(initSkipPhases), summaryNames(initPhaseSummary("trainer-ref", "chart-ref", "image")),
		"init summary and its --skip-phases set disagree")
	assert.Equal(t, sorted(resetSkipPhases), summaryNames(resetPhaseSummary()),
		"reset summary and its --skip-phases set disagree")
}

// TestPrintPhaseListMarksExactlySkippedPhases covers the (skipped) branch,
// which no test reached: RunInit and RunReset are never invoked here and the
// UAT always passes --auto-approve.
func TestPrintPhaseListMarksExactlySkippedPhases(t *testing.T) {
	skip := map[string]bool{phaseHelm: true}
	var out bytes.Buffer
	printPhaseList(&out, skip, resetPhaseSummary())

	for line := range strings.SplitSeq(strings.TrimRight(out.String(), "\n"), "\n") {
		require.True(t, strings.HasPrefix(line, "    - ["), "unexpected summary line %q", line)
		name := strings.TrimSpace(line[strings.Index(line, "[")+1 : strings.Index(line, "]")])
		assert.Equal(t, skip[name], strings.HasSuffix(line, "(skipped)"),
			"phase %s skip marking disagrees with the skip map", name)
	}
	assert.Contains(t, out.String(), "(skipped)", "the skipped branch was not exercised")
}

func TestParseSkipPhases(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse-skip-phases",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Input   string   `yaml:"input"`
			Allowed []string `yaml:"allowed"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		if len(in.Allowed) == 0 {
			in.Allowed = []string{phaseCR, phaseHelm, phaseDeps}
		}
		result, parseErr := parseSkipPhases(in.Input, in.Allowed...)

		b, err := json.MarshalIndent(struct {
			Phases map[string]bool `json:"phases,omitempty"`
			Error  string          `json:"error,omitempty"`
		}{Phases: result, Error: errorString(parseErr)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// Tests for nvcrectl setup init/reset helpers
// ---------------------------------------------------------------------------

func TestParseImage(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse-image",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Input string `yaml:"input"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		name, tag := parseImage(in.Input)

		b, err := json.MarshalIndent(struct {
			Name string `json:"name"`
			Tag  string `json:"tag"`
		}{name, tag}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestDefaultImage(t *testing.T) {
	img := defaultImage("dev")
	assert.Equal(t, defaultImageRegistry+"/"+defaultImageRepository+":"+"dev", img)
	assert.Contains(t, img, "ghcr.io/nvidia/cluster-readiness-engine/manager:")
}

func TestInitCommandChartRefFlagDefaults(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "init-chart-ref-flags",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Flags []string `yaml:"flags"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		cmd := newInitCommand("v0.0.0-test")
		defaults := make(map[string]string, len(in.Flags))
		for _, name := range in.Flags {
			f := cmd.Flags().Lookup(name)
			if f == nil {
				return fmt.Errorf("init has no --%s flag", name)
			}
			defaults[name] = f.DefValue
		}

		b, err := json.MarshalIndent(defaults, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// ---------------------------------------------------------------------------
// Tests for YAML helpers (split, decode, patch)
// ---------------------------------------------------------------------------

func TestSplitYAMLDocuments(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "split-yaml-documents",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		docs := splitYAMLDocuments([]byte(tc.Inputs["input.yaml"]))

		b, err := json.MarshalIndent(struct {
			Count int `json:"count"`
		}{len(docs)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
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
	assert.Equal(t, "(namespace: nvcre)", nsLabel("nvcre"))
}

// nsLabel returns a formatted namespace label or empty string for cluster-scoped resources.
func nsLabel(ns string) string {
	if ns == "" {
		return ""
	}
	return "(namespace: " + ns + ")"
}

// splitYAMLDocuments splits a multi-document YAML byte slice into individual non-empty documents.
func splitYAMLDocuments(data []byte) [][]byte {
	var docs [][]byte
	for doc := range bytes.SplitSeq(data, []byte("\n---")) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

// decodeUnstructured decodes a single YAML document into an Unstructured object.
func decodeUnstructured(doc []byte) (*unstructured.Unstructured, error) {
	jsonData, err := sigsyaml.YAMLToJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("convert YAML to JSON: %w", err)
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(jsonData); err != nil {
		return nil, fmt.Errorf("unmarshal JSON: %w", err)
	}
	return obj, nil
}
