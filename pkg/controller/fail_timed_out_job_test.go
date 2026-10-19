// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestFailTimedOutJobAttribution pins which nodes a timed-out Job records.
//
// This path writes the Job status directly rather than through setJobFailed,
// so it is the only place on the failure path that has to seed FailedNodes
// itself; before ADR-099 it seeded nothing and a timed-out certification
// reported no failed nodes at all.
//
// The unschedulable case is the one worth pinning. A Job that timed out while
// its pods could not be placed never ran on those nodes, and the Job tier
// already treats scheduling-blocked time as not-running (it pauses the runtime
// clock and skips stall detection), so recording them would make them
// indistinguishable from nodes that ran and crashed: only failedNodeDetails
// carries the message that tells the two apart.
func TestFailTimedOutJobAttribution(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "fail-timed-out-job",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// SchedulingBlocked marks the Job's pods as unschedulable, the
			// branch that must not attribute the group's nodes.
			SchedulingBlocked bool     `yaml:"schedulingBlocked"`
			GroupNodes        []string `yaml:"groupNodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const ns = "ns"
		now := metav1.Now()

		job := &nvcrev1alpha1.Job{Name: "wf-group-0", Namespace: ns}
		if input.SchedulingBlocked {
			job.Status.SchedulingBlockedSince = &now
		}

		workflow := &nvcrev1alpha1.Workflow{Name: "wf", Namespace: ns}
		orch := &nvcrev1alpha1.OrchestrationStatus{
			TotalNodes: len(input.GroupNodes), NodesPerJob: len(input.GroupNodes),
			TotalGroups: 1, CurrentIteration: 1,
		}
		group := &nvcrev1alpha1.GroupStatus{
			Name:  testGroupZero,
			Nodes: input.GroupNodes,
			Phase: nvcrev1alpha1.GroupRunning,
			JobRef: &nvcrev1alpha1.WorkloadReference{
				APIVersion: nvcrev1alpha1.GroupVersion.String(), Kind: kindJob,
				Name: job.Name, Namespace: ns,
			},
			StartTime: &now,
		}
		orch.Groups = []nvcrev1alpha1.GroupStatus{*group}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(job, workflow).
			WithStatusSubresource(job, workflow).Build()

		r := &WorkflowReconciler{Client: c, Scheme: scheme}
		// The return value is the group outcome, which completeTerminalGroup
		// owns and complete-terminal-group already covers. What matters here
		// is what landed on the Job's status before that ran.
		_, _ = r.failTimedOutJob(context.Background(), workflow, orch, group, job)

		type entry struct {
			Name    string `json:"name"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		out := struct {
			FailedNodes []entry `json:"failedNodes"`
			Condition   string  `json:"failedCondition"`
			Message     string  `json:"conditionMessage"`
		}{FailedNodes: []entry{}}
		for _, fn := range job.Status.FailedNodes {
			out.FailedNodes = append(out.FailedNodes, entry{
				Name: fn.Name, Reason: string(fn.Reason), Message: fn.Message,
			})
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == nvcrev1alpha1.JobFailed && cond.Status == metav1.ConditionTrue {
				out.Condition = cond.Reason
				out.Message = cond.Message
			}
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
