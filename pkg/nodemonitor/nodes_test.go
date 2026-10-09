// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodemonitor

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestDiscoverNodes covers both discoverers against the same pod set, because
// the difference between them is the whole point and is invisible when each is
// tested alone.
//
// DiscoverNodesForJob answers "where is this job alive right now" and filters to
// Running or Pending. DiscoverPlacedNodesForJob answers "where did this job run"
// and keeps every bound pod whatever its phase. Attribution needs the second
// one: a job that fails fast leaves Failed pods behind, so the phase-filtered
// helper would report no nodes in exactly the case failed-node attribution
// exists for, and an Unpinned run would name no failed nodes at all.
func TestDiscoverNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "discover-nodes",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Namespace string `yaml:"namespace"`
			JobName   string `yaml:"jobName"`
			Pods      []struct {
				Name     string `yaml:"name"`
				JobLabel string `yaml:"jobLabel"`
				NodeName string `yaml:"nodeName"`
				Phase    string `yaml:"phase"`
			} `yaml:"pods"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		objs := make([]runtime.Object, 0, len(input.Pods))
		for _, p := range input.Pods {
			pod := &corev1.Pod{}
			pod.Name = p.Name
			pod.Namespace = input.Namespace
			pod.Labels = map[string]string{NVCREJobLabel: p.JobLabel}
			pod.Spec.NodeName = p.NodeName
			pod.Status.Phase = corev1.PodPhase(p.Phase)
			objs = append(objs, pod)
		}

		// No field index is registered on the fake client, so the index lookup
		// errors and the label-selector fallback carries the query. That is the
		// path a controller takes before its index is ready, so it is worth
		// being the path under test.
		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		d := NewNodeDiscoverer(c)

		ctx := context.Background()
		live, err := d.DiscoverNodesForJob(ctx, input.Namespace, input.JobName)
		if err != nil {
			return err
		}
		placed, err := d.DiscoverPlacedNodesForJob(ctx, input.Namespace, input.JobName)
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(struct {
			Live   []string `json:"live"`
			Placed []string `json:"placed"`
		}{Live: live, Placed: placed}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
