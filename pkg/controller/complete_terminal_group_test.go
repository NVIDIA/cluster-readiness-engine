// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// completeTerminalGroup owns the ordering that issue #121 is about: on
// failure the workload is deleted FIRST, then nothing else happens — no
// dependency cleanup, no retry reset, no phase change — until the workload's
// pods are gone. These cases pin the retry-path behavior the integration
// harness does not cover: a retried group is only reset to Pending after the
// drain (so the relaunch cannot land on nodes whose old pods still hold
// GPUs), and a job whose Failed condition carries ReasonJobTimedOut is never
// retried even when retry budget remains — timed-out jobs re-enter this path
// while their pods drain, and without the exclusion that re-entry would
// silently re-enable retry of timeouts.
//
// The Job's WorkloadRef points at a ConfigMap stand-in so "the workload was
// deleted even while the group is held draining" is observable through the
// fake client.
func TestCompleteTerminalGroup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "complete-terminal-group",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			RetryFailedGroups int    `yaml:"retryFailedGroups"`
			GroupRetries      int    `yaml:"groupRetries"`
			JobFailedReason   string `yaml:"jobFailedReason"`
			JobSucceeded      bool   `yaml:"jobSucceeded"`
			Pods              []struct {
				Name  string `yaml:"name"`
				Phase string `yaml:"phase"`
			} `yaml:"pods"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const ns = "ns"
		now := metav1.Now()

		job := &nvcrev1alpha1.Job{
			Name: "wf-group-0-iter-1", Namespace: ns,
			Status: nvcrev1alpha1.JobStatus{
				WorkloadRef: &nvcrev1alpha1.WorkloadReference{
					APIVersion: "v1", Kind: kindConfigMap, Name: "workload-cm", Namespace: ns,
				},
			},
		}
		if input.JobFailedReason != "" {
			job.Status.Conditions = append(job.Status.Conditions, metav1.Condition{
				Type:               nvcrev1alpha1.JobFailed,
				Status:             metav1.ConditionTrue,
				Reason:             input.JobFailedReason,
				LastTransitionTime: now,
			})
		}
		if input.JobSucceeded {
			job.Status.Conditions = append(job.Status.Conditions, metav1.Condition{
				Type:               nvcrev1alpha1.JobSucceeded,
				Status:             metav1.ConditionTrue,
				Reason:             "WorkloadSucceeded",
				LastTransitionTime: now,
			})
		}

		workflow := &nvcrev1alpha1.Workflow{
			Name: "wf", Namespace: ns,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{
					Execution: nvcrev1alpha1.ExecutionSpec{RetryFailedGroups: input.RetryFailedGroups},
				},
			},
			Status: nvcrev1alpha1.WorkflowStatus{
				DependencyRefs: []nvcrev1alpha1.DependencyResourceRef{{
					APIVersion: "v1", Kind: kindConfigMap, Name: "dep-cm", Namespace: ns,
					Scope: labelJob, GroupName: "group-0", Iteration: 1,
				}},
				Orchestration: &nvcrev1alpha1.OrchestrationStatus{
					TotalNodes: 1, NodesPerJob: 1, TotalGroups: 1, CurrentIteration: 1,
					Groups: []nvcrev1alpha1.GroupStatus{{
						Name:    "group-0",
						Nodes:   []string{testNodeA},
						Phase:   nvcrev1alpha1.GroupRunning,
						Retries: input.GroupRetries,
						JobRef: &nvcrev1alpha1.WorkloadReference{
							APIVersion: nvcrev1alpha1.GroupVersion.String(), Kind: kindJob,
							Name: job.Name, Namespace: ns,
						},
						StartTime: &now,
					}},
				},
			},
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		objs := []client.Object{
			job.DeepCopy(),
			&corev1.ConfigMap{Name: "dep-cm", Namespace: ns},
			&corev1.ConfigMap{Name: "workload-cm", Namespace: ns},
		}
		for _, sp := range input.Pods {
			objs = append(objs, &corev1.Pod{
				Name: sp.Name, Namespace: ns,
				Labels: map[string]string{nodemonitor.NVCREJobLabel: job.Name},
				Status: corev1.PodStatus{Phase: corev1.PodPhase(sp.Phase)},
			})
		}
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
		r := &WorkflowReconciler{Client: c, Scheme: scheme}

		g := &workflow.Status.Orchestration.Groups[0]
		ts := getJobTerminalState(job)
		draining, err := r.completeTerminalGroup(context.Background(), workflow, workflow.Status.Orchestration, g, job, ts)
		if err != nil {
			return err
		}

		exists := func(name string, obj client.Object) (bool, error) {
			err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return err == nil, err
		}
		depExists, err := exists("dep-cm", &corev1.ConfigMap{})
		if err != nil {
			return err
		}
		workloadExists, err := exists("workload-cm", &corev1.ConfigMap{})
		if err != nil {
			return err
		}
		jobExists, err := exists(job.Name, &nvcrev1alpha1.Job{})
		if err != nil {
			return err
		}

		deps := []string{}
		for _, ref := range workflow.Status.DependencyRefs {
			deps = append(deps, fmt.Sprintf("%s/%s", ref.Kind, ref.Name))
		}

		out := struct {
			Draining          bool     `json:"draining"`
			GroupPhase        string   `json:"groupPhase"`
			Retries           int      `json:"retries"`
			HasJobRef         bool     `json:"hasJobRef"`
			HasCompletionTime bool     `json:"hasCompletionTime"`
			DependencyRefs    []string `json:"dependencyRefs"`
			DepExists         bool     `json:"depConfigMapExists"`
			WorkloadExists    bool     `json:"workloadExists"`
			JobExists         bool     `json:"jobExists"`
		}{
			Draining:          draining,
			GroupPhase:        string(g.Phase),
			Retries:           g.Retries,
			HasJobRef:         g.JobRef != nil,
			HasCompletionTime: g.CompletionTime != nil,
			DependencyRefs:    deps,
			DepExists:         depExists,
			WorkloadExists:    workloadExists,
			JobExists:         jobExists,
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
