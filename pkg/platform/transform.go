// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

const (
	// JobTemplateWorkloadLabelsPath names workload metadata on a Job or
	// Workflow spec. Callers validating those API surfaces should use it in
	// field-specific errors.
	JobTemplateWorkloadLabelsPath = "spec.jobTemplate.spec.workloadMetadata.labels"
	// WorkloadRunWorkloadLabelsPath names the source field on a WorkloadRun.
	// The generated Workflow has a jobTemplate, but reporting that internal
	// path would point WorkloadRun users at a field their API does not expose.
	WorkloadRunWorkloadLabelsPath = "spec.workloadMetadata.labels"
)

const (
	// trainerAPIGroup and kindClusterTrainingRuntime are the defaults
	// Kubeflow's TrainJob CRD applies to an underspecified runtimeRef.
	trainerAPIGroup            = "trainer.kubeflow.org"
	kindClusterTrainingRuntime = "ClusterTrainingRuntime"

	keyAPIVersion = "apiVersion"
)

// ResolvedWorkflowTransforms carries the intents applied to a WorkflowSpec
// after catalog and platform overrides have resolved.
type ResolvedWorkflowTransforms struct {
	// GangScheduler is the owner's gang-scheduling intent, before defaulting.
	GangScheduler *nvcrev1alpha1.GangSchedulerSpec

	// Image replaces the workload container image. Empty means no override.
	Image string

	// WorkloadLabels are the workload-object labels the owner resolved from
	// its own API surface — for Certification, global labels already merged
	// with per-category labels.
	WorkloadLabels map[string]string
}

// ApplyResolvedWorkflowTransforms applies every post-resolve transform to a
// resolved WorkflowSpec in one named stage, then persists the gang-scheduling
// intent and checks the result.
//
// It exists because a third post-resolve rewrite landed: ADR-077 deliberately
// left gang scheduling and image replacement as two focused helpers called in
// sequence at three call sites, and named this the refactor trigger. Those
// helpers and their tests are unchanged; this composes them.
//
// The order is pinned:
//
//  1. ApplyGangSchedulerToDependencies
//  2. ApplyImageToJobTemplate
//  3. ApplyImageToDependencies
//  4. resolve and write JobTemplate.spec.workloadMetadata
//
// Gang scheduling and image replacement still touch disjoint fields, so only
// step 4's position matters: writing workload metadata last makes the resolved
// catalog and override JobSpec the base map, which makes its precedence
// unambiguous.
//
// It then records the resolved intent on spec.gangScheduler and runs
// ValidateResolvedJobTemplate, so a caller cannot forget either.
func ApplyResolvedWorkflowTransforms(
	spec *nvcrev1alpha1.WorkflowSpec, t ResolvedWorkflowTransforms,
) error {
	if err := ApplyGangSchedulerToDependencies(spec.Dependencies, t.GangScheduler); err != nil {
		return err
	}

	ApplyImageToJobTemplate(&spec.JobTemplate, t.Image)
	if err := ApplyImageToDependencies(spec.Dependencies, t.Image); err != nil {
		return err
	}

	if err := applyWorkloadMetadata(&spec.JobTemplate.Spec, t, JobTemplateWorkloadLabelsPath); err != nil {
		return err
	}

	spec.GangScheduler = ResolvedGangScheduler(t.GangScheduler)
	return ValidateResolvedJobTemplate(
		&spec.JobTemplate.Spec, spec.Dependencies, spec.GangScheduler,
		JobTemplateWorkloadLabelsPath)
}

