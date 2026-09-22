// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// ADR-080 decision 5: where a hand-placed action Warning shared its reason with
// the Failed transition, the transition owns the notification on success and the
// unconditional emission becomes a fallback on status-write failure. These tests
// pin both halves at the three Workflow-tier sites. The recorder is the observer
// because the golden projection only sees the aggregated Event.

// statusFailureMode selects how the fake client's status subresource behaves.
type statusFailureMode int

const (
	statusWrites statusFailureMode = iota
	statusFailsHard
	statusConflictsForever
	statusFailsOnce
)

func newFallbackScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, nvcrev1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// errSimulatedStatus is the non-conflict status error injected below. The
// fallback message must carry it verbatim so an operator can see both causes.
var errSimulatedStatus = errors.New("simulated status failure")

func newFallbackClient(
	t *testing.T, mode statusFailureMode, objs ...client.Object,
) client.Client {
	t.Helper()
	builder := fake.NewClientBuilder().
		WithScheme(newFallbackScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&nvcrev1alpha1.Workflow{})

	switch mode {
	case statusWrites:
	case statusFailsHard:
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				context.Context, client.Client, string, client.Object,
				...client.SubResourceUpdateOption,
			) error {
				return errSimulatedStatus
			},
		})
	case statusFailsOnce:
		failed := false
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, _ string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if !failed {
					failed = true
					return errSimulatedStatus
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		})
	case statusConflictsForever:
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				_ context.Context, _ client.Client, _ string, obj client.Object,
				_ ...client.SubResourceUpdateOption,
			) error {
				return apierrors.NewConflict(
					schema.GroupResource{
						Group:    nvcrev1alpha1.GroupVersion.Group,
						Resource: workflowResourceName,
					},
					obj.GetName(), errors.New("simulated stale write"))
			},
		})
	}
	return builder.Build()
}

// invalidPatchOverride is an always-matching override whose jsonPatch cannot be
// decoded, so applyOverrides fails deterministically. An empty WhenSpec matches
// every context.
func invalidPatchOverride() []nvcrev1alpha1.OverrideSpec {
	return []nvcrev1alpha1.OverrideSpec{{
		JobTemplatePatch: &apiextensionsv1.JSON{Raw: []byte(`{"not":"a patch array"}`)},
	}}
}

func newOverrideWorkflow() *nvcrev1alpha1.Workflow {
	return &nvcrev1alpha1.Workflow{
		Name: "override-workflow", Namespace: testNS,
		Spec: nvcrev1alpha1.WorkflowSpec{
			Overrides: invalidPatchOverride(),
		},
	}
}

// TestWorkflowEarlyOverrideGuardEvents covers the early applyOverrides guard in
// reconcileJob. It had no hand-placed emission before ADR-080, and decision 5
// gives it the same treatment as the tracked site so one reason is handled
// evenly.
func TestWorkflowEarlyOverrideGuardEvents(t *testing.T) {
	t.Run("successful status write emits only the transition", func(t *testing.T) {
		ctx := context.Background()
		workflow := newOverrideWorkflow()
		c := newFallbackClient(t, statusWrites, workflow)
		recorder := events.NewFakeRecorder(10)
		r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}

		current := &nvcrev1alpha1.Workflow{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workflow), current))
		_, err := r.reconcileJob(ctx, current)
		require.Error(t, err)

		require.Len(t, recorder.Events, 1)
		event := <-recorder.Events
		require.Contains(t, event, "Warning OverrideError")
		require.Contains(t, event, "Failed to apply overrides:",
			"the transition carries the condition's message, not the old Override failed: wording")
		require.NotContains(t, event, ReasonOverrideErrorStatusUpdateFailed)
	})

	t.Run("hard status error emits only the fallback", func(t *testing.T) {
		ctx := context.Background()
		workflow := newOverrideWorkflow()
		c := newFallbackClient(t, statusFailsHard, workflow)
		recorder := events.NewFakeRecorder(10)
		r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}

		current := &nvcrev1alpha1.Workflow{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workflow), current))
		_, err := r.reconcileJob(ctx, current)
		require.Error(t, err)

		require.Len(t, recorder.Events, 1)
		assertFallbackEvent(t, <-recorder.Events, ReasonOverrideErrorStatusUpdateFailed)
	})

	t.Run("conflict exhaustion emits one fallback, not one per attempt", func(t *testing.T) {
		ctx := context.Background()
		workflow := newOverrideWorkflow()
		c := newFallbackClient(t, statusConflictsForever, workflow)
		recorder := events.NewFakeRecorder(10)
		r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}

		current := &nvcrev1alpha1.Workflow{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workflow), current))
		_, err := r.reconcileJob(ctx, current)
		require.Error(t, err)

		require.Len(t, recorder.Events, 1,
			"the fallback is emitted after the retry budget is spent, not inside the loop")
		event := <-recorder.Events
		require.Contains(t, event, ReasonOverrideErrorStatusUpdateFailed)
	})
}

