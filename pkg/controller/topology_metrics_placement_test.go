// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestTopologyMetricsPlacement drives updateTopologyMetric over an Unpinned
// workflow, at both points in its life.
//
// Unpinned ignores the topology key rather than rejecting it, because the key
// arrives from a GB200/GB300 catalog override the user never wrote. Rejecting
// it would fail the exact Nemotron-on-GB200 spec that motivated ADR-089. So the
// key is present on the spec and the metrics code runs normally, which makes
// what it records worth pinning rather than assuming.
//
// An Unpinned group carries no Domains, so getNodeDomain's single-domain fast
// path never fires and every node resolves through a live label lookup. That is
// the opposite of how a Pinned group resolves, and it is only correct because
// the nodes exist to be read. Both cases below depend on that path.
func TestTopologyMetricsPlacement(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "topology-metrics",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			TopologyKey string `yaml:"topologyKey"`
			Placement   string `yaml:"placement"`
			// Nodes are the cluster objects the domain lookup reads. They exist
			// independently of the group, which is the point: before the step 9a
			// backfill the group names none of them.
			Nodes []struct {
				Name   string `yaml:"name"`
				Domain string `yaml:"domain"`
			} `yaml:"nodes"`
			Groups []struct {
				Name  string   `yaml:"name"`
				Phase string   `yaml:"phase"`
				Nodes []string `yaml:"nodes"`
			} `yaml:"groups"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		require.NoError(t, corev1.AddToScheme(scheme))
		require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

		objs := make([]client.Object, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := &corev1.Node{}
			node.Name = n.Name
			node.Labels = map[string]string{input.TopologyKey: n.Domain}
			objs = append(objs, node)
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

		groups := make([]nvcrev1alpha1.GroupStatus, 0, len(input.Groups))
		for _, g := range input.Groups {
			groups = append(groups, nvcrev1alpha1.GroupStatus{
				Name:  g.Name,
				Phase: nvcrev1alpha1.GroupPhase(g.Phase),
				Nodes: g.Nodes,
			})
		}

		wf := &nvcrev1alpha1.Workflow{}
		wf.Name = "wf-topo-placement-" + tc.Name
		wf.Namespace = "default"
		wf.Spec.Orchestration.Placement = input.Placement
		wf.Spec.Orchestration.Topology = &nvcrev1alpha1.TopologySpec{
			TopologyKey: input.TopologyKey,
		}
		wf.Status.Orchestration = &nvcrev1alpha1.OrchestrationStatus{
			Placement:   input.Placement,
			TotalGroups: len(groups),
			Groups:      groups,
		}

		r := &WorkflowReconciler{Client: c, Scheme: scheme}
		r.updateTopologyMetric(context.Background(), wf)

		// Read the gauges back per node rather than counting them, so a case can
		// show which domain a node landed in. A bare count would not distinguish
		// "recorded nothing" from "recorded under the wrong domain".
		type nodeGauge struct {
			Domain    string  `json:"domain"`
			Node      string  `json:"node"`
			Validated float64 `json:"validated"`
			Failed    float64 `json:"failed"`
		}
		var gauges []nodeGauge
		for _, n := range input.Nodes {
			v := promtest.ToFloat64(topologyValidatedNodesGauge.
				WithLabelValues(wf.Namespace, wf.Name, input.TopologyKey, n.Domain, n.Name))
			f := promtest.ToFloat64(topologyFailedNodesGauge.
				WithLabelValues(wf.Namespace, wf.Name, input.TopologyKey, n.Domain, n.Name))
			gauges = append(gauges, nodeGauge{
				Domain: n.Domain, Node: n.Name, Validated: v, Failed: f,
			})
		}
		sort.Slice(gauges, func(i, j int) bool {
			if gauges[i].Domain != gauges[j].Domain {
				return gauges[i].Domain < gauges[j].Domain
			}
			return gauges[i].Node < gauges[j].Node
		})

		data, err := json.MarshalIndent(gauges, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
