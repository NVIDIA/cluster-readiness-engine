// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const workflowResourceName = "workflows"

// A workload observation made before a competing terminal write must not
// resurrect the Job or replace its terminal reason on the conflict retry.
func TestJobPhaseWritePreservesConcurrentTerminalDecision(t *testing.T) {
	for _, terminal := range []string{nvcrev1alpha1.JobFailed, nvcrev1alpha1.JobSucceeded} {
		winnerReason := ReasonJobTimedOut
		if terminal == nvcrev1alpha1.JobSucceeded {
			winnerReason = ReasonWorkloadCompleted
		}
		for _, target := range []string{nvcrev1alpha1.JobInProgress, nvcrev1alpha1.JobSucceeded, nvcrev1alpha1.JobFailed} {
			t.Run(terminal+"/"+target, func(t *testing.T) {
				ctx := context.Background()
				job := &nvcrev1alpha1.Job{Name: "competing-terminal", Namespace: testNS,
					Status: nvcrev1alpha1.JobStatus{Conditions: []metav1.Condition{{
						Type: nvcrev1alpha1.JobInProgress, Status: metav1.ConditionTrue,
						Reason: ReasonWorkloadRunning, Message: "already running",
					}}}}
				writes := 0
				c := fake.NewClientBuilder().WithScheme(newWorkflowScheme(t)).WithObjects(job).
					WithStatusSubresource(job).WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, _ string,
						obj client.Object, opts ...client.SubResourceUpdateOption) error {
						writes++
						if writes == 1 {
							winner := &nvcrev1alpha1.Job{}
							require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), winner))
							// Match Workflow's additive timeout write, without changing exclusivity.
							meta.SetStatusCondition(&winner.Status.Conditions, metav1.Condition{
								Type: terminal, Status: metav1.ConditionTrue, Reason: winnerReason,
								Message: "concurrent terminal decision",
							})
							require.NoError(t, c.Status().Update(ctx, winner))
							return apierrors.NewConflict(schema.GroupResource{Resource: testJobsResource}, obj.GetName(), errSimulatedStatus)
						}
						return c.Status().Update(ctx, obj, opts...)
					},
				}).Build()
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), job))
				recorder := events.NewFakeRecorder(10)
				r := &JobReconciler{Client: c, Recorder: recorder}
				recordJobStatus(job.Namespace, job.Name, "", "in_progress")
				t.Cleanup(func() { cleanupJobMetrics(job.Namespace, job.Name) })
				var err error
				if target == nvcrev1alpha1.JobFailed {
					err = r.setJobFailed(ctx, job, ReasonWorkloadFailed, "stale deleted-workload observation")
				} else {
					err = r.setExclusiveCondition(ctx, job, target, ReasonWorkloadRunning, "stale workload observation",
						func(j *nvcrev1alpha1.Job) bool { j.Status.RestartCount++; return true })
				}
				require.NoError(t, err)
				require.Equal(t, 1, writes, "retry must not write over the terminal winner")
				require.Empty(t, recorder.Events, "a discarded transition must not emit")
				persisted := &nvcrev1alpha1.Job{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), persisted))
				require.Equal(t, winnerReason, condReason(persisted.Status.Conditions, terminal))
				require.True(t, condIsTrue(persisted.Status.Conditions, terminal))
				require.True(t, condIsTrue(persisted.Status.Conditions, nvcrev1alpha1.JobInProgress),
					"preserve the additive timeout shape; exclusivity repair is a separate change")
				require.Zero(t, persisted.Status.RestartCount)
				require.Empty(t, persisted.Status.FailedNodes)
				// The discarded mutation must not report its requested phase to metrics.
				// Refreshing metrics from the terminal winner is a separate concern.
				require.Equal(t, float64(1), promtest.ToFloat64(jobStatusGauge.WithLabelValues(job.Namespace, job.Name, "", "in_progress")))
				require.Zero(t, promtest.ToFloat64(jobStatusGauge.WithLabelValues(job.Namespace, job.Name, "", "failed")))
				require.Zero(t, promtest.ToFloat64(jobStatusGauge.WithLabelValues(job.Namespace, job.Name, "", "succeeded")))
			})
		}
	}
}

