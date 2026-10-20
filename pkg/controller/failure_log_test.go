// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// failedContainerOf decides whose log status.failureLog carries. When an MPI
// launcher fails, JobSet tears the workers down while NVCRE records the
// failure, so a worker's sshd can exit non-zero too. The pod list has no
// meaningful order, so the pick has to be the container that failed first
// (issue #498), with OOMKilled still winning as the root cause.
func TestFailedContainerOf(t *testing.T) {
	at := func(sec int) metav1.Time { return metav1.NewTime(time.Unix(1_700_000_000+int64(sec), 0)) }
	terminated := func(name string, code int32, reason string, finished metav1.Time) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, FinishedAt: finished,
		}}}
	}
	pod := func(name string, init, main []corev1.ContainerStatus) corev1.Pod {
		return corev1.Pod{
			Name:   name,
			Status: corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: main},
		}
	}
	// The pods' main containers are all named "node" in the catalog; distinct
	// names here make each case's expected pick readable.
	const launcher, mpirun, sshd = "launcher-a", "mpirun", "sshd"
	running := corev1.ContainerStatus{Name: sshd, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}

	tests := []struct {
		name          string
		pods          []corev1.Pod
		wantPod       string
		wantContainer string
	}{{
		name: "launcher failed before a worker's sshd exited, worker listed first",
		pods: []corev1.Pod{
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 255, "Error", at(40))}),
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
			pod("worker-1", nil, []corev1.ContainerStatus{running}),
		},
		wantPod: launcher, wantContainer: mpirun,
	}, {
		name: "first launcher attempt over its replacement",
		pods: []corev1.Pod{
			pod("launcher-b", nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(30))}),
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
		},
		wantPod: launcher, wantContainer: mpirun,
	}, {
		name: "OOMKilled outranks an earlier failure",
		pods: []corev1.Pod{
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 137, "OOMKilled", at(20))}),
		},
		wantPod: "worker-0", wantContainer: sshd,
	}, {
		name: "an init container failure is found (the KAI launcher wait)",
		pods: []corev1.Pod{
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 255, "Error", at(1300))}),
			pod(launcher, []corev1.ContainerStatus{
				terminated("fix-ssh-permissions", 0, "Completed", at(1)),
				terminated("wait-for-workers", 1, "Error", at(1201)),
			}, []corev1.ContainerStatus{{Name: mpirun, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}),
		},
		wantPod: launcher, wantContainer: "wait-for-workers",
	}, {
		name: "a recorded finish time outranks a missing one",
		pods: []corev1.Pod{
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 255, "Error", metav1.Time{})}),
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
		},
		wantPod: launcher, wantContainer: mpirun,
	}, {
		name: "nothing failed",
		pods: []corev1.Pod{
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 0, "Completed", at(10))}),
			pod("worker-0", nil, []corev1.ContainerStatus{running}),
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPod, gotContainer := failedContainerOf(tt.pods)
			if tt.wantPod == "" {
				require.Nil(t, gotPod)
				require.Nil(t, gotContainer)
				return
			}
			require.NotNil(t, gotPod)
			require.Equal(t, tt.wantPod, gotPod.Name)
			require.Equal(t, tt.wantContainer, gotContainer.Name)
		})
	}
}
