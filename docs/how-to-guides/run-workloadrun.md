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

One caveat for MPI runs: the container env reaches `mpirun` on the launcher
and the `sshd` process on each worker, but `mpirun` starts the ranks on
workers through SSH, and `sshd` gives every session a fresh, sanitized
environment — so the MPI ranks themselves may not inherit `spec.env`. A
variable the ranks must see should be passed as `-x NAME=value` in
`spec.framework.mpi.mpiArgs`, which forwards it through `mpirun` itself.

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

`schedulerName` is required. `queue` is optional and defaults to `default-queue`; when set, it must be a valid Kubernetes label value (at most 63 characters, beginning and ending with an alphanumeric character, containing only alphanumerics, hyphens, underscores, or dots).

When `gangScheduler` is set, NVCRE modifies every pod template generated for the workload — for MPI frameworks that includes both the launcher and the worker pods:

- The configured scheduler name is injected as `schedulerName` in each pod spec, so the pods bypass the default scheduler.
- The queue is applied as the `kai.scheduler/queue` label on the pod template metadata, so a gang-aware scheduler can hold all pods in the gang until they can be placed together.

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
