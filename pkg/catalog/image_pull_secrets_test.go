// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// kindTrainingRuntime is the dependency kind whose replicatedJobs name the pod
// sets a Certification creates for an entry.
const kindTrainingRuntime = "TrainingRuntime"

type imagePullSecretsInput struct {
	GPUProducts      []string                      `json:"gpuProducts"`
	NodesPerJob      int32                         `json:"nodesPerJob"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets"`
	NicResourceName  string                        `json:"nicResourceName"`
	MlnxPerNode      int32                         `json:"mlnxPerNode"`
}

type imagePullSecretsProjection struct {
	Entries    []imagePullSecretsEntry `json:"entries"`
	Violations []string                `json:"violations"`
}

type imagePullSecretsEntry struct {
	Entry          string                 `json:"entry"`
	ReplicatedJobs []replicatedJobSecrets `json:"replicatedJobs"`
}

type replicatedJobSecrets struct {
	Name             string   `json:"name"`
	ImagePullSecrets []string `json:"imagePullSecrets"`
}

// TestCatalogImagePullSecrets verifies that every registered entry forwards
// BuildConfig.ImagePullSecrets to every pod set its base TrainingRuntime
// declares, through workload.trainJob.runtimePatches in the base jobTemplate.
// Only the base entry may carry the list: overrides merge their jobTemplate
// with a strategic merge patch that replaces arrays, so a list restated in a
// fragment would be the one that survives. Base replicatedJob names come from
// the TrainingRuntime dependency, so an entry that adds a pod set (the MPI
// launcher, say) without extending its patch is reported. Coverage is total:
// an entry that fails to build at the case's product and node count is a
// violation, not a skip.
func TestCatalogImagePullSecrets(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "image-pull-secrets",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in imagePullSecretsInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		proj := projectImagePullSecrets(t, in)
		require.Empty(tc.T, proj.Violations, "image pull secrets not forwarded")
		out, err := json.MarshalIndent(proj, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(out)
		return nil
	})
}

func projectImagePullSecrets(t testing.TB, in imagePullSecretsInput) imagePullSecretsProjection {
	proj := imagePullSecretsProjection{Entries: []imagePullSecretsEntry{}, Violations: []string{}}
	want := make([]string, 0, len(in.ImagePullSecrets))
	for _, s := range in.ImagePullSecrets {
		want = append(want, s.Name)
	}
	for _, category := range List() {
		entry := Lookup(category.Domain, category.Variant)
		require.NotNil(t, entry, "catalog.List returned %s/%s but Lookup does not know it",
			category.Domain, category.Variant)
		for _, product := range in.GPUProducts {
			source := fmt.Sprintf("%s/%s@%s", category.Domain, category.Variant, product)
			target := nvcrev1alpha1.TargetSpec{
				NodeSelector: map[string]string{"nvidia.com/gpu.product": product},
			}
			spec, err := entry.Build(target, BuildConfig{
				NodesPerJob:      in.NodesPerJob,
				GPUArchitecture:  GPUArchFromNodeSelector(target.NodeSelector),
				ImagePullSecrets: in.ImagePullSecrets,
				NicResourceName:  in.NicResourceName,
				MlnxPerNode:      in.MlnxPerNode,
			})
			if err != nil {
				proj.Violations = append(proj.Violations, source+": build failed: "+err.Error())
				continue
			}
			baseJobs, err := baseReplicatedJobNames(spec)
			if err != nil {
				proj.Violations = append(proj.Violations, source+": "+err.Error())
				continue
			}
			if spec.JobTemplate.Spec.Workload.TrainJob == nil {
				proj.Violations = append(proj.Violations, source+": jobTemplate has no trainJob workload")
				continue
			}

			// Secrets per replicatedJob across every base runtime patch, in
			// patch order; a job patched twice concatenates, which the
			// exact-list check below reports.
			patched := map[string][]string{}
			var patchedOrder []string
			for _, patch := range spec.JobTemplate.Spec.Workload.TrainJob.RuntimePatches {
				for _, rj := range patchReplicatedJobs(patch) {
					if _, seen := patched[rj.Name]; !seen {
						patchedOrder = append(patchedOrder, rj.Name)
						patched[rj.Name] = []string{}
					}
					for _, s := range patchImagePullSecrets(rj) {
						patched[rj.Name] = append(patched[rj.Name], s.Name)
					}
				}
			}

			// Base jobs first, in runtime order, then any job the patches
			// name that the runtime does not.
			jobs := slices.Clone(baseJobs)
			for _, name := range patchedOrder {
				if !slices.Contains(jobs, name) {
					jobs = append(jobs, name)
				}
			}
			projected := imagePullSecretsEntry{Entry: source, ReplicatedJobs: []replicatedJobSecrets{}}
			for _, name := range jobs {
				got := patched[name]
				if got == nil {
					got = []string{}
				}
				projected.ReplicatedJobs = append(projected.ReplicatedJobs, replicatedJobSecrets{Name: name, ImagePullSecrets: got})
				switch {
				case len(want) == 0 && len(got) > 0:
					proj.Violations = append(proj.Violations, fmt.Sprintf(
						"%s: replicatedJob %q carries imagePullSecrets %v with none configured", source, name, got))
				case len(want) > 0 && slices.Contains(baseJobs, name) && !slices.Equal(got, want):
					proj.Violations = append(proj.Violations, fmt.Sprintf(
						"%s: replicatedJob %q carries imagePullSecrets %v, want %v", source, name, got, want))
				}
			}
			proj.Entries = append(proj.Entries, projected)
		}
	}
	sort.Slice(proj.Entries, func(i, j int) bool { return proj.Entries[i].Entry < proj.Entries[j].Entry })
	sort.Strings(proj.Violations)
	return proj
}

