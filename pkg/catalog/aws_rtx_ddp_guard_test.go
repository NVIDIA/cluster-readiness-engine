// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestAWSRTXDDPGuard pins the script's acceptance boundary, rather than its
// rendered text: unrelated or misplaced waits must never qualify a source.
func TestAWSRTXDDPGuard(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	script := awsRTXDDPGuardScript(t)
	fixture, err := os.ReadFile("testdata/aws-rtx-ddp-guard/get_model.py")
	require.NoError(t, err)
	known := string(fixture)
	pre := "        ddp_stream.wait_stream(current_stream)\n"
	post := "        current_stream.wait_stream(ddp_stream)\n"
	require.Equal(t, 1, strings.Count(known, pre))
	require.Equal(t, 1, strings.Count(known, post))
	unfixed := strings.ReplaceAll(strings.ReplaceAll(known, pre, ""), post, "")
	elsewhere := "\ndef unrelated():\n    ddp_stream.wait_stream(current_stream)\n    current_stream.wait_stream(ddp_stream)\n"

	for _, tc := range []struct {
		name   string
		source string
		accept bool
	}{
		{"inspected-function", known, true},
		{"comments-and-whitespace", strings.Replace(known, pre, "\n        # ordering\n"+pre, 1), true},
		{"custom-code-outside-function", known + "\ncustom_option = True\n", true},
		{"waits-in-unrelated-function", unfixed + elsewhere, false},
		{"waits-after-copies", strings.Replace(strings.Replace(known, pre, "", 1), "    return model", "    ddp_stream.wait_stream(current_stream)\n    return model", 1), false},
		{"conditional-wait", strings.Replace(known, pre, "        if False:\n    "+pre, 1), false},
		{"missing-pre-wait", strings.Replace(known, pre, "", 1), false},
		{"missing-post-wait", strings.Replace(known, post, "", 1), false},
		{"waits-only-in-comments", unfixed + "\n# ddp_stream.wait_stream(current_stream)\n# current_stream.wait_stream(ddp_stream)\n", false},
		{"duplicate-functions", known + "\n" + known, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "training.py")
			require.NoError(t, os.WriteFile(path, []byte(tc.source), 0o600))
			code := awsRTXDDPGuardForPath(script, path)
			require.NotEqual(t, script, code)
			output, runErr := exec.Command(python, "-c", code).CombinedOutput()
			if tc.accept {
				require.NoError(t, runErr, "%s", output)
			} else {
				require.Error(t, runErr, "%s", output)
				require.Contains(t, string(output), "requires the inspected core_v0.15.2 source")
			}
			actual, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, tc.source, string(actual), "accepted and rejected alternatives stay unchanged")
		})
	}
}

func awsRTXDDPGuardScript(t *testing.T) string {
	t.Helper()
	rendered, err := makeLibTemplate(TemplateFuncsWithLib())(
		"deps/aws-rtxpro6000-training-image.yaml", TemplateData{EntryName: "test"})
	require.NoError(t, err)
	var deps []map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(rendered), &deps))
	jobs, found, err := unstructured.NestedSlice(deps[0], "spec", "template", "spec", "replicatedJobs")
	require.NoError(t, err)
	require.True(t, found)
	containers, found, err := unstructured.NestedSlice(jobs[0].(map[string]any),
		"template", "spec", "template", "spec", "initContainers")
	require.NoError(t, err)
	require.True(t, found)
	var script string
	for _, raw := range containers {
		container := raw.(map[string]any)
		if container["name"] == "megatron-ddp-stream-order" {
			script = container["args"].([]any)[0].(string)
		}
	}
	require.NotEmpty(t, script)
	return script
}

func awsRTXDDPGuardForPath(script, path string) string {
	return strings.NewReplacer(
		`"/mnt/workspace/megatron-lm/megatron/training/training.py"`, strconv.Quote(path),
		`"/mnt/workspace/.megatron-lm.lock"`, strconv.Quote(filepath.Join(filepath.Dir(path), ".megatron-lm.lock")),
	).Replace(script)
}
