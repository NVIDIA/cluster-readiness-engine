---
title: Workflow
description: CRD reference for the Workflow resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`Workflow` manages a single certification category run. It is created by the `Certification` controller — one per category — and is not typically created directly by users.

## Spec fields

_Fields documented so far:_

| Field | Type | Description |
|-------|------|-------------|
| `jobTemplate.spec.workloadMetadata` | WorkloadMetadata | Optional, immutable. Labels applied to the workload object each generated Job creates. This is a plain `JobSpec` field, so it carries the same admission and transition rules as a direct Job's. See [Job workload object labels](job.md#workload-object-labels) |
| `jobTemplate.spec.nodeHealthMonitor` | NodeHealthMonitor | Optional, immutable in presence and value. Copied to each generated Job |
| `jobTemplate.spec.goodputMeasurement` | GoodputMeasurementConfig | Optional, immutable in presence and value. Copied to each generated Job |
| `jobTemplate.spec.bandwidthMeasurement` | BandwidthMeasurementConfig | Optional, immutable in presence and value. Copied to each generated Job |
| `gangScheduler` | GangSchedulerSpec | Optional, immutable. The resolved gang-scheduling intent of the WorkloadRun or Certification that generated this Workflow. See [Gang scheduling consistency contract](#gang-scheduling-consistency-contract) |
| `orchestration.placement` | string | Optional, `Pinned` or `Unpinned`. Empty means `Pinned`. See [Placement](#placement) |

Because `spec.jobTemplate.spec` is a plain `JobSpec`, its `workloadMetadata` cannot be added, removed, or changed after the Workflow is created. Editing it mid-flight is rejected, so every group and iteration produces Jobs with the same workload labels. Raw `spec.overrides` remain editable; a Job's own metadata becomes immutable at the moment that Job is created.

The measurement fields in `spec.jobTemplate.spec` follow the same transition rules: `nodeHealthMonitor`, `goodputMeasurement`, and `bandwidthMeasurement` cannot be added, removed, or changed after the Workflow is created. Other mutable fields in the Job template remain editable.

## Gang scheduling consistency contract

`spec.gangScheduler` is not a second place to configure gang scheduling — it is a consistency contract. When a WorkloadRun or Certification configures gang scheduling, the generated Workflow records the intent with its queue and queue label key already resolved to their effective values.

The field sits outside `jobTemplate`, `dependencies`, and `orchestration`, so no override can reach it, and it is immutable in both presence and value. That is what lets a rendered Workflow submitted on its own enforce the same contract without fetching its original owner, and without relying on mutable annotations.

After every matching override resolves, and before dependencies or Jobs are created, the Workflow controller checks the effective manifests against the intent:

- The workload object's queue label is NVCRE's to write, so an override that **removed** it gets it restored on the generated Job, and an override that **changed** it fails.
- Every effective runtime Job-template and pod-template queue label must equal the configured queue, and pod scheduler names must match the configured scheduler. `TrainJob` `runtimePatches` are folded in when computing the effective values, for launcher and worker templates alike. A missing or conflicting runtime value **fails and is never silently repaired** — rewriting the operator's own runtime override would hide the disagreement rather than surface it.
- An override that repoints `runtimeRef` at a runtime outside the Workflow's dependencies fails, as does a runtime shape that prevents checking these fields at all.

The check runs again before each later group or iteration is launched, so no subsequent override or runtime-patch mutation can bypass it. A conflict marks the Workflow `Failed` with reason `GangSchedulingConflict` before any child is created. The restore half writes only to the working copy used to create children; the stored Workflow's immutable field is never patched.

Workflows without `gangScheduler` — including hand-authored ones — receive workload-label validation and passthrough, not inferred queue checks, and keep responsibility for their own scheduling configuration.

### Accepted and conflicting overrides

Take a Workflow whose persisted intent is queue `team-a`:

```yaml
spec:
  gangScheduler:
    schedulerName: kai-scheduler
    queue: team-a
    queueLabelKey: kai.scheduler/queue
  jobTemplate:
    spec:
      workloadMetadata:
        labels:
          kai.scheduler/queue: team-a
      workload:
        trainJob:
          runtimeRef:
            kind: TrainingRuntime
            name: nccl-runtime
```

An override that touches unrelated fields, or that removes the workload object's queue label, is accepted. The removed label is restored on the generated Job:

```yaml
spec:
  overrides:
    - when:
        platform:
          equals: aws
      jobTemplatePatch:
        - op: remove
          path: /spec/workloadMetadata/labels/kai.scheduler~1queue
```

An override that changes the workload object's queue label fails the Workflow with reason `GangSchedulingConflict`, naming the key, both values, and the field path:

```yaml
spec:
  overrides:
    - when:
        platform:
          equals: aws
      jobTemplate:
        spec:
          workloadMetadata:
            labels:
              kai.scheduler/queue: team-b
```

```text
spec.jobTemplate.spec.workloadMetadata.labels["kai.scheduler/queue"]: conflicting label value "team-b", expected "team-a"
```

An override that rewrites a runtime queue label fails the same way and the runtime is left as the override wrote it, never repaired:

```yaml
spec:
  overrides:
    - when:
        platform:
          equals: aws
      dependencies:
        - apiVersion: trainer.kubeflow.org/v1alpha1
          kind: TrainingRuntime
          metadata:
            name: nccl-runtime
          spec:
            template:
              spec:
                replicatedJobs:
                  - name: node
                    template:
                      metadata:
                        labels:
                          kai.scheduler/queue: team-b
```

```text
gangScheduler is configured with queue "team-a" but TrainingRuntime "nccl-runtime" replicatedJob "node" sets label "kai.scheduler/queue" to "team-b" on template.metadata.labels
```

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | Exclusive set: `InProgress`, `Succeeded`, `Failed`. Independent (additive): `ValidationFailed` — aggregates Job validation failures; set True alongside `Failed` whenever any Job failed performance threshold validation, even when hardware failures determine the `Failed` reason. When the threshold miss is the only cause, `Failed` carries reason `JobValidationFailed` |
| `namespace` | string | Resolved namespace where Jobs and dependencies are created |
| `succeededNodesRef` | TypedLocalObjectReference | ConfigMap reference for the succeeded-nodes list |
| `failedNodesRef` | TypedLocalObjectReference | ConfigMap reference for the failed-nodes list |
| `orchestration.completedIterations` | int | Number of fully completed iterations |
| `orchestration.currentIteration` | int | The iteration currently in progress (1-based) |
| `orchestration.totalNodes` | int | Total nodes discovered from the target |
| `orchestration.nodesPerJob` | int | Nodes per job (auto-detected from workload template) |
| `orchestration.detectedGPUArchitecture` | string | Detected GPU architecture (e.g. `gb200`) |
| `orchestration.gpuProducts` | []string | Distinct `nvidia.com/gpu.product` label values of the discovered nodes, as written on the nodes. `detectedGPUArchitecture` is the parsed form and cannot be matched back to a label, so the raw values are recorded here and carried onto every job as a node affinity term |
| `orchestration.detectedPlatform` | string | Detected cloud platform (e.g. `aws`) |
| `orchestration.placement` | string | The resolved placement mode for this run, `Pinned` or `Unpinned` |
| `orchestration.appliedOverrides` | []AppliedOverride | Which spec overrides matched and were applied |
| `orchestration.groups` | []GroupStatus | Per-group job status for the current iteration |

## Placement

`spec.orchestration.placement` decides how many jobs a Workflow runs and who picks the nodes.

**`Pinned`** is the default and the behavior every Workflow has always had. The discovered target is partitioned into groups of `nodesPerJob`, every node lands in some group, and each job gets a required `kubernetes.io/hostname` node affinity naming its group's machines. A 12-node target with `nodesPerJob: 2` produces six jobs.

**`Unpinned`** runs exactly one job of exactly `nodesPerJob` nodes, no matter how many the target matches, and carries no hostname affinity. The same 12-node target with `nodesPerJob: 2` produces one two-node job and leaves the other ten untouched. The scheduler chooses the machines, constrained to the target by node affinity.

The requested size is honored exactly under `Unpinned`, or the run fails saying why. It is never clamped to fit the fleet, snapped to the nearest size the model constraints allow, or auto-selected when no size is given.

Both modes carry `target.nodeSelector`, `target.matchExpressions`, and the detected GPU products onto every pod as a required node affinity, so the target is enforced by the scheduler at bind time rather than only at discovery time.

Two fields are incompatible with `Unpinned` and fail the Workflow with reason `PartitionError`:

- `orchestration.diagnose`, which is definitionally about subdividing a group to isolate a fault.
- `orchestration.topology.strictDomain`, which asks for one job per topology domain.

`orchestration.topology.topologyKey` on its own is **ignored**, not rejected. It only feeds the partitioner, which `Unpinned` does not run, and it is injected by catalog overrides on GB200 and GB300 rather than written by the operator. Rejecting it would fail those runs with a message about a field that does not appear in the submitted YAML. To confine an unpinned job to one topology domain, name the domain label in `target.matchExpressions`, which is carried onto the pods.

## Naming

Workflows are named `<certificationName>-<domain>-<variant>`.

## Lifecycle

1. Pulls `WorkflowSpec` from the catalog for the assigned `{domain, variant}`.
2. Detects platform and GPU architecture; applies matching overrides.
3. Creates a child `Job`.
4. Manages iteration count; marks Succeeded or Failed when complete.
