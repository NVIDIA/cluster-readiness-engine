// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	gzip "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// multiIterationInput describes a whole run, iteration by iteration. Each
// group runs one or more attempts per iteration; every attempt but the last is
// a failure that the group's retry budget absorbs.
type multiIterationInput struct {
	Placement         string `json:"placement"`
	RetryFailedGroups int    `json:"retryFailedGroups"`
	// DeleteWorkflow runs handleDeletion after the terminal status, to show
	// which Jobs survive the Workflow's teardown.
	DeleteWorkflow bool                      `json:"deleteWorkflow"`
	Iterations     []multiIterationIteration `json:"iterations"`
}

type multiIterationIteration struct {
	Groups []multiIterationGroup `json:"groups"`
}

type multiIterationGroup struct {
	Name     string                `json:"name"`
	Nodes    []string              `json:"nodes"`
	Attempts []multiIterationJobIn `json:"attempts"`
}

// multiIterationJobIn is the terminal state the Job tier left an attempt in.
type multiIterationJobIn struct {
	// Outcome is one of: succeeded, threshold (Succeeded plus
	// ValidationFailed), hardware (HardwareFailed plus Failed), workload
	// (Failed).
	Outcome string `json:"outcome"`
	// FailedNodes overrides the nodes the Job reports as failed. Defaults to
	// the group's nodes for every outcome but succeeded.
	FailedNodes []string `json:"failedNodes,omitempty"`
}

// Attempt outcomes in input.yaml.
const (
	miOutcomeSucceeded = "succeeded"
	miOutcomeThreshold = "threshold"
	miOutcomeHardware  = "hardware"
	miOutcomeWorkload  = "workload"
)

type multiIterationOutput struct {
	Terminal         string                     `json:"terminal"`
	Reason           string                     `json:"reason"`
	Message          string                     `json:"message"`
	ValidationFailed bool                       `json:"validationFailedCondition"`
	IterationHistory []multiIterationHistoryOut `json:"iterationHistory"`
	SucceededNodes   []string                   `json:"succeededNodes"`
	FailedNodes      []string                   `json:"failedNodes"`
	Jobs             []string                   `json:"jobs"`
	JobsAfterDelete  *[]string                  `json:"jobsAfterWorkflowDeletion,omitempty"`
}

type multiIterationHistoryOut struct {
	Iteration int      `json:"iteration"`
	Groups    []string `json:"groups"`
}

