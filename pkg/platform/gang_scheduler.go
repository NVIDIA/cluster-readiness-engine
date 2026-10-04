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

	keyReplicatedJobs = "replicatedJobs"
	// keyDependsOn is the JobSet gate the launcher carries outside KAI.
	keyDependsOn        = "dependsOn"
	keyKind             = "kind"
	kindTrainingRuntime = "TrainingRuntime"

	// schedulerNameKAI is KAI Scheduler's name. It is the gang scheduler whose
	// JobSet ordering cannot use the launcher's dependsOn gate: KAI's jobset
	// grouper maps spec.startupPolicy.startupPolicyOrder == "InOrder" to a root
	// minSubGroup of 1 and defaults an unset order to "AnyOrder", so a JobSet
	// whose launcher is dependsOn-gated asks KAI to place a launcher sub-group
	// that JobSet will not create until the workers are Ready. Measured live on
	// KAI v0.16.4: the workers stay Pending and the scheduler reports the
	// unsatisfiable sub-group.
	schedulerNameKAI = "kai-scheduler"
)

// isKAIGangScheduler reports whether the runtime opts the workload into KAI
// Scheduler. Under KAI the launcher ordering cannot be expressed as a JobSet
// gate or startup policy at all: this scheduler deletes the launcher's
// dependsOn gate and any startupPolicy and holds the launcher back inside the
// pod instead (see launcherWaitScript and schedulerNameKAI).
func isKAIGangScheduler(cfg RuntimeConfig) bool {
	return cfg.GangSchedulerName == schedulerNameKAI
}

// launcherWaitScript is the shell fragment that holds the launcher back until
// every worker answers sshd. It exists because the JobSet cannot express that
// barrier under KAI: a dependsOn gate or an InOrder startup policy leaves the
// launcher sub-group empty, and KAI requires every sub-group of the PodGroup to
// have pods before it schedules anything (measured live on KAI v0.16.4). The
// worker list is Trainer's MPI hostfile, whose entries are "<endpoint> slots=<n>".
func launcherWaitScript() string {
	return `
[ -f /root/.ssh/id_rsa ] || { mkdir -p /root/.ssh && chmod 700 /root/.ssh && cp /tmp/mpi-ssh-raw/* /root/.ssh/ 2>/dev/null; chmod 600 /root/.ssh/id_rsa 2>/dev/null; }
hosts=$(awk '{print $1}' ` + mpiHostfilePath + ` 2>/dev/null)
if [ -z "$hosts" ]; then
  echo "ERROR: no hosts in ` + mpiHostfilePath + ` (missing, empty, or unreadable); refusing to start mpirun."
  exit 1
fi
# With mpi.runLauncherAsNode the hostfile also lists this pod's own endpoint. Probing it would
# deadlock: its sshd lives in a container that only starts once this init container finishes.
# So skip our own endpoint and keep probing every worker.
self=${HOSTNAME:-}
[ -n "$self" ] || self=$(hostname 2>/dev/null)
i=0
ok=0
while [ $i -lt 240 ]; do
  ok=1
  for h in $hosts; do
    case "$h" in
      "$self"|"$self".*) continue ;;
    esac
    if ! timeout 10 ssh -n -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 "$h" true >/dev/null 2>&1; then
      ok=0
      # Every tenth attempt, report which host is not answering and why: without
      # this, a blocked network path and a slow worker look identical and the
      # only signal is a bare 20-minute timeout.
      if [ $((i % 10)) -eq 0 ]; then
        echo "waiting: $h has not answered sshd yet (attempt $((i+1))/240, $(($i*5))s)"
        timeout 10 ssh -n -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 "$h" true 2>&1 | head -3 | sed 's/^/  ssh: /'
      fi
    fi
  done
  [ "$ok" = "1" ] && break
  i=$((i+1))
  sleep 5
done
if [ "$ok" != "1" ]; then
  echo "ERROR: not every worker answered sshd within 20 minutes; refusing to start mpirun."
  echo "  hostfile: $(cat ` + mpiHostfilePath + ` 2>/dev/null | tr '\n' ' ')"
  echo "  check that the workers and this launcher share a network policy allowing pod-to-pod port 22"
  exit 1
fi
echo "workers answered sshd after $((i*5))s"
`
}

// launcherWaitMount is the volume mount the wait step needs: Trainer's MPI
// hostfile, which the Trainer MPI plugin mounts into the launcher pod.
func launcherWaitMount() map[string]any {
	return map[string]any{keyName: volumeNameMPIHostfile, keyMountPath: mpiHostfileDir, keyReadOnly: true}
}

