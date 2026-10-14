// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/rbac"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// fakeAccessReviewer answers every review the same way and records whether
// each was asked fresh.
type fakeAccessReviewer struct {
	denied []AccessCheck
	err    error
	fresh  []bool
}

func (f *fakeAccessReviewer) Review(_ context.Context, _ string, _ []AccessCheck, fresh bool) ([]AccessCheck, error) {
	f.fresh = append(f.fresh, fresh)
	return f.denied, f.err
}

// TestNamespaceAccessWait pins the wait for namespace access (ADR-092
// decision 4): when it starts, what it writes and emits, when it times out,
// how it ends, and how a Forbidden write is classified.
func TestNamespaceAccessWait(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "namespace-access-wait", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// Call selects the gate, forbidden-write handling, or node-result
			// classification. requeue uses an in-memory unsaved transition.
			Call       string `json:"call"`
			WriteError string `json:"writeError"`
			NoReviewer bool   `json:"noReviewer"`
			Classified bool   `json:"classified"`
			// Denied and ReviewError are the reviewer's answer.
			Denied      []AccessCheck `json:"denied"`
			ReviewError string        `json:"reviewError"`
			// Waiting is how long the Workflow has already waited, with
			// WaitingReason; empty means it never waited. Granted is a
			// NamespaceAccess condition already True.
			Waiting       string `json:"waiting"`
			WaitingReason string `json:"waitingReason"`
			Granted       bool   `json:"granted"`
			// InProgress is the Workflow's current InProgress reason, if any.
			InProgress string `json:"inProgress"`
			// ServiceAccount is the controller's namespace/name; empty is the
			// setup init default.
			ServiceAccount string `json:"serviceAccount"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
		workflow := &nvcrev1alpha1.Workflow{Name: testWorkflowName, Namespace: testNS, Generation: 1}
		if input.InProgress != "" {
			workflow.Status.Conditions = append(workflow.Status.Conditions, metav1.Condition{
				Type: nvcrev1alpha1.WorkflowInProgress, Status: metav1.ConditionTrue,
				Reason: input.InProgress, Message: "already running", ObservedGeneration: 1,
				LastTransitionTime: metav1.NewTime(now.Add(-time.Hour)),
			})
		}
		if input.Waiting != "" {
			waited, err := time.ParseDuration(input.Waiting)
			if err != nil {
				return err
			}
			workflow.Status.Conditions = append(workflow.Status.Conditions, metav1.Condition{
				Type: nvcrev1alpha1.WorkflowNamespaceAccess, Status: metav1.ConditionFalse,
				Reason: input.WaitingReason, Message: "waiting", ObservedGeneration: 1,
				LastTransitionTime: metav1.NewTime(now.Add(-waited)),
			})
		}
		if input.Granted {
			workflow.Status.Conditions = append(workflow.Status.Conditions, metav1.Condition{
				Type: nvcrev1alpha1.WorkflowNamespaceAccess, Status: metav1.ConditionTrue,
				Reason: ReasonNamespaceAccessGranted, Message: "granted", ObservedGeneration: 1,
				LastTransitionTime: metav1.NewTime(now.Add(-time.Hour)),
			})
		}

		scheme := newFallbackScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workflow).
			WithStatusSubresource(&nvcrev1alpha1.Workflow{}).Build()
		reviewer := &fakeAccessReviewer{denied: input.Denied}
		if input.ReviewError != "" {
			reviewer.err = errors.New(input.ReviewError)
		}
		recorder := events.NewFakeRecorder(10)
		r := &WorkflowReconciler{
			Client: c, Scheme: scheme, Recorder: recorder,
			AccessReviewer: reviewer, now: func() time.Time { return now },
		}
		if input.NoReviewer {
			r.AccessReviewer = nil
		}
		if input.ServiceAccount != "" {
			ns, name, _ := strings.Cut(input.ServiceAccount, "/")
			r.ServiceAccount = rbac.ServiceAccount{Namespace: ns, Name: name}
		}

		ctx := context.Background()
		var (
			stop    bool
			result  struct{ RequeueAfter time.Duration }
			callErr error
		)
		switch input.Call {
		case "gate":
			res, w, err := r.gateNamespaceAccess(ctx, workflow)
			stop, result.RequeueAfter, callErr = w, res.RequeueAfter, err
		case "forbidden":
			writeErr := namespaceAccessTestWriteError(input.WriteError)
			res, w, err := r.awaitAccessOnForbidden(ctx, workflow, writeErr)
			stop, result.RequeueAfter, callErr = w, res.RequeueAfter, err
		case "requeue":
			writeErr := namespaceAccessTestWriteError(input.WriteError)
			if input.Classified {
				writeErr = &accessDeniedWriteError{err: writeErr, access: namespaceAccessResult{
					denied:         map[string][]AccessCheck{testNS: input.Denied},
					serviceAccount: r.controllerServiceAccount(),
				}}
			}
			inMemory := workflow.DeepCopy()
			inMemory.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{CompletedIterations: 1}
			res, err := r.requeueAfterForbiddenWrite(ctx, inMemory)(ctrl.Result{}, writeErr)
			result.RequeueAfter, callErr = res.RequeueAfter, err
		case "node-results":
			callErr = r.nodeResultsWriteError(ctx, workflow, namespaceAccessTestWriteError(input.WriteError), "record")
		default:
			return fmt.Errorf("unknown call %q", input.Call)
		}

		got := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(workflow), got); err != nil {
			return err
		}
		type conditionView struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
			// Since is how long before now the condition last flipped, for
			// NamespaceAccess, whose transition time is the wait's clock.
			Since string `json:"since,omitempty"`
		}
		conditions := []conditionView{}
		for _, cond := range got.Status.Conditions {
			view := conditionView{Type: cond.Type, Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message}
			if cond.Type == nvcrev1alpha1.WorkflowNamespaceAccess {
				view.Since = now.Sub(cond.LastTransitionTime.Time).String()
			}
			conditions = append(conditions, view)
		}
		emitted := []string{}
		for len(recorder.Events) > 0 {
			emitted = append(emitted, <-recorder.Events)
		}

		output := map[string]any{
			"stop":            stop,
			"requeueAfter":    result.RequeueAfter.String(),
			"conditions":      conditions,
			"events":          emitted,
			"reviewsAskFresh": reviewer.fresh,
		}
		if input.Call == "requeue" {
			delete(output, "stop")
			output["orchestrationPersisted"] = got.Status.Orchestration != nil
		}
		if input.Call == "node-results" {
			delete(output, "stop")
			delete(output, "requeueAfter")
			delete(output, "conditions")
			output["endsPass"] = callErr != nil
			if denied, ok := errors.AsType[*accessDeniedWriteError](callErr); ok {
				output["originalForbiddenPreserved"] = apierrors.IsForbidden(callErr)
				output["carriedDenials"] = denied.access.denied
				if denied.access.reviewErr != nil {
					output["carriedReviewError"] = denied.access.reviewErr.Error()
				}
			}
		}
		if callErr != nil {
			output["error"] = callErr.Error()
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func namespaceAccessTestWriteError(value string) error {
	switch value {
	case "no-error":
		return nil
	case "", "forbidden":
		return apierrors.NewForbidden(schema.GroupResource{Resource: resourceConfigMaps}, "node-results", errors.New("injected"))
	default:
		return errors.New(value)
	}
}