// TestMultiIterationFailure drives a multi-iteration run through the real
// terminal path (completeTerminalGroup for every attempt, handleIterationComplete
// for every iteration, which ends in setFinalStatus) and pins issue #470:
//
//   - A group that fails an earlier iteration keeps its Job across the
//     rollover, so the final reason comes from it: a threshold miss in
//     iteration 1 ends in JobValidationFailed with the ValidationFailed
//     condition, and a hardware failure in JobHardwareFailed, instead of the
//     generic IterationsFailed. The Jobs of succeeded groups are still deleted
//     at rollover.
//   - On terminal failure a node that failed any iteration is not in the
//     succeeded-nodes list (ADR-062), including under Unpinned placement where
//     iteration 2 ran on different nodes.
//   - The kept Job is deleted with the Workflow.
//   - A group that fails and then passes on retry within one iteration is
//     pinned as it behaves today on a Succeeded Workflow: its nodes are in both
//     lists. That case is tracked separately from #470.
func TestMultiIterationFailure(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "multi-iteration-failure",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in multiIterationInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		out, err := runMultiIteration(context.Background(), in)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func runMultiIteration(ctx context.Context, in multiIterationInput) (*multiIterationOutput, error) {
	const ns = "ns"
	scheme, err := newConflictTestScheme()
	if err != nil {
		return nil, err
	}

	wf := &nvcrev1alpha1.Workflow{
		Name: "wf", Namespace: ns, Generation: 1, UID: "47000000-0000",
		Finalizers: []string{workflowFinalizer},
		Spec: nvcrev1alpha1.WorkflowSpec{
			Orchestration: nvcrev1alpha1.OrchestrationSpec{
				Iterations: len(in.Iterations),
				Placement:  in.Placement,
				Execution:  nvcrev1alpha1.ExecutionSpec{RetryFailedGroups: in.RetryFailedGroups},
			},
		},
		Status: nvcrev1alpha1.WorkflowStatus{
			Conditions:    seededConditions(),
			Orchestration: &nvcrev1alpha1.OrchestrationStatus{CurrentIteration: 1},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(wf.DeepCopy()).
		WithStatusSubresource(&nvcrev1alpha1.Workflow{}).
		WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(client.Object) []string { return nil }).
		Build()
	r := &WorkflowReconciler{Client: c, APIReader: c, Scheme: scheme, JobRequeueInterval: time.Second}

	// persist writes the in-memory status to the fake API server, the way
	// the reconciler's transition writes do, so groupViewIsStale sees the
	// state the next call acts on.
	persist := func() error {
		live := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(wf), live); err != nil {
			return err
		}
		wf.Status.DeepCopyInto(&live.Status)
		if err := c.Status().Update(ctx, live); err != nil {
			return err
		}
		wf.ResourceVersion = live.ResourceVersion
		return nil
	}

	for i, iter := range in.Iterations {
		next, err := runOneIteration(ctx, c, r, wf, persist, iter, i+1)
		if err != nil {
			return nil, err
		}
		wf = next
	}

	out := &multiIterationOutput{IterationHistory: []multiIterationHistoryOut{}}
	for _, cond := range wf.Status.Conditions {
		if cond.Status != metav1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case nvcrev1alpha1.WorkflowSucceeded, nvcrev1alpha1.WorkflowFailed:
			out.Terminal, out.Reason, out.Message = cond.Type, cond.Reason, cond.Message
		case nvcrev1alpha1.WorkflowValidationFailed:
			out.ValidationFailed = true
		}
	}
	if out.Terminal == "" {
		return nil, fmt.Errorf("workflow did not reach a terminal condition: %+v", wf.Status.Conditions)
	}
	for _, h := range wf.Status.Orchestration.IterationHistory {
		row := multiIterationHistoryOut{Iteration: h.Iteration}
		for _, g := range h.Groups {
			row.Groups = append(row.Groups, fmt.Sprintf("%s %s %s", g.Name, g.Phase, g.JobName))
		}
		out.IterationHistory = append(out.IterationHistory, row)
	}
	if out.SucceededNodes, err = readMultiIterationSucceeded(ctx, c, ns, wf.Status.SucceededNodesRef); err != nil {
		return nil, err
	}
	if out.FailedNodes, err = readMultiIterationFailed(ctx, c, ns, wf.Status.FailedNodesRef); err != nil {
		return nil, err
	}
	if out.Jobs, err = listMultiIterationJobs(ctx, c, ns); err != nil {
		return nil, err
	}

	if in.DeleteWorkflow {
		// handleDeletion waits for Jobs to drain; the fake client deletes a
		// Job without finalizers at once, so a second pass sees them gone.
		for range 3 {
			if _, err := r.handleDeletion(ctx, wf); err != nil {
				return nil, err
			}
		}
		after, err := listMultiIterationJobs(ctx, c, ns)
		if err != nil {
			return nil, err
		}
		out.JobsAfterDelete = &after
	}
	return out, nil
}

