// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	trainer "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A group retry must survive anything that stops a reconcile partway (issue
// #389, gap 4): a later group's API error, a failed Workflow status write, or
// a restart between deleting the failed attempt and recording the retry. The
// retry used to delete the failed Job and its job-scoped dependencies first
// and record the retry after, so any of those left the group Running on a
// Job that was gone, and the next pass failed it with retry budget unused.
//
// Each case runs a sequence of passes, each one Workflow reconcile
// (reconcileJob), so a pass takes the branch production would: observe the
// Running groups while any group runs, launch the Pending ones only once none
// does. The fake client plays the API server, faults are injected on chosen
// passes, and the golden records what the API server holds after every pass,
// so it reads as the timeline of one retry.
//
// Group a is the one that fails. Group b, when present, keeps running and is
// where the later-group error is injected; while it runs, group a cannot
// launch, and its failed attempt must still be retired. Group a's job-scoped
// dependency is a ConfigMap the job template names, so it is copied per group
// and attempt.
func TestDurableGroupRetry(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "durable-group-retry", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			RetryFailedGroups int `json:"retryFailedGroups"`
			// MaxConcurrent limits how many groups run at once; 0 is unlimited.
			MaxConcurrent int `json:"maxConcurrent"`
			// GroupB adds a second group whose Job keeps running.
			GroupB bool `json:"groupB"`
			// GroupA is group a's persisted state. A Running group is on the Job
			// of attempt `retries`; a Pending one has recorded `retries`
			// already, so its previous attempt is `retries`-1.
			GroupA struct {
				Phase   string `json:"phase"`
				Retries int    `json:"retries"`
			} `json:"groupA"`
			// OldJob is the Job of group a's failed attempt: failed (terminal,
			// its pods gone), terminating (failed, deletion requested, held by
			// the Job controller's finalizer), or absent.
			OldJob string `json:"oldJob"`
			// CurrentJob creates the Job of the attempt being launched, as if
			// a previous pass had created it and stopped before recording it.
			CurrentJob bool `json:"currentJob"`
			// Dep is group a's job-scoped dependency copy: previous (left by
			// the failed attempt), previous-terminating (its delete requested,
			// held by a finalizer), current (created for the attempt being
			// launched), or absent.
			Dep string `json:"dep"`
			// DepTracked records a ref for the dependency in the Workflow.
			DepTracked bool `json:"depTracked"`
			// Faults, by pass number (1-based).
			GroupBJobGetErrorPasses []int `json:"groupBJobGetErrorPasses"`
			StatusWriteErrorPasses  []int `json:"statusWriteErrorPasses"`
			// ReleaseFinalizersAfterPass removes the finalizers that hold
			// terminating objects, after the given pass.
			ReleaseFinalizersAfterPass int `json:"releaseFinalizersAfterPass"`
			Passes                     int `json:"passes"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		ctx := context.Background()
		const (
			ns        = "ns"
			wfName    = "wf"
			depName   = "dep-cm"
			finalizer = "test.nvcre.nvidia.com/hold"
		)

		groups := []nvcrev1alpha1.GroupStatus{{Name: "a", Nodes: []string{testNodeA}}}
		if in.GroupB {
			groups = append(groups, nvcrev1alpha1.GroupStatus{Name: "b", Nodes: []string{testNodeB}})
		}
		workflow := &nvcrev1alpha1.Workflow{
			Name: wfName, Namespace: ns, UID: "wf-uid",
			Spec: nvcrev1alpha1.WorkflowSpec{
				Dependencies: []nvcrev1alpha1.DependencySpec{{Raw: []byte(
					`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + depName + `"}}`)}},
				JobTemplate: nvcrev1alpha1.JobTemplateSpec{Spec: nvcrev1alpha1.JobSpec{
					Workload: nvcrev1alpha1.WorkloadSpec{TrainJob: &trainer.TrainJobSpec{
						RuntimeRef: trainer.RuntimeRef{Name: depName},
					}},
				}},
				Orchestration: nvcrev1alpha1.OrchestrationSpec{
					Execution: nvcrev1alpha1.ExecutionSpec{
						RetryFailedGroups: in.RetryFailedGroups,
						MaxConcurrent:     in.MaxConcurrent,
					},
				},
			},
			Status: nvcrev1alpha1.WorkflowStatus{Orchestration: &nvcrev1alpha1.OrchestrationStatus{
				TotalNodes: len(groups), NodesPerJob: 1, TotalGroups: len(groups), CurrentIteration: 1,
				Groups: groups,
			}},
		}
		r := &WorkflowReconciler{Recorder: events.NewFakeRecorder(100)}
		jobName := func(group string, attempt int) string {
			return r.getGroupJobName(workflow, group, 1, attempt)
		}
		depCopy := depName + depJobSuffix(len(groups), false, "a", 1, wfName)

		ga := &workflow.Status.Orchestration.Groups[0]
		ga.Phase = nvcrev1alpha1.GroupPhase(in.GroupA.Phase)
		ga.Retries = in.GroupA.Retries
		failedAttempt := ga.Retries
		if ga.Phase == nvcrev1alpha1.GroupRunning {
			ga.JobRef = &nvcrev1alpha1.WorkloadReference{Kind: kindJob, Name: jobName("a", failedAttempt), Namespace: ns}
		} else {
			failedAttempt--
		}
		stampAttempt := func(obj client.Object, attempt int) {
			if attempt > 0 {
				obj.GetAnnotations()[annotationGroupAttempt] = strconv.Itoa(attempt)
			}
		}
		objects := []client.Object{}
		owned := func(obj client.Object) {
			obj.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(workflow,
				nvcrev1alpha1.GroupVersion.WithKind("Workflow"))})
		}
		failed := []metav1.Condition{{Type: nvcrev1alpha1.JobFailed, Status: metav1.ConditionTrue,
			Reason: ReasonWorkloadFailed, LastTransitionTime: metav1.Now()}}
		if in.OldJob != testAbsent {
			job := &nvcrev1alpha1.Job{Name: jobName("a", failedAttempt), Namespace: ns,
				Labels: map[string]string{"nvcre.nvidia.com/workflow": wfName, "nvcre.nvidia.com/group": "a"},
				Status: nvcrev1alpha1.JobStatus{Conditions: failed,
					FailedNodes: []nvcrev1alpha1.FailedNode{{Name: testNodeA, Reason: ReasonWorkloadFailed}}}}
			owned(job)
			if in.OldJob == "terminating" {
				job.Finalizers = []string{finalizer}
				job.DeletionTimestamp = new(metav1.Now())
			}
			objects = append(objects, job)
		}
		if in.CurrentJob {
			job := &nvcrev1alpha1.Job{Name: jobName("a", ga.Retries), Namespace: ns}
			owned(job)
			objects = append(objects, job)
		}
		if in.GroupB {
			gb := &workflow.Status.Orchestration.Groups[1]
			gb.Phase = nvcrev1alpha1.GroupRunning
			gb.JobRef = &nvcrev1alpha1.WorkloadReference{Kind: kindJob, Name: jobName("b", 0), Namespace: ns}
			job := &nvcrev1alpha1.Job{Name: jobName("b", 0), Namespace: ns}
			owned(job)
			objects = append(objects, job)
		}
		if in.Dep != testAbsent {
			dep := &corev1.ConfigMap{Name: depCopy, Namespace: ns,
				Labels:      map[string]string{labelWorkflowTracking: wfName},
				Annotations: map[string]string{annotationWorkflowUID: string(workflow.UID)}}
			switch in.Dep {
			case "previous":
				stampAttempt(dep, failedAttempt)
			case "previous-terminating":
				stampAttempt(dep, failedAttempt)
				dep.Finalizers = []string{finalizer}
				dep.DeletionTimestamp = new(metav1.Now())
			case "current":
				stampAttempt(dep, ga.Retries)
			default:
				return fmt.Errorf("unknown dep %q", in.Dep)
			}
			objects = append(objects, dep)
		}
		if in.DepTracked {
			workflow.Status.DependencyRefs = []nvcrev1alpha1.DependencyResourceRef{{
				APIVersion: "v1", Kind: kindConfigMap, Name: depCopy, Namespace: ns,
				Scope: scopeJob, GroupName: "a", Iteration: 1,
			}}
		}
		objects = append(objects, workflow)

		pass := 0
		failOn := func(passes []int) bool { return slices.Contains(passes, pass) }
		errInjected := errors.New("injected")
		c := fake.NewClientBuilder().WithScheme(newFallbackScheme(t)).WithObjects(objects...).
			WithStatusSubresource(&nvcrev1alpha1.Workflow{}, &nvcrev1alpha1.Job{}).
			WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(client.Object) []string { return nil }).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*nvcrev1alpha1.Job); ok && key.Name == jobName("b", 0) && failOn(in.GroupBJobGetErrorPasses) {
						return apierrors.NewInternalError(errInjected)
					}
					return c.Get(ctx, key, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if _, ok := obj.(*nvcrev1alpha1.Workflow); ok && failOn(in.StatusWriteErrorPasses) {
						return apierrors.NewInternalError(errInjected)
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
		r.Client, r.Scheme = c, c.Scheme()

		var timeline []map[string]any
		for pass = 1; pass <= in.Passes; pass++ {
			passErr := durableRetryPass(ctx, r, client.ObjectKeyFromObject(workflow))
			if pass == in.ReleaseFinalizersAfterPass {
				if err := releaseTestFinalizers(ctx, c, ns, finalizer); err != nil {
					return err
				}
			}
			snap, err := durableRetrySnapshot(ctx, c, client.ObjectKeyFromObject(workflow))
			if err != nil {
				return err
			}
			snap["pass"] = pass
			if passErr != nil {
				snap["error"] = passErr.Error()
			}
			timeline = append(timeline, snap)
		}
		data, err := json.MarshalIndent(timeline, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// durableRetryPass runs one Workflow reconcile from a fresh read, through the
// same dispatch production takes.
func durableRetryPass(ctx context.Context, r *WorkflowReconciler, key client.ObjectKey) error {
	wf := &nvcrev1alpha1.Workflow{}
	if err := r.Get(ctx, key, wf); err != nil {
		return err
	}
	_, err := r.reconcileJob(ctx, wf)
	return err
}

// releaseTestFinalizers lets every object held by the test finalizer finish
// deleting, as the controllers that own those finalizers eventually would.
func releaseTestFinalizers(ctx context.Context, c client.Client, ns, finalizer string) error {
	lists := []client.ObjectList{&nvcrev1alpha1.JobList{}, &corev1.ConfigMapList{}}
	for _, list := range lists {
		if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
			return err
		}
		items, err := metaItems(list)
		if err != nil {
			return err
		}
		for _, obj := range items {
			if !slices.Contains(obj.GetFinalizers(), finalizer) {
				continue
			}
			obj.SetFinalizers(slices.DeleteFunc(obj.GetFinalizers(), func(f string) bool { return f == finalizer }))
			if err := c.Update(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func metaItems(list client.ObjectList) ([]client.Object, error) {
	var out []client.Object
	switch l := list.(type) {
	case *nvcrev1alpha1.JobList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *corev1.ConfigMapList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	default:
		return nil, fmt.Errorf("unexpected list %T", list)
	}
	return out, nil
}

// durableRetrySnapshot records what the API server holds: group phases and
// retries, the Jobs and job-scoped dependency copies that exist (terminating
// ones marked), the Workflow's dependency refs, and the failed nodes recorded.
func durableRetrySnapshot(ctx context.Context, c client.Client, key client.ObjectKey) (map[string]any, error) {
	wf := &nvcrev1alpha1.Workflow{}
	if err := c.Get(ctx, key, wf); err != nil {
		return nil, err
	}
	groups := []map[string]any{}
	for _, g := range wf.Status.Orchestration.Groups {
		view := map[string]any{"name": g.Name, "phase": g.Phase, "retries": g.Retries}
		if g.JobRef != nil {
			view["jobRef"] = g.JobRef.Name
		}
		groups = append(groups, view)
	}
	describe := func(obj client.Object) string {
		s := obj.GetName()
		if a := obj.GetAnnotations()[annotationGroupAttempt]; a != "" {
			s += " attempt=" + a
		}
		if obj.GetDeletionTimestamp() != nil {
			s += " (terminating)"
		}
		return s
	}
	var jobs nvcrev1alpha1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(key.Namespace)); err != nil {
		return nil, err
	}
	jobNames := []string{}
	for i := range jobs.Items {
		jobNames = append(jobNames, describe(&jobs.Items[i]))
	}
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms, client.InNamespace(key.Namespace)); err != nil {
		return nil, err
	}
	deps := []string{}
	failedNodes := []string{}
	for i := range cms.Items {
		cm := &cms.Items[i]
		if wf.Status.FailedNodesRef != nil && cm.Name == wf.Status.FailedNodesRef.Name {
			nodes, err := noderesults.DecodeFailedNodesFromConfigMap(cm)
			if err != nil {
				return nil, err
			}
			for _, n := range nodes {
				failedNodes = append(failedNodes, n.Name+":"+string(n.Reason))
			}
			continue
		}
		if cm.Labels[labelWorkflowTracking] == key.Name {
			deps = append(deps, describe(cm))
		}
	}
	refs := []string{}
	for _, ref := range wf.Status.DependencyRefs {
		refs = append(refs, ref.Name)
	}
	return map[string]any{
		"groups": groups, "jobs": jobNames, "dependencies": deps,
		"dependencyRefs": refs, "failedNodes": failedNodes,
	}, nil
}