// TestWorkflowHeterogeneousPlatformEvents covers the discoverAndPartition site
// where an eventf(Warning) previously preceded a Failed write carrying the same
// reason and the same message.
func TestWorkflowHeterogeneousPlatformEvents(t *testing.T) {
	newFixture := func(t *testing.T, mode statusFailureMode) (*WorkflowReconciler, *nvcrev1alpha1.Workflow, *events.FakeRecorder) {
		t.Helper()
		workflow := &nvcrev1alpha1.Workflow{
			Name: "hetero-workflow", Namespace: testNS,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{
					Target: &nvcrev1alpha1.TargetSpec{
						NodeSelector: map[string]string{testGPUProductLabel: testGPUProductH100},
					},
				},
			},
		}
		// Two nodes on different clouds. detectPlatformConsistent treats that as
		// a misconfiguration rather than something to filter.
		awsNode := &corev1.Node{
			Name: "aws-node",
			Labels: map[string]string{
				testGPUProductLabel: testGPUProductH100,
				GPUNodeLabel:        present,
			},
			Spec: corev1.NodeSpec{ProviderID: testProviderIDAWS},
		}
		gcpNode := &corev1.Node{
			Name: "gcp-node",
			Labels: map[string]string{
				testGPUProductLabel: testGPUProductH100,
				GPUNodeLabel:        present,
			},
			Spec: corev1.NodeSpec{ProviderID: "gce://project/zone/instance"},
		}
		c := newFallbackClient(t, mode, workflow, awsNode, gcpNode)
		recorder := events.NewFakeRecorder(10)
		r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}
		current := &nvcrev1alpha1.Workflow{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(workflow), current))
		return r, current, recorder
	}

	t.Run("failed persistence then success emits distinct reasons", func(t *testing.T) {
		r, workflow, recorder := newFixture(t, statusFailsOnce)
		assertDiscoveryFallbackThenTransition(t, r, workflow, recorder,
			ReasonHeterogeneousPlatformStatusUpdateFailed, "HeterogeneousPlatform")
	})

	t.Run("successful status write emits only the transition", func(t *testing.T) {
		ctx := context.Background()
		r, workflow, recorder := newFixture(t, statusWrites)

		_, err := r.discoverAndPartition(ctx, workflow, r.ensureOrchestrationStatus(workflow))
		require.Error(t, err)

		require.Len(t, recorder.Events, 1,
			"the removed pre-write Warning and the transition would otherwise be two rows")
		event := <-recorder.Events
		require.Contains(t, event, "Warning HeterogeneousPlatform")
		require.Contains(t, event, "heterogeneous platforms detected")
	})

	t.Run("hard status error emits only the fallback", func(t *testing.T) {
		ctx := context.Background()
		r, workflow, recorder := newFixture(t, statusFailsHard)

		_, err := r.discoverAndPartition(ctx, workflow, r.ensureOrchestrationStatus(workflow))
		require.Error(t, err)

		require.Len(t, recorder.Events, 1)
		assertFallbackEvent(t, <-recorder.Events, ReasonHeterogeneousPlatformStatusUpdateFailed)
	})

	t.Run("conflict exhaustion emits one fallback", func(t *testing.T) {
		ctx := context.Background()
		r, workflow, recorder := newFixture(t, statusConflictsForever)

		_, err := r.discoverAndPartition(ctx, workflow, r.ensureOrchestrationStatus(workflow))
		require.Error(t, err)

		require.Len(t, recorder.Events, 1)
		require.Contains(t, <-recorder.Events, ReasonHeterogeneousPlatformStatusUpdateFailed)
	})
}