// applyWorkloadMetadata composes the workload-object labels for the resolved
// Job template, in the order fixed by ADR-079: labels the resolved JobSpec
// already carries, then the owner's resolved labels replacing them by key,
// then the gang-scheduler queue label under the insert/same/conflict rule.
//
// The cap is checked after the queue label is inserted, because the composed
// map is the one that reaches the API and no single admission rule ever saw
// it whole. Exceeding the cap fails rather than dropping labels.
func applyWorkloadMetadata(
	js *nvcrev1alpha1.JobSpec, t ResolvedWorkflowTransforms,
	workloadLabelsPath string,
) error {
	labels := workload.MergeLabels(
		workload.LabelsOf(js.WorkloadMetadata), t.WorkloadLabels)

	if gs := ResolvedGangScheduler(t.GangScheduler); gs != nil {
		merged, err := workload.InsertConsistentLabel(
			labels, gs.QueueLabelKey, gs.Queue, workloadLabelsPath)
		if err != nil {
			return err
		}
		labels = merged
	}

	if err := workload.ValidateLabels(labels, workloadLabelsPath); err != nil {
		return err
	}
	js.WorkloadMetadata = workload.MetadataFrom(labels)
	return nil
}

// ApplyWorkloadRunScheduling composes the workload-object metadata and records
// the resolved gang-scheduling intent on a WorkflowSpec that WorkloadRun just
// built. It is the construction-time half of the same contract
// ApplyResolvedWorkflowTransforms provides for Certification.
//
// WorkloadRun needs no ApplyGangSchedulerToDependencies call: its runtime
// dependency is built with the scheduler and queue already in place by
// BuildTorchRuntime, BuildMPIRuntime and BuildExecRuntime, unlike a catalog
// runtime that ADR-076 has to rewrite after overrides resolve.
//
// This merge is provisional. WorkloadRun's structural overrides run later and
// may change either metadata level, so the persisted intent is what makes the
// Workflow controller's post-override check authoritative.
func ApplyWorkloadRunScheduling(
	spec *nvcrev1alpha1.WorkflowSpec,
	gs *nvcrev1alpha1.GangSchedulerSpec,
	md *nvcrev1alpha1.WorkloadMetadata,
) error {
	if err := applyWorkloadMetadata(&spec.JobTemplate.Spec, ResolvedWorkflowTransforms{
		GangScheduler:  gs,
		WorkloadLabels: workload.LabelsOf(md),
	}, WorkloadRunWorkloadLabelsPath); err != nil {
		return err
	}
	spec.GangScheduler = ResolvedGangScheduler(gs)
	return nil
}

// ResolvedGangScheduler returns a copy of gs with queue and queueLabelKey
// filled in with the defaults the write paths already apply, so the value can
// be persisted on a Workflow and later compared without re-deriving them. It
// returns nil when gs configures nothing, matching the guard in
// ApplyGangSchedulerToDependencies: the CRD requires a non-empty
// schedulerName, but nvcrectl renders straight from a file without consulting
// the API server.
func ResolvedGangScheduler(gs *nvcrev1alpha1.GangSchedulerSpec) *nvcrev1alpha1.GangSchedulerSpec {
	if gs == nil || gs.SchedulerName == "" {
		return nil
	}
	resolved := gs.DeepCopy()
	resolved.Queue = gangSchedulerQueue(gs.Queue)
	resolved.QueueLabelKey = gangSchedulerQueueLabelKey(gs.QueueLabelKey)
	return resolved
}

// ValidateResolvedJobTemplate checks a resolved Job spec against everything
// NVCRE promises about it, and is the entry point every post-override call
// site should use.
//
// Workload metadata is validated whatever the scheduling configuration.
// Admission covers a Job or Workflow that reaches an API server, but offline
// `nvcrectl render` never does, and an override can introduce a reserved key
// or an oversized map after the owning resource was admitted. Gating that
// check behind gangScheduler, as ValidateResolvedGangScheduling alone does,
// would let resolved offline output carry labels the cluster will reject.
//
// The gang-scheduling contract is then checked when an intent is persisted.
func ValidateResolvedJobTemplate(
	js *nvcrev1alpha1.JobSpec,
	deps []nvcrev1alpha1.DependencySpec,
	gs *nvcrev1alpha1.GangSchedulerSpec,
	workloadLabelsPath string,
) error {
	if err := workload.ValidateLabels(
		workload.LabelsOf(js.WorkloadMetadata), workloadLabelsPath); err != nil {
		return err
	}
	return ValidateResolvedGangScheduling(js, deps, gs, workloadLabelsPath)
}