// runOneIteration runs every group's attempts for one iteration through
// completeTerminalGroup, then handleIterationComplete, and returns the
// Workflow as the API server holds it afterwards.
func runOneIteration(
	ctx context.Context, c client.Client, r *WorkflowReconciler, wf *nvcrev1alpha1.Workflow,
	persist func() error, iter multiIterationIteration, iteration int,
) (*nvcrev1alpha1.Workflow, error) {
	const ns = "ns"
	orch := wf.Status.Orchestration
	if iteration == 1 {
		for _, g := range iter.Groups {
			orch.Groups = append(orch.Groups, nvcrev1alpha1.GroupStatus{Name: g.Name, Phase: nvcrev1alpha1.GroupPending})
		}
		orch.TotalGroups = len(orch.Groups)
	}
	if orch.CurrentIteration != iteration {
		return nil, fmt.Errorf("iteration %d: controller is on iteration %d", iteration, orch.CurrentIteration)
	}
	for gi, gin := range iter.Groups {
		g := &orch.Groups[gi]
		if g.Name != gin.Name {
			return nil, fmt.Errorf("iteration %d: group %d is %q, input has %q", iteration, gi, g.Name, gin.Name)
		}
		for ai, att := range gin.Attempts {
			job, err := newMultiIterationJob(ns, gin, att, iteration, ai)
			if err != nil {
				return nil, err
			}
			if err := c.Create(ctx, job.DeepCopy()); err != nil {
				return nil, err
			}
			now := metav1.Now()
			g.Phase = nvcrev1alpha1.GroupRunning
			g.Nodes = append([]string(nil), gin.Nodes...)
			g.StartTime = &now
			g.JobRef = &nvcrev1alpha1.WorkloadReference{
				APIVersion: nvcrev1alpha1.GroupVersion.String(), Kind: kindJob, Name: job.Name, Namespace: ns,
			}
			if err := persist(); err != nil {
				return nil, err
			}
			outcome, err := r.completeTerminalGroup(ctx, wf, orch, g, job, getJobTerminalState(job))
			if err != nil {
				return nil, err
			}
			if outcome != groupChanged {
				return nil, fmt.Errorf("iteration %d group %s attempt %d: outcome %d, want changed",
					iteration, g.Name, ai, outcome)
			}
			last := ai == len(gin.Attempts)-1
			if retried := g.Phase == nvcrev1alpha1.GroupPending; retried == last {
				return nil, fmt.Errorf("iteration %d group %s attempt %d: phase %s does not match the input",
					iteration, g.Name, ai, g.Phase)
			}
		}
	}
	if err := persist(); err != nil {
		return nil, err
	}
	if _, err := r.handleIterationComplete(ctx, wf, orch); err != nil {
		return nil, err
	}
	// Start the next pass from what the API server holds.
	fresh := &nvcrev1alpha1.Workflow{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(wf), fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func newMultiIterationJob(ns string, g multiIterationGroup, att multiIterationJobIn, iteration, attempt int) (*nvcrev1alpha1.Job, error) {
	name := fmt.Sprintf("wf-%s-iter-%d", g.Name, iteration)
	if attempt > 0 {
		name = fmt.Sprintf("%s-retry-%d", name, attempt)
	}
	ts := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cond := func(t, reason string) metav1.Condition {
		return metav1.Condition{Type: t, Status: metav1.ConditionTrue, Reason: reason, LastTransitionTime: ts}
	}
	job := &nvcrev1alpha1.Job{
		Name: name, Namespace: ns,
		Labels: map[string]string{labelWorkflowTracking: "wf", "nvcre.nvidia.com/group": g.Name},
	}
	failedNodes := att.FailedNodes
	if failedNodes == nil {
		failedNodes = g.Nodes
	}
	var reason nvcrev1alpha1.NodeFailureReason
	switch att.Outcome {
	case miOutcomeSucceeded:
		job.Status.Conditions = []metav1.Condition{cond(nvcrev1alpha1.JobSucceeded, "WorkloadCompleted")}
		return job, nil
	case miOutcomeThreshold:
		job.Status.Conditions = []metav1.Condition{
			cond(nvcrev1alpha1.JobSucceeded, "WorkloadCompleted"),
			cond(nvcrev1alpha1.JobValidationFailed, "ThresholdViolated"),
		}
		reason = ReasonThresholdViolation
	case miOutcomeHardware:
		job.Status.Conditions = []metav1.Condition{
			cond(nvcrev1alpha1.JobHardwareFailed, "XidError"),
			cond(nvcrev1alpha1.JobFailed, "HardwareFailure"),
		}
		reason = ReasonHardwareFailureDetected
	case miOutcomeWorkload:
		job.Status.Conditions = []metav1.Condition{cond(nvcrev1alpha1.JobFailed, "WorkloadFailed")}
		reason = ReasonWorkloadFailed
	default:
		return nil, fmt.Errorf("unknown outcome %q", att.Outcome)
	}
	job.Status.FailedNodes = noderesults.NodesWithFailureDetails(failedNodes, reason, att.Outcome)
	if meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobFailed) == nil &&
		meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobValidationFailed) == nil {
		return nil, fmt.Errorf("outcome %q produced no failure", att.Outcome)
	}
	return job, nil
}

func readMultiIterationSucceeded(ctx context.Context, c client.Client, ns string, ref *corev1.TypedLocalObjectReference) ([]string, error) {
	nodes := []string{}
	if ref == nil {
		return nodes, nil
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, cm); err != nil {
		return nil, err
	}
	csv, err := gzip.GunzipString(cm.BinaryData[noderesults.SucceededNodesConfigMapKey])
	if err != nil {
		return nil, err
	}
	for n := range strings.SplitSeq(csv, ",") {
		if n != "" {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

func readMultiIterationFailed(ctx context.Context, c client.Client, ns string, ref *corev1.TypedLocalObjectReference) ([]string, error) {
	nodes := []string{}
	if ref == nil {
		return nodes, nil
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nodes, nil
		}
		return nil, err
	}
	failed, err := noderesults.DecodeFailedNodesFromConfigMap(cm)
	if err != nil {
		return nil, err
	}
	for _, fn := range failed {
		nodes = append(nodes, fn.Name+" "+string(fn.Reason))
	}
	return nodes, nil
}

func listMultiIterationJobs(ctx context.Context, c client.Client, ns string) ([]string, error) {
	var list nvcrev1alpha1.JobList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	names := []string{}
	for _, j := range list.Items {
		names = append(names, j.Name)
	}
	sort.Strings(names)
	return names, nil
}
