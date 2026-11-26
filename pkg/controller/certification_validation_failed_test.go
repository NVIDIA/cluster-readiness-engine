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

// TestMirrorWorkflowValidationFailed pins the message format of the
// Certification's ValidationFailed condition together with how the list in it
// accumulates and how categories are named: a first category, a second one
// appended, the same one mirrored again (no change), two categories of one
// domain/variant that differ in options (listed separately), options that add
// no suffix (plain name), a Workflow that met its thresholds (no mirror), and a
// message without the prefix (the list starts over).
func TestMirrorWorkflowValidationFailed(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "mirror-workflow-validation-failed",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// Index is the category whose terminal Workflow is mirrored.
			Index         int                         `json:"index"`
			Workflow      nvcrev1alpha1.Workflow      `json:"workflow"`
			Certification nvcrev1alpha1.Certification `json:"certification"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		changed := false
		if mirror := mirrorWorkflowValidationFailed(&input.Workflow,
			validationFailedCategoryName(&input.Certification, input.Index)); mirror != nil {
			changed = mirror(&input.Certification)
		}
		// SetStatusCondition stamps LastTransitionTime with wall-clock time on
		// insert; zero it so the golden is deterministic.
		for i := range input.Certification.Status.Conditions {
			input.Certification.Status.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		b, err := json.MarshalIndent(struct {
			Changed    bool               `json:"changed"`
			Conditions []metav1.Condition `json:"conditions"`
		}{changed, input.Certification.Status.Conditions}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
