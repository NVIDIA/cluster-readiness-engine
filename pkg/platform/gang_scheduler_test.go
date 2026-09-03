// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// applyGangSchedulerInput is the shape of each case's input.yaml: the gang
// scheduler settings a Certification would carry, plus the dependency list a
// resolved Workflow would carry. Omitting gangScheduler models a Certification
// that never asked for gang scheduling.
type applyGangSchedulerInput struct {
	GangScheduler *nvcrev1alpha1.GangSchedulerSpec `json:"gangScheduler,omitempty"`
	Dependencies  []nvcrev1alpha1.DependencySpec   `json:"dependencies"`
}

// replicatedJobProjection records the two fields the helper is allowed to
// write, per replicatedJob. templateLabels is nil (JSON null) when the job has
// no template.metadata.labels at all, which distinguishes "left alone" from
// "given an empty labels map".
type replicatedJobProjection struct {
	Name           string            `json:"name"`
	SchedulerName  string            `json:"schedulerName"`
	TemplateLabels map[string]string `json:"templateLabels"`
}

// dependencyProjection is the golden-file view of one dependency after the
// helper ran. rawUnchanged is the byte-for-byte comparison that guards the
// pass-through paths: a dependency the helper is supposed to skip must come
// back with the exact bytes it went in with. resource carries the whole mutated
// object so an unintended edit anywhere else in the manifest shows up in the
// diff too.
type dependencyProjection struct {
	Index          int                       `json:"index"`
	Kind           string                    `json:"kind"`
	RawUnchanged   bool                      `json:"rawUnchanged"`
	ReplicatedJobs []replicatedJobProjection `json:"replicatedJobs"`
	Resource       any                       `json:"resource"`
}

// TestApplyGangSchedulerToDependencies drives the helper the Certification
// controller and nvcrectl certification render both call. The cases cover the
// runtime shapes the catalog actually produces (torch: one replicatedJob, no
// template.metadata; MPI: node plus launcher, launcher already labelled) and
// every path that must leave a dependency alone.
func TestApplyGangSchedulerToDependencies(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "apply-gang-scheduler-deps",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in applyGangSchedulerInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		before := make([][]byte, len(in.Dependencies))
		for i := range in.Dependencies {
			before[i] = bytes.Clone(in.Dependencies[i].Raw)
		}

		if err := ApplyGangSchedulerToDependencies(in.Dependencies, in.GangScheduler); err != nil {
			return err
		}

		out := make([]dependencyProjection, 0, len(in.Dependencies))
		for i := range in.Dependencies {
			proj, err := projectDependency(i, before[i], in.Dependencies[i])
			if err != nil {
				return err
			}
			out = append(out, proj)
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// projectDependency walks the mutated dependency with its own type assertions
// rather than reusing the production nestedSlice helper, so a bug in that
// walker cannot hide itself from the golden files.
func projectDependency(index int, before []byte, dep nvcrev1alpha1.DependencySpec) (dependencyProjection, error) {
	proj := dependencyProjection{
		Index:          index,
		RawUnchanged:   bytes.Equal(before, dep.Raw),
		ReplicatedJobs: []replicatedJobProjection{},
	}

	obj := map[string]any{}
	if err := json.Unmarshal(dep.Raw, &obj); err != nil {
		return proj, err
	}
	proj.Resource = obj
	proj.Kind, _ = obj[keyKind].(string)

	jobs, _ := mapAt(mapAt(mapAt(obj, keySpec), keyTemplate), keySpec)[keyReplicatedJobs].([]any)
	for _, rj := range jobs {
		job, isMap := rj.(map[string]any)
		if !isMap {
			proj.ReplicatedJobs = append(proj.ReplicatedJobs, replicatedJobProjection{
				Name: "(not an object)",
			})
			continue
		}
		name, _ := job[keyName].(string)
		jobTemplate := mapAt(job, keyTemplate)
		schedulerName, _ := mapAt(mapAt(mapAt(jobTemplate, keySpec), keyTemplate), keySpec)[keySchedulerName].(string)

		proj.ReplicatedJobs = append(proj.ReplicatedJobs, replicatedJobProjection{
			Name:           name,
			SchedulerName:  schedulerName,
			TemplateLabels: stringLabels(mapAt(mapAt(jobTemplate, keyMetadata), keyLabels)),
		})
	}
	return proj, nil
}

// mapAt returns parent[key] when it is a JSON object and nil otherwise.
// Reading a nil map is legal in Go, so a missing level short-circuits the rest
// of the walk without a panic.
func mapAt(parent map[string]any, key string) map[string]any {
	m, _ := parent[key].(map[string]any)
	return m
}

// stringLabels narrows a decoded labels map to strings, preserving nil so the
// golden file shows null when no labels map exists.
func stringLabels(labels map[string]any) map[string]string {
	if labels == nil {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		s, _ := v.(string)
		out[k] = s
	}
	return out
}