func TestConditionFlip(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "condition-flip",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Before  []metav1.Condition `yaml:"before"`
			After   []metav1.Condition `yaml:"after"`
			Watched []string           `yaml:"watched"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		result := conditionFlip(input.Before, input.After, input.Watched)
		clearTransitionTimes(result)
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestExclusiveConditionTransition(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "exclusive-condition-transition",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Before         []metav1.Condition `yaml:"before"`
			Target         string             `yaml:"target"`
			Reason         string             `yaml:"reason"`
			Message        string             `yaml:"message"`
			ExtraChanged   bool               `yaml:"extraChanged"`
			ConflictWinner bool               `yaml:"conflictWinner"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		workflow := &nvcrev1alpha1.Workflow{
			Name: "workflow", Namespace: testNS,
			Status: nvcrev1alpha1.WorkflowStatus{Conditions: input.Before},
		}
		firstWrite := true
		builder := fake.NewClientBuilder().
			WithScheme(newWorkflowScheme(t)).
			WithObjects(workflow).
			WithStatusSubresource(&nvcrev1alpha1.Workflow{})
		if input.ConflictWinner {
			builder = builder.WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(
					ctx context.Context, c client.Client, _ string, obj client.Object,
					opts ...client.SubResourceUpdateOption,
				) error {
					if firstWrite {
						firstWrite = false
						if err := c.Status().Update(ctx, obj, opts...); err != nil {
							return err
						}
						return apierrors.NewConflict(
							schema.GroupResource{Group: nvcrev1alpha1.GroupVersion.Group, Resource: workflowResourceName},
							obj.GetName(), errors.New("simulated competing writer"))
					}
					return c.Status().Update(ctx, obj, opts...)
				},
			})
		}
		c := builder.Build()
		current := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, types.NamespacedName{Name: workflow.Name, Namespace: workflow.Namespace}, current); err != nil {
			return err
		}

		var extras []func(*nvcrev1alpha1.Workflow) bool
		if input.ExtraChanged {
			extras = append(extras, func(w *nvcrev1alpha1.Workflow) bool {
				w.Status.SucceededNodesRef = &corev1.TypedLocalObjectReference{Name: "succeeded-nodes"}
				return true
			})
		}
		changed, transition, err := setExclusiveStatusCondition(
			ctx,
			c,
			current,
			func(w *nvcrev1alpha1.Workflow) *[]metav1.Condition { return &w.Status.Conditions },
			[]string{
				nvcrev1alpha1.WorkflowInProgress,
				nvcrev1alpha1.WorkflowSucceeded,
				nvcrev1alpha1.WorkflowFailed,
			},
			input.Target,
			input.Reason,
			input.Message,
			extras...,
		)
		if err != nil {
			return err
		}
		for i := range current.Status.Conditions {
			current.Status.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		if transition != nil {
			transition.Condition.LastTransitionTime = metav1.Time{}
		}
		output := struct {
			Changed    bool                 `json:"changed"`
			Transition *conditionTransition `json:"transition,omitempty"`
			Conditions []metav1.Condition   `json:"conditions"`
			ExtraSet   bool                 `json:"extraSet"`
		}{
			Changed:    changed,
			Transition: transition,
			Conditions: current.Status.Conditions,
			ExtraSet:   current.Status.SucceededNodesRef != nil,
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestWorkloadRunTransitionEventsAreDeduplicated(t *testing.T) {
	ctx := context.Background()
	run := &nvcrev1alpha1.WorkloadRun{Name: testRunName, Namespace: testNS}
	c := fake.NewClientBuilder().WithScheme(newWorkflowScheme(t)).WithObjects(run).
		WithStatusSubresource(run).Build()
	recorder := events.NewFakeRecorder(10)
	r := &WorkloadRunReconciler{Client: c, Recorder: recorder}
	write := func(phase, reason, message string) {
		t.Helper()
		// A fresh read models each reconciliation; do not reuse the mutated object.
		current := &nvcrev1alpha1.WorkloadRun{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(run), current))
		require.NoError(t, r.setWorkloadRunConditionAndUpdate(ctx, current, phase, reason, message))
	}
	for range 5 {
		write(nvcrev1alpha1.WorkloadRunInProgress, "Started", "starting")
	}
	require.Len(t, recorder.Events, 1)
	require.Equal(t, "Normal Started starting", <-recorder.Events)
	write(nvcrev1alpha1.WorkloadRunInProgress, "StillRunning", "starting")
	write(nvcrev1alpha1.WorkloadRunInProgress, "StillRunning", "progress changed")
	require.Empty(t, recorder.Events)
	write(nvcrev1alpha1.WorkloadRunSucceeded, "Completed", "done")
	require.Len(t, recorder.Events, 1)
	require.Equal(t, "Normal Completed done", <-recorder.Events)
}

func TestCertificationTransitionEventsAreDeduplicated(t *testing.T) {
	ctx := context.Background()
	certification := &nvcrev1alpha1.Certification{
		Name: "certification", Namespace: testNS,
	}
	c := fake.NewClientBuilder().
		WithScheme(newWorkflowScheme(t)).
		WithObjects(certification).
		WithStatusSubresource(&nvcrev1alpha1.Certification{}).
		Build()
	current := &nvcrev1alpha1.Certification{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(certification), current))
	recorder := events.NewFakeRecorder(10)
	r := &CertificationReconciler{Client: c, Recorder: recorder}

	require.NoError(t, r.setExclusiveCondition(ctx, current,
		nvcrev1alpha1.CertificationInProgress, "Started", "starting"))
	for range 5 {
		require.NoError(t, r.setExclusiveCondition(ctx, current,
			nvcrev1alpha1.CertificationInProgress, "Started", "starting"))
	}
	require.NoError(t, r.setExclusiveCondition(ctx, current,
		nvcrev1alpha1.CertificationInProgress, "StillRunning", "starting"))
	require.NoError(t, r.setExclusiveCondition(ctx, current,
		nvcrev1alpha1.CertificationInProgress, "StillRunning", "progress changed"))
	require.NoError(t, r.setExclusiveCondition(ctx, current,
		nvcrev1alpha1.CertificationSucceeded, "Completed", "done"))

	require.Len(t, recorder.Events, 2)
	require.Equal(t, "Normal Started starting", <-recorder.Events)
	require.Equal(t, "Normal Completed done", <-recorder.Events)
}

