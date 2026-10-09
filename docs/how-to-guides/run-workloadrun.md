---
title: Run a WorkloadRun
description: Run an ad-hoc distributed workload against a specific set of nodes.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`WorkloadRun` lets you run any distributed workload — training, NCCL benchmark, or custom script — without the full certification pipeline.

## Basic example

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: my-workload
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      mpirunPath: /usr/local/mpi/bin/mpirun
      binary: /usr/local/bin/all_reduce_perf_mpi
      args: ["-b", "8", "-e", "32G", "-f", "2", "-n", "100"]
  numNodes: 4
```

`numNodes` is the number of nodes **per job group**, not a total: the orchestrator partitions all eligible nodes into groups of that size. For example, `numNodes: 4` on 16 eligible nodes produces four 4-node jobs.

```bash
nvcrectl workloadrun run \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  --wait my-workload.yaml
```

## Targeting specific nodes

```yaml
spec:
  target:
    nodeSelector:
      kubernetes.io/hostname: gpu-node-01
```

The target is carried onto every workload pod as a required node affinity, so the scheduler enforces it at bind time, not only when NVCRE picks nodes.

## Running one job instead of a fleet-wide sweep

By default `numNodes` is a group size. NVCRE partitions every node matching the target into groups of that size and runs one job per group, so `numNodes: 2` against 18 eligible nodes runs nine jobs concurrently.

Set `placement: Unpinned` to run a single job of exactly `numNodes` nodes:

```yaml
spec:
  numNodes: 8
  orchestration:
    placement: Unpinned
```

The other target nodes go untested, which is the point. Use this when you want to exercise a workload once rather than certify a fleet, or when you do not want NVCRE choosing which machines to use: under `Unpinned` no `kubernetes.io/hostname` affinity is set, so the scheduler places the pods anywhere in the target.

The requested size is honored exactly or the run fails saying why. It is never clamped to fit the fleet.

### Confining an unpinned job to one topology domain

`orchestration.topology.topologyKey` only drives partitioning, so under `Unpinned` it does nothing. To keep a single job inside one domain, name the domain label in the target instead, which does reach the pods:

```yaml
spec:
  target:
    matchExpressions:
      - key: nvidia.com/gpu.clique
        operator: In
        values: ["clique-0"]
  numNodes: 8
  orchestration:
    placement: Unpinned
```

### Use a gang scheduler for multi-node unpinned jobs

`Pinned` jobs name their nodes, so the group lands on the machines NVCRE chose or not at all. `Unpinned` hands placement to the scheduler, so on a contended cluster some pods can bind while the rest sit Pending indefinitely. Set [`spec.gangScheduler`](#gang-scheduling) whenever `numNodes > 1` under `Unpinned` so the gang schedules whole or not at all.

NVCRE never reports a half-placed job as a complete one, so this costs you a stalled job rather than a false pass.

## Environment variables

`spec.env` sets container-level environment on the workload containers for
every framework — for MPI that is both the launcher and the worker containers.
The variables are merged with the auto-detected NCCL defaults, and a value you
set overrides a default with the same name.

```yaml
spec:
  env:
    - name: NCCL_DEBUG
      value: TRACE
```

For MPI runs, `mpirun` starts the ranks on workers through SSH, and `sshd`
gives every session a fresh, sanitized environment, so the ranks do not
inherit container env. The controller therefore also forwards the merged env
to the ranks with `mpirun -x NAME=value`: the full set of auto-detected NCCL
defaults plus `spec.env`, except names a platform override already forwards
(the platform value then replaces the default). A `valueFrom` variable is
forwarded as `-x NAME`, which `mpirun` reads from the launcher container's
env. At the ranks, a `spec.env` value overrides both the defaults and a
platform-forwarded value, and a `-x NAME=value` you pass in
`spec.framework.mpi.mpiArgs` overrides `spec.env`.

## With bandwidth measurement

```yaml
spec:
  bandwidthMeasurement:
    logProfileRef: nccl-bandwidth
    testType: all_reduce
```

## With goodput measurement

```yaml
spec:
  goodputMeasurement:
    logProfileRef: megatron-training
