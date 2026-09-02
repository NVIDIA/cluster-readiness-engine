// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A group whose Job has gone terminal must have its phase written to the
// Workflow status even when the Workflow's conditions do not change in the same
// reconcile. The group phases are mutated on workflow.Status directly, and used
// to be persisted only as a side effect of the condition write — which
// updateStatusWithRetry skips when its mutate reports no change.
func TestGroupCompletionPersists(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "group-completion-persist",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Groups                      int    `yaml:"groups"`
			JobCondition                string `yaml:"jobCondition"`
			PreexistingConditionMessage string `yaml:"preexistingConditionMessage"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		groups := make([]nvcrev1alpha1.GroupStatus, 0, in.Groups)
		jobs := make([]*nvcrev1alpha1.Job, 0, in.Groups)
		for i := 0; i < in.Groups; i++ {
			name := []string{"g0", "g1", "g2"}[i]
			job := &nvcrev1alpha1.Job{
				Name: name + "-job", Namespace: "ns",
				Status: nvcrev1alpha1.JobStatus{Conditions: []metav1.Condition{{
					Type: in.JobCondition, Status: metav1.ConditionTrue,
					Reason: "WorkloadCompleted", LastTransitionTime: metav1.Now(),
				}}},
			}
			jobs = append(jobs, job)
			groups = append(groups, nvcrev1alpha1.GroupStatus{
				Name:   name,
				Phase:  nvcrev1alpha1.GroupRunning,
				Nodes:  []string{"node" + name},
				JobRef: &nvcrev1alpha1.WorkloadReference{Name: name + "-job", Namespace: "ns"},
			})
		}

		wf := &nvcrev1alpha1.Workflow{
			Name: "wf", Namespace: "ns", Generation: 1,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{Iterations: 1},
			},
			Status: nvcrev1alpha1.WorkflowStatus{
				// Seed every condition exactly as the reconcile will recompute them,
				// ObservedGeneration included. Without that the condition write
				// reports a change for an unrelated reason and masks the bug.
				Conditions: []metav1.Condition{
					{
						Type: nvcrev1alpha1.WorkflowInProgress, Status: metav1.ConditionTrue,
						Reason: ReasonJobRunning, Message: in.PreexistingConditionMessage,
						ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
					},
					{
						Type: nvcrev1alpha1.WorkflowSucceeded, Status: metav1.ConditionFalse,
						Reason: ReasonNotApplicable, Message: "",
						ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
					},
					{
						Type: nvcrev1alpha1.WorkflowFailed, Status: metav1.ConditionFalse,
						Reason: ReasonNotApplicable, Message: "",
						ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
					},
				},
				Orchestration: &nvcrev1alpha1.OrchestrationStatus{
					CurrentIteration: 1,
					Groups:           groups,
				},
			},
		}

		b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wf).WithStatusSubresource(wf)
		for _, j := range jobs {
			b = b.WithObjects(j).WithStatusSubresource(j)
		}
		c := b.Build()

		r := &WorkflowReconciler{Client: c, Scheme: scheme, JobRequeueInterval: time.Second}
		if _, err := r.updateStatusFromJobs(context.Background(), wf); err != nil {
			return err
		}

		got := &nvcrev1alpha1.Workflow{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "wf", Namespace: "ns"}, got); err != nil {
			return err
		}

		type groupOut struct {
			Name          string `json:"name"`
			Phase         string `json:"phase"`
			HasCompletion bool   `json:"hasCompletionTime"`
		}
		out := struct {
			Groups []groupOut `json:"groups"`
		}{}
		for _, g := range got.Status.Orchestration.Groups {
			out.Groups = append(out.Groups, groupOut{
				Name: g.Name, Phase: string(g.Phase), HasCompletion: g.CompletionTime != nil,
			})
		}
		bts, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(bts) + "\n"
		return nil
	})
}

var _ = ctrl.Result{}