func TestCertificationTransitionEventRequiresSuccessfulStatusWrite(t *testing.T) {
	tests := []struct {
		name           string
		conflicts      int
		wantErr        bool
		wantEventCount int
	}{
		{name: "non-conflict error", conflicts: -1, wantErr: true, wantEventCount: 0},
		{name: "conflict exhaustion", conflicts: 100, wantErr: true, wantEventCount: 0},
		{name: "conflict then success", conflicts: 1, wantErr: false, wantEventCount: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			certification := &nvcrev1alpha1.Certification{
				Name: "certification", Namespace: testNS,
			}
			remaining := tt.conflicts
			c := fake.NewClientBuilder().
				WithScheme(newWorkflowScheme(t)).
				WithObjects(certification).
				WithStatusSubresource(&nvcrev1alpha1.Certification{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(
						ctx context.Context, c client.Client, _ string, obj client.Object,
						opts ...client.SubResourceUpdateOption,
					) error {
						if remaining < 0 {
							return errors.New("simulated status failure")
						}
						if remaining > 0 {
							remaining--
							return apierrors.NewConflict(
								schema.GroupResource{Group: nvcrev1alpha1.GroupVersion.Group, Resource: "certifications"},
								obj.GetName(), errors.New("simulated stale write"))
						}
						return c.Status().Update(ctx, obj, opts...)
					},
				}).
				Build()
			current := &nvcrev1alpha1.Certification{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(certification), current))
			recorder := events.NewFakeRecorder(10)
			r := &CertificationReconciler{Client: c, Recorder: recorder}

			err := r.setExclusiveCondition(ctx, current,
				nvcrev1alpha1.CertificationInProgress, "Started", "starting")
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, recorder.Events, tt.wantEventCount)
		})
	}
}

func TestWorkloadRunBuildFailureEventFallback(t *testing.T) {
	tests := []struct {
		name      string
		statusErr error
		wantEvent string
	}{
		{
			name:      "status success emits transition",
			wantEvent: "Warning BuildFailed workloadrun run: exec framework selected but spec.framework.exec is nil",
		},
		{
			name:      "status failure emits fallback",
			statusErr: errors.New("simulated status failure"),
			wantEvent: "Warning BuildFailedStatusUpdateFailed WorkloadRun build failed: workloadrun run: exec framework selected but spec.framework.exec is nil; recording the WorkloadRun Failed condition also failed: simulated status failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			run := &nvcrev1alpha1.WorkloadRun{
				Name: testRunName, Namespace: testNS,
			}
			builder := fake.NewClientBuilder().
				WithScheme(newWorkflowScheme(t)).
				WithObjects(run).
				WithStatusSubresource(&nvcrev1alpha1.WorkloadRun{})
			if tt.statusErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(
						context.Context, client.Client, string, client.Object,
						...client.SubResourceUpdateOption,
					) error {
						return tt.statusErr
					},
				})
			}
			recorder := events.NewFakeRecorder(10)
			r := &WorkloadRunReconciler{Client: builder.Build(), Recorder: recorder}

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			if tt.statusErr != nil {
				require.ErrorIs(t, err, tt.statusErr)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, recorder.Events, 1)
			require.Equal(t, tt.wantEvent, <-recorder.Events)
		})
	}
}

func clearTransitionTimes(results []conditionFlipResult) {
	for i := range results {
		results[i].Condition.LastTransitionTime = metav1.Time{}
	}
}

func TestWorkloadRunBuildFailureThenSuccessfulPersistence(t *testing.T) {
	ctx := context.Background()
	run := &nvcrev1alpha1.WorkloadRun{Name: testRunName, Namespace: testNS}
	failNext := true
	c := fake.NewClientBuilder().WithScheme(newWorkflowScheme(t)).WithObjects(run).
		WithStatusSubresource(run).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, _ string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failNext {
				failNext = false
				return errSimulatedStatus
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	}).Build()
	recorder := events.NewFakeRecorder(10)
	r := &WorkloadRunReconciler{Client: c, Recorder: recorder}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	_, err := r.Reconcile(ctx, req)
	require.ErrorIs(t, err, errSimulatedStatus)
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	require.Len(t, recorder.Events, 2)
	require.Contains(t, <-recorder.Events, "Warning "+ReasonBuildFailedStatusUpdateFailed+" ")
	require.Contains(t, <-recorder.Events, "Warning BuildFailed ")
}