// TestWorkflowFallbackThenTransitionUseDistinctReasons pins the reason split
// that keeps a failed attempt and a later successful one from aggregating into
// one row: the fallback says the status write failed, the transition says the
// Workflow reached Failed.
func TestWorkflowFallbackThenTransitionUseDistinctReasons(t *testing.T) {
	ctx := context.Background()
	workflow := newOverrideWorkflow()

	failNext := true
	c := fake.NewClientBuilder().
		WithScheme(newFallbackScheme(t)).
		WithObjects(workflow).
		WithStatusSubresource(&nvcrev1alpha1.Workflow{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context, c client.Client, _ string, obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if failNext {
					failNext = false
					return errSimulatedStatus
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
	recorder := events.NewFakeRecorder(10)
	r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}

	// First reconcile: the status write fails, so only the fallback is emitted.
	first := &nvcrev1alpha1.Workflow{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workflow), first))
	_, err := r.reconcileJob(ctx, first)
	require.Error(t, err)

	// Second reconcile: persistence succeeds, so the transition is emitted with
	// the original reason.
	second := &nvcrev1alpha1.Workflow{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workflow), second))
	_, err = r.reconcileJob(ctx, second)
	require.Error(t, err)

	require.Len(t, recorder.Events, 2)
	fallback := <-recorder.Events
	transition := <-recorder.Events
	assertFallbackEvent(t, fallback, ReasonOverrideErrorStatusUpdateFailed)
	require.Contains(t, transition, "Warning OverrideError")
	require.NotContains(t, transition, ReasonOverrideErrorStatusUpdateFailed)
}

// TestWorkflowTransitionFallbackNilRecorder pins that the new fallback sites
// tolerate an unset Recorder, mirroring TestJobWarnfNilRecorder.
func TestWorkflowTransitionFallbackNilRecorder(t *testing.T) {
	ctx := context.Background()
	workflow := newOverrideWorkflow()
	c := newFallbackClient(t, statusFailsHard, workflow)
	r := &WorkflowReconciler{Client: c, Scheme: c.Scheme()}

	current := &nvcrev1alpha1.Workflow{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workflow), current))
	require.NotPanics(t, func() {
		_, err := r.reconcileJob(ctx, current)
		require.Error(t, err)
	})
}

// assertFallbackEvent checks decision 5's contract for a fallback row: it names
// the status-update failure under its own reason, carries both the original and
// the status error, and never claims the object reached Failed.
func assertFallbackEvent(t *testing.T, event, wantReason string) {
	t.Helper()
	require.Contains(t, event, "Warning "+wantReason)
	require.Contains(t, event, errSimulatedStatus.Error(),
		"the fallback must carry the status error")
	require.Contains(t, event, "also failed",
		"the fallback must say that recording the Failed condition was unsuccessful")
}

// TestWorkflowTrackedOverrideGuardEvents covers the second OverrideError site,
// the applyOverridesWithTracking call inside discoverAndPartition. It is reached
// only after platform detection succeeds, so its nodes share one platform.
func TestWorkflowTrackedOverrideGuardEvents(t *testing.T) {
	newFixture := func(t *testing.T, mode statusFailureMode) (*WorkflowReconciler, *nvcrev1alpha1.Workflow, *events.FakeRecorder) {
		t.Helper()
		workflow := &nvcrev1alpha1.Workflow{
			Name: "tracked-override-workflow", Namespace: testNS,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Overrides: invalidPatchOverride(),
				Orchestration: nvcrev1alpha1.OrchestrationSpec{
					Target: &nvcrev1alpha1.TargetSpec{
						NodeSelector: map[string]string{testGPUProductLabel: testGPUProductH100},
					},
				},
			},
		}
		node := &corev1.Node{
			Name: "aws-node",
			Labels: map[string]string{
				testGPUProductLabel: testGPUProductH100,
				GPUNodeLabel:        present,
			},
			Spec: corev1.NodeSpec{ProviderID: testProviderIDAWS},
		}
		c := newFallbackClient(t, mode, workflow, node)
		recorder := events.NewFakeRecorder(10)
		r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}
		current := &nvcrev1alpha1.Workflow{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(workflow), current))
		return r, current, recorder
	}

	t.Run("failed persistence then success emits distinct reasons", func(t *testing.T) {
		r, workflow, recorder := newFixture(t, statusFailsOnce)
		assertDiscoveryFallbackThenTransition(t, r, workflow, recorder,
			ReasonOverrideErrorStatusUpdateFailed, "OverrideError")
	})

	t.Run("conflict exhaustion emits one fallback", func(t *testing.T) {
		r, workflow, recorder := newFixture(t, statusConflictsForever)
		_, err := r.discoverAndPartition(context.Background(), workflow, r.ensureOrchestrationStatus(workflow))
		require.Error(t, err)
		require.Len(t, recorder.Events, 1)
		require.Contains(t, <-recorder.Events, ReasonOverrideErrorStatusUpdateFailed)
	})

	t.Run("successful status write emits only the transition", func(t *testing.T) {
		ctx := context.Background()
		r, workflow, recorder := newFixture(t, statusWrites)

		_, err := r.discoverAndPartition(ctx, workflow, r.ensureOrchestrationStatus(workflow))
		require.Error(t, err)

		require.Len(t, recorder.Events, 1)
		event := <-recorder.Events
		require.Contains(t, event, "Warning OverrideError")
		require.Contains(t, event, "Failed to apply overrides:",
			"the transition message is the condition's, replacing the old Override failed: wording")
	})

	t.Run("hard status error emits only the fallback", func(t *testing.T) {
		ctx := context.Background()
		r, workflow, recorder := newFixture(t, statusFailsHard)

		_, err := r.discoverAndPartition(ctx, workflow, r.ensureOrchestrationStatus(workflow))
		require.Error(t, err)

		require.Len(t, recorder.Events, 1)
		assertFallbackEvent(t, <-recorder.Events, ReasonOverrideErrorStatusUpdateFailed)
	})
}