// ValidateResolvedGangScheduling checks that the effective manifests NVCRE is
// about to submit still place the workload in the queue its owner configured,
// and restores the one label NVCRE itself owns.
//
// The asymmetry between the two halves is deliberate:
//
//   - The workload-object queue label is NVCRE's to write. It follows the
//     insert/same/conflict rule, so an override that removed it gets it back
//     and an override that changed it fails.
//   - Runtime queue labels and pod scheduler names are assertions over
//     dependency payloads the operator may have redirected. A missing or
//     conflicting runtime value fails and is never repaired, because silently
//     rewriting someone's runtime override would hide the disagreement rather
//     than surface it.
//
// js is the working copy used to create children, not the stored Workflow, so
// restoring the label here does not touch the Workflow's immutable metadata.
//
// A nil gs is a no-op. Callers wanting the checks that apply whatever the
// scheduling configuration should use ValidateResolvedJobTemplate instead.
//
// gs is re-resolved rather than trusted. NVCRE persists it already resolved,
// but queue and queueLabelKey are optional on the CRD, so a hand-authored
// Workflow can name only a schedulerName. Defaulting here keeps that case
// checking the same queue the write paths would have used instead of an empty
// key. Re-resolving an already-resolved value changes nothing.
func ValidateResolvedGangScheduling(
	js *nvcrev1alpha1.JobSpec,
	deps []nvcrev1alpha1.DependencySpec,
	gs *nvcrev1alpha1.GangSchedulerSpec,
	workloadLabelsPath string,
) error {
	gs = ResolvedGangScheduler(gs)
	if gs == nil {
		return nil
	}

	// NVCRE-owned: restore a removed key, accept an identical value, reject a
	// different one.
	labels, err := workload.InsertConsistentLabel(
		workload.LabelsOf(js.WorkloadMetadata),
		gs.QueueLabelKey, gs.Queue, workloadLabelsPath)
	if err != nil {
		return err
	}
	if err := workload.ValidateLabels(labels, workloadLabelsPath); err != nil {
		return err
	}
	js.WorkloadMetadata = workload.MetadataFrom(labels)

	return validateRuntimeGangScheduling(js, deps, gs)
}

