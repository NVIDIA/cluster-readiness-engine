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

The `--workload-registry` flags create a pull secret and add it to the run's `spec.imagePullSecrets`. Every pod the run creates uses it; on `mpi` that includes the launcher pod as well as the workers.

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

## Volumes, config files, and init containers

`spec.volumes`, `spec.volumeMounts`, `spec.config`, and `spec.initContainers`
apply to every framework. On `torch` and `exec` they land on the node pods,
where the workload container runs. On MPI they land on the worker pods, where
`mpirun` starts the ranks over SSH. The launcher pod, which only runs
`mpirun`, gets none of your volumes, mounts, or init containers: a
PersistentVolumeClaim, a hugepages emptyDir, or a data-staging init container
is never attached to it. The one exception is `spec.config`, which is also
mounted read-only at `/config` on the launcher, so a binary, rankfile,
appfile, or MCA parameter file you ship there is visible to `mpirun` itself
as well as to the ranks.

A memory-backed `/dev/shm` volume is always provided, so you do not need to
declare one. `spec.config` is mounted at `/config` with mode 0755, so scripts
you ship through `config.inline` are executable as-is; with
`config.configMapRef` the named ConfigMap is mounted the same way and nothing
is created.

The built-in volumes take the names `dshm` and `config-volume` and the paths
`/dev/shm` and, when `spec.config` is set, `/config`. On MPI the name
`mpi-ssh-auth` and the paths `/tmp/mpi-ssh-raw` and `/root/.ssh` are taken
too, by the SSH key Secret Kubeflow Trainer injects and by the worker's `sshd`
setup; on `torch` and `exec` the name `kubeflow-trainer-token` and the path
`/var/run/secrets/kubeflow/trainer` are taken by the token Trainer's status
plugin injects when it is enabled. Those three directories are taken together
with every path below them, since a mount at `/root/.ssh/authorized_keys`
hides that file as surely as one at `/root/.ssh`; `/dev/shm` and `/config`
are taken exactly, so nothing below them is refused. In every pod the
workload container is named `node`, so an init container cannot take that
name, and a mount may
name `config-volume` only when `spec.config` creates it. A WorkloadRun that
reuses one of them fails with a `Failed` condition, reason `BuildFailed`, whose
message names the field, before any Workflow is created, and
`nvcrectl workloadrun render` refuses it the same way. Without that check a
volume named `mpi-ssh-auth` or `kubeflow-trainer-token` would be silently
replaced by Trainer's own, since Trainer upserts those volumes by name.

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: nccl-from-pvc
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      mpirunPath: /usr/local/mpi/bin/mpirun
      binary: /config/run_nccl.sh
  numNodes: 4
  config:
    inline:
      run_nccl.sh: |
        #!/bin/sh
        exec /usr/local/bin/all_reduce_perf_mpi -b 8 -e 32G -f 2 -n 100
  volumes:
    - name: datasets
      persistentVolumeClaim:
        claimName: nccl-datasets
  volumeMounts:
    - name: datasets
      mountPath: /data
  initContainers:
    - name: list-data
      image: nvcr.io/nvidia/pytorch:26.01-py3
      command: ["ls", "-l", "/data"]
      volumeMounts:
        - name: datasets
          mountPath: /data
```

The claim must be `ReadWriteMany` (or `ReadOnlyMany`, mounted read-only)
whenever more than one worker pod mounts it at the same time. Under the
default placement that is any run against more than one eligible node, since
every job group runs concurrently with one worker per node; under
`placement: Unpinned` it is any `numNodes` greater than one.

### Host RDMA devices on MPI

A cluster that exposes its RDMA devices as host device nodes instead of
through a device plugin needs the devices mounted and the worker container
privileged. The kubelet builds a container's device cgroup allowlist from its
requested resources, so a `hostPath` mount of `/dev/infiniband` on its own
leaves the devices visible but makes `open()` on them fail with `EPERM`.
WorkloadRun has no field for that, so set it with an override on the
generated TrainingRuntime, which is named `<workloadrun-name>-runtime`. The
`dependencies` entry below merges into it (containers merge by name, so the
built-in mounts stay) and sets `privileged` on the worker `node` container
only:

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: nccl-hostpath-rdma
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      mpirunPath: /usr/local/mpi/bin/mpirun
      binary: /usr/local/bin/all_reduce_perf_mpi
      args: ["-b", "8", "-e", "32G", "-f", "2"]
  numNodes: 2
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB200
  volumes:
    - name: ib-dev
      hostPath:
        path: /dev/infiniband
        type: Directory
  volumeMounts:
    - name: ib-dev
      mountPath: /dev/infiniband
  overrides:
    - when: {}
      dependencies:
        - apiVersion: trainer.kubeflow.org/v1alpha1
          kind: TrainingRuntime
          metadata:
            name: nccl-hostpath-rdma-runtime
          spec:
            template:
              spec:
                replicatedJobs:
                  - name: node
                    template:
                      spec:
                        template:
                          spec:
                            containers:
                              - name: node
                                securityContext:
                                  privileged: true
```

`privileged` also drops the runtime's default seccomp, AppArmor, and SELinux
confinement for that container, and a CRI-O runtime may refuse a privileged
container that requests `nvidia.com/gpu`. That is the same trade the
catalog's `rdmaDeviceAccess: hostPath` makes for a Certification; see
[ADR-088](../designs/088-oci-gb200-roce-and-rdma-device-access.md). `type: Directory`
keeps a node without the device tree from starting the pod over an empty
directory; such a pod stays in `ContainerCreating` with a `FailedMount` event
until `spec.timeoutPerJob`.

### Worker start-up under KAI

Under a KAI gang scheduler the launcher cannot be gated on worker readiness,
so its init container probes every worker's `sshd` itself, for up to 20
minutes. Anything that delays a worker's start now delays that probe: a slow
image pull, a volume that is slow to attach, a `hostPath` directory missing on
a node, or an init container that stages data. Past the limit the init
container exits with `not every worker answered sshd within 20 minutes` and
points at the worker pods, whose events carry the actual cause. The launcher
pod uses `restartPolicy: Never` and, under KAI, the launcher Job's
`backoffLimit` is 0 (unless an override sets another), so the launcher Job
fails there without a second 20-minute wait and `status.failureLog` carries
the init container's message. Outside KAI the launcher is only created once
every worker is Ready, and the Job waits until `spec.timeoutPerJob`; there the
launcher Job's `backoffLimit` of 1 gives a failed launcher one replacement pod.

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
