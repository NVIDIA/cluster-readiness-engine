// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// The first group deletes its failed Job for a retry before a later group's
// node-result write is forbidden. The first retry must survive the access wait,
// while the unfinished second transition must not be persisted.
func TestNamespaceAccessPreservesEarlierGroupRetry(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "namespace-access-group-retry"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			ReviewError string `json:"reviewError"`
			Recover     bool   `json:"recover"`
			// FirstRecordsFailedNodes has the first group report failed nodes
			// too: its write creates the node-results ConfigMap and points
			// the Workflow at it, and the second group's update of the same
			// ConfigMap is the one forbidden.
			FirstRecordsFailedNodes bool `json:"firstRecordsFailedNodes"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		ctx := context.Background()
		workflow := &nvcrev1alpha1.Workflow{Name: testWorkflowName, Namespace: testNS}
		workflow.Spec.Orchestration.Execution.RetryFailedGroups = 1
		workflow.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{CurrentIteration: 1, Groups: []nvcrev1alpha1.GroupStatus{
			{Name: "a", Phase: nvcrev1alpha1.GroupRunning, Nodes: []string{"node-a"}, JobRef: &nvcrev1alpha1.WorkloadReference{Name: "a-job", Namespace: testNS}},
			{Name: "b", Phase: nvcrev1alpha1.GroupRunning, Nodes: []string{"retry-node-b"}, JobRef: &nvcrev1alpha1.WorkloadReference{Name: "b-job", Namespace: testNS}},
		}}
		first := &nvcrev1alpha1.Job{Name: "a-job", Namespace: testNS, Status: nvcrev1alpha1.JobStatus{Conditions: []metav1.Condition{{Type: nvcrev1alpha1.JobFailed, Status: metav1.ConditionTrue, Reason: ReasonWorkloadFailed}}}}
		second := first.DeepCopy()
		second.Name = "b-job"
		second.Status.FailedNodes = []nvcrev1alpha1.FailedNode{{Name: "retry-node-b", Reason: "WorkloadFailed"}}
		if input.FirstRecordsFailedNodes {
			first.Status.FailedNodes = []nvcrev1alpha1.FailedNode{{Name: "node-a", Reason: "WorkloadFailed"}}
		}
		rejectWrites := true
		c := fake.NewClientBuilder().WithScheme(newFallbackScheme(t)).WithObjects(workflow, first, second).
			WithStatusSubresource(workflow, first, second).
			WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(client.Object) []string { return nil }).
			WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && rejectWrites && !input.FirstRecordsFailedNodes {
					return apierrors.NewForbidden(schema.GroupResource{Resource: resourceConfigMaps}, obj.GetName(), errors.New("revoked"))
				}
				return c.Create(ctx, obj, opts...)
			}, Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && rejectWrites && input.FirstRecordsFailedNodes {
					return apierrors.NewForbidden(schema.GroupResource{Resource: resourceConfigMaps}, obj.GetName(), errors.New("revoked"))
				}
				return c.Update(ctx, obj, opts...)
			}}).Build()
		denied := AccessCheck{Resource: resourceConfigMaps, Verb: verbCreate}
		if input.FirstRecordsFailedNodes {
			denied.Verb = verbUpdate
		}
		reviewer := &fakeAccessReviewer{denied: []AccessCheck{denied}}
		if input.ReviewError != "" {
			reviewer.err = errors.New(input.ReviewError)
		}
		r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: events.NewFakeRecorder(20), AccessReviewer: reviewer}
		if _, err := r.requeueAfterForbiddenWrite(ctx, workflow)(r.updateStatusFromJobs(ctx, workflow)); err != nil {
			return err
		}
		stored := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(workflow), stored); err != nil {
			return err
		}
		output := map[string]any{"waiting": retryGroupView(stored), "firstJobDeleted": apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(first), &nvcrev1alpha1.Job{}))}
		if input.Recover {
			rejectWrites = false
			reviewer.denied = nil
			reviewer.err = nil
			if _, _, err := r.gateNamespaceAccess(ctx, stored); err != nil {
				return err
			}
			if _, err := r.updateStatusFromJobs(ctx, stored); err != nil {
				return err
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workflow), stored); err != nil {
				return err
			}
			output["afterRecovery"] = retryGroupView(stored)
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func retryGroupView(workflow *nvcrev1alpha1.Workflow) map[string]any {
	groups := make([]map[string]any, 0, len(workflow.Status.Orchestration.Groups))
	for _, g := range workflow.Status.Orchestration.Groups {
		groups = append(groups, map[string]any{
			"name": g.Name, "phase": g.Phase, "retries": g.Retries, "hasJobRef": g.JobRef != nil, "hasCompletionTime": g.CompletionTime != nil,
		})
	}
	reason := ""
	for _, c := range workflow.Status.Conditions {
		if c.Type == nvcrev1alpha1.WorkflowNamespaceAccess {
			reason = c.Reason
		}
	}
	return map[string]any{
		"groups": groups, "namespaceAccessReason": reason, "hasFailedNodesRef": workflow.Status.FailedNodesRef != nil,
	}
}
