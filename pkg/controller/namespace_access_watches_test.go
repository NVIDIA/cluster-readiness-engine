// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

func TestNamespaceAccessRecoveryWatch(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "namespace-access-recovery-watch"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			OldStatus string                      `json:"oldStatus"`
			NewStatus string                      `json:"newStatus"`
			Deleting  bool                        `json:"deleting"`
			Groups    []nvcrev1alpha1.GroupStatus `json:"groups"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		old := recoveryWatchWorkflow(input.OldStatus)
		current := recoveryWatchWorkflow(input.NewStatus)
		if input.Groups != nil {
			current.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{Groups: input.Groups}
		}
		if input.Deleting {
			now := metav1.Now()
			current.DeletionTimestamp = &now
		}
		original := current.DeepCopy()
		p := namespaceAccessGrantedPredicate()
		update := p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current})
		requests := []string{}
		if update {
			for _, request := range namespaceAccessToJobs(context.Background(), current) {
				requests = append(requests, request.String())
			}
		}
		require.Equal(tc.T, original, current, "the recovery event must not change group clocks or status")
		output := map[string]any{
			"update": update, "requests": requests,
			"create":  p.Create(event.CreateEvent{Object: current}),
			"delete":  p.Delete(event.DeleteEvent{Object: current}),
			"generic": p.Generic(event.GenericEvent{Object: current}),
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func recoveryWatchWorkflow(status string) *nvcrev1alpha1.Workflow {
	w := &nvcrev1alpha1.Workflow{Name: testWorkflowName, Namespace: testNS}
	if status != "" {
		w.Status.Conditions = []metav1.Condition{{Type: nvcrev1alpha1.WorkflowNamespaceAccess, Status: metav1.ConditionStatus(status)}}
	}
	return w
}

// A recovery event must move a queued Job ahead of its delayed error retry.
// This uses controller-runtime's production priority queue, with a long delay
// so a missing wake-up cannot accidentally pass on the next ordinary retry.
func TestNamespaceAccessRecoveryBypassesJobBackoff(t *testing.T) {
	queue := priorityqueue.New[reconcile.Request]("access-recovery-test", func(opts *priorityqueue.Opts[reconcile.Request]) {
		opts.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Minute, time.Minute)
	})
	defer queue.ShutDown()
	current := recoveryWatchWorkflow("True")
	current.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{Groups: []nvcrev1alpha1.GroupStatus{
		{Name: "group-0", Phase: nvcrev1alpha1.GroupRunning, JobRef: &nvcrev1alpha1.WorkloadReference{Name: "waiting-job", Namespace: testNS}},
	}}
	request := reconcile.Request{Namespace: testNS, Name: "waiting-job"}
	queue.AddRateLimited(request)
	e := event.UpdateEvent{ObjectOld: recoveryWatchWorkflow("False"), ObjectNew: current}
	require.True(t, namespaceAccessGrantedPredicate().Update(e))
	handler.EnqueueRequestsFromMapFunc(namespaceAccessToJobs).Update(context.Background(), e, queue)
	ready := make(chan reconcile.Request, 1)
	go func() {
		item, shutdown := queue.Get()
		if !shutdown {
			ready <- item
			queue.Done(item)
		}
	}()
	select {
	case got := <-ready:
		require.Equal(t, request, got)
	case <-time.After(time.Second):
		t.Fatal("Job remained behind the error backoff after namespace access returned")
	}
}
