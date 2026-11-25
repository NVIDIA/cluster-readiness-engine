// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const awsB300DDPGuardContainer = "megatron-ddp-stream-order"

const awsB300TrainingImage = "nvcr.io/nvidia/pytorch:26.04-py3@sha256:be06a21bd95a46bce1a5cfc0576051a40209f328440edaa2ba5cd35abf85ca1a"

func renderAWSB300TrainingRuntime(t *testing.T, file string) map[string]any {
	t.Helper()
	rendered, err := makeLibTemplate(TemplateFuncsWithLib())(file, TemplateData{EntryName: "nemotron5-8b", GpusPerNode: 8, NodesPerJob: 2})
	require.NoError(t, err)
	var deps []map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(rendered), &deps))
	require.Len(t, deps, 1)
	jobs, found, err := unstructured.NestedSlice(deps[0], "spec", "template", "spec", "replicatedJobs")
	require.NoError(t, err)
	require.True(t, found)
	podSpec, found, err := unstructured.NestedMap(jobs[0].(map[string]any), "template", "spec", "template", "spec")
	require.NoError(t, err)
	require.True(t, found)
	return podSpec
}

// The B300 training fragment requests 16 EFA devices and the entry's GPU count.
func TestAWSB300TrainingRequestsSixteenEFA(t *testing.T) {
	podSpec := renderAWSB300TrainingRuntime(t, "deps/aws-b300-efa-resources-training.yaml")
	containers, found, err := unstructured.NestedSlice(podSpec, "containers")
	require.NoError(t, err)
	require.True(t, found)
	for _, kind := range []string{"requests", "limits"} {
		count, found, err := unstructured.NestedFieldNoCopy(containers[0].(map[string]any), "resources", kind, "vpc.amazonaws.com/efa")
		require.NoError(t, err)
		require.True(t, found)
		require.EqualValues(t, 16, count)
		gpus, _, err := unstructured.NestedString(containers[0].(map[string]any), "resources", kind, "nvidia.com/gpu")
		require.NoError(t, err)
		require.Equal(t, "8", gpus)
	}
}

// The trainer and both init containers render the same pinned image.
func TestAWSB300TrainingImagePinnedOnEveryContainer(t *testing.T) {
	podSpec := renderAWSB300TrainingRuntime(t, "deps/aws-b300-training-image.yaml")
	containers, found, err := unstructured.NestedSlice(podSpec, "containers")
	require.NoError(t, err)
	require.True(t, found)
	initContainers, found, err := unstructured.NestedSlice(podSpec, "initContainers")
	require.NoError(t, err)
	require.True(t, found)
	names := map[string]bool{}
	for _, c := range append(containers, initContainers...) {
		m := c.(map[string]any)
		names[m["name"].(string)] = true
		require.Equal(t, awsB300TrainingImage, m["image"], "container %s", m["name"])
	}
	require.Equal(t, map[string]bool{"node": true, "megatron-clone": true, awsB300DDPGuardContainer: true}, names)
}

func awsB300GuardContainer(t *testing.T, file string) map[string]any {
	t.Helper()
	initContainers, found, err := unstructured.NestedSlice(renderAWSB300TrainingRuntime(t, file), "initContainers")
	require.NoError(t, err)
	require.True(t, found)
	for _, raw := range initContainers {
		container := raw.(map[string]any)
		if container["name"] == awsB300DDPGuardContainer {
			delete(container, "image")
			return container
		}
	}
	require.FailNow(t, "no "+awsB300DDPGuardContainer+" init container in "+file)
	return nil
}

func TestAWSB300DDPGuardMatchesAWSRTX(t *testing.T) {
	b300 := awsB300GuardContainer(t, "deps/aws-b300-training-image.yaml")
	rtx := awsB300GuardContainer(t, "deps/aws-rtxpro6000-training-image.yaml")
	args := b300["args"].([]any)
	script := args[0].(string)
	require.Contains(t, script, "AWS B300")
	args[0] = strings.ReplaceAll(script, "AWS B300", "AWS RTX")
	require.Equal(t, rtx, b300)
}
