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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