// validateRuntimeGangScheduling asserts the referenced TrainingRuntime still
// carries the configured queue and scheduler on every replicated job, with
// the TrainJob's runtimePatches folded in.
func validateRuntimeGangScheduling(
	js *nvcrev1alpha1.JobSpec,
	deps []nvcrev1alpha1.DependencySpec,
	gs *nvcrev1alpha1.GangSchedulerSpec,
) error {
	trainJob := js.Workload.TrainJob
	if trainJob == nil {
		return fmt.Errorf(
			"gangScheduler is configured but spec.jobTemplate.spec.workload has no trainJob, so its runtime scheduling cannot be checked")
	}

	// Kubeflow's Trainer selects the runtime by group, kind and name together,
	// so all three have to match the dependency being inspected. Checking the
	// name alone would let an override repoint runtimeRef at a different kind
	// or group and still pass against the original dependency, which would
	// assert a guarantee about a resource the workload never uses.
	apiGroup, kind, runtimeName := resolveRuntimeRef(trainJob.RuntimeRef)
	if apiGroup != trainerAPIGroup {
		return fmt.Errorf(
			"gangScheduler is configured but the workload references runtime apiGroup %q rather than %q, so its scheduling cannot be checked",
			apiGroup, trainerAPIGroup)
	}
	if kind != kindTrainingRuntime {
		// A ClusterTrainingRuntime is cluster-scoped and is never supplied as
		// a Workflow dependency, so there is nothing here to inspect. Failing
		// is the honest answer: NVCRE cannot promise a queue it cannot see.
		return fmt.Errorf(
			"gangScheduler is configured but the workload references runtime kind %q; only a namespaced %s supplied as a Workflow dependency can be checked",
			kind, kindTrainingRuntime)
	}

	runtime, err := findTrainingRuntime(deps, runtimeName)
	if err != nil {
		return err
	}

	replicatedJobs, ok := nestedSlice(runtime, keySpec, keyTemplate, keySpec, keyReplicatedJobs)
	if !ok {
		return fmt.Errorf(
			"gangScheduler is configured but TrainingRuntime %q has no spec.template.spec.replicatedJobs, so its queue labels cannot be checked",
			runtimeName)
	}

	// Patch labels are keyed by replicated job name and apply across all
	// managers, which is how Kueue and the catalog both reach these fields.
	patchedJobLabels, patchedPodLabels := runtimePatchLabels(trainJob.RuntimePatches)

	for i, rj := range replicatedJobs {
		job, isMap := rj.(map[string]any)
		if !isMap {
			return fmt.Errorf(
				"gangScheduler is configured but TrainingRuntime %q replicatedJobs[%d] is not an object, so its queue label cannot be checked",
				runtimeName, i)
		}
		name, _ := job[keyName].(string)

		jobTemplate, ok := nestedMap(job, keyTemplate)
		if !ok {
			return fmt.Errorf(
				"gangScheduler is configured but TrainingRuntime %q replicatedJob %q has no template, so its queue label cannot be checked",
				runtimeName, name)
		}
		podTemplate, ok := nestedMap(jobTemplate, keySpec, keyTemplate)
		if !ok {
			return fmt.Errorf(
				"gangScheduler is configured but TrainingRuntime %q replicatedJob %q has no pod template, so its queue label cannot be checked",
				runtimeName, name)
		}

		jobLabels := effectiveLabels(jobTemplate, patchedJobLabels[name])
		if err := assertQueueLabel(jobLabels, gs, runtimeName, name, "template.metadata.labels"); err != nil {
			return err
		}

		podLabels := effectiveLabels(podTemplate, patchedPodLabels[name])
		if err := assertQueueLabel(podLabels, gs, runtimeName, name, "template.spec.template.metadata.labels"); err != nil {
			return err
		}

		// runtimePatches cannot reach schedulerName — PodSpecPatch does not
		// expose it — so the dependency payload is the effective value.
		podSpec, ok := nestedMap(podTemplate, keySpec)
		if !ok {
			return fmt.Errorf(
				"gangScheduler is configured but TrainingRuntime %q replicatedJob %q has no pod spec, so its scheduler name cannot be checked",
				runtimeName, name)
		}
		scheduler, _ := podSpec[keySchedulerName].(string)
		if scheduler != gs.SchedulerName {
			return fmt.Errorf(
				"gangScheduler is configured with schedulerName %q but TrainingRuntime %q replicatedJob %q sets schedulerName %q",
				gs.SchedulerName, runtimeName, name, scheduler)
		}
	}
	return nil
}

// resolveRuntimeRef returns the reference's effective apiGroup, kind and name,
// applying the defaults Kubeflow's TrainJob CRD applies. The defaults matter:
// a runtimeRef that names only a runtime resolves to a
// ClusterTrainingRuntime, not to the namespaced TrainingRuntime a catalog
// entry ships, so reading the fields raw would mistake one for the other.
// Only an absent field is defaulted. An explicitly empty string is preserved,
// because the CRD accepts it and Trainer then selects the runtime with that
// empty group or kind — treating it as the default would validate a different
// resource than the one the workload resolves, and report a guarantee that
// does not hold.
func resolveRuntimeRef(ref trainerv1alpha1.RuntimeRef) (apiGroup, kind, name string) {
	apiGroup = trainerAPIGroup
	if ref.APIGroup != nil {
		apiGroup = *ref.APIGroup
	}
	kind = kindClusterTrainingRuntime
	if ref.Kind != nil {
		kind = *ref.Kind
	}
	return apiGroup, kind, ref.Name
}

