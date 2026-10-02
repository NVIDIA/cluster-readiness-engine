// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podutil

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetReplicatedJobPodPicksOnePodDeterministically(t *testing.T) {
	base := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	pod := func(name, index string, phase corev1.PodPhase, restartAttempt string, created time.Duration) *corev1.Pod {
		labels := map[string]string{
			"jobset.sigs.k8s.io/jobset-name":           "w",
			"jobset.sigs.k8s.io/replicatedjob-name":    "launcher",
			"batch.kubernetes.io/job-completion-index": index,
		}
		if restartAttempt != "" {
			labels[jobSetRestartAttemptLabel] = restartAttempt
		}
		return &corev1.Pod{
			Name: name, Namespace: "ns", Labels: labels,
			CreationTimestamp: metav1.NewTime(base.Add(created)),
			Status:            corev1.PodStatus{Phase: phase},
		}
	}

	tests := []struct {
		name string
		pods []*corev1.Pod
		want string
	}{
		{
			name: "lowest completion index wins over phase",
			pods: []*corev1.Pod{
				pod("idx1-succeeded", "1", corev1.PodSucceeded, "", 0),
				pod("idx0-failed", "0", corev1.PodFailed, "", 0),
			},
			want: "idx0-failed",
		},
		{
			name: "succeeded replacement over the failed pod it replaced",
			pods: []*corev1.Pod{
				pod("old-failed", "0", corev1.PodFailed, "", 0),
				pod("new-succeeded", "0", corev1.PodSucceeded, "", time.Minute),
			},
			want: "new-succeeded",
		},
		{
			name: "running over failed",
			pods: []*corev1.Pod{
				pod("failed", "0", corev1.PodFailed, "", time.Minute),
				pod("running", "0", corev1.PodRunning, "", 0),
			},
			want: "running",
		},
		{
			name: "higher JobSet restart attempt over an older one in the same phase",
			pods: []*corev1.Pod{
				pod("attempt-2", "0", corev1.PodSucceeded, "2", 0),
				pod("attempt-1", "0", corev1.PodSucceeded, "1", time.Minute),
			},
			want: "attempt-2",
		},
		{
			name: "newer creation time when phase and attempt tie",
			pods: []*corev1.Pod{
				pod("older", "0", corev1.PodSucceeded, "", 0),
				pod("newer", "0", corev1.PodSucceeded, "", time.Second),
			},
			want: "newer",
		},
		{
			name: "name breaks a full tie",
			pods: []*corev1.Pod{
				pod("b", "0", corev1.PodSucceeded, "", 0),
				pod("a", "0", corev1.PodSucceeded, "", 0),
			},
			want: "a",
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tt.pods))
			for _, p := range tt.pods {
				objs = append(objs, p)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

			got, err := NewWorkerDiscoverer(c).GetReplicatedJobPod(context.Background(), "ns", "w", "launcher")
			require.NoError(t, err)
			require.Equal(t, tt.want, got.Name)
		})
	}
}
