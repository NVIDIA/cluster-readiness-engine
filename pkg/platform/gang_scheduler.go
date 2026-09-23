// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	// keySchedulerName is the pod spec field naming the scheduler that binds the pod.
	keySchedulerName = "schedulerName"
	// labelKeyGangQueue is the label KAI Scheduler reads to place a workload in
	// a queue. It is the queue label key used when the user names no other.
	labelKeyGangQueue = "kai.scheduler/queue"
	// defaultGangQueue is the queue used when the user names a scheduler but no queue.
	defaultGangQueue = "default-queue"

	keyReplicatedJobs   = "replicatedJobs"
	keyKind             = "kind"
	kindTrainingRuntime = "TrainingRuntime"
)

// gangSchedulerQueue returns the effective queue name, defaulting to "default-queue".
func gangSchedulerQueue(queue string) string {
	if queue != "" {
		return queue
	}
	return defaultGangQueue
}

// gangSchedulerQueueLabelKey returns the effective queue label key, defaulting
// to "kai.scheduler/queue". A scheduler that reads a different label, such as
// the NVIDIA Run:ai platform with "runai/queue", is selected by setting
// gangScheduler.queueLabelKey.
func gangSchedulerQueueLabelKey(key string) string {
	if key != "" {
		return key
	}
	return labelKeyGangQueue
}

// ApplyGangSchedulerToDependencies rewrites every TrainingRuntime dependency in
// place so each of its replicatedJobs runs under the configured gang scheduler.
// For each replicatedJob it sets schedulerName on the pod spec and the queue
// label (gangScheduler.queueLabelKey, "kai.scheduler/queue" when unset) on both
// the job template metadata and the pod template metadata, matching what
// BuildTorchRuntime and BuildMPIRuntime already emit for a WorkloadRun. The
// pod-level copy keeps queue assignment from depending on the Trainer/JobSet
// layer propagating template metadata onto the pods.
//
// It is a no-op when gs is nil, so a Certification that does not ask for gang
// scheduling renders byte-identically to before. It is also a no-op when the
// scheduler name is empty, matching applyGangScheduler: the CRD requires a
// non-empty schedulerName, but nvcrectl renders straight from a file without
// consulting the API server, so without this guard a typo would render pod
// templates pinned to an empty scheduler and carrying a stray queue label.
//
// Callers must invoke this after overrides are resolved. The scheduler name is
// overwritten unconditionally rather than filled in only when absent, because
// some catalog entries hardcode schedulerName: default-scheduler and the whole
// point of the field is to replace it.
func ApplyGangSchedulerToDependencies(deps []nvcrev1alpha1.DependencySpec, gs *nvcrev1alpha1.GangSchedulerSpec) error {
	if gs == nil || gs.SchedulerName == "" {
		return nil
	}
	queue := gangSchedulerQueue(gs.Queue)
	queueKey := gangSchedulerQueueLabelKey(gs.QueueLabelKey)

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
		for _, rj := range replicatedJobs {
			job, isMap := rj.(map[string]any)
			if !isMap {
				continue
			}
			// Pod spec: replicatedJobs[].template.spec.template.spec. The
			// nesting is JobTemplateSpec -> JobSpec -> PodTemplateSpec -> PodSpec.
			jobTemplate := ensureMap(job, keyTemplate)
			podTemplate := ensureMap(ensureMap(jobTemplate, keySpec), keyTemplate)
			podSpec := ensureMap(podTemplate, keySpec)
			podSpec[keySchedulerName] = gs.SchedulerName

			// Queue label: replicatedJobs[].template.metadata.labels and
			// replicatedJobs[].template.spec.template.metadata.labels, so the
			// pods carry the label themselves.
			jobLabels := ensureMap(ensureMap(jobTemplate, keyMetadata), keyLabels)
			jobLabels[queueKey] = queue
			podLabels := ensureMap(ensureMap(podTemplate, keyMetadata), keyLabels)
			podLabels[queueKey] = queue
		}

		raw, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal dependency %d: %w", i, err)
		}
		deps[i].Raw = raw
	}
	return nil
}