// findTrainingRuntime returns the decoded TrainingRuntime dependency named
// name, matching the dependency's apiVersion group and kind as well so the
// inspected resource is the one Trainer will resolve.
//
// An override that redirects runtimeRef at a runtime NVCRE was not given fails
// rather than passing unchecked: the whole point of the persisted intent is
// that the manifests NVCRE submits are consistent, and it cannot vouch for a
// runtime it cannot see.
func findTrainingRuntime(
	deps []nvcrev1alpha1.DependencySpec, name string,
) (map[string]any, error) {
	for i := range deps {
		if len(deps[i].Raw) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := json.Unmarshal(deps[i].Raw, &obj); err != nil {
			return nil, fmt.Errorf("unmarshal dependency %d: %w", i, err)
		}
		if kind, _ := obj[keyKind].(string); kind != kindTrainingRuntime {
			continue
		}
		apiVersion, _ := obj[keyAPIVersion].(string)
		if group, _, _ := strings.Cut(apiVersion, "/"); group != trainerAPIGroup {
			continue
		}
		metadata, ok := nestedMap(obj, keyMetadata)
		if !ok {
			continue
		}
		if depName, _ := metadata[keyName].(string); depName == name {
			return obj, nil
		}
	}
	return nil, fmt.Errorf(
		"gangScheduler is configured but the workload references runtime %q, which is not among the Workflow's %s.%s dependencies",
		name, kindTrainingRuntime, trainerAPIGroup)
}

// runtimePatchLabels collects the Job-template and pod-template labels the
// TrainJob's runtimePatches set, keyed by replicated job name, so launcher and
// worker templates are both covered.
func runtimePatchLabels(
	patches []trainerv1alpha1.RuntimePatch,
) (jobLabels, podLabels map[string]map[string]string) {
	jobLabels = map[string]map[string]string{}
	podLabels = map[string]map[string]string{}
	for _, patch := range patches {
		if patch.TrainingRuntimeSpec == nil || patch.TrainingRuntimeSpec.Template == nil ||
			patch.TrainingRuntimeSpec.Template.Spec == nil {
			continue
		}
		for _, rj := range patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs {
			if rj.Template == nil {
				continue
			}
			if rj.Template.Metadata != nil {
				jobLabels[rj.Name] = workload.MergeLabels(jobLabels[rj.Name], rj.Template.Metadata.Labels)
			}
			if rj.Template.Spec != nil && rj.Template.Spec.Template != nil &&
				rj.Template.Spec.Template.Metadata != nil {
				podLabels[rj.Name] = workload.MergeLabels(
					podLabels[rj.Name], rj.Template.Spec.Template.Metadata.Labels)
			}
		}
	}
	return jobLabels, podLabels
}

// effectiveLabels reads metadata.labels off a template and overlays the labels
// a runtimePatch sets for the same template, which is what the Trainer
// controller will do.
func effectiveLabels(template map[string]any, patched map[string]string) map[string]string {
	labels := map[string]string{}
	if metadata, ok := nestedMap(template, keyMetadata); ok {
		if raw, ok := metadata[keyLabels].(map[string]any); ok {
			for key, value := range raw {
				if str, isStr := value.(string); isStr {
					labels[key] = str
				}
			}
		}
	}
	maps.Copy(labels, patched)
	return labels
}

// assertQueueLabel requires the configured queue under the configured key.
// A missing key is as much of a failure as a wrong value: without it the
// scheduler will not hold the gang.
func assertQueueLabel(
	labels map[string]string, gs *nvcrev1alpha1.GangSchedulerSpec,
	runtimeName, jobName, path string,
) error {
	value, present := labels[gs.QueueLabelKey]
	if !present {
		return fmt.Errorf(
			"gangScheduler is configured with queue %q but TrainingRuntime %q replicatedJob %q is missing label %q on %s",
			gs.Queue, runtimeName, jobName, gs.QueueLabelKey, path)
	}
	if value != gs.Queue {
		return fmt.Errorf(
			"gangScheduler is configured with queue %q but TrainingRuntime %q replicatedJob %q sets label %q to %q on %s",
			gs.Queue, runtimeName, jobName, gs.QueueLabelKey, value, path)
	}
	return nil
}

// nestedMap walks obj down the given keys and returns the map found at the
// end, reporting false if any step is missing or is not a map. Unlike
// ensureMap it never creates structure: neither validation nor preservation
// may invent the fields it is reading.
func nestedMap(obj map[string]any, keys ...string) (map[string]any, bool) {
	cur := obj
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}
