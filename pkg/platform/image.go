// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// ApplyImageToDependencies rewrites every TrainingRuntime dependency in place
// so the image of the primary workload container, containers[0], of each
// replicatedJob's pod template is the given image. Init containers whose image
// exactly equals the primary's pre-override image follow it: the catalog
// derives those inits from the workload ref (the MPI launcher's
// fix-ssh-permissions, the training entries' megatron-clone), so they must
// run whatever image the workload runs, including any source the operator
// provisioned inside it. Init containers with a distinct image are
// infrastructure and stay pinned by construction (the GCP tcpxo-daemon init
// container keeps GCP's RxDM image). Non-init containers beyond index 0 are
// never touched, whatever their image.
//
// It is a strict no-op when image is empty, so a Certification that never set
// options.image renders byte-identically to before, matching
// ApplyGangSchedulerToDependencies's guard. Unlike that helper's ensureMap
// labels write, it never creates structure: a replicatedJob with no
// containers list is left alone, byte for byte.
//
// Callers must invoke this after overrides are resolved, at the same point
// gang scheduling is applied: platform overrides choose images too (the AWS
// EFA overrides swap the workers to an nccl-tests build), and the whole point
// of options.image is to replace whatever the catalog landed on. It is a
// deliberate structural twin of ApplyGangSchedulerToDependencies rather than
// a shared walker; a third post-resolve rewriter (issue #212 is the
// candidate) is the refactor trigger.
func ApplyImageToDependencies(deps []nvcrev1alpha1.DependencySpec, image string) error {
	if image == "" {
		return nil
	}

	for i := range deps {
		if len(deps[i].Raw) == 0 {
			continue
		}

		obj := map[string]any{}
		if err := json.Unmarshal(deps[i].Raw, &obj); err != nil {
			return fmt.Errorf("unmarshal dependency %d: %w", i, err)
		}
		if kind, _ := obj[keyKind].(string); kind != kindTrainingRuntime {
			continue
		}

		replicatedJobs, ok := nestedSlice(obj, keySpec, keyTemplate, keySpec, keyReplicatedJobs)
		if !ok {
			continue
		}
		changed := false
		for _, rj := range replicatedJobs {
			job, isMap := rj.(map[string]any)
			if !isMap {
				continue
			}
			// Primary container: replicatedJobs[].template.spec.template.spec
			// .containers[0]. The nesting is JobTemplateSpec -> JobSpec ->
			// PodTemplateSpec -> PodSpec, same as the gang scheduler walk.
			containers, found := nestedSlice(job, keyTemplate, keySpec, keyTemplate, keySpec, keyContainers)
			if !found || len(containers) == 0 {
				continue
			}
			primary, isMap := containers[0].(map[string]any)
			if !isMap {
				continue
			}
			// Capture the primary's pre-override image: it is the equality key
			// that decides which init containers follow the override.
			previous, _ := primary[keyImage].(string)
			primary[keyImage] = image
			changed = true

			// Workload-derived init containers carry the exact pre-override
			// primary image and follow it; anything else (a distinct ref, or
			// even the same repository at another tag) stays pinned. The
			// previous != "" guard keeps a primary without an image field from
			// matching inits that also lack one.
			if previous == "" {
				continue
			}
			inits, hasInits := nestedSlice(job, keyTemplate, keySpec, keyTemplate, keySpec, keyInitContainers)
			if !hasInits {
				continue
			}
			for _, ic := range inits {
				initContainer, isMap := ic.(map[string]any)
				if !isMap {
					continue
				}
				if img, _ := initContainer[keyImage].(string); img == previous {
					initContainer[keyImage] = image
				}
			}
		}
		// Marshal back only when a container was rewritten, so a runtime whose
		// shape did not match comes back with the exact bytes it went in with.
		if !changed {
			continue
		}

		raw, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal dependency %d: %w", i, err)
		}
		deps[i].Raw = raw
	}
	return nil
}

// ApplyImageToJobTemplate points the job template's trainer at the given
// image (jobTemplate.spec.workload.trainJob.trainer.image), the sibling of
// ApplyImageToDependencies for the typed half of the workload. It is a no-op
// when image is empty, and it never creates structure: a template without a
// trainJob workload or without a trainer has no trainer image to replace and
// is left alone.
func ApplyImageToJobTemplate(jt *nvcrev1alpha1.JobTemplateSpec, image string) {
	if image == "" {
		return
	}
	trainJob := jt.Spec.Workload.TrainJob
	if trainJob == nil || trainJob.Trainer == nil {
		return
	}
	trainJob.Trainer.Image = &image
}
