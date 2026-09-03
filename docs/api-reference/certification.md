---
title: Certification
description: CRD reference for the Certification resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`Certification` is the top-level resource that defines a suite of certification categories to run against a GPU node pool.

## Example

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-cluster-cert
  namespace: nvcre
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  enableMNNVL: false
  gangScheduler:
    schedulerName: kai-scheduler
    queue: high-priority
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron5-8b
      options:
        maxSteps: 50
        nodesPerJob: 8
        # Optional: override the DGX-class CPU/memory defaults
        # (limits: cpu "128" / memory 800Gi; requests: cpu "64" / memory 500Gi)
        # so training pods can schedule on smaller GPU nodes.
        resources:
          limits:
            cpu: "6"
            memory: 48Gi
          requests:
            cpu: "4"
            memory: 32Gi
```

## Spec fields

_Fields documented so far:_

| Field | Type | Description |
|-------|------|-------------|
| `gangScheduler` | GangSchedulerSpec | Optional. Opts every category's workload pods into a gang-aware scheduler such as KAI Scheduler. When set, the scheduler name is injected as `schedulerName` into every pod template of every category's resolved `TrainingRuntime` dependency (for MPI-based categories, both the launcher and the worker pods) and the queue is applied as the `kai.scheduler/queue` label on each replicated job's template metadata, so the scheduler holds all pods until the entire gang can be placed. Applied after the catalog and platform overrides resolve, so it also replaces a scheduler name a catalog entry hardcodes |
| `gangScheduler.schedulerName` | string | Required; minimum length 1. Name of the gang-aware scheduler to use (e.g., `kai-scheduler`). Injected as `schedulerName` in each workload pod spec |
| `gangScheduler.queue` | string | Optional. Scheduler queue to submit the workloads to; defaults to `default-queue` when unset. When non-empty, must be a valid Kubernetes label value: at most 63 characters, beginning and ending with an alphanumeric character, and containing only alphanumerics, hyphens, underscores, or dots (pattern `^$\|^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`) |

Like the rest of `spec`, `gangScheduler` is immutable after the Certification is created.

## Spec immutability

<Warning>
The entire `spec` is **immutable** after the Certification is created (a `self == oldSelf` transition rule on the CRD rejects every update with `spec is immutable after creation`). The controller never applies edits to an active run, so mutable fields would either be silently ignored or applied only to later categories. To run with different inputs, delete the Certification and create a new one. The minimum is 1 category.
</Warning>

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | InProgress, Succeeded, Failed (mutually exclusive) |
| `categoryStatuses` | []CertificationCategoryStatus | Per-category status including `domain`, `variant`, `status`, `workflowRef`, `succeededNodesRef`, and `failedNodesRef` |

Each `categoryStatuses` entry includes a `failedNodesRef` — a `TypedLocalObjectReference` pointing to a ConfigMap that stores the failed-node list (name, reason, message) for that category. To read the failed nodes:

```bash
# Get the ConfigMap name from the category status
kubectl get certification <name> -o jsonpath='{.status.categoryStatuses[0].failedNodesRef.name}'
# Read the ConfigMap contents
kubectl get configmap <ref-name> -o yaml
```

## Lifecycle

1. Controller creates one `Workflow` per entry in `spec.categories`. The spec cannot be changed after creation — delete and recreate to modify it.
2. Workflows run **sequentially** — the controller processes one category at a time. `maxConcurrent` controls job/group parallelism *within* a single Workflow (how many node groups run at once), not across categories.
3. When all Workflows complete, Certification is marked `Succeeded` or `Failed`.
4. Failed nodes are recorded in ConfigMaps referenced by `status.categoryStatuses[].failedNodesRef`. NVCRE does not taint or cordon nodes.
