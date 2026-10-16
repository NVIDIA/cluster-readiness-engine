// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// applyEnvInput is the shape of each case's input.yaml: the environment a
// Certification's resolved options would carry, plus the job template and
// dependency list a resolved Workflow would carry at the point the transform
// stage runs. Omitting env models a Certification that never set the field,
// which must leave the template byte for byte as it found it.
type applyEnvInput struct {
	Env          []corev1.EnvVar                `json:"env,omitempty"`
	JobTemplate  nvcrev1alpha1.JobTemplateSpec  `json:"jobTemplate,omitempty"`
	Dependencies []nvcrev1alpha1.DependencySpec `json:"dependencies"`
}

// applyEnvProjection is the whole golden file. Args and Env are recorded in
// full rather than as a diff: the insertion position inside Args is the part
// of this transform most likely to regress silently, and only the whole list
// shows where a pair landed relative to the test binary. DepsUnchanged pins
// that the transform never rewrites a dependency, which is what lets the
// torch branch rely on Kubeflow's own by-name upsert.
type applyEnvProjection struct {
	Args          []string        `json:"args"`
	Env           []corev1.EnvVar `json:"env"`
	Err           string          `json:"err,omitempty"`
	DepsUnchanged bool            `json:"depsUnchanged"`
}

// TestApplyEnvToJobTemplate drives the transform the Certification controller
// and nvcrectl certification render both call for spec.env and
// spec.categories[].options.env (ADR-097).
//
// The cases cover both surfaces and the edges that decide whether a variable
// reaches a rank at all: an MPI entry gaining a name it does not carry (the
// pair must land before the test binary, not after it), an MPI entry whose
// catalog already forwards that name (replaced in place, so the single
// occurrence isMNNVLEnabledInJobTemplate scans for survives), an MPI entry
// with no -x pair to anchor an insertion (fails closed rather than guessing),
// a torch entry whose dependency runtime already sets the name (trainer env
// wins by Kubeflow's own upsert, with no dependency rewrite), a runtimeRef
// matching no dependency, and an empty env leaving everything alone.
func TestApplyEnvToJobTemplate(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "apply-env",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in applyEnvInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		before := make([][]byte, len(in.Dependencies))
		for i := range in.Dependencies {
			before[i] = bytes.Clone(in.Dependencies[i].Raw)
		}

		out := applyEnvProjection{Args: []string{}, Env: []corev1.EnvVar{}}
		if err := ApplyEnvToJobTemplate(&in.JobTemplate, in.Dependencies, in.Env); err != nil {
			out.Err = err.Error()
		}

		if tj := in.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
			if tj.Trainer.Args != nil {
				out.Args = tj.Trainer.Args
			}
			if tj.Trainer.Env != nil {
				out.Env = tj.Trainer.Env
			}
		}

		out.DepsUnchanged = true
		for i := range in.Dependencies {
			if !bytes.Equal(before[i], in.Dependencies[i].Raw) {
				out.DepsUnchanged = false
			}
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
