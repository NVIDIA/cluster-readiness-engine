// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
)

func TestTimeoutEventSurvivesPodDrainReentryWithoutDuplication(t *testing.T) {
	for _, mode := range []string{"success", testStatusHardError, "conflict"} {
		t.Run(mode, func(t *testing.T) { testTimeoutEventPersistence(t, mode) })
	}
}

func testTimeoutEventPersistence(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	job := &nvcrev1alpha1.Job{Name: "timeout-job", Namespace: testNS,
		Status: nvcrev1alpha1.JobStatus{
			WorkloadStartTime: &started,
			Conditions:        []metav1.Condition{{Type: nvcrev1alpha1.JobInProgress, Status: metav1.ConditionTrue}},
		}}
	wf := &nvcrev1alpha1.Workflow{Name: "timeout-workflow", Namespace: testNS,
		Spec: nvcrev1alpha1.WorkflowSpec{Orchestration: nvcrev1alpha1.OrchestrationSpec{
			Execution: nvcrev1alpha1.ExecutionSpec{TimeoutPerJob: &metav1.Duration{Duration: time.Second}},
		}},
		Status: nvcrev1alpha1.WorkflowStatus{Orchestration: &nvcrev1alpha1.OrchestrationStatus{
			Groups: []nvcrev1alpha1.GroupStatus{{Name: "timeout-group", Phase: nvcrev1alpha1.GroupRunning,
				JobRef: &nvcrev1alpha1.WorkloadReference{Name: job.Name, Namespace: testNS}}},
		}},
	}
	pod := &corev1.Pod{Name: "draining-pod", Namespace: testNS,
		Labels: map[string]string{nodemonitor.NVCREJobLabel: job.Name},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	c := fake.NewClientBuilder().WithScheme(newFallbackScheme(t)).WithObjects(job, wf, pod).
		WithStatusSubresource(job, wf).
		WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(obj client.Object) []string {
			return []string{obj.GetLabels()[nodemonitor.NVCREJobLabel]}
		}).Build()
	recorder := events.NewFakeRecorder(10)
	r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}
	if mode != "success" {
		attempts := 0
		r.Client = interceptor.NewClient(c, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, _ string,
				obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*nvcrev1alpha1.Job); ok {
					attempts++
					if attempts <= 2 {
						if mode == "conflict" {
							return apierrors.NewConflict(schema.GroupResource{Resource: testJobsResource}, obj.GetName(), errSimulatedStatus)
						}
						return errSimulatedStatus
					}
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		})
		for i := range 2 {
			current := &nvcrev1alpha1.Workflow{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(wf), current))
			_, err := r.updateStatusFromJobs(ctx, current)
			require.Error(t, err)
			require.Equal(t, i+1, attempts, "direct timeout writes retry on the next reconcile")
			require.Empty(t, recorder.Events)
			persisted := &nvcrev1alpha1.Job{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), persisted))
			require.False(t, condIsTrue(persisted.Status.Conditions, nvcrev1alpha1.JobFailed))
		}
	}
	for range 2 {
		current := &nvcrev1alpha1.Workflow{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(wf), current))
		_, err := r.updateStatusFromJobs(ctx, current)
		require.NoError(t, err)
		require.Equal(t, nvcrev1alpha1.GroupRunning, current.Status.Orchestration.Groups[0].Phase)
	}
	require.NoError(t, c.Delete(ctx, pod))
	current := &nvcrev1alpha1.Workflow{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(wf), current))
	_, err := r.updateStatusFromJobs(ctx, current)
	require.NoError(t, err)
	require.Equal(t, nvcrev1alpha1.GroupFailed, current.Status.Orchestration.Groups[0].Phase)
	count := 0
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, "Warning "+ReasonJobTimedOut+" ") {
			count++
		}
	}
	require.Equal(t, 1, count)
}
