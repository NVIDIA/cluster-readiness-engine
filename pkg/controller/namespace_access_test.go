// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// accessTestMapper serves the dependency kinds the catalog uses, plus a
// cluster-scoped kind. A kind it does not serve stands for a CRD that is not
// installed.
func accessTestMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "ConfigMap"},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		{Group: "trainer.kubeflow.org", Version: "v1alpha1", Kind: "TrainingRuntime"},
		resourcev1.SchemeGroupVersion.WithKind("ResourceClaimTemplate"),
		{Group: "resource.nvidia.com", Version: "v1beta1", Kind: "ComputeDomain"},
	} {
		m.Add(gvk, meta.RESTScopeNamespace)
	}
	m.Add(schema.GroupVersionKind{Group: "trainer.kubeflow.org", Version: "v1alpha1", Kind: "ClusterTrainingRuntime"},
		meta.RESTScopeRoot)
	return m
}

// TestRequiredNamespaceAccess pins the permissions checked before a Workflow
// writes anything: the fixed set every Workflow needs, plus each namespaced
// dependency kind in the namespace it is created in.
func TestRequiredNamespaceAccess(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "required-namespace-access", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var workflow nvcrev1alpha1.Workflow
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input_workflow.yaml"]), &workflow); err != nil {
			return err
		}
		required := requiredNamespaceAccess(&workflow, accessTestMapper())
		out := map[string][]string{}
		for ns, checks := range required {
			for _, c := range checks {
				out[ns] = append(out[ns], c.String())
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

// TestSSARReviewerCache pins the review cache: answers are reused within the
// TTL, fresh bypasses them, and a failed review is never cached or read as
// permission.
func TestSSARReviewerCache(t *testing.T) {
	const ns = "team-a"
	check := AccessCheck{Resource: resourceConfigMaps, Verb: verbCreate}

	for _, tc := range []struct {
		name string
		// steps are reviews in order: advance the clock, then review.
		steps []struct {
			advance time.Duration
			fresh   bool
		}
		allowed     []bool // the API server's answer to each review it receives
		failCall    int    // 1-based review that fails; 0 for none
		wantCalls   int
		wantDenied  []bool
		wantErrStep int // 1-based step that returns an error; 0 for none
	}{
		{
			name: "answer is reused within the TTL",
			steps: []struct {
				advance time.Duration
				fresh   bool
			}{{}, {advance: 10 * time.Second}},
			allowed:    []bool{true},
			wantCalls:  1,
			wantDenied: []bool{false, false},
		},
		{
			name: "answer expires after the TTL",
			steps: []struct {
				advance time.Duration
				fresh   bool
			}{{}, {advance: accessReviewCacheTTL}},
			allowed:    []bool{false, true},
			wantCalls:  2,
			wantDenied: []bool{true, false},
		},
		{
			name: "fresh bypasses a cached answer",
			steps: []struct {
				advance time.Duration
				fresh   bool
			}{{}, {fresh: true}},
			allowed:    []bool{true, false},
			wantCalls:  2,
			wantDenied: []bool{false, true},
		},
		{
			name: "a failed review is not cached",
			steps: []struct {
				advance time.Duration
				fresh   bool
			}{{}, {}},
			allowed:     []bool{false, true},
			failCall:    1,
			wantCalls:   2,
			wantDenied:  []bool{false, false},
			wantErrStep: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			calls := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					review, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
					require.True(t, ok)
					require.Equal(t, ns, review.Spec.ResourceAttributes.Namespace)
					calls++
					if calls == tc.failCall {
						return errors.New("webhook timeout")
					}
					review.Status.Allowed = tc.allowed[calls-1]
					return nil
				},
			}).Build()

			now := time.Unix(1_700_000_000, 0)
			r := NewAccessReviewer(c).(*ssarReviewer)
			r.now = func() time.Time { return now }

			for i, step := range tc.steps {
				now = now.Add(step.advance)
				denied, err := r.Review(context.Background(), ns, []AccessCheck{check}, step.fresh)
				if tc.wantErrStep == i+1 {
					require.Error(t, err)
					continue
				}
				require.NoError(t, err)
				require.Equal(t, tc.wantDenied[i], slices.Contains(denied, check), "step %d", i+1)
			}
			require.Equal(t, tc.wantCalls, calls)
		})
	}
}

// TestSSARReviewerCachedDenials pins the cache-only read the check before
// observing and completing relies on: it reports a cached denial, never a
// cached grant, nothing once the answer expires, and never asks the API
// server.
func TestSSARReviewerCachedDenials(t *testing.T) {
	const ns = "team-a"
	create := AccessCheck{Resource: resourceConfigMaps, Verb: verbCreate}
	get := AccessCheck{Resource: resourceConfigMaps, Verb: verbGet}

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	calls := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			calls++
			review := obj.(*authorizationv1.SelfSubjectAccessReview)
			review.Status.Allowed = review.Spec.ResourceAttributes.Verb == verbGet
			return nil
		},
	}).Build()
	now := time.Unix(1_700_000_000, 0)
	r := NewAccessReviewer(c).(*ssarReviewer)
	r.now = func() time.Time { return now }

	require.Empty(t, r.CachedDenials(ns, []AccessCheck{create, get}), "nothing cached yet")
	_, err := r.Review(context.Background(), ns, []AccessCheck{create, get}, true)
	require.NoError(t, err)
	require.Equal(t, 2, calls)

	require.Equal(t, []AccessCheck{create}, r.CachedDenials(ns, []AccessCheck{create, get}))
	now = now.Add(accessReviewCacheTTL)
	require.Empty(t, r.CachedDenials(ns, []AccessCheck{create, get}), "expired answers are not denials")
	require.Equal(t, 2, calls, "CachedDenials never reviews")
}

