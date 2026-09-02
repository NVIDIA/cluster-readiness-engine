// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestNodeHealthChangePredicate verifies that nodeHealthChangePredicate triggers
// (or suppresses) reconciliation for the correct categories of node changes.
// Annotation-only updates (e.g. kubelet heartbeats) must NOT trigger reconciles
// because on large clusters they caused constant reconcile storms (issue #119).
func TestNodeHealthChangePredicate(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "node-health-predicate",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var oldNode, newNode corev1.Node
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_old_node.yaml"]), &oldNode); err != nil {
			return err
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_new_node.yaml"]), &newNode); err != nil {
			return err
		}

		r := &JobReconciler{}
		result := r.nodeHealthChangePredicate().Update(event.UpdateEvent{
			ObjectOld: &oldNode,
			ObjectNew: &newNode,
		})

		b, err := json.MarshalIndent(map[string]any{"result": result}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
