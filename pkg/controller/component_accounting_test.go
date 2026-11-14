// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestComponentAccounting pins the Workflow tier's side of ADR-090: the total
// it publishes, whether that total is marked final, and what retiring a
// superseded attempt leaves behind.
//
// Driven through the real status writers rather than by calling
// recomputeComponentResults directly, and that is the point rather than a
// stylistic choice. The finality marker is computed by the caller, so a test
// that called the recompute itself with final=true would have passed while no
// run in the system could ever publish a final total: every extra runs before
// applyExclusiveConditions does, so reading the conditions off the object saw
// the phase being replaced and never the one being applied. Only a test that
// goes through setWorkflowSucceeded can see that.
//
// Retirement is driven the same way, against real ConfigMaps in the client,
// because its bug was also invisible from the inside: it addressed shards in
// Status.Namespace, which nothing in the product writes, so every delete went
// to namespace "" and the shards it was meant to remove survived to break the
// bundle walk.
func TestComponentAccounting(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "component-accounting",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input accountingInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const (
			ns          = "ns"
			workflowUID = "0123456789abcdef-fixed-for-the-golden"
		)

		workflow := &nvcrev1alpha1.Workflow{Name: "wf", Namespace: ns}
		workflow.UID = types.UID(workflowUID)
		// Deliberately NOT setting Status.Namespace. The product never writes
		// it on a Workflow, so a fixture that sets it tests a path production
		// never takes, which is how the retirement namespace bug survived.
		workflow.Status.Conditions = []metav1.Condition{{
			Type:               nvcrev1alpha1.WorkflowInProgress,
			Status:             metav1.ConditionTrue,
			Reason:             "JobRunning",
			LastTransitionTime: metav1.Now(),
		}}
		workflow.Status.Orchestration = input.orchestration()

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		objs := []client.Object{workflow.DeepCopy()}
		for _, name := range input.ExistingShards {
			cm := &corev1.ConfigMap{}
			cm.Name = name
			cm.Namespace = ns
			objs = append(objs, cm)
		}

		builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
			WithStatusSubresource(&nvcrev1alpha1.Workflow{})
		if input.DeleteFails {
			builder = builder.WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(
					context.Context, client.WithWatch, client.Object, ...client.DeleteOption,
				) error {
					return fmt.Errorf("simulated delete failure")
				},
			})
		}
		c := builder.Build()

		r := &WorkflowReconciler{Client: c, Scheme: scheme}
		ctx := context.Background()

		live := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wf"}, live); err != nil {
			return err
		}

		out := accountingOutput{}

		// Retirement first, in memory, exactly as completeTerminalGroup does:
		// it mutates the group list that the status write then persists.
		if input.Retire {
			for i := range live.Status.Orchestration.Groups {
				r.retireComponentSeals(ctx, live, &live.Status.Orchestration.Groups[i])
			}
		}

		var err error
		switch input.Phase {
		case "Succeeded":
			err = r.setWorkflowSucceeded(ctx, live, "AllGroupsSucceeded", "done",
				applyOrchestration(live.Status.Orchestration))
		case "Failed":
			err = r.setWorkflowFailed(ctx, live, "GroupFailed", "failed",
				applyOrchestration(live.Status.Orchestration))
		default:
			err = r.setWorkflowInProgress(ctx, live, "JobRunning", "running",
				applyOrchestration(live.Status.Orchestration))
		}
		if err != nil {
			out.Error = err.Error()
		}

		// Read back: the total must be durable, not just in memory.
		persisted := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wf"}, persisted); err != nil {
			return err
		}
		if cr := persisted.Status.ComponentResults; cr != nil {
			out.Published = &publishedTotals{
				ShardCount:        cr.ShardCount,
				RowCount:          cr.RowCount,
				TruncatedRowCount: cr.TruncatedRowCount,
				DegradedCount:     cr.DegradedComponentCount,
				Selector:          cr.Selector,
				Final:             cr.Final,
			}
		}
		for _, g := range persisted.Status.Orchestration.Groups {
			for _, s := range g.ComponentSeals {
				out.RemainingSeals = append(out.RemainingSeals,
					fmt.Sprintf("%s attempt=%d job=%s shards=%d rows=%d phase=%s",
						g.Name, s.Attempt, s.JobName, s.ShardCount, s.RowCount, s.Phase))
			}
		}

		var cms corev1.ConfigMapList
		if err := c.List(ctx, &cms, client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range cms.Items {
			out.SurvivingShards = append(out.SurvivingShards, cms.Items[i].Name)
		}
		slices.Sort(out.SurvivingShards)

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// accountingInput is the test case description.
type accountingInput struct {
	// Phase is the condition this status write applies: Succeeded, Failed, or
	// anything else for InProgress. It is what decides finality.
	Phase string `json:"phase"`
	// Groups are the live groups and the seals they carry.
	Groups []accountingGroup `json:"groups"`
	// History is one entry per snapshotted iteration, each with one seal per
	// group, as snapshotIteration leaves it.
	History []accountingGroup `json:"history"`
	// Retired is fault isolation's folded total.
	Retired *accountingSeal `json:"retired"`
	// Retire runs retireComponentSeals over every group first.
	Retire bool `json:"retire"`
	// ExistingShards are ConfigMaps present before retirement runs.
	ExistingShards []string `json:"existingShards"`
	// DeleteFails makes every delete fail, so the test can pin that a seal
	// whose shards survived is kept rather than dropped.
	DeleteFails bool `json:"deleteFails"`
}

type accountingGroup struct {
	Name    string           `json:"name"`
	Retries int              `json:"retries"`
	Seals   []accountingSeal `json:"seals"`
}

type accountingSeal struct {
	Attempt           int32  `json:"attempt"`
	JobName           string `json:"jobName"`
	ShardCount        int32  `json:"shardCount"`
	RowCount          int64  `json:"rowCount"`
	TruncatedRowCount int64  `json:"truncatedRowCount"`
	DegradedCount     int32  `json:"degradedComponentCount"`
	Phase             string `json:"phase"`
}

func (s accountingSeal) seal() nvcrev1alpha1.GroupComponentSeal {
	phase := nvcrev1alpha1.ComponentResultPhase(s.Phase)
	if phase == "" {
		phase = nvcrev1alpha1.ComponentResultsComplete
	}
	return nvcrev1alpha1.GroupComponentSeal{
		Attempt:                s.Attempt,
		JobName:                s.JobName,
		ShardCount:             s.ShardCount,
		RowCount:               s.RowCount,
		TruncatedRowCount:      s.TruncatedRowCount,
		DegradedComponentCount: s.DegradedCount,
		Phase:                  phase,
	}
}

func (in accountingInput) orchestration() *nvcrev1alpha1.OrchestrationStatus {
	orch := &nvcrev1alpha1.OrchestrationStatus{}
	for _, g := range in.Groups {
		group := nvcrev1alpha1.GroupStatus{Name: g.Name, Retries: g.Retries}
		for _, s := range g.Seals {
			group.ComponentSeals = append(group.ComponentSeals, s.seal())
		}
		orch.Groups = append(orch.Groups, group)
	}
	for _, g := range in.History {
		it := nvcrev1alpha1.IterationResult{}
		for _, s := range g.Seals {
			sealed := s.seal()
			it.Groups = append(it.Groups, nvcrev1alpha1.GroupIterationResult{
				Name:          g.Name,
				ComponentSeal: &sealed,
			})
		}
		orch.IterationHistory = append(orch.IterationHistory, it)
	}
	if in.Retired != nil {
		s := in.Retired.seal()
		orch.RetiredComponentResults = &nvcrev1alpha1.ComponentResultTotals{
			ShardCount:             s.ShardCount,
			RowCount:               s.RowCount,
			TruncatedRowCount:      s.TruncatedRowCount,
			DegradedComponentCount: s.DegradedComponentCount,
		}
	}
	return orch
}

type accountingOutput struct {
	Published *publishedTotals `json:"published"`
	// RemainingSeals is what retirement left on the groups.
	RemainingSeals []string `json:"remainingSeals,omitempty"`
	// SurvivingShards is every ConfigMap still in the namespace. A superseded
	// attempt's shard appearing here is the bundle-breaking case.
	SurvivingShards []string `json:"survivingShards,omitempty"`
	Error           string   `json:"error,omitempty"`
}

type publishedTotals struct {
	ShardCount        int32  `json:"shardCount"`
	RowCount          int64  `json:"rowCount"`
	TruncatedRowCount int64  `json:"truncatedRowCount"`
	DegradedCount     int32  `json:"degradedComponentCount"`
	Selector          string `json:"selector"`
	Final             bool   `json:"final"`
}
