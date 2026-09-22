// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// ADR-080 decision 5 prerequisite: the two generic catch-alls around
// createWorkflowForCategory must not rewrite the specific WorkflowFailed reason
// the Create path already persisted. Otherwise Events would say WorkflowFailed
// while Conditions settled on WorkflowValidationFailed, and the design's
// same-words promise would break.

// errCreateRejected stands in for an API server rejecting the Workflow Create
// for a reason that is not AlreadyExists (a webhook, a quota, an invalid field).
var errCreateRejected = errors.New("admission webhook denied the request")

// newRejectedCreateFixture wires a Certification whose child Workflow Create is
// rejected, with the status subresource behaving per mode.
func newRejectedCreateFixture(
	t *testing.T, mode statusFailureMode,
) (*CertificationReconciler, *nvcrev1alpha1.Certification, *events.FakeRecorder) {
	t.Helper()

	category := firstCatalogCategory(t)
	certification := &nvcrev1alpha1.Certification{
		Name: "reject-cert", Namespace: testNS,
		Spec: nvcrev1alpha1.CertificationSpec{
			Target: nvcrev1alpha1.TargetSpec{
				NodeSelector: map[string]string{GPUNodeLabel: present},
			},
			Categories: []nvcrev1alpha1.CertificateCategory{category},
		},
	}
	node := &corev1.Node{
		Name: testNodeA,
		Labels: map[string]string{
			GPUNodeLabel:        present,
			testGPUProductLabel: testGPUProductH100,
		},
		Spec: corev1.NodeSpec{ProviderID: testProviderIDAWS},
	}

	funcs := interceptor.Funcs{
		Create: func(
			ctx context.Context, c client.WithWatch, obj client.Object,
			opts ...client.CreateOption,
		) error {
			if _, ok := obj.(*nvcrev1alpha1.Workflow); ok {
				return errCreateRejected
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	switch mode {
	case statusWrites:
	case statusFailsHard:
		funcs.SubResourceUpdate = func(
			context.Context, client.Client, string, client.Object,
			...client.SubResourceUpdateOption,
		) error {
			return errSimulatedStatus
		}
	case statusFailsOnce:
		failStatus := true
		funcs.SubResourceUpdate = func(
			ctx context.Context, c client.Client, _ string, obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			if failStatus {
				failStatus = false
				return errSimulatedStatus
			}
			return c.Status().Update(ctx, obj, opts...)
		}
	case statusConflictsForever:
		funcs.SubResourceUpdate = func(
			_ context.Context, _ client.Client, _ string, obj client.Object,
			_ ...client.SubResourceUpdateOption,
		) error {
			return apierrors.NewConflict(
				schema.GroupResource{
					Group:    nvcrev1alpha1.GroupVersion.Group,
					Resource: "certifications",
				},
				obj.GetName(), errors.New("simulated stale write"))
		}
	}

	c := fake.NewClientBuilder().
		WithScheme(newFallbackScheme(t)).
		WithObjects(certification, node).
		WithStatusSubresource(&nvcrev1alpha1.Certification{}).
		WithInterceptorFuncs(funcs).
		Build()

	recorder := events.NewFakeRecorder(10)
	r := &CertificationReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}
	current := &nvcrev1alpha1.Certification{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(certification), current))
	return r, current, recorder
}

// firstCatalogCategory returns a real catalog entry so createWorkflowForCategory
// reaches its Create call instead of failing the lookup.
func firstCatalogCategory(t *testing.T) nvcrev1alpha1.CertificateCategory {
	t.Helper()
	entries := catalog.List()
	require.NotEmpty(t, entries, "catalog must be registered via blank import")
	return nvcrev1alpha1.CertificateCategory{
		Domain:  entries[0].Domain,
		Variant: entries[0].Variant,
	}
}

// TestCertificationRejectedCreatePersistsWorkflowFailed is the core of the
// decision 5 prerequisite: on a rejected Create with a successful status write,
// the persisted reason stays WorkflowFailed and the catch-all does not rewrite
// it to WorkflowValidationFailed.
func TestCertificationRejectedCreatePersistsWorkflowFailed(t *testing.T) {
	ctx := context.Background()
	r, certification, recorder := newRejectedCreateFixture(t, statusWrites)

	_, err := r.initializeCategoryStatuses(ctx, certification)
	require.Error(t, err)
	require.ErrorIs(t, err, errCreateRejected,
		"the Create error must survive so the reconcile retries")
	rejected, ok := errors.AsType[*workflowCreateRejectedError](err)
	require.True(t, ok, "callers identify the Create-rejected path by type")
	require.ErrorIs(t, rejected, errCreateRejected)

	stored := &nvcrev1alpha1.Certification{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(certification), stored))
	failed := meta.FindStatusCondition(stored.Status.Conditions, nvcrev1alpha1.CertificationFailed)
	require.NotNil(t, failed)
	require.Equal(t, metav1.ConditionTrue, failed.Status)
	require.Equal(t, ReasonWorkflowFailed, failed.Reason,
		"the generic catch-all must not overwrite the specific reason")

	// Two rows with distinct reasons: the rejected Create and the outcome.
	require.Len(t, recorder.Events, 2)
	action := <-recorder.Events
	transition := <-recorder.Events
	require.Contains(t, action, "Warning "+ReasonWorkflowCreationError)
	require.Contains(t, transition, "Warning "+ReasonWorkflowFailed)
}

