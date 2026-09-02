// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/orchestration"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// orchestrationConflictOutput is what the tests persist-check after the call:
// the orchestration state read back from the (fake) API server, not from the
// in-memory object the reconciler mutated.
type orchestrationConflictOutput struct {
	// StatusWrites counts status subresource update attempts, including the
	// rejected ones, proving the injected conflicts were actually hit.
	StatusWrites        int                       `json:"statusWrites"`
	CompletedIterations int                       `json:"completedIterations"`
	CurrentIteration    int                       `json:"currentIteration"`
	TotalGroups         int                       `json:"totalGroups"`
	IterationHistory    int                       `json:"iterationHistory"`
	Groups              []orchestrationGroupOut   `json:"groups"`
	Diagnose            *orchestrationDiagnoseOut `json:"diagnose,omitempty"`
	TrueCondition       string                    `json:"trueCondition"`
	Reason              string                    `json:"reason"`
}

type orchestrationGroupOut struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

type orchestrationDiagnoseOut struct {
	Stage string `json:"stage"`
	Round int    `json:"round"`
}

// newConflictInjectingClient builds a fake client seeded with wf whose status
// subresource rejects the first `conflicts` update attempts with a 409, the
// way a stale cached object loses the optimistic-concurrency race on a real
// API server. writes reports every attempt, rejected or not.
func newConflictInjectingClient(scheme *runtime.Scheme, wf *nvcrev1alpha1.Workflow, conflicts int, writes *int) client.Client {
	remaining := conflicts
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(wf).
		WithStatusSubresource(wf).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context, cl client.Client, subResourceName string,
				obj client.Object, opts ...client.SubResourceUpdateOption,
			) error {
				*writes++
				if remaining > 0 {
					remaining--
					return apierrors.NewConflict(
						schema.GroupResource{Group: nvcrev1alpha1.GroupVersion.Group, Resource: "workflows"},
						obj.GetName(), errors.New("simulated stale write"))
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
}

func newConflictTestScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	// setFinalStatus records succeeded nodes in a ConfigMap.
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

// seededConditions returns the InProgress/Succeeded/Failed trio as a mid-run
// Workflow carries them, so the tests exercise the transition writes the
// reconciler performs when an iteration completes.
func seededConditions() []metav1.Condition {
	return []metav1.Condition{
		{
			Type: nvcrev1alpha1.WorkflowInProgress, Status: metav1.ConditionTrue,
			Reason: ReasonJobRunning, Message: "Iteration running",
			ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
		},
		{
			Type: nvcrev1alpha1.WorkflowSucceeded, Status: metav1.ConditionFalse,
			Reason: ReasonNotApplicable, ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
		},
		{
			Type: nvcrev1alpha1.WorkflowFailed, Status: metav1.ConditionFalse,
			Reason: ReasonNotApplicable, ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
		},
	}
}

func persistedOrchestrationOutput(c client.Client, writes int) (string, error) {
	got := &nvcrev1alpha1.Workflow{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "wf", Namespace: "ns"}, got); err != nil {
		return "", err
	}

	out := orchestrationConflictOutput{StatusWrites: writes}
	if orch := got.Status.Orchestration; orch != nil {
		out.CompletedIterations = orch.CompletedIterations
		out.CurrentIteration = orch.CurrentIteration
		out.TotalGroups = orch.TotalGroups
		out.IterationHistory = len(orch.IterationHistory)
		for _, g := range orch.Groups {
			out.Groups = append(out.Groups, orchestrationGroupOut{Name: g.Name, Phase: string(g.Phase)})
		}
		if orch.Diagnose != nil {
			out.Diagnose = &orchestrationDiagnoseOut{Stage: orch.Diagnose.Stage, Round: orch.Diagnose.Round}
		}
	}
	for _, cond := range got.Status.Conditions {
		if cond.Status == metav1.ConditionTrue {
			out.TrueCondition = cond.Type
			out.Reason = cond.Reason
		}
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// TestHandleIterationCompleteSurvivesConflict is the regression test for the
// iteration-progress half of issue #120: handleIterationComplete increments
// CompletedIterations, snapshots the iteration history, and resets group
// phases on the cached object before the condition write. When that write
// loses the optimistic-concurrency race, updateStatusWithRetry re-fetches the
// Workflow in place and re-applies only closure mutations — so without
// applyOrchestration, the completed-iterations counter regressed and the
// finished iteration was re-processed.
func TestHandleIterationCompleteSurvivesConflict(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "iteration-complete-conflict",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Conflicts           int      `json:"conflicts"`
			Iterations          int      `json:"iterations"`
			CompletedIterations int      `json:"completedIterations"`
			GroupPhases         []string `json:"groupPhases"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme, err := newConflictTestScheme()
		if err != nil {
			return err
		}

		groups := make([]nvcrev1alpha1.GroupStatus, 0, len(in.GroupPhases))
		completion := metav1.Now()
		for i, phase := range in.GroupPhases {
			name := fmt.Sprintf("g%d", i)
			groups = append(groups, nvcrev1alpha1.GroupStatus{
				Name:  name,
				Nodes: []string{"node-" + name},
				Phase: nvcrev1alpha1.GroupPhase(phase),
				// The Jobs do not exist; the deletion loop tolerates NotFound.
				JobRef:         &nvcrev1alpha1.WorkloadReference{Name: name + "-job", Namespace: "ns"},
				CompletionTime: &completion,
			})
		}

		wf := &nvcrev1alpha1.Workflow{
			Name: "wf", Namespace: "ns", Generation: 1, UID: "01234567-89ab",
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{Iterations: in.Iterations},
			},
			Status: nvcrev1alpha1.WorkflowStatus{
				Conditions: seededConditions(),
				Orchestration: &nvcrev1alpha1.OrchestrationStatus{
					TotalGroups:         len(groups),
					CurrentIteration:    in.CompletedIterations + 1,
					CompletedIterations: in.CompletedIterations,
					Groups:              groups,
				},
			},
		}

		writes := 0
		c := newConflictInjectingClient(scheme, wf, in.Conflicts, &writes)

		r := &WorkflowReconciler{Client: c, Scheme: scheme, JobRequeueInterval: time.Second}
		if _, err := r.handleIterationComplete(context.Background(), wf, wf.Status.Orchestration); err != nil {
			return err
		}

		out, err := persistedOrchestrationOutput(c, writes)
		if err != nil {
			return err
		}
		tc.Actual = out
		return nil
	})
}

// TestDiagnoseSetGroupsSurvivesConflict is the regression test for the
// diagnose half of issue #120: diagnoseSetGroups replaces Groups, TotalGroups,
// and CurrentIteration on the cached object before the condition write.
// Without applyOrchestration, a 409 conflict re-fetch dropped the new groups
// and stage transition, so the same diagnose round ran again — duplicate
// groups and a Workflow looping past the intended iteration count.
func TestDiagnoseSetGroupsSurvivesConflict(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "diagnose-set-groups-conflict",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Conflicts           int    `json:"conflicts"`
			CompletedIterations int    `json:"completedIterations"`
			Stage               string `json:"stage"`
			Round               int    `json:"round"`
			Groups              []struct {
				Name  string   `json:"name"`
				Nodes []string `json:"nodes"`
			} `json:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme, err := newConflictTestScheme()
		if err != nil {
			return err
		}

		wf := &nvcrev1alpha1.Workflow{
			Name: "wf", Namespace: "ns", Generation: 1, UID: "01234567-89ab",
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{
					Diagnose: &nvcrev1alpha1.DiagnoseSpec{},
				},
			},
			Status: nvcrev1alpha1.WorkflowStatus{
				Conditions: seededConditions(),
				Orchestration: &nvcrev1alpha1.OrchestrationStatus{
					TotalGroups:         1,
					CurrentIteration:    in.CompletedIterations,
					CompletedIterations: in.CompletedIterations,
					Groups: []nvcrev1alpha1.GroupStatus{{
						Name: "previous-round", Nodes: []string{testNodeA, "node-b"},
						Phase: nvcrev1alpha1.GroupSucceeded,
					}},
					Diagnose: &nvcrev1alpha1.DiagnoseStatus{Stage: in.Stage, Round: in.Round},
				},
			},
		}

		nextGroups := make([]orchestration.Group, 0, len(in.Groups))
		for _, g := range in.Groups {
			nextGroups = append(nextGroups, orchestration.Group{Name: g.Name, Nodes: g.Nodes})
		}

		writes := 0
		c := newConflictInjectingClient(scheme, wf, in.Conflicts, &writes)

		r := &WorkflowReconciler{Client: c, Scheme: scheme, JobRequeueInterval: time.Second}
		orch := wf.Status.Orchestration
		if _, err := r.diagnoseSetGroups(context.Background(), wf, orch, orch.Diagnose, nextGroups, "advancing diagnose stage"); err != nil {
			return err
		}

		out, err := persistedOrchestrationOutput(c, writes)
		if err != nil {
			return err
		}
		tc.Actual = out
		return nil
	})
}
