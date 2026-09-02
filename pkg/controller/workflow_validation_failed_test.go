// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

func TestApplyWorkflowValidationFailed(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-workflow-validation-failed",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Message  string                 `json:"message"`
			Workflow nvcrev1alpha1.Workflow `json:"workflow"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		changed := applyWorkflowValidationFailed(input.Message)(&input.Workflow)
		// SetStatusCondition stamps LastTransitionTime with wall-clock time on
		// insert; zero it so the golden is deterministic.
		for i := range input.Workflow.Status.Conditions {
			input.Workflow.Status.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		b, err := json.MarshalIndent(map[string]any{
			"changed":    changed,
			"conditions": input.Workflow.Status.Conditions,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