func TestCertificationNextCategoryRejectedCreate(t *testing.T) {
	for _, mode := range []statusFailureMode{statusWrites, statusFailsHard, statusConflictsForever} {
		t.Run([]string{"persisted", "hard-error", "conflicts"}[mode], func(t *testing.T) {
			ctx := context.Background()
			r, certification, recorder := newRejectedCreateFixture(t, mode)
			certification.Status.CategoryStatuses = []nvcrev1alpha1.CertificationCategoryStatus{{}}
			_, err := r.processNextCategory(ctx, certification)
			require.ErrorIs(t, err, errCreateRejected)
			stored := &nvcrev1alpha1.Certification{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(certification), stored))
			failed := meta.FindStatusCondition(stored.Status.Conditions, nvcrev1alpha1.CertificationFailed)
			if mode == statusWrites {
				require.NotNil(t, failed)
				require.Equal(t, ReasonWorkflowFailed, failed.Reason)
				require.Len(t, recorder.Events, 2)
				require.Contains(t, <-recorder.Events, "Warning "+ReasonWorkflowCreationError)
				require.Contains(t, <-recorder.Events, "Warning "+ReasonWorkflowFailed)
			} else {
				require.Nil(t, failed)
				require.Len(t, recorder.Events, 1)
				require.Contains(t, <-recorder.Events, "Warning "+ReasonWorkflowCreationError)
				if mode == statusFailsHard {
					require.ErrorIs(t, err, errSimulatedStatus)
				} else {
					require.True(t, apierrors.IsConflict(err))
				}
			}
		})
	}
}

// TestCertificationRejectedCreateStatusFailureKeepsRetrying covers the failed
// inner status write. The Certification must stay non-terminal, both causes must
// be returned, the action Warning must remain, and no transition may be emitted
// for a phase that was never persisted.
func TestCertificationRejectedCreateStatusFailureKeepsRetrying(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode statusFailureMode
	}{
		{name: "non-conflict status error", mode: statusFailsHard},
		{name: "exhausted status conflicts", mode: statusConflictsForever},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			r, certification, recorder := newRejectedCreateFixture(t, tt.mode)

			_, err := r.initializeCategoryStatuses(ctx, certification)
			require.Error(t, err)
			require.ErrorIs(t, err, errCreateRejected,
				"the Create cause must be preserved alongside the status failure")
			if tt.mode == statusFailsHard {
				require.ErrorIs(t, err, errSimulatedStatus)
			} else {
				require.True(t, apierrors.IsConflict(err))
			}

			stored := &nvcrev1alpha1.Certification{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(certification), stored))
			require.Nil(t,
				meta.FindStatusCondition(stored.Status.Conditions, nvcrev1alpha1.CertificationFailed),
				"a failed status write must leave the Certification non-terminal so the retry can run")

			require.Len(t, recorder.Events, 1,
				"the action Warning is retained; no transition may claim an unpersisted phase")
			require.Contains(t, <-recorder.Events, "Warning "+ReasonWorkflowCreationError)
		})
	}
}

// TestCertificationRejectedCreateRetryEmitsTransitionOnce covers the sequence a
// real cluster produces: a failed status write, then a retry whose Create is
// rejected again but whose status write lands. The transition is emitted exactly
// once, on the successful persistence.
func TestCertificationRejectedCreateRetryEmitsTransitionOnce(t *testing.T) {
	for _, next := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial-category", true: "next-category"}[next], func(t *testing.T) {
			testCertificationRejectedCreateRetry(t, next)
		})
	}
}

func testCertificationRejectedCreateRetry(t *testing.T, next bool) {
	t.Helper()
	ctx := context.Background()
	r, certification, recorder := newRejectedCreateFixture(t, statusFailsOnce)

	for range 2 {
		current := &nvcrev1alpha1.Certification{}
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(certification), current))
		var err error
		if next {
			current.Status.CategoryStatuses = []nvcrev1alpha1.CertificationCategoryStatus{{}}
			_, err = r.processNextCategory(ctx, current)
		} else {
			_, err = r.initializeCategoryStatuses(ctx, current)
		}
		require.Error(t, err)
	}

	stored := &nvcrev1alpha1.Certification{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(certification), stored))
	failed := meta.FindStatusCondition(stored.Status.Conditions, nvcrev1alpha1.CertificationFailed)
	require.NotNil(t, failed)
	require.Equal(t, ReasonWorkflowFailed, failed.Reason)

	// Two action Warnings (one per rejected Create) and exactly one transition.
	var transitions int
	require.Len(t, recorder.Events, 3)
	for range 3 {
		if event := <-recorder.Events; !strings.Contains(event, ReasonWorkflowCreationError) {
			transitions++
			require.Contains(t, event, "Warning "+ReasonWorkflowFailed)
		}
	}
	require.Equal(t, 1, transitions,
		"only the successful status write may emit a transition")
}
