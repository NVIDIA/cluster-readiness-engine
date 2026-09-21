// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// transformInput is the shape of each case's input.yaml: the resolved
// WorkflowSpec a catalog lookup and override pass would have produced, plus
// the intents the owning Certification carries.
type transformInput struct {
	Spec nvcrev1alpha1.WorkflowSpec `json:"spec"`

	GangScheduler  *nvcrev1alpha1.GangSchedulerSpec `json:"gangScheduler,omitempty"`
	Image          string                           `json:"image,omitempty"`
	WorkloadLabels map[string]string                `json:"workloadLabels,omitempty"`
}

// transformResult is the golden view of the resolved spec.
//
// PersistedGangScheduler is the whole value written to spec.gangScheduler, so
// the golden shows the defaults being resolved rather than copied through: a
// Certification naming only a schedulerName must come back with an explicit
// queue and queueLabelKey, because that is what later post-override checks
// compare against.
//
// RuntimeQueueLabels and TrainerImage are here so the pinned order stays
// observable: gang scheduling still reaches the runtime, and the image
// override still reaches the trainer, even though the workload metadata is
// now written after both.
type transformResult struct {
	Error string `json:"error,omitempty"`

	WorkloadLabels         map[string]string                `json:"workloadLabels"`
	PersistedGangScheduler *nvcrev1alpha1.GangSchedulerSpec `json:"persistedGangScheduler"`
	RuntimeQueueLabels     map[string]string                `json:"runtimeQueueLabels,omitempty"`
	TrainerImage           string                           `json:"trainerImage,omitempty"`
	RuntimeImages          map[string]string                `json:"runtimeImages,omitempty"`
}