// TestNodeResultsWriteError pins which node-result write failures end a pass:
// only Forbidden, which reconcileJob turns into a wait for access. Anything
// else stays best effort, logged and dropped, as before ADR-092.
func TestNodeResultsWriteError(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: resourceConfigMaps}, "results",
		errors.New("injected"))
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{name: "write succeeded"},
		{name: "forbidden ends the pass", err: forbidden, want: forbidden},
		{name: "other failure is dropped", err: errors.New("etcd request too large")},
		{name: "conflict is dropped", err: apierrors.NewConflict(schema.GroupResource{Resource: resourceConfigMaps},
			"results", errors.New("injected"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, nodeResultsWriteError(context.Background(), tc.err, "record"))
		})
	}
}

// TestRequeueAfterForbiddenWrite pins what a pass that ended on a forbidden
// write does. A denial or a review that cannot run records the wait in this
// pass, on the Workflow as persisted, never on the in-memory copy whose
// transitions the pass left unsaved; so the wait's backoff and timeout run
// even though failed reviews are not cached. A write the fresh review allows
// keeps its error, as an admission or quota rejection; anything else passes
// through untouched, and the review is always fresh.
func TestRequeueAfterForbiddenWrite(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: resourceConfigMaps}, "results",
		errors.New("injected"))
	other := errors.New("connection refused")
	deny := []AccessCheck{{Resource: resourceConfigMaps, Verb: verbCreate}}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		err         error
		denied      []AccessCheck
		reviewErr   error
		nilReviewer bool
		// waitingFor ages an existing NamespaceAccessCheckFailed wait.
		waitingFor time.Duration

		wantResult  ctrl.Result
		wantErr     error
		wantReviews []bool
		// wantTrue is the persisted exclusive condition's reason; empty
		// means nothing was written.
		wantTrue string
	}{
		{name: "no error"},
		{name: "other error passes through", err: other, wantErr: other},
		{name: "allowed keeps the error", err: forbidden, wantErr: forbidden, wantReviews: []bool{true}},
		{name: "no reviewer keeps the error", err: forbidden, nilReviewer: true, wantErr: forbidden},
		{name: "confirmed denial waits now", err: forbidden, denied: deny,
			wantResult: ctrl.Result{RequeueAfter: defaultWorkflowRequeueInterval}, wantReviews: []bool{true},
			wantTrue: ReasonNamespacePermissionsMissing},
		{name: "review error waits now", err: forbidden, reviewErr: other,
			wantResult: ctrl.Result{RequeueAfter: defaultWorkflowRequeueInterval}, wantReviews: []bool{true},
			wantTrue: ReasonNamespaceAccessCheckFailed},
		{name: "review error backs off", err: forbidden, reviewErr: other, waitingFor: time.Minute,
			wantResult: ctrl.Result{RequeueAfter: time.Minute}, wantReviews: []bool{true},
			wantTrue: ReasonNamespaceAccessCheckFailed},
		{name: "review error times out", err: forbidden, reviewErr: other, waitingFor: 11 * time.Minute,
			wantReviews: []bool{true}, wantTrue: ReasonNamespaceAccessCheckFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			persisted := &nvcrev1alpha1.Workflow{Name: testWorkflowName, Namespace: testNS, Generation: 1}
			if tc.waitingFor > 0 {
				persisted.Status.Conditions = []metav1.Condition{{
					Type: nvcrev1alpha1.WorkflowNamespaceAccess, Status: metav1.ConditionFalse,
					Reason: ReasonNamespaceAccessCheckFailed, Message: "waiting", ObservedGeneration: 1,
					LastTransitionTime: metav1.NewTime(now.Add(-tc.waitingFor)),
				}}
			}
			scheme := newFallbackScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(persisted).
				WithStatusSubresource(&nvcrev1alpha1.Workflow{}).Build()

			reviewer := &fakeAccessReviewer{denied: tc.denied, err: tc.reviewErr}
			r := &WorkflowReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}
			if !tc.nilReviewer {
				r.AccessReviewer = reviewer
			}

			// The pass's in-memory copy carries a transition it did not save.
			inMemory := persisted.DeepCopy()
			inMemory.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{CompletedIterations: 1}

			result, err := r.requeueAfterForbiddenWrite(context.Background(), inMemory)(ctrl.Result{}, tc.err)
			require.Equal(t, tc.wantResult, result)
			require.Equal(t, tc.wantErr, err)
			require.Equal(t, tc.wantReviews, reviewer.fresh)

			got := &nvcrev1alpha1.Workflow{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(persisted), got))
			require.Nil(t, got.Status.Orchestration, "the unsaved in-memory transition was persisted")
			var trueReason string
			for _, cond := range got.Status.Conditions {
				if cond.Status == metav1.ConditionTrue && cond.Type != nvcrev1alpha1.WorkflowNamespaceAccess {
					trueReason = cond.Reason
				}
			}
			require.Equal(t, tc.wantTrue, trueReason)
			if tc.waitingFor > 10*time.Minute {
				require.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, nvcrev1alpha1.WorkflowFailed),
					"a review that never runs must still end at the timeout")
			}
		})
	}
}
