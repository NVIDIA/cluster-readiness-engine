// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// pullSecretsReplicatedJob records the imagePullSecrets one replicatedJob's
// pod spec receives through the resolved TrainJob's runtimePatches, by name
// and in patch order. The names come from the resolved TrainingRuntime
// dependency, not from the patches, so a pod set the runtime declares and
// the patches never reach shows up here with an empty list.
type pullSecretsReplicatedJob struct {
	Name             string   `json:"name"`
	ImagePullSecrets []string `json:"imagePullSecrets"`
}

// pullSecretsWorkflow is the per-Workflow projection written to the golden
// file. Category is the Workflow's domain/variant labels, because the
// Workflow name carries a hash suffix and does not say which entry it is.
type pullSecretsWorkflow struct {
	Workflow       string                     `json:"workflow"`
	Category       string                     `json:"category"`
	ReplicatedJobs []pullSecretsReplicatedJob `json:"replicatedJobs"`
}

type pullSecretsProjection struct {
	Workflows  []pullSecretsWorkflow `json:"workflows"`
	Violations []string              `json:"violations"`
}

// TestCertificationRenderImagePullSecrets pins that Certification
// spec.imagePullSecrets reaches every pod set of every category after the
// platform overrides resolve, through the same path "nvcrectl certification
// render --platform <csp>" uses.
//
// Only the two training entries forwarded the list; the five communication
// entries and diagnostics/dcgm-level4 rendered no pull secret on any pod, so
// against a private registry those categories failed at image pull. The fix
// lives in each base entry's runtimePatches, and this test covers what the
// catalog-level test cannot: that the list survives the platform override
// chain. Overrides merge their jobTemplate with a strategic merge patch that
// replaces arrays, so a fragment restating runtimePatches would silently drop
// the secrets on exactly the platforms it targets. GCP H100 is the heaviest
// override set and the case most likely to catch that.
//
// Violations are checked before the golden so a regression reads as the
// offending workflow and pod set rather than as a golden diff.
func TestCertificationRenderImagePullSecrets(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-image-pull-secrets",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `json:"platform"`
			GPUArch  string `json:"gpuArch"`
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
		gpuArch, err := catalog.ParseGPUArchFlag(cfg.GPUArch)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, cfg.Platform, gpuArch, nil, "")
		if err != nil {
			return err
		}
		if err := resolveWorkflowsOffline(cert, workflows, cfg.Platform, gpuArch); err != nil {
			return err
		}

		// A category's own list replaces the spec-level list for that category
		// (ResolveOptions in pkg/controller takes a non-empty per-category list
		// wholesale); every other category inherits the spec-level list.
		specWant := secretNames(cert.Spec.ImagePullSecrets)

		proj := pullSecretsProjection{Workflows: []pullSecretsWorkflow{}, Violations: []string{}}
		for i := range workflows {
			projected, projectErr := projectImagePullSecrets(&workflows[i])
			if projectErr != nil {
				return projectErr
			}
			proj.Workflows = append(proj.Workflows, projected)
			want := specWant
			if own := categoryPullSecrets(cert, projected.Category); len(own) > 0 {
				want = own
			}
			for _, rj := range projected.ReplicatedJobs {
				if !slices.Equal(rj.ImagePullSecrets, want) {
					proj.Violations = append(proj.Violations, fmt.Sprintf(
						"%s (%s): replicatedJob %q carries imagePullSecrets %v, want %v",
						projected.Workflow, projected.Category, rj.Name, rj.ImagePullSecrets, want))
				}
			}
		}
		sort.Slice(proj.Workflows, func(i, j int) bool {
			return proj.Workflows[i].Workflow < proj.Workflows[j].Workflow
		})
		sort.Strings(proj.Violations)
		require.Empty(tc.T, proj.Violations, "image pull secrets not forwarded")

		data, err := json.MarshalIndent(proj, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// projectImagePullSecrets lists the resolved TrainingRuntime's replicatedJobs
// in declaration order and, for each, the imagePullSecrets every resolved
// runtime patch gives its pod spec. A job patched twice concatenates, which
// the exact-list check in the test reports.
func projectImagePullSecrets(wf *nvcrev1alpha1.Workflow) (pullSecretsWorkflow, error) {
	out := pullSecretsWorkflow{
		Workflow: wf.Name,
		Category: fmt.Sprintf("%s/%s",
			wf.Labels["nvcre.nvidia.com/category-domain"], wf.Labels["nvcre.nvidia.com/category-variant"]),
		ReplicatedJobs: []pullSecretsReplicatedJob{},
	}

	patched := map[string][]string{}
	if tj := wf.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil {
		for _, patch := range tj.RuntimePatches {
			if patch.TrainingRuntimeSpec == nil || patch.TrainingRuntimeSpec.Template == nil ||
				patch.TrainingRuntimeSpec.Template.Spec == nil {
				continue
			}
			for _, rj := range patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs {
				if _, seen := patched[rj.Name]; !seen {
					patched[rj.Name] = []string{}
				}
				// Every level is optional on the kubeflow patch types; the
				// bare MPI launcher entry stops at the name.
				if rj.Template == nil || rj.Template.Spec == nil || rj.Template.Spec.Template == nil ||
					rj.Template.Spec.Template.Spec == nil {
					continue
				}
				for _, s := range rj.Template.Spec.Template.Spec.ImagePullSecrets {
					patched[rj.Name] = append(patched[rj.Name], s.Name)
				}
			}
		}
	}

	found := false
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
		if typeMeta.Kind != trainerv1alpha1.TrainingRuntimeKind {
			continue
		}

		var rt trainerv1alpha1.TrainingRuntime
		if err := json.Unmarshal(raw, &rt); err != nil {
			return out, err
		}
		found = true
		for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
			secrets := patched[rj.Name]
			if secrets == nil {
				secrets = []string{}
			}
			out.ReplicatedJobs = append(out.ReplicatedJobs, pullSecretsReplicatedJob{
				Name:             rj.Name,
				ImagePullSecrets: secrets,
			})
		}
	}
	if !found {
		return out, fmt.Errorf("%s: no TrainingRuntime dependency found", wf.Name)
	}
	return out, nil
}

// secretNames lists the names in a LocalObjectReference list, in order.
func secretNames(refs []corev1.LocalObjectReference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

// categoryPullSecrets returns the per-category imagePullSecrets of the
// category rendered as "domain/variant", or nil when it sets none.
func categoryPullSecrets(cert *nvcrev1alpha1.Certification, category string) []string {
	for _, c := range cert.Spec.Categories {
		if c.Domain+"/"+c.Variant != category {
			continue
		}
		if c.Options == nil {
			return nil
		}
		return secretNames(c.Options.ImagePullSecrets)
	}
	return nil
}