// PreserveSchedulingFields copies the gang-scheduling fields ADR-076 writes
// from original into renamed, and returns the corrected document.
//
// It exists because per-job dependency renaming rewrites every quoted
// occurrence of a dependency name, which also catches a queue label value that
// happens to equal one — a queue named after the runtime it serves. The queue
// a user configured is not a reference and must survive renaming intact.
//
// Crucially this preserves rather than repairs. Unlike
// ApplyGangSchedulerToDependencies it asserts nothing and inserts nothing: a
// field absent from original stays absent, and a value the operator overrode
// to something else is carried through unchanged. An override that redirects
// the queue therefore survives renaming and is rejected by validation, which
// is the reject-without-repair contract. Re-applying the configured intent
// here instead would silently overwrite that override and let the Job run in a
// queue the operator did not choose.
//
// It is a no-op when gs configures nothing, since then there is no queue label
// key to identify.
func PreserveSchedulingFields(
	original, renamed []byte, gs *nvcrev1alpha1.GangSchedulerSpec,
) ([]byte, error) {
	if gs == nil || gs.SchedulerName == "" || len(original) == 0 || len(renamed) == 0 {
		return renamed, nil
	}
	queueKey := gangSchedulerQueueLabelKey(gs.QueueLabelKey)

	var before, after map[string]any
	if err := json.Unmarshal(original, &before); err != nil {
		return nil, fmt.Errorf("unmarshal pre-rename dependency: %w", err)
	}
	if err := json.Unmarshal(renamed, &after); err != nil {
		return nil, fmt.Errorf("unmarshal renamed dependency: %w", err)
	}
	if kind, _ := after[keyKind].(string); kind != kindTrainingRuntime {
		return renamed, nil
	}

	beforeJobs, okBefore := nestedSlice(before, keySpec, keyTemplate, keySpec, keyReplicatedJobs)
	afterJobs, okAfter := nestedSlice(after, keySpec, keyTemplate, keySpec, keyReplicatedJobs)
	if !okBefore || !okAfter || len(beforeJobs) != len(afterJobs) {
		// A shape that does not line up is left exactly as renaming produced
		// it; validation is what reports an unusable runtime.
		return renamed, nil
	}

	changed := false
	for i := range afterJobs {
		src, srcOK := beforeJobs[i].(map[string]any)
		dst, dstOK := afterJobs[i].(map[string]any)
		if !srcOK || !dstOK {
			continue
		}
		srcJob, ok1 := nestedMap(src, keyTemplate)
		dstJob, ok2 := nestedMap(dst, keyTemplate)
		if !ok1 || !ok2 {
			continue
		}
		changed = restoreLabel(srcJob, dstJob, queueKey) || changed

		srcPod, ok1 := nestedMap(srcJob, keySpec, keyTemplate)
		dstPod, ok2 := nestedMap(dstJob, keySpec, keyTemplate)
		if !ok1 || !ok2 {
			continue
		}
		changed = restoreLabel(srcPod, dstPod, queueKey) || changed

		// The scheduler name is a plain string that could equally collide
		// with a dependency name.
		srcSpec, ok1 := nestedMap(srcPod, keySpec)
		dstSpec, ok2 := nestedMap(dstPod, keySpec)
		if !ok1 || !ok2 {
			continue
		}
		if was, present := srcSpec[keySchedulerName].(string); present {
			if now, _ := dstSpec[keySchedulerName].(string); now != was {
				dstSpec[keySchedulerName] = was
				changed = true
			}
		}
	}

	// Return the input bytes untouched when nothing needed restoring, so a
	// dependency renaming did not disturb round-trips byte for byte.
	if !changed {
		return renamed, nil
	}
	corrected, err := json.Marshal(after)
	if err != nil {
		return nil, fmt.Errorf("marshal renamed dependency: %w", err)
	}
	return corrected, nil
}

// restoreLabel copies one label from src's metadata.labels to dst's, reporting
// whether it had to change anything. A label absent from src is left absent in
// dst rather than created.
func restoreLabel(src, dst map[string]any, key string) bool {
	srcLabels, ok := nestedMap(src, keyMetadata, keyLabels)
	if !ok {
		return false
	}
	was, present := srcLabels[key].(string)
	if !present {
		return false
	}
	dstLabels, ok := nestedMap(dst, keyMetadata, keyLabels)
	if !ok {
		return false
	}
	if now, _ := dstLabels[key].(string); now == was {
		return false
	}
	dstLabels[key] = was
	return true
}

// nestedSlice walks obj down the given keys and returns the slice found at the
// end. It reports false if any step is missing or is not the expected type, so
// a dependency whose shape does not match is skipped rather than panicking.
func nestedSlice(obj map[string]any, keys ...string) ([]any, bool) {
	cur := obj
	for i, k := range keys {
		v, present := cur[k]
		if !present {
			return nil, false
		}
		if i == len(keys)-1 {
			s, ok := v.([]any)
			return s, ok
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = m
	}
	return nil, false
}

// ensureMap returns parent[key] as a map, creating it when absent. A catalog
// entry that omits template.metadata entirely (most of them do) still gets the
// queue label.
func ensureMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	parent[key] = created
	return created
}
