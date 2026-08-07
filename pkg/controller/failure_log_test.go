// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestReadLogTailKeepsTheEnd(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{
			name:  "shorter than the cap is returned whole",
			input: "line one\nline two\n",
			max:   1024,
			want:  "line one\nline two\n",
		},
		{
			name:  "longer than the cap keeps the end, not the start",
			input: "START-DROP-THIS" + strings.Repeat("x", 100) + "END-KEEP-THIS",
			max:   20,
			want:  "xxxxxxxEND-KEEP-THIS",
		},
		{
			name:  "empty stream",
			input: "",
			max:   1024,
			want:  "",
		},
		{
			name:  "exactly the cap",
			input: "abcde",
			max:   5,
			want:  "abcde",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readLogTail(strings.NewReader(tt.input), tt.max)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.LessOrEqual(t, len(got), tt.max)
		})
	}
}

// The reader must keep the end even when the stream arrives in many small
// chunks, which is how a live pod log stream behaves.
func TestReadLogTailAcrossChunkedReads(t *testing.T) {
	var sb strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&sb, "log line %d\n", i)
	}
	full := sb.String()

	got, err := readLogTail(iotest_oneByteReader{strings.NewReader(full)}, 512)
	require.NoError(t, err)
	require.Len(t, got, 512)
	require.Equal(t, full[len(full)-512:], got, "must be the last 512 bytes of the stream")
	require.Contains(t, got, "log line 4999", "the most recent line must survive")
}

// oneByteReader forces the loop to append across many iterations.
type iotest_oneByteReader struct{ r io.Reader }

func (o iotest_oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

// A structured document is why the old 30-line tail was wrong. This is the real
// shape of dcgmi diag --json, measured on dcgm 4.5.2 against an A100: the run
// summary and the closing braces are at the end, and the per-test status that
// matters is earlier in the document.
func TestReadLogTailOnStructuredOutput(t *testing.T) {
	doc := `{
	"Diagnostic" : {
		"test_categories" : [
			{
				"category" : "Hardware",
				"tests" : [
					{
						"name" : "diagnostic",
						"results" : [ { "status" : "Fail", "gpu_id" : 0 } ]
					}
				]
			}
		]
	},
	"metadata" : { "version" : "4.5.2" }
}`

	// The old behaviour: a small tail keeps only the closing structure.
	small, err := readLogTail(strings.NewReader(doc), 60)
	require.NoError(t, err)
	require.NotContains(t, small, `"status" : "Fail"`,
		"a small tail misses the failing test, which is the bug being fixed")

	// The new cap is far larger than this document, so the failure survives.
	full, err := readLogTail(strings.NewReader(doc), failureLogMaxBytes)
	require.NoError(t, err)
	require.Contains(t, full, `"status" : "Fail"`)
	require.Equal(t, doc, full)
}

func TestFailureLogLimits(t *testing.T) {
	require.Equal(t, 800, failureLogTailLines,
		"30 lines truncated dcgmi diag --json to its closing braces")
	require.Equal(t, 32*1024, failureLogMaxBytes)
}

// A Job that fails after its pod is gone must still carry a record saying so.
// Before this, captureFailureLog returned silently and status.failureLog was
// absent, so the operator saw a failed Job with no diagnostics at all.
func TestCaptureFailureLogAlwaysRecordsSomething(t *testing.T) {
	tests := []struct {
		name       string
		pods       []client.Object
		wantReason string
		wantTail   string
	}{
		{
			name:       "pod already deleted",
			pods:       nil,
			wantReason: "PodNotFound",
			wantTail:   "deleted before its logs could be read",
		},
		{
			name: "pod present but nothing terminated badly",
			pods: []client.Object{&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "p", Namespace: "ns",
					Labels: map[string]string{"cre.nvidia.com/job": "j"},
				},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "node",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}},
			}},
			wantReason: "NoTerminatedContainer",
			wantTail:   "none had a container terminated with a non-zero exit code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, burninv1alpha1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))

			job := &burninv1alpha1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
			}

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.pods...).Build()
			// Clientset is only dereferenced when a failed pod is found, which is
			// exactly the path these cases do not take.
			r := &JobReconciler{Client: c, Scheme: scheme, Clientset: &kubernetes.Clientset{}}

			r.captureFailureLog(context.Background(), job)

			require.NotNil(t, job.Status.FailureLog, "a failed Job must never carry no record")
			require.Equal(t, tt.wantReason, job.Status.FailureLog.Reason)
			require.Contains(t, job.Status.FailureLog.Tail, tt.wantTail)
		})
	}
}