func assertDiscoveryFallbackThenTransition(t *testing.T, r *WorkflowReconciler, workflow *nvcrev1alpha1.Workflow,
	recorder *events.FakeRecorder, fallbackReason, transitionReason string,
) {
	t.Helper()
	ctx := context.Background()
	for range 2 {
		current := &nvcrev1alpha1.Workflow{}
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(workflow), current))
		_, err := r.discoverAndPartition(ctx, current, r.ensureOrchestrationStatus(current))
		require.Error(t, err)
	}
	require.Len(t, recorder.Events, 2)
	assertFallbackEvent(t, <-recorder.Events, fallbackReason)
	transition := <-recorder.Events
	require.Contains(t, transition, "Warning "+transitionReason+" ")
	require.NotContains(t, transition, fallbackReason)
}

// TestWorkflowOverrideAppliedEmitsPerOverride pins that the retained
// informational events are untouched by ADR-080. They legitimately repeat one
// reason with different messages, which is why the design keeps them out of the
// golden projection and asserts them here instead: the recorder sees every call,
// while the API aggregates them into one row and loses the individual messages.
func TestWorkflowOverrideAppliedEmitsPerOverride(t *testing.T) {
	ctx := context.Background()
	workflow := &nvcrev1alpha1.Workflow{
		Name: "multi-override-workflow", Namespace: testNS,
		Spec: nvcrev1alpha1.WorkflowSpec{Overrides: []nvcrev1alpha1.OverrideSpec{
			{JobTemplatePatch: &apiextensionsv1.JSON{Raw: []byte(`[{"op":"add","path":"/metadata","value":{"labels":{"first":"applied"}}}]`)}},
			{JobTemplatePatch: &apiextensionsv1.JSON{Raw: []byte(`[{"op":"add","path":"/metadata/labels/second","value":"applied"}]`)}},
		}},
	}
	c := newFallbackClient(t, statusWrites, workflow)
	recorder := events.NewFakeRecorder(10)
	r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}

	applied, err := applyOverridesWithTracking(&workflow.Spec, OverrideContext{})
	require.NoError(t, err)
	require.Len(t, applied, 2)
	for _, result := range applied {
		require.False(t, result.NoOp)
	}
	r.logOverrideResults(ctx, workflow, applied, OverrideContext{})
	require.Len(t, recorder.Events, 2)
	first, second := <-recorder.Events, <-recorder.Events
	require.Contains(t, first, "Normal OverrideApplied Override[0] matched")
	require.Contains(t, second, "Normal OverrideApplied Override[1] matched")
	require.Contains(t, first, "patching jobTemplatePatch")
	require.Contains(t, second, "patching jobTemplatePatch")
}

// TestWorkflowNoOverridesMatchedEvent pins the other retained informational
// event, which fires only when overrides exist and none matched.
func TestWorkflowNoOverridesMatchedEvent(t *testing.T) {
	ctx := context.Background()
	workflow := &nvcrev1alpha1.Workflow{
		Name: "unmatched-override-workflow", Namespace: testNS,
		Spec: nvcrev1alpha1.WorkflowSpec{Overrides: invalidPatchOverride()},
	}
	c := newFallbackClient(t, statusWrites, workflow)
	recorder := events.NewFakeRecorder(10)
	r := &WorkflowReconciler{Client: c, Scheme: c.Scheme(), Recorder: recorder}

	r.logOverrideResults(ctx, workflow, nil, OverrideContext{Platform: testPlatformAWS, GPUArchitecture: "h100"})

	require.Len(t, recorder.Events, 1)
	require.Equal(t,
		"Normal NoOverridesMatched 0 of 1 overrides matched (platform=aws, gpuArchitecture=h100)",
		<-recorder.Events)
}
