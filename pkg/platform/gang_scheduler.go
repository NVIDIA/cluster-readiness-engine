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
	// labelKeyGangQueue is the label KAI Scheduler reads to place a workload in a queue.
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

// ApplyGangSchedulerToDependencies rewrites every TrainingRuntime dependency in
// place so each of its replicatedJobs runs under the configured gang scheduler.
// For each replicatedJob it sets schedulerName on the pod spec and the
// kai.scheduler/queue label on the job template metadata, matching what
// BuildTorchRuntime and BuildMPIRuntime already emit for a WorkloadRun.
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
			podSpec := ensureMap(ensureMap(ensureMap(jobTemplate, keySpec), keyTemplate), keySpec)
			podSpec[keySchedulerName] = gs.SchedulerName

			// Queue label: replicatedJobs[].template.metadata.labels.
			labels := ensureMap(ensureMap(jobTemplate, keyMetadata), keyLabels)
			labels[labelKeyGangQueue] = queue
		}

		raw, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal dependency %d: %w", i, err)
		}
		deps[i].Raw = raw
	}
	return nil
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