```

## Gang scheduling

Distributed workloads can deadlock under the default scheduler when only some of their pods fit on the cluster: the placed pods hold GPUs while waiting for peers that never arrive. Set `spec.gangScheduler` to opt every workload pod into a gang-aware scheduler, such as KAI Scheduler, which holds all pods until the entire gang can be placed at once.

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: gang-scheduled-workload
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      mpirunPath: /usr/local/mpi/bin/mpirun
      binary: /usr/local/bin/all_reduce_perf_mpi
      args: ["-b", "8", "-e", "32G", "-f", "2", "-n", "100"]
  numNodes: 4          # nodes per job group; all eligible nodes are partitioned into 4-node jobs
  gangScheduler:
    schedulerName: kai-scheduler   # required
    queue: high-priority           # optional; defaults to "default-queue"
```

On a cluster running the NVIDIA Run:ai platform, name its scheduler and its queue label key instead, and make `queue` name an existing Run:ai queue:

```yaml
  gangScheduler:
    schedulerName: runai-scheduler
    queueLabelKey: runai/queue
    queue: team-a   # must name an existing Run:ai queue
```

Run the WorkloadRun in a namespace associated with a Run:ai project (the platform's scheduling components act on project namespaces), and make sure the named queue exists: Run:ai validates the queue rather than falling back to a default. When the workload manifest cannot set `schedulerName`, Run:ai's `runai/enforce-scheduler-name` namespace annotation enforces the scheduler namespace-wide, but it does not translate the queue label key, so `queueLabelKey` is still needed.

`schedulerName` is required. `queue` is optional and defaults to `default-queue`; on Run:ai that default is not a real queue, so always set `queue` explicitly to an existing Run:ai queue. When non-empty, `queue` must be a valid Kubernetes label value (at most 63 characters, beginning and ending with an alphanumeric character, containing only alphanumerics, hyphens, underscores, or dots). `queueLabelKey` is optional and defaults to `kai.scheduler/queue`; when non-empty, it must be a valid Kubernetes label key and cannot be `app.kubernetes.io/managed-by` or use the `nvcre.nvidia.com/` prefix.

When `gangScheduler` is set, NVCRE labels the submitted workload object and modifies every pod template generated for the workload — for MPI frameworks that includes both the launcher and the worker pods:

- The configured scheduler name is injected as `schedulerName` in each pod spec, so the pods bypass the default scheduler.
- The queue is applied as a label (`queueLabelKey`, `kai.scheduler/queue` when unset) on the submitted workload object (the `TrainJob`), on the Job template metadata, and on the pod template metadata. Schedulers that select a queue from the submitted object, such as Kueue and KAI Scheduler, read it before any pod exists; the pod-level copies let a gang-aware scheduler hold all pods in the gang until they can be placed together.

See [API Reference: WorkloadRun](../api-reference/workloadrun.md) for validation details.

## Platform overrides

The controller detects the platform (from `spec.providerID`) and GPU
architecture (from the `nvidia.com/gpu.product` node label) and applies
platform-specific overrides automatically — the same `_lib/` fragments the
certification catalog uses. For MPI workloads, override `mpiArgs` are
prepended to the launcher command ahead of your own
`spec.framework.mpi.mpiArgs`, so your values still win under OpenMPI's
duplicate-parameter handling.

| Platform | GPU | Framework | Effect |
|----------|-----|-----------|--------|
| AWS | all | all | Removes the EFA OFI NCCL plugin (`rm -rf /opt/amazon`, `unset NCCL_NET_PLUGIN`) |
| AWS | all | MPI | Forwards `-x NCCL_NET_PLUGIN=none` to workers via mpirun |
| AWS | GB300 | MPI | Pins OpenMPI transport to TCP on `eth0` (`--mca pml ob1`, `--mca btl tcp,self`, …), disables UCC/HCOLL (SIGSEGV in `MPI_Init` on RoCE otherwise), and forwards the RoCE NCCL env (`NCCL_SOCKET_IFNAME=eth0`, `NCCL_IB_GID_INDEX=3`, …) via `mpirun -x` |
| AWS | GB200/GB300 | all | ComputeDomain + DRA resource claims; GB300 adds the RoCE `ResourceClaimTemplate` |

Use `nvcrectl workloadrun render --platform aws my-workload.yaml` to preview
the exact rendered Workflow, including the platform-applied mpirun args.

## View results

```bash
nvcrectl workloadrun report my-workload
```

See [API Reference: WorkloadRun](../api-reference/workloadrun.md) for the full spec.
