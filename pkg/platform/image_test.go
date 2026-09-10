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

// applyImageInput is the shape of each case's input.yaml: the effective image
// a Certification's resolved options would carry, plus the job template and
// dependency list a resolved Workflow would carry. Omitting image models a
// Certification that never set options.image.
type applyImageInput struct {
	Image        string                         `json:"image,omitempty"`
	JobTemplate  nvcrev1alpha1.JobTemplateSpec  `json:"jobTemplate,omitempty"`
	Dependencies []nvcrev1alpha1.DependencySpec `json:"dependencies"`
}

// imageReplicatedJob records, per replicatedJob, exactly the container images
// the helper may and may not touch: containerImages[0] is the primary the
// helper replaces, entries of initContainerImages follow only when they
// carried the primary's exact pre-override image, and everything else must
// come back untouched.
type imageReplicatedJob struct {
	Name                string   `json:"name"`
	ContainerImages     []string `json:"containerImages"`
	InitContainerImages []string `json:"initContainerImages"`
}

// imageDependency is the golden-file view of one dependency after the helpers
// ran. rawUnchanged guards the pass-through paths byte for byte, exactly like
// the gang scheduler test's projection; resource carries the whole mutated
// object so an unintended edit anywhere else in the manifest shows up too.
type imageDependency struct {
	Index          int                  `json:"index"`
	Kind           string               `json:"kind"`
	RawUnchanged   bool                 `json:"rawUnchanged"`
	ReplicatedJobs []imageReplicatedJob `json:"replicatedJobs"`
	Resource       any                  `json:"resource"`
}

// applyImageProjection is the whole golden file: the trainer image the job
// template ends up with, then every dependency.
type applyImageProjection struct {
	TrainerImage *string           `json:"trainerImage"`
	Dependencies []imageDependency `json:"dependencies"`
}

// TestApplyImageToDependencies drives the two helpers the Certification
// controller and nvcrectl certification render both call. The cases cover the
// runtime shapes the catalog actually produces (MPI: node plus launcher, the
// launcher's workload-derived fix-ssh-permissions init following the
// override; training: the megatron-clone init following for the same reason;
// GCP H100: the tcpxo-daemon infrastructure init staying pinned), the
// equality rule's edges (a sidecar sharing the primary image, an init on the
// same repository at another tag), and the paths that must leave a dependency
// alone, byte for byte.
func TestApplyImageToDependencies(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "apply-image-deps",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in applyImageInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		before := make([][]byte, len(in.Dependencies))
		for i := range in.Dependencies {
			before[i] = bytes.Clone(in.Dependencies[i].Raw)
		}

		ApplyImageToJobTemplate(&in.JobTemplate, in.Image)
		if err := ApplyImageToDependencies(in.Dependencies, in.Image); err != nil {
			return err
		}

		out := applyImageProjection{Dependencies: []imageDependency{}}
		if tj := in.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
			out.TrainerImage = tj.Trainer.Image
		}
		for i := range in.Dependencies {
			proj, err := projectImageDependency(i, before[i], in.Dependencies[i])
			if err != nil {
				return err
			}
			out.Dependencies = append(out.Dependencies, proj)
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// projectImageDependency walks the mutated dependency with its own type
// assertions rather than reusing the production nestedSlice helper, so a bug
// in that walker cannot hide itself from the golden files.
func projectImageDependency(index int, before []byte, dep nvcrev1alpha1.DependencySpec) (imageDependency, error) {
	proj := imageDependency{
		Index:          index,
		RawUnchanged:   bytes.Equal(before, dep.Raw),
		ReplicatedJobs: []imageReplicatedJob{},
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
			proj.ReplicatedJobs = append(proj.ReplicatedJobs, imageReplicatedJob{
				Name: "(not an object)",
			})
			continue
		}
		name, _ := job[keyName].(string)
		podSpec := mapAt(mapAt(mapAt(mapAt(job, keyTemplate), keySpec), keyTemplate), keySpec)

		proj.ReplicatedJobs = append(proj.ReplicatedJobs, imageReplicatedJob{
			Name:                name,
			ContainerImages:     containerImages(podSpec, keyContainers),
			InitContainerImages: containerImages(podSpec, "initContainers"),
		})
	}
	return proj, nil
}

// containerImages lists the image of every entry in podSpec[key], in order,
// preserving nil when the pod spec has no such list at all so the golden file
// distinguishes "absent" from "empty".
func containerImages(podSpec map[string]any, key string) []string {
	list, ok := podSpec[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		m, _ := c.(map[string]any)
		img, _ := m[keyImage].(string)
		out = append(out, img)
	}
	return out
}
