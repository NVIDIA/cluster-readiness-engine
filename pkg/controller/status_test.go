// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func newWorkflowScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, crev1alpha1.AddToScheme(s))
	return s
}

// TestUpdateStatusWithRetryRecoversFromConflict is the behaviour the retry exists
// for: a stale cached object loses the optimistic-concurrency race, and the
// mutation is re-applied to freshly-read state instead of failing the reconcile.
func TestUpdateStatusWithRetryRecoversFromConflict(t *testing.T) {
	tests := []struct {
		name          string
		conflicts     int
		wantErr       bool
		wantMinWrites int
	}{
		{name: "no conflict writes once", conflicts: 0, wantErr: false, wantMinWrites: 1},
		{name: "single conflict retries and succeeds", conflicts: 1, wantErr: false, wantMinWrites: 2},
		{name: "conflicts beyond the retry budget surface an error", conflicts: 100, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			wf := &crev1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
			}

			remaining := tt.conflicts
			writes := 0

			c := fake.NewClientBuilder().
				WithScheme(newWorkflowScheme(t)).
				WithObjects(wf).
				WithStatusSubresource(&crev1alpha1.Workflow{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(
						ctx context.Context, cl client.Client, subResourceName string,
						obj client.Object, opts ...client.SubResourceUpdateOption,
					) error {
						writes++
						if remaining > 0 {
							remaining--
							return apierrors.NewConflict(
								schema.GroupResource{Group: crev1alpha1.GroupVersion.Group, Resource: "workflows"},
								obj.GetName(), errors.New("simulated stale write"))
						}
						return cl.Status().Update(ctx, obj, opts...)
					},
				}).
				Build()

			obj := &crev1alpha1.Workflow{}
			require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "wf", Namespace: "default"}, obj))

			mutations := 0
			err := updateStatusWithRetry(ctx, c, obj, func(w *crev1alpha1.Workflow) bool {
				mutations++
				return meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
					Type:    crev1alpha1.WorkflowInProgress,
					Status:  metav1.ConditionTrue,
					Reason:  ReasonJobRunning,
					Message: "running",
				})
			})

			if tt.wantErr {
				require.Error(t, err)
				require.True(t, apierrors.IsConflict(err), "expected the conflict to surface, got %v", err)
				return
			}

			require.NoError(t, err)
			require.GreaterOrEqual(t, writes, tt.wantMinWrites)

			// The condition must actually be persisted, not merely applied in memory.
			stored := &crev1alpha1.Workflow{}
			require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "wf", Namespace: "default"}, stored))
			require.True(t, CondIsTrue(stored.Status.Conditions, crev1alpha1.WorkflowInProgress))
		})
	}
}

// TestUpdateStatusWithRetrySkipsWriteWhenUnchanged keeps no-op reconciles from
// generating API traffic: reconcilers call the status setters unconditionally on
// every pass, so a mutation that changes nothing must not issue a write.
func TestUpdateStatusWithRetrySkipsWriteWhenUnchanged(t *testing.T) {
	ctx := context.Background()

	wf := &crev1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
	}

	writes := 0
	c := fake.NewClientBuilder().
		WithScheme(newWorkflowScheme(t)).
		WithObjects(wf).
		WithStatusSubresource(&crev1alpha1.Workflow{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context, cl client.Client, subResourceName string,
				obj client.Object, opts ...client.SubResourceUpdateOption,
			) error {
				writes++
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()

	obj := &crev1alpha1.Workflow{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "wf", Namespace: "default"}, obj))

	require.NoError(t, updateStatusWithRetry(ctx, c, obj, func(*crev1alpha1.Workflow) bool {
		return false
	}))
	require.Zero(t, writes, "a no-op mutation must not issue a status write")
}