// patchReplicatedJobs walks the pointer chain of a runtime patch down to its
// replicatedJobs; every level is optional on the kubeflow patch types.
func patchReplicatedJobs(patch trainerv1alpha1.RuntimePatch) []trainerv1alpha1.ReplicatedJobPatch {
	if patch.TrainingRuntimeSpec == nil || patch.TrainingRuntimeSpec.Template == nil ||
		patch.TrainingRuntimeSpec.Template.Spec == nil {
		return nil
	}
	return patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs
}

// patchImagePullSecrets returns the pod spec imagePullSecrets of one
// replicatedJob patch, or nil when the patch stops short of the pod spec (the
// bare MPI launcher entry).
func patchImagePullSecrets(rj trainerv1alpha1.ReplicatedJobPatch) []corev1.LocalObjectReference {
	if rj.Template == nil || rj.Template.Spec == nil || rj.Template.Spec.Template == nil ||
		rj.Template.Spec.Template.Spec == nil {
		return nil
	}
	return rj.Template.Spec.Template.Spec.ImagePullSecrets
}

// baseReplicatedJobNames returns the replicatedJob names of the base
// TrainingRuntime dependency, in declaration order.
func baseReplicatedJobNames(spec nvcrev1alpha1.WorkflowSpec) ([]string, error) {
	for _, dep := range spec.Dependencies {
		var obj map[string]any
		if err := json.Unmarshal(dep.Raw, &obj); err != nil {
			continue
		}
		if obj["kind"] != kindTrainingRuntime {
			continue
		}
		var runtime struct {
			Spec struct {
				Template struct {
					Spec struct {
						ReplicatedJobs []struct {
							Name string `json:"name"`
						} `json:"replicatedJobs"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(dep.Raw, &runtime); err != nil {
			return nil, err
		}
		names := make([]string, 0, len(runtime.Spec.Template.Spec.ReplicatedJobs))
		for _, rj := range runtime.Spec.Template.Spec.ReplicatedJobs {
			names = append(names, rj.Name)
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("TrainingRuntime dependency declares no replicatedJobs")
		}
		return names, nil
	}
	return nil, fmt.Errorf("no TrainingRuntime dependency found")
}
