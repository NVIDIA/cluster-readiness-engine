// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// labelKeyEnvironment is an ordinary user label, alongside the queue labels
// whose values collide with the dependency name.
const (
	labelKeyEnvironment = "environment"
	labelKeyQueue       = "kai.scheduler/queue"
	replicatedJobNode   = "node"
)

// TestSuffixJobSpecPreservesWorkloadMetadata pins that per-job dependency
// renaming cannot rewrite a workload label.
//
// suffixJobSpec renames dependency references by blind quoted-string
// substitution over the whole marshaled JobSpec. Workload label values are
// user data with no relation to dependency names, and they can legitimately
// coincide with one — a queue named after the runtime it serves is the obvious
// case. Without excluding the field, launching a group would silently submit
// the workload into a queue nobody named, and the controller would disagree
// with what `nvcrectl render` printed.
//
// This is a single invariant with no structured output worth snapshotting, and
// it guards a silent data change rather than an error path, which is exactly
// the kind of thing a golden file would not make obvious.
func TestSuffixJobSpecPreservesWorkloadMetadata(t *testing.T) {
	const runtimeName = "nccl-runtime"

	spec := &nvcrev1alpha1.JobSpec{
		WorkloadMetadata: &nvcrev1alpha1.WorkloadMetadata{
			Labels: map[string]nvcrev1alpha1.WorkloadLabelValue{
				// Both values deliberately equal the dependency name being
				// renamed, which is the collision the exclusion exists for.
				labelKeyQueue:               runtimeName,
				"kueue.x-k8s.io/queue-name": runtimeName,
				labelKeyEnvironment:         "burn-in",
			},
		},
		Workload: nvcrev1alpha1.WorkloadSpec{
			TrainJob: &trainerv1alpha1.TrainJobSpec{
				RuntimeRef: trainerv1alpha1.RuntimeRef{
					Name: runtimeName,
					Kind: new("TrainingRuntime"),
				},
			},
		},
	}

	replacements := map[string]string{runtimeName: runtimeName + "-group-0-job"}

	patched, err := suffixJobSpec(spec, replacements)
	if err != nil {
		t.Fatalf("suffixJobSpec: %v", err)
	}

	// The reference must be renamed: that is the whole point of the step.
	if got := patched.Workload.TrainJob.RuntimeRef.Name; got != replacements[runtimeName] {
		t.Errorf("runtimeRef.name = %q, want it renamed to %q", got, replacements[runtimeName])
	}

	// The labels must not be.
	for key, want := range spec.WorkloadMetadata.Labels {
		if got := patched.WorkloadMetadata.Labels[key]; got != want {
			t.Errorf("label %q = %q, want %q unchanged by dependency renaming", key, got, want)
		}
	}

	// The result must not alias the input, so a later mutation of one cannot
	// reach the other.
	patched.WorkloadMetadata.Labels[labelKeyEnvironment] = "mutated"
	if spec.WorkloadMetadata.Labels[labelKeyEnvironment] != "burn-in" {
		t.Error("patched workload metadata aliases the source spec")
	}
}

// TestSuffixJobSpecPreservesRuntimePatchLabels pins the same protection for
// the labels inside trainJob.runtimePatches.
//
// These carry queue labels just as the runtime dependency does, and are just
// as prone to colliding with a dependency name. The patches cannot be detached
// wholesale the way workloadMetadata is, because a patch's volumes can hold a
// real PVC reference that does need renaming — so the test pins both halves:
// the label survives and the claim reference is rewritten.
func TestSuffixJobSpecPreservesRuntimePatchLabels(t *testing.T) {
	const runtimeName = "shared-runtime"
	const pvcName = "ckpt-pvc"

	spec := &nvcrev1alpha1.JobSpec{
		Workload: nvcrev1alpha1.WorkloadSpec{
			TrainJob: &trainerv1alpha1.TrainJobSpec{
				RuntimeRef: trainerv1alpha1.RuntimeRef{
					Name: runtimeName,
					Kind: new("TrainingRuntime"),
				},
				RuntimePatches: []trainerv1alpha1.RuntimePatch{{
					Manager: "nvcre.nvidia.com/catalog",
					TrainingRuntimeSpec: &trainerv1alpha1.TrainingRuntimeSpecPatch{
						Template: &trainerv1alpha1.JobSetTemplatePatch{
							Spec: &trainerv1alpha1.JobSetSpecPatch{
								ReplicatedJobs: []trainerv1alpha1.ReplicatedJobPatch{{
									Name: replicatedJobNode,
									Template: &trainerv1alpha1.JobTemplatePatch{
										Metadata: &metav1.ObjectMeta{
											// Collides with the runtime name.
											Labels: map[string]string{labelKeyQueue: runtimeName},
										},
										Spec: &trainerv1alpha1.JobSpecPatch{
											Template: &trainerv1alpha1.PodTemplatePatch{
												Metadata: &metav1.ObjectMeta{
													Labels: map[string]string{labelKeyQueue: runtimeName},
												},
												Spec: &trainerv1alpha1.PodSpecPatch{
													Volumes: []corev1.Volume{{
														Name: "ckpt",
														PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
															ClaimName: pvcName,
														},
													}},
												},
											},
										},
									},
								}},
							},
						},
					},
				}},
			},
		},
	}

	replacements := map[string]string{
		runtimeName: runtimeName + "-group-0-job",
		pvcName:     pvcName + "-group-0-job",
	}

	patched, err := suffixJobSpec(spec, replacements)
	if err != nil {
		t.Fatalf("suffixJobSpec: %v", err)
	}

	rj := patched.Workload.TrainJob.RuntimePatches[0].
		TrainingRuntimeSpec.Template.Spec.ReplicatedJobs[0].Template

	if got := rj.Metadata.Labels[labelKeyQueue]; got != runtimeName {
		t.Errorf("patch job-template queue label = %q, want %q unchanged", got, runtimeName)
	}
	if got := rj.Spec.Template.Metadata.Labels[labelKeyQueue]; got != runtimeName {
		t.Errorf("patch pod-template queue label = %q, want %q unchanged", got, runtimeName)
	}

	// The PVC reference inside the same patch must still be renamed, which is
	// why the patches cannot simply be excluded wholesale.
	claim := rj.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName
	if claim != replacements[pvcName] {
		t.Errorf("patch volume claimName = %q, want it renamed to %q", claim, replacements[pvcName])
	}
}

// TestSuffixJobSpecWithoutWorkloadMetadata covers the ordinary path: a spec
// with no workload metadata must come back with none rather than an empty
// object, so a Job created from it stays byte-identical to what earlier
// versions produced.
func TestSuffixJobSpecWithoutWorkloadMetadata(t *testing.T) {
	spec := &nvcrev1alpha1.JobSpec{
		Workload: nvcrev1alpha1.WorkloadSpec{
			TrainJob: &trainerv1alpha1.TrainJobSpec{
				RuntimeRef: trainerv1alpha1.RuntimeRef{Name: "rt"},
			},
		},
	}

	patched, err := suffixJobSpec(spec, map[string]string{"rt": "rt-job"})
	if err != nil {
		t.Fatalf("suffixJobSpec: %v", err)
	}
	if patched.WorkloadMetadata != nil {
		t.Errorf("workloadMetadata = %+v, want nil", patched.WorkloadMetadata)
	}
	if got := patched.Workload.TrainJob.RuntimeRef.Name; got != "rt-job" {
		t.Errorf("runtimeRef.name = %q, want %q", got, "rt-job")
	}
}