// TestApplyResolvedWorkflowTransforms drives the named post-resolve stage that
// the Certification controller and both nvcrectl render paths now share.
//
// The cases prove the three properties the stage is responsible for: the
// composition order for workload labels (resolved catalog JobSpec, then the
// owner's resolved labels, then the gang-scheduler queue invariant), that the
// two pre-existing transforms still reach their disjoint fields from the new
// position, and that the resolved gang-scheduling intent is persisted with its
// defaults filled in.
func TestApplyResolvedWorkflowTransforms(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "apply-resolved-transforms",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in transformInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		result := transformResult{}
		err := ApplyResolvedWorkflowTransforms(&in.Spec, ResolvedWorkflowTransforms{
			GangScheduler:  in.GangScheduler,
			Image:          in.Image,
			WorkloadLabels: in.WorkloadLabels,
		})
		if err != nil {
			result.Error = err.Error()
		}

		result.WorkloadLabels = projectWorkloadLabels(&in.Spec)
		result.PersistedGangScheduler = in.Spec.GangScheduler
		result.RuntimeQueueLabels = projectRuntimeQueueLabels(&in.Spec)
		if tj := in.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil &&
			tj.Trainer != nil && tj.Trainer.Image != nil {
			result.TrainerImage = *tj.Trainer.Image
		}
		result.RuntimeImages = projectRuntimeImages(&in.Spec)

		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// validateGangResult is the golden view of a post-override check.
//
// WorkloadLabels is recorded after the call because the check is also the one
// place that restores the workload-object queue label NVCRE owns, so a case
// can show a removed label coming back. RuntimeQueueLabels is recorded for the
// opposite reason: it must show a missing or wrong runtime label staying
// exactly as it was, never repaired.
type validateGangResult struct {
	Error              string            `json:"error,omitempty"`
	WorkloadLabels     map[string]string `json:"workloadLabels"`
	RuntimeQueueLabels map[string]string `json:"runtimeQueueLabels,omitempty"`
}

// TestValidateResolvedGangScheduling drives the authoritative post-override
// check. The persisted intent in each case's spec.gangScheduler stands in for
// the value a WorkloadRun or Certification recorded at construction time, and
// the rest of the spec stands in for whatever the overrides resolved to.
//
// The cases pin the deliberate asymmetry at the centre of this helper. The
// workload-object queue label is NVCRE's to write, so a removal is restored
// and a change is rejected. Runtime queue labels and pod scheduler names are
// assertions over dependency payloads an operator may have redirected, so a
// missing or conflicting value is rejected and never repaired — silently
// rewriting someone's runtime override would hide the disagreement instead of
// surfacing it.
func TestValidateResolvedGangScheduling(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "validate-resolved-gang",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in transformInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		result := validateGangResult{}
		err := ValidateResolvedGangScheduling(
			&in.Spec.JobTemplate.Spec, in.Spec.Dependencies, in.Spec.GangScheduler,
			JobTemplateWorkloadLabelsPath)
		if err != nil {
			result.Error = err.Error()
		}

		result.WorkloadLabels = projectWorkloadLabels(&in.Spec)
		result.RuntimeQueueLabels = projectRuntimeQueueLabels(&in.Spec)

		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// TestValidateResolvedJobTemplate drives the entry point every post-override
// call site uses. Its cases are the ones that must hold whatever the
// scheduling configuration: an override can introduce a reserved key, a
// malformed key or an oversized map long after the owning WorkloadRun or
// Certification was admitted, and offline `nvcrectl render` never reaches an
// API server that would catch it.
//
// The gang-specific behavior has its own cases under validate-resolved-gang;
// these exist to pin that the label half is not gated behind gangScheduler.
func TestValidateResolvedJobTemplate(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "validate-resolved-job-template",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in transformInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		result := validateGangResult{}
		if err := ValidateResolvedJobTemplate(
			&in.Spec.JobTemplate.Spec, in.Spec.Dependencies, in.Spec.GangScheduler,
			JobTemplateWorkloadLabelsPath); err != nil {
			result.Error = err.Error()
		}

		result.WorkloadLabels = projectWorkloadLabels(&in.Spec)
		result.RuntimeQueueLabels = projectRuntimeQueueLabels(&in.Spec)

		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// preserveResult is the golden view of a rename-then-preserve round trip.
// Renamed shows what a blind substitution alone produced; Preserved shows what
// the dependency looks like after the scheduling fields are put back.
type preserveResult struct {
	Error        string            `json:"error,omitempty"`
	RenamedOnly  map[string]string `json:"renamedOnly"`
	Preserved    map[string]string `json:"preserved"`
	SourceIntact bool              `json:"sourceIntact"`
}

// TestPreserveSchedulingFields covers the distinction that makes per-job
// dependency renaming safe without turning it into silent repair.
//
// Renaming rewrites every quoted occurrence of a dependency name, so a queue
// value equal to one gets mangled. Putting the pre-rename value back fixes
// that. Re-applying the configured intent instead would look similar on the
// passing cases and be wrong on the important one: it would overwrite an
// operator's conflicting override, and the Job would run in a queue nobody
// chose rather than failing validation.
//
// So the cases come in pairs: a collision that must be restored, and an
// override that must be carried through untouched so validation can reject it.
func TestPreserveSchedulingFields(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "preserve-scheduling-fields",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			GangScheduler *nvcrev1alpha1.GangSchedulerSpec `json:"gangScheduler,omitempty"`
			Replacements  map[string]string                `json:"replacements"`
			Dependency    nvcrev1alpha1.DependencySpec     `json:"dependency"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		original := append([]byte(nil), in.Dependency.Raw...)

		renamed := string(original)
		for old, newVal := range in.Replacements {
			renamed = strings.ReplaceAll(renamed, `"`+old+`"`, `"`+newVal+`"`)
		}

		result := preserveResult{}
		result.RenamedOnly = projectRuntimeQueueLabels(specWithDependency([]byte(renamed)))

		// Preservation compares the renamed document against the pre-rename
		// one, which is the only place the original values still exist.
		preserved, err := PreserveSchedulingFields(original, []byte(renamed), in.GangScheduler)
		if err != nil {
			result.Error = err.Error()
			return marshalPreserve(tc, result)
		}
		result.Preserved = projectRuntimeQueueLabels(specWithDependency(preserved))
		result.SourceIntact = string(original) == string(in.Dependency.Raw)

		return marshalPreserve(tc, result)
	})
}

func marshalPreserve(tc *testutil.TestCase, result preserveResult) error {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	tc.Actual = string(b) + "\n"
	return nil
}

// specWithDependency wraps one raw dependency so the shared projection helper
// can read it.
func specWithDependency(raw []byte) *nvcrev1alpha1.WorkflowSpec {
	return &nvcrev1alpha1.WorkflowSpec{
		Dependencies: []nvcrev1alpha1.DependencySpec{{Raw: raw}},
	}
}

// projectWorkloadLabels reads the resolved workload-object labels as a plain
// map. It returns nil rather than an empty map when the field is absent, which
// is the difference between "no workloadMetadata" and "workloadMetadata with
// nothing in it".
func projectWorkloadLabels(spec *nvcrev1alpha1.WorkflowSpec) map[string]string {
	md := spec.JobTemplate.Spec.WorkloadMetadata
	if md == nil || len(md.Labels) == 0 {
		return nil
	}
	labels := make(map[string]string, len(md.Labels))
	for key, value := range md.Labels {
		labels[key] = string(value)
	}
	return labels
}

// projectRuntimeQueueLabels flattens every queue-bearing label in every
// TrainingRuntime dependency, keyed by "<runtime>/<replicatedJob>/<level>", so
// one golden field covers the Job-template and pod-template copies of both the
// worker and the launcher. Absent labels are simply missing from the map,
// which is what a "must stay absent after a failed check" case asserts.
func projectRuntimeQueueLabels(spec *nvcrev1alpha1.WorkflowSpec) map[string]string {
	out := map[string]string{}
	for i := range spec.Dependencies {
		if len(spec.Dependencies[i].Raw) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := json.Unmarshal(spec.Dependencies[i].Raw, &obj); err != nil {
			continue
		}
		if kind, _ := obj["kind"].(string); kind != kindTrainingRuntime {
			continue
		}
		metadata, _ := nestedMap(obj, keyMetadata)
		runtimeName, _ := metadata[keyName].(string)

		replicatedJobs, ok := nestedSlice(obj, keySpec, keyTemplate, keySpec, keyReplicatedJobs)
		if !ok {
			continue
		}
		for _, rj := range replicatedJobs {
			job, isMap := rj.(map[string]any)
			if !isMap {
				continue
			}
			name, _ := job[keyName].(string)
			jobTemplate, ok := nestedMap(job, keyTemplate)
			if !ok {
				continue
			}
			collectQueueLabels(out, jobTemplate, runtimeName+"/"+name+"/job")
			if podTemplate, ok := nestedMap(jobTemplate, keySpec, keyTemplate); ok {
				collectQueueLabels(out, podTemplate, runtimeName+"/"+name+"/pod")
				if podSpec, ok := nestedMap(podTemplate, keySpec); ok {
					if scheduler, _ := podSpec[keySchedulerName].(string); scheduler != "" {
						out[runtimeName+"/"+name+"/schedulerName"] = scheduler
					}
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// projectRuntimeImages reads the primary container image of every replicated
// job, keyed by "<runtime>/<replicatedJob>". It is here so a case can show the
// image transform still reaching the dependencies from its position in the
// pinned order, not only the typed trainer field.
func projectRuntimeImages(spec *nvcrev1alpha1.WorkflowSpec) map[string]string {
	out := map[string]string{}
	for i := range spec.Dependencies {
		if len(spec.Dependencies[i].Raw) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := json.Unmarshal(spec.Dependencies[i].Raw, &obj); err != nil {
			continue
		}
		if kind, _ := obj["kind"].(string); kind != kindTrainingRuntime {
			continue
		}
		metadata, _ := nestedMap(obj, keyMetadata)
		runtimeName, _ := metadata[keyName].(string)

		replicatedJobs, ok := nestedSlice(obj, keySpec, keyTemplate, keySpec, keyReplicatedJobs)
		if !ok {
			continue
		}
		for _, rj := range replicatedJobs {
			job, isMap := rj.(map[string]any)
			if !isMap {
				continue
			}
			name, _ := job[keyName].(string)
			containers, found := nestedSlice(job, keyTemplate, keySpec, keyTemplate, keySpec, keyContainers)
			if !found || len(containers) == 0 {
				continue
			}
			primary, isMap := containers[0].(map[string]any)
			if !isMap {
				continue
			}
			if image, _ := primary[keyImage].(string); image != "" {
				out[runtimeName+"/"+name] = image
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectQueueLabels records the queue-ish labels on one template under the
// given prefix. Both the KAI default key and the Run:ai key are collected so a
// case using a custom queueLabelKey still shows its label.
func collectQueueLabels(out map[string]string, template map[string]any, prefix string) {
	metadata, ok := nestedMap(template, keyMetadata)
	if !ok {
		return
	}
	labels, ok := metadata[keyLabels].(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{labelKeyGangQueue, "runai/queue"} {
		if value, present := labels[key].(string); present {
			out[prefix+"/"+key] = value
		}
	}
}
