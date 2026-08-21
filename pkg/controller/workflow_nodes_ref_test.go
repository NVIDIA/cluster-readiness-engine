// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

func TestApplySucceededNodesRef(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-succeeded-nodes-ref",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		return runApplyNodesRefCase(tc, func(ref *corev1.TypedLocalObjectReference, w *crev1alpha1.Workflow) (bool, *corev1.TypedLocalObjectReference) {
			fn := applySucceededNodesRef(ref)
			changed := fn != nil && fn(w)
			return changed, w.Status.SucceededNodesRef
		})
	})
}

func TestApplyFailedNodesRef(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-failed-nodes-ref",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		return runApplyNodesRefCase(tc, func(ref *corev1.TypedLocalObjectReference, w *crev1alpha1.Workflow) (bool, *corev1.TypedLocalObjectReference) {
			fn := applyFailedNodesRef(ref)
			changed := fn != nil && fn(w)
			return changed, w.Status.FailedNodesRef
		})
	})
}

func runApplyNodesRefCase(tc *testutil.TestCase, apply func(*corev1.TypedLocalObjectReference, *crev1alpha1.Workflow) (bool, *corev1.TypedLocalObjectReference)) error {
	var input struct {
		Ref      *corev1.TypedLocalObjectReference `json:"ref"`
		Workflow crev1alpha1.Workflow              `json:"workflow"`
	}
	if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
		return err
	}
	changed, ref := apply(input.Ref, &input.Workflow)
	b, err := json.MarshalIndent(map[string]any{"changed": changed, "ref": ref}, "", "  ")
	if err != nil {
		return err
	}
	tc.Actual = string(b) + "\n"
	return nil
}
