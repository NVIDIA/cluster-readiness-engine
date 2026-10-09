// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A reconcile that sees a Running group whose Job is gone or being deleted is
// about to fail that group, which cannot be undone. The Workflow itself deletes
// a group's Job on retry and between iterations and moves the group off Running
// in the same reconcile, so a reconcile started by the Job's deletion event can
// be reading a cached Workflow from before that write. The guard confirms the
// group against the API server first and leaves it alone when the cached view
// is behind, and the tail write carries over only the groups the pass changed,
// so a stale group is neither failed nor written back over the live state.
//
// The fake client plays the API server: the stored Workflow is the live object,
// and the copy handed to the reconcile is the cached view, edited to disagree.
func TestStaleGroupViewDoesNotFailRetriedGroup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "stale-group-view",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		type view struct {
			Phase   string `yaml:"phase"`
			JobRef  string `yaml:"jobRef"`
			Retries int    `yaml:"retries"`
		}
		var in struct {
			Groups []struct {
				Name string `yaml:"name"`
				// Cached is the view the reconcile runs on; Live is the object
				// the API server holds.
				Cached view `yaml:"cached"`
				Live   view `yaml:"live"`
				// Job: absent (no object), deleting (deletionTimestamp set,
				// finalizer held), failed (terminal WorkloadFailed, which takes
				// the retry arm), or none when the cached view has no JobRef.
				Job string `yaml:"job"`
			} `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		group := func(name string, v view) nvcrev1alpha1.GroupStatus {
			g := nvcrev1alpha1.GroupStatus{
				Name:    name,
				Phase:   nvcrev1alpha1.GroupPhase(v.Phase),
				Nodes:   []string{},
				Retries: v.Retries,
			}
			if v.JobRef != "" {
				g.JobRef = &nvcrev1alpha1.WorkloadReference{Name: v.JobRef, Namespace: "ns"}
			}
			return g
		}

		liveGroups := make([]nvcrev1alpha1.GroupStatus, 0, len(in.Groups))
		cachedGroups := make([]nvcrev1alpha1.GroupStatus, 0, len(in.Groups))
		var jobs []client.Object
		for _, gi := range in.Groups {
			liveGroups = append(liveGroups, group(gi.Name, gi.Live))
			cachedGroups = append(cachedGroups, group(gi.Name, gi.Cached))
			switch gi.Job {
			case "deleting":
				now := metav1.Now()
				jobs = append(jobs, &nvcrev1alpha1.Job{
					Name: gi.Cached.JobRef, Namespace: "ns",
					Finalizers:        []string{jobFinalizer},
					DeletionTimestamp: &now,
				})
			case "failed":
				jobs = append(jobs, &nvcrev1alpha1.Job{
					Name: gi.Cached.JobRef, Namespace: "ns",
					Status: nvcrev1alpha1.JobStatus{Conditions: []metav1.Condition{{
						Type: nvcrev1alpha1.JobFailed, Status: metav1.ConditionTrue,
						Reason: ReasonWorkloadFailed, LastTransitionTime: metav1.Now(),
					}}},
				})
			}
		}

		// The object is created in the cached state and then advanced to the
		// live state with a status write, so the cached copy carries the older
		// resourceVersion, as it would in a real informer. That is what makes
		// a write from the cached view conflict and go through the refetch,
		// which is where the per-group merge does its work.
		wf := &nvcrev1alpha1.Workflow{
			Name: "wf", Namespace: "ns", Generation: 1,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{
					Iterations: 1,
					Execution:  nvcrev1alpha1.ExecutionSpec{RetryFailedGroups: 1},
				},
			},
			Status: nvcrev1alpha1.WorkflowStatus{
				Orchestration: &nvcrev1alpha1.OrchestrationStatus{
					TotalGroups:      len(in.Groups),
					CurrentIteration: 1,
					Groups:           cachedGroups,
				},
			},
		}
		b := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(wf).WithStatusSubresource(wf).
			// The pod-drain barrier lists pods through this index. Registering it
			// makes the deleting cases pass the barrier on an empty list, the way
			// a drained job does, rather than on a list error.
			WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(obj client.Object) []string {
				pod, ok := obj.(*corev1.Pod)
				if !ok {
					return nil
				}
				if jn, found := pod.Labels[nodemonitor.NVCREJobLabel]; found {
					return []string{jn}
				}
				return nil
			})
		for _, j := range jobs {
			b = b.WithObjects(j).WithStatusSubresource(j)
		}
		c := b.Build()

		ctx := context.Background()
		cached := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, types.NamespacedName{Name: "wf", Namespace: "ns"}, cached); err != nil {
			return err
		}
		live := cached.DeepCopy()
		live.Status.Orchestration.Groups = liveGroups
		if err := c.Status().Update(ctx, live); err != nil {
			return err
		}

		r := &WorkflowReconciler{Client: c, Scheme: scheme, JobRequeueInterval: time.Second}
		res, err := r.updateStatusFromJobs(ctx, cached)
		if err != nil {
			return err
		}

		after := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, types.NamespacedName{Name: "wf", Namespace: "ns"}, after); err != nil {
			return err
		}
		type groupOut struct {
			Name          string `json:"name"`
			LivePhase     string `json:"livePhaseAfter"`
			LiveJobRef    string `json:"liveJobRefAfter"`
			LiveRetries   int    `json:"liveRetriesAfter"`
			HasCompletion bool   `json:"liveHasCompletionTime"`
		}
		out := struct {
			Deferred bool       `json:"deferred"`
			Groups   []groupOut `json:"groups"`
		}{Deferred: res.RequeueAfter == requeueImmediate}
		for _, ag := range after.Status.Orchestration.Groups {
			o := groupOut{
				Name: ag.Name, LivePhase: string(ag.Phase),
				LiveRetries: ag.Retries, HasCompletion: ag.CompletionTime != nil,
			}
			if ag.JobRef != nil {
				o.LiveJobRef = ag.JobRef.Name
			}
			out.Groups = append(out.Groups, o)
		}
		bts, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(bts) + "\n"
		return nil
	})
}
