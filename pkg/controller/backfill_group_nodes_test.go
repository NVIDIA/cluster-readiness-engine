// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// backfillGroupNodes is the only way an Unpinned group ever learns which
// machines ran it, so these cases pin the two gates that decide whether a
// placement is recorded, and the three ways the answer can be "not this one".
//
// The partial-placement gate is the subtle one and it is deliberately
// asymmetric. While the job runs, a prefix is refused: a multi-node job is
// routinely observed with some pods bound and the rest still Pending, and
// persisting the prefix would leave a short list that every later read treats
// as the whole group, because the len(g.Nodes) == 0 guard stops re-firing.
// Once the job is terminal the gate has to come off, since the caller's loop
// only visits groups that are still Running and a job that failed with half
// its pods bound is exactly the case attribution exists for.
//
// Pods carry spec.nodeName rather than a status phase for a reason:
// DiscoverPlacedNodesForJob keeps every phase on purpose, so a Succeeded or
// Failed pod still reports where it ran. The unbound case proves the one pod a
// placement question cannot answer is skipped rather than recorded as "".
func TestBackfillGroupNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "backfill-group-nodes",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Placement     string   `yaml:"placement"`
			NodesPerJob   int      `yaml:"nodesPerJob"`
			Terminal      bool     `yaml:"terminal"`
			ExistingNodes []string `yaml:"existingNodes"`
			NoDiscoverer  bool     `yaml:"noDiscoverer"`
			NoJob         bool     `yaml:"noJob"`
			Pods          []struct {
				Name     string `yaml:"name"`
				NodeName string `yaml:"nodeName"`
				Phase    string `yaml:"phase"`
				JobLabel string `yaml:"jobLabel"`
			} `yaml:"pods"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const ns = "ns"
		job := &nvcrev1alpha1.Job{Name: "wf-job", Namespace: ns}

		workflow := &nvcrev1alpha1.Workflow{
			Name: "wf", Namespace: ns,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{Placement: input.Placement},
			},
		}
		orch := &nvcrev1alpha1.OrchestrationStatus{NodesPerJob: input.NodesPerJob}
		g := &nvcrev1alpha1.GroupStatus{
			Name:  testGroupZero,
			Nodes: input.ExistingNodes,
			Phase: nvcrev1alpha1.GroupRunning,
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		// noJob leaves the Job object out of the cluster entirely, which is the
		// shape of the two deletion branches: the group is still Running, the
		// Job is gone, and the pods have not been collected yet.
		var objs []client.Object
		if !input.NoJob {
			objs = append(objs, job.DeepCopy())
		}
		for _, sp := range input.Pods {
			labelValue := sp.JobLabel
			if labelValue == "" {
				labelValue = job.Name
			}
			objs = append(objs, &corev1.Pod{
				Name: sp.Name, Namespace: ns,
				Labels: map[string]string{nodemonitor.NVCREJobLabel: labelValue},
				Spec:   corev1.PodSpec{NodeName: sp.NodeName},
				Status: corev1.PodStatus{Phase: corev1.PodPhase(sp.Phase)},
			})
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
			WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(obj client.Object) []string {
				pod, ok := obj.(*corev1.Pod)
				if !ok {
					return nil
				}
				if jn, found := pod.Labels[nodemonitor.NVCREJobLabel]; found {
					return []string{jn}
				}
				return nil
			}).
			Build()

		r := &WorkflowReconciler{Client: c, Scheme: scheme}
		if !input.NoDiscoverer {
			r.NodeDiscoverer = nodemonitor.NewNodeDiscoverer(c)
		}

		changed := r.backfillGroupNodes(
			context.Background(), workflow, orch, g, job.Namespace, job.Name, input.Terminal)

		out := struct {
			Changed    bool     `json:"changed"`
			GroupNodes []string `json:"groupNodes"`
		}{
			Changed:    changed,
			GroupNodes: g.Nodes,
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