// launcherWaitInitContainer wraps launcherWaitScript for paths that inject the
// barrier as its own init container (the Certification path renders the runtime
// from a catalog template, so the script cannot be appended to an existing one).
func launcherWaitInitContainer(image string) map[string]any {
	// The wait step needs the same SSH material the launcher uses, so it mounts
	// the runtime's SSH-key volumes too: the init container that ran before it
	// leaves the private key in the shared ssh-keys volume, and ssh reads it
	// from /root/.ssh.
	return map[string]any{
		keyName:   "wait-for-workers",
		keyImage:  image,
		"command": []string{"sh", "-c"},
		"args":    []any{launcherWaitScript()},
		keyVolumeMounts: []any{
			launcherWaitMount(),
			map[string]any{keyName: mpiSSHAuthName, keyMountPath: mpiSSHMountPath, keyReadOnly: true},
			map[string]any{keyName: volumeNameSSHKeys, keyMountPath: "/root/.ssh"},
		},
	}
}

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
// For the KAI + MPI shape it also rewrites the JobSet ordering: it deletes the
// launcher's dependsOn gate together with the JobSet startupPolicy (the two are
// mutually exclusive) and appends the wait-for-workers init container, because
// KAI requires every sub-group of the PodGroup to have pods before the group is
// schedulable — an ordered or gated launcher sub-group is empty until the
// workers are ready and can never be admitted. The wait therefore lives inside
// the launcher pod (launcherWaitScript); a dependency between replicated jobs
// that is not the launcher's node-ready gate keeps its meaning (measured live on
// KAI v0.16.4; see schedulerNameKAI).
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

		if gs.SchedulerName == schedulerNameKAI && hasMPIPolicy(obj) {
			// KAI schedules a JobSet only when every sub-group of the PodGroup has
			// pods, so an MPI launcher may not be gated or ordered behind the
			// workers: the wait moves into the launcher pod (launcherWaitScript).
			// Only the gate BuildMPIRuntime emits is rewritten — an unrelated
			// dependency between replicated jobs keeps its meaning — and any
			// startup policy goes with it, because a JobSet cannot carry both and
			// an InOrder policy would leave the same empty launcher sub-group.
			if launcherJob, ok := launcherReplicatedJob(replicatedJobs); ok {
				if isNodeReadyGate(launcherJob[keyDependsOn]) {
					delete(launcherJob, keyDependsOn)
					if jobSetSpec := jobSetSpecOf(obj); jobSetSpec != nil {
						delete(jobSetSpec, keyStartupPolicy)
					}
					podSpec := launcherPodSpec(launcherJob)
					if podSpec != nil && !hasWaitInitContainer(podSpec) {
						inits, _ := podSpec[keyInitContainers].([]any)
						podSpec[keyInitContainers] = append(inits, launcherWaitInitContainer(launcherImage(launcherJob)))
					}
				}
			}
		}

		raw, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal dependency %d: %w", i, err)
		}
		deps[i].Raw = raw
	}
	return nil
}

// keyStartupPolicy is the JobSet field that orders replicated jobs. It is
// mutually exclusive with a dependsOn gate, so removing one removes the other.
const keyStartupPolicy = "startupPolicy"

// hasMPIPolicy reports whether a TrainingRuntime dependency runs an MPI workload
// (its template declares mlPolicy.mpi). Only those have the launcher gate this
// rewrite understands; a torch runtime that happens to call a replicated job
// "launcher" is left alone.
func hasMPIPolicy(obj map[string]any) bool {
	// mlPolicy is a sibling of the JobSet template on the TrainingRuntime
	// (spec.mlPolicy), not a field inside it.
	_, ok := nestedMap(obj, keySpec, "mlPolicy", "mpi")
	return ok
}

// isNodeReadyGate reports whether value is exactly the gate BuildMPIRuntime
// emits: a single dependency on the "node" replicated job reaching Ready. Any
// other dependency expresses something this rewrite must not reinterpret.
func isNodeReadyGate(value any) bool {
	list, ok := value.([]any)
	if !ok || len(list) != 1 {
		return false
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		return false
	}
	name, _ := entry[keyName].(string)
	status, _ := entry["status"].(string)
	return name == nodeJobName && status == "Ready"
}

// jobSetSpecOf returns the JobSet spec inside a TrainingRuntime dependency.
func jobSetSpecOf(obj map[string]any) map[string]any {
	spec, ok := nestedMap(obj, keySpec, keyTemplate, keySpec)
	if !ok {
		return nil
	}
	return spec
}

// hasWaitInitContainer reports whether the launcher already carries the wait
// step, so a second pass over the same object cannot inject it twice.
func hasWaitInitContainer(podSpec map[string]any) bool {
	inits, _ := podSpec[keyInitContainers].([]any)
	for _, c := range inits {
		if container, ok := c.(map[string]any); ok {
			if name, _ := container[keyName].(string); name == "wait-for-workers" {
				return true
			}
		}
	}
	return false
}

// launcherReplicatedJob returns the replicated job named "launcher".
func launcherReplicatedJob(replicatedJobs []any) (map[string]any, bool) {
	for _, rj := range replicatedJobs {
		job, ok := rj.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := job[keyName].(string); name == "launcher" {
			return job, true
		}
	}
	return nil, false
}

// launcherPodSpec walks replicatedJobs[].template.spec.template.spec.
func launcherPodSpec(launcherJob map[string]any) map[string]any {
	return ensureMap(ensureMap(ensureMap(ensureMap(launcherJob, keyTemplate), keySpec), keyTemplate), keySpec)
}

// launcherImage returns the image of the launcher's first container, which the
// wait init container reuses so no extra pull is needed.
func launcherImage(launcherJob map[string]any) string {
	podSpec := launcherPodSpec(launcherJob)
	containers, _ := podSpec[keyContainers].([]any)
	for _, c := range containers {
		if container, ok := c.(map[string]any); ok {
			if image, ok := container[keyImage].(string); ok && image != "" {
				return image
			}
		}
	}
	return ""
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
