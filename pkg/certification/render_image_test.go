// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// imageContainer records one container image of one replicatedJob of one
// TrainingRuntime dependency, keyed so a golden file reads as "which pod,
// which container, whose image".
type imageContainer struct {
	Dependency    string `json:"dependency"`
	ReplicatedJob string `json:"replicatedJob"`
	// Kind is "container" for entries of containers[] and "initContainer" for
	// entries of initContainers[]. containers[0] takes the override, and an
	// initContainer follows only when its image equals the primary's
	// pre-override image (workload-derived inits like fix-ssh-permissions and
	// megatron-clone); everything else keeps its catalog image.
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Image string `json:"image"`
}

// imageWorkflow is the per-Workflow projection written to the golden file.
type imageWorkflow struct {
	Workflow string `json:"workflow"`
	// TrainerImage is jobTemplate.spec.workload.trainJob.trainer.image, nil
	// when the template has no trainer.
	TrainerImage *string          `json:"trainerImage"`
	Containers   []imageContainer `json:"containers"`
}

// TestCertificationRenderImage covers the resolution that makes options.image
// work: per-category options.image beats the spec-level value, the spec-level
// value applies to every category that does not override it, and a category
// without either keeps the images the catalog and platform overrides landed
// on. Like the gang scheduler render test, the cases drive
// resolveWorkflowsOffline, the same function runCertificationRender calls, so
// what they record is what "nvcrectl certification render --platform aws"
// prints and what the certification controller creates.
func TestCertificationRenderImage(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-image",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `json:"platform"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		certPath := filepath.Join(tc.T.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}

		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, cfg.Platform)
		if err != nil {
			return err
		}
		if err := resolveWorkflowsOffline(cert, workflows, cfg.Platform); err != nil {
			return err
		}

		result := make([]imageWorkflow, 0, len(workflows))
		for i := range workflows {
			projected, projectErr := projectWorkflowImages(&workflows[i])
			if projectErr != nil {
				return projectErr
			}
			result = append(result, projected)
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// projectWorkflowImages walks a resolved Workflow in declaration order:
// dependencies as the catalog lists them, replicatedJobs as the runtime lists
// them, containers before initContainers within each pod.
func projectWorkflowImages(wf *nvcrev1alpha1.Workflow) (imageWorkflow, error) {
	out := imageWorkflow{
		Workflow:   wf.Name,
		Containers: []imageContainer{},
	}
	if tj := wf.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
		out.TrainerImage = tj.Trainer.Image
	}

	for i := range wf.Spec.Dependencies {
		raw := wf.Spec.Dependencies[i].Raw
		if len(raw) == 0 {
			continue
		}

		var typeMeta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return out, err
		}
		if typeMeta.Kind != "TrainingRuntime" {
			continue
		}

		var rt trainerv1alpha1.TrainingRuntime
		if err := json.Unmarshal(raw, &rt); err != nil {
			return out, err
		}
		for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
			for _, c := range rj.Template.Spec.Template.Spec.Containers {
				out.Containers = append(out.Containers, imageContainer{
					Dependency:    rt.Name,
					ReplicatedJob: rj.Name,
					Kind:          "container",
					Name:          c.Name,
					Image:         c.Image,
				})
			}
			for _, c := range rj.Template.Spec.Template.Spec.InitContainers {
				out.Containers = append(out.Containers, imageContainer{
					Dependency:    rt.Name,
					ReplicatedJob: rj.Name,
					Kind:          "initContainer",
					Name:          c.Name,
					Image:         c.Image,
				})
			}
		}
	}
	return out, nil
}
