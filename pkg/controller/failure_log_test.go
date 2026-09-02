// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// The stored tail was 30 lines, right for a workload whose last line is the
// error and wrong for one printing a structured document: a failed dcgmi diag
// stored 392 bytes ending in "status" : "Pass". The byte cap also has to trim
// from the front, because the newest output is the part that matters.
func TestFailureLogTail(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "failure-log-tail",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			MaxBytes int    `yaml:"maxBytes"`
			Text     string `yaml:"text"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got, err := readLogTail(strings.NewReader(in.Text), in.MaxBytes)
		if err != nil {
			return err
		}

		b, err := json.MarshalIndent(struct {
			Kept      string `json:"kept"`
			KeptBytes int    `json:"keptBytes"`
		}{got, len(got)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// A Job that fails after its pod is gone must still carry a record saying so.
// captureFailureLog used to return silently, leaving status.failureLog unset.
func TestFailureLogCapture(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "failure-log-capture",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Pods []struct {
				Name    string `yaml:"name"`
				Running bool   `yaml:"running"`
			} `yaml:"pods"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		objs := make([]client.Object, 0, len(in.Pods))
		for _, sp := range in.Pods {
			pod := &corev1.Pod{
				Name: sp.Name, Namespace: "ns",
				Labels: map[string]string{"nvcre.nvidia.com/job": "j"},
			}
			if sp.Running {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  "node",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}
			}
			objs = append(objs, pod)
		}

		job := &nvcrev1alpha1.Job{Name: "j", Namespace: "ns"}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
			WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(obj client.Object) []string {
				pod, ok := obj.(*corev1.Pod)
				if !ok {
					return nil
				}
				if jn, found := pod.Labels[nodemonitor.NVCREJobLabel]; found {
					return []string{jn}
				}
				return nil
			}).
			Build()
		// Clientset is only dereferenced once a failed pod is found, which is
		// the path these cases do not take.
		r := &JobReconciler{Client: c, Scheme: scheme, Clientset: &kubernetes.Clientset{}}

		r.captureFailureLog(context.Background(), job)

		b, err := json.MarshalIndent(job.Status.FailureLog, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// Two properties are not input-in, JSON-out: that the window survives a stream
// arriving one byte at a time, and the limits themselves.
func TestReadLogTailAcrossChunkedReads(t *testing.T) {
	var sb strings.Builder
	for i := range 5000 {
		sb.WriteString("log line ")
		sb.WriteString(strings.Repeat("x", i%3))
		sb.WriteString("\n")
	}
	full := sb.String()

	got, err := readLogTail(oneByteReader{strings.NewReader(full)}, 512)
	require.NoError(t, err)
	require.Len(t, got, 512)
	require.Equal(t, full[len(full)-512:], got)
}

type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestFailureLogLimits(t *testing.T) {
	require.Equal(t, 800, failureLogTailLines,
		"30 lines truncated dcgmi diag --json to its closing braces")
	require.Equal(t, 32*1024, failureLogMaxBytes)
}
