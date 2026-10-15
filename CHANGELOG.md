<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Changelog

All notable changes to the NVIDIA Cluster Readiness Engine (NVCRE) are documented in
this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Entries are distilled from the GitHub release notes for each tag;
the full pull request list for a release is in its release notes. Pre-release tags
(`-rc.N`) are not listed here; their changes appear under the stable release that
followed them.

## [Unreleased]

### Fixed

- On the `mpi` framework a WorkloadRun's `spec.volumes`, `spec.volumeMounts`,
  `spec.config`, and `spec.initContainers` were silently dropped: the rendered
  TrainingRuntime carried none of them while `torch` and `exec` honoured them, so an
  NCCL run that reached its RDMA devices through `hostPath` mounts came up without
  them and a binary shipped in `spec.config` was not reachable at `/config`. They now
  land on the MPI worker pods, where the ranks run, and `spec.config` is also mounted
  read-only on the launcher pod, where `mpirun` runs; the launcher is otherwise
  unchanged. A new `mpi` WorkloadRun that already set any of them now mounts them, so
  a volume that cannot attach, such as a `ReadWriteOnce` claim across nodes, holds the
  pods in `ContainerCreating` where it used to be ignored; existing objects are
  unaffected because `spec` is immutable and the Workflow is never rebuilt. Under KAI
  the launcher's wait for the workers' `sshd` now names slow worker start-up (image
  pull, volume attach, init containers) alongside network policy when it gives up
  (#483)
- A WorkloadRun whose `spec.volumes` or `spec.volumeMounts` reuse a built-in volume
  name or mount path (`dshm`, `config-volume`, `/dev/shm`, `/config` with
  `spec.config`, on `mpi` `mpi-ssh-auth`, `/tmp/mpi-ssh-raw` and `/root/.ssh`, and
  on `torch` and `exec` `kubeflow-trainer-token` and
  `/var/run/secrets/kubeflow/trainer`, the SSH and token directories together with
  every path below them), or whose `spec.initContainers` take the
  workload container's name `node`, now fails with `BuildFailed` naming the entry,
  and `nvcrectl workloadrun render` refuses it. A volume named `mpi-ssh-auth` or
  `kubeflow-trainer-token` used to be silently replaced by the one Kubeflow Trainer
  upserts under that name, and the other collisions surfaced only at pod creation
  (#483)
- The MPI launcher pod of a WorkloadRun received no `spec.imagePullSecrets`; only the
  worker pods did, so a launcher pulling from a private registry failed at image pull
  while the workers started. It now gets the same secrets as the workers (#483)

### Changed

- NCCL bandwidth metrics now include the Job's `workflow` label. This changes
  Prometheus series identity, so queries matching an absent or empty `workflow`
  label will not match new measurements. Measurements created before this change
  retain the empty label until they are removed.

## [0.7.0] - 2026-10-12

### Added

- `orchestration.placement: Unpinned` on a WorkloadRun, or on a hand-written Workflow,
  runs exactly one job of exactly `numNodes` nodes and leaves the rest of the target
  untouched. The default, `Pinned`, still partitions every target node into groups of
  `numNodes` and pins each job to its group by a `kubernetes.io/hostname` affinity.
  Under Unpinned no hostname affinity is written and the scheduler places the pods
  inside the target. The size is honored or the run fails with reason `PartitionError`:
  a workload that declares no size, or a target with fewer qualifying nodes than
  `numNodes`, fails instead of being resized. `diagnose`, `topology.strictDomain`, and
  `testScale: intra-node` or `intra-rack` are rejected beside it, and a
  `topology.topologyKey` is ignored. `nvcrectl workloadrun run` rejects a `--node-list`
  naming fewer nodes than `numNodes` and rejects `--topology-domain` outright rather
  than rewriting `numNodes`; `workloadrun render` and `workflow render` apply the same
  checks. The Workflow records `status.orchestration.placement` and `gpuProducts`, each
  group's `nodes` is backfilled from where the pods actually landed, and the report
  prints a `Placement` line with how many of the target nodes were exercised and no
  `Scale` line. Certification has no placement option: its verdict covers every node it
  targets (#454)
- Nscale NKS clusters, platform `nscale`, detected from the `nscale://` providerID, on
  B200 and GB300. Every catalog entry and the WorkloadRun path claim GPUs through a
  `gpu.nvidia.com` ResourceClaimTemplate in place of the `nvidia.com/gpu` request, and
  RDMA NICs through an `rdma.nscale.com` claim sized from `mlnxPerNode` (8 on B200, 4 on
  GB300, omitted at 0). `dcgm-level4`, which runs `hostNetwork`, and
  `nccl-loopback-nvswitch`, which exercises NVLink only, claim GPUs alone, and
  `dcgm-level4` connects to `nvidia-dcgm-dra.nvidia-platform.svc:5555`. The NCCL
  environment is InfiniBand with no HCA names pinned and `NCCL_NET` left unset, since
  pinning it to `IB` selects NCCL's built-in transport over the HPC-X plugin and loses
  SHARP; on B200 `nccl-all-reduce`, `nccl-all-gather`, and `nccl-alltoall` add
  `NCCL_IB_QPS_PER_CONNECTION=8`, `NCCL_MIN_NCHANNELS=32`, and `NCCL_MIN_CTAS=64`.
  Nscale runs no GPU Feature Discovery, so on GB300 the `nvidia.com/gpu.clique` topology
  key becomes `topology.nks.nscale.com/accelerator-domain`, which needed a new optional
  `diagnose` field on `OrchestrationOverrideSpec`. The previous Nscale integration,
  matched on `openstack://` plus an `nscale.com/rdmashare` allocatable, is removed, so
  `openstack://` resolves to `onprem` again (#380)
- The x86 HGX B300 GPU architecture (`b300`, product label `NVIDIA-B300-SXM6-AC`) is in
  the catalog's GPU defaults table at eight GPUs and eight NICs per node, so it renders
  at the right rank count and `--gpu-arch b300` is accepted, and the Nemotron 5 8B and
  56B training entries carry a `b300` parallelism row. A new on-prem override for x86
  HGX B200/B300 nodes tolerates the `nvidia.com/gpu` taint with any value, on the MPI
  launcher too, and makes `nicResourceName` and `mlnxPerNode` request RDMA devices on
  those nodes, with NIC auto-detection widened to match. It sets no NCCL environment. On
  every on-prem fragment, `nicResourceName` with `mlnxPerNode: 0` now omits the NIC
  request instead of rendering a count of zero (#464)
- A Warning event, reason `GPUArchitectureDefaults`, on the Certification or WorkloadRun
  when the detected GPU architecture is missing from the catalog's defaults table,
  naming the four-GPU fallback the catalog has for it and the counts the run is actually
  sized at. The Certification and WorkloadRun render commands, offline and with
  `--dry-run`, and `nvcrectl cluster info` print the same message (#464)
- `WorkloadSchedulingBlocked`, a Job `InProgress` reason for a workload whose pods the
  scheduler has rejected (`PodScheduled=False`, reason `Unschedulable`) for longer than
  the Job's `spec.schedulingStallGraceSeconds` (default 300). The message relays the
  scheduler's own diagnosis from the pod condition, the Workflow reports
  `JobSchedulingBlocked` and appends that diagnosis to its own message, and
  `nvcrectl certification report` prints it on a `Blocked:` line (JSON `statusDetail`)
  for a Running category. `SchedulingGated` pods are held on purpose and do not count.
  The `timeoutPerJob` clock pauses at `status.schedulingBlockedSince` from the first
  blocked observation, grace window included; on recovery `workloadStartTime` advances
  by the paused interval, so runtime used before the block still counts, and
  `status.schedulingResumedTime` restarts the startup and training stall budgets. A
  single blocked episode longer than `timeoutPerJob` still times the Job out. Previously
  the Job reported `WorkloadRunning` while its pods sat Pending, and `timeoutPerJob`
  kept counting the whole time (#371)
- `startupStallTimeoutSeconds` on `CategoryOptions`, so the budget for the first parsed
  training step is set once at `spec.startupStallTimeoutSeconds` or per category at
  `spec.categories[].options.startupStallTimeoutSeconds`, which wins when both are set.
  `nvcrectl certification run --startup-stall-timeout-seconds` sets the spec-level value
  and is rejected when combined with `--cert-file`. The `nemotron5-8b` and
  `nemotron5-56b` entries render the option into `Job.spec.startupStallTimeoutSeconds`
  and keep 1200 s when it is unset; `stallMultiplier` stays 3. Previously the window was
  a literal 1200 in the catalog entries and no `Certification` field or `nvcrectl` flag
  could change it, so a workload whose first step landed after 20 minutes failed
  `WorkloadStalled` with no supported way to widen the window (#394)
- `rdmaDeviceAccess` on the Certification spec and under `categories[].options`, with
  values `hostPath` and `devicePlugin`, for clusters that expose RDMA devices as host
  device nodes with no device plugin to request them from. `hostPath` mounts the host's
  `/dev/infiniband` (hostPath `type: Directory`) into the `node` container and runs that
  container privileged; the mount alone is not enough, because kubelet builds the device
  cgroup allowlist from requested resources, so `open()` on the device nodes fails with
  `EPERM` without the privilege. Unset means `devicePlugin`, which renders what every
  entry rendered before. A pod carrying `hostPath` is rejected under a restricted or
  baseline Pod Security Standard, so it is opt-in rather than a platform default.
  `nvcrectl certification render` rejects an invalid value the same way the CRD enum
  does, and the value joins the Workflow name suffix (`hostrdma` / `dprdma`) so two
  categories of one variant that differ only here get distinct Workflows. The five NCCL
  entries and the two nemotron training entries honor it on every platform (#467)
- GCP H100 detects the TCPXO NCCL plugin release from the `nccl-tcpxo-installer` pods on
  the target nodes and picks the NCCL and training images and the paired `tcpxo-daemon`
  from it: v1.0.15 and v1.0.16 run `pytorch:25.06-py3` (CUDA 12) with daemon v1.0.21 and
  v1.0.22, v1.0.17 runs the CUDA 13 images with daemon v1.0.23. A build suffix is
  ignored, so a rebuild such as `v1.0.17-1` gets the v1.0.17 images. v1.0.15 is the
  minimum supported release. A newer release renders the latest mapping, and an older
  release or none found renders v1.0.15; each fallback emits a Warning
  `TCPXOPluginDetection` event. `certification render --dry-run` runs the same detection
  (#445)
- `status.transport` on BandwidthMeasurement records the NCCL network names seen in
  `NCCL INFO Using network` log lines as a sorted set of distinct values, for example
  `IB` or `Socket`, when the LogProfile carries the new optional `networkTransport`
  pattern. The shipped `nccl-bandwidth` LogProfile matches
  `NCCL INFO Using network (?P<transport>.+)`; a custom pattern must name its capture
  group `transport`, and a LogProfile without the pattern still compiles and records
  nothing. The certification report prints a `Transport:` line per category, per group,
  and per clique, and `--results-file` carries the same `transport` arrays. The field is
  record-only, no threshold or pass/fail verdict reads it, and it is omitted when the
  line never appears. Names are unioned from the first log sample on and again from the
  final read, so a transport line logged before any bandwidth row, or in a Job that
  finished between samples, is still recorded (#444)

### Changed

- The GCP H100 NCCL entries (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`,
  `nccl-loopback`, `nccl-loopback-nvswitch`) and training entries (`nemotron5-8b`,
  `nemotron5-56b`) run the CUDA image that matches the detected TCPXO plugin, so plugin
  v1.0.15 and v1.0.16 now get `pytorch:25.06-py3` instead of the base
  `pytorch:26.01-py3` for NCCL and `pytorch:25.08-py3` for training, neither of which
  could load the CUDA 12 plugin (#437, #445)
- The GCP H100 `tcpxo-daemon` follows the detected plugin release instead of the
  `v1.0.8` pin (#445)
- The target selector now reaches the workload pods in both placement modes.
  `target.nodeSelector` and `target.matchExpressions` are carried onto every pod as a
  required node affinity, with an `nvidia.com/gpu.product` `In` term built from the
  distinct product labels discovery kept, all inside the one `NodeSelectorTerm` that
  holds the hostname list under Pinned, so the requirements AND and the hostname values
  are unchanged. Before, the target was a discovery-only filter: nothing wrote it to the
  pod spec and only the hostname pin kept pods on the nodes it had approved. On a
  DRA-only fleet whose product labels were synthesized from ResourceSlices, the product
  term is omitted under Pinned, and under Unpinned the run fails with `PartitionError`
  when discovery also excluded nodes of another architecture, since the term would match
  nothing on the API server. An MPI job under Unpinned tolerates
  `nvidia.com/gpu=present:NoSchedule` and `kubernetes.io/arch=arm64:NoSchedule` on its
  launcher and workers instead of the blanket `Exists` toleration Pinned keeps;
  `target.taintSelectors` still takes precedence (#454)
- WorkloadRun MPI ranks receive the base NCCL environment and `spec.env` through
  `mpirun -x` on every platform. The launcher previously forwarded only `NCCL_DEBUG` and
  `NCCL_MNNVL_ENABLE` from that set, so the other NCCL defaults and all of `spec.env`
  reached the launcher and worker containers but never the ranks, which start under
  `sshd` with a fresh environment. A name that platform or user `mpiArgs` already
  forward is sent once, and a `spec.env` value replaces a platform `-x` pair of the same
  name, so the documented user-wins precedence now holds at the ranks. The NCCL settings
  every MPI WorkloadRun runs with change as a result: on AWS EFA nodes, for example,
  `NCCL_SHM_DISABLE=1`, `NCCL_NET_GDR_LEVEL=PHB`, and `NCCL_SOCKET_IFNAME=eth0` now
  reach the ranks (#380)
- `NCCL_DEBUG_SUBSYS=NET,INIT` is dropped from the base WorkloadRun NCCL environment, so
  every WorkloadRun, torch and MPI, on every platform logs with NCCL's default subsystem
  filter rather than only the `NET` and `INIT` lines. AWS GB300 still forwards
  `NCCL_DEBUG_SUBSYS=NET,INIT` from its own RoCE transport env (#380)
- `nvcrectl certification render` names Workflows through the controller's rule instead
  of a local `<cert>-<domain>-<variant>` copy that omitted the options suffix. Rendered
  names now carry the suffix the controller has always built from a category's
  `options`: `nodesPerJob` (`n<N>`), `enableMNNVL` (`mnnvl` / `nomnnvl`), `testScale`,
  `enableCheckpoint` (`ckpt`), `maxSteps` (`s<N>`), `exitDurationMins` (`e<N>`), and
  `gpusPerNode` (`g<N>`), plus the new `rdmaDeviceAccess` token, then truncated with the
  same hash, so a rendered name matches what the controller creates. Names on a cluster
  do not change (#467)

### Fixed

- `communication/nccl-loopback` and `nccl-loopback-nvswitch` on on-prem GB200/GB300,
  Mistral GB300, and GCP GB200/GB300 rendered a TrainingRuntime with both `torch` and
  `mpi` policies, which the API server rejects at dependency creation, and lost the
  entry's tolerate-everything toleration. The platform comm fragments no longer set
  `mlPolicy` (the MPI entries' base runtimes already do) and the loopback entries
  restore the toleration after each of them (#464)
- The MPI launcher pod of the on-prem GB200/GB300 communication entries had no
  toleration for the arm64 and GPU taints the node pods tolerate, so on a fleet with no
  untainted node it stayed Pending; it now tolerates both (#464)
- OCI GB200 `nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`, `nccl-loopback`,
  `nccl-loopback-nvswitch`, `nemotron5-8b`, and `nemotron5-56b` now set the RoCEv2 GID
  index: `NCCL_IB_GID_INDEX=3` for the ranks and `UCX_IB_GID_INDEX=3` for the MPI
  launcher, which initializes UCX from its container env rather than from the
  `mpirun -x` args. The three NCCL perf entries set it from a `platform: oci` block that
  a later `gpuArchitecture in [gb200, gb300]` block replaced, since overrides apply in
  file order and `trainer.args` replaces whole, and the training entries had OCI arms
  for L40/L40S and GB300 only, so GB200 rendered no GID index at all and the queue pair
  failed at connection setup with `IBV_WC_REM_ACCESS_ERR`. Those five entries now carry
  an `oci` + `gb200` arm after the architecture block that renders `oci-roce-env.yaml`
  plus the new `_lib/nccl/oci-gb200-roce-env.yaml`, which adds the UCX variable; the two
  loopback entries append both variables with a `jobTemplatePatch` (#467)
- OCI GB300 `nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`, `nemotron5-8b`, and
  `nemotron5-56b` ship an NCCL topology file in a ConfigMap mounted at `/etc/nccl` and
  set `NCCL_TOPO_FILE` to it. Without the file NCCL paired GPUs with the wrong RoCE
  rails (#458)
- OCI GB300 entries no longer force `NCCL_DMABUF_ENABLE=0` when MNNVL is off. NCCL keeps
  its default of enabled and turns DMA-BUF off itself on a node that does not support it
  (#458)
- OCI GB200 workloads no longer request `nvidia.com/mlnxnics` or attach
  `network-operator/sriov-net` by default. The `gb200` architecture default of 8 applied
  unchanged on OCI, where the shapes do not consistently advertise the resource and the
  NetworkAttachmentDefinition is absent on a stock cluster, so every worker pod stayed
  Pending on `Insufficient nvidia.com/mlnxnics`. The OCI GB200 platform default is now
  `mlnxPerNode: 0`; sites running the SR-IOV device plugin set `mlnxPerNode` to their
  own count (#443)
- `mlnxPerNode: 0` now omits the NIC request. No template branched on the count, so zero
  rendered a literal `nvidia.com/mlnxnics: "0"` request and, on OCI, a null
  `k8s.v1.cni.cncf.io/networks` annotation. The Azure, OCI, TogetherAI, and Forge
  templates, and the Azure WorkloadRun override, drop the request entirely at zero, the
  OCI template drops the annotation with it, and a platform default can now lower an
  architecture's count to none (#443)
- A Certification with a checkpointed training run, and a WorkloadRun with
  `spec.checkpoint`, now complete on clusters that enforce RBAC. The checkpoint PVC is a
  job-scoped dependency, and the controller sets the Job as its owner with an Update,
  which the manager role did not grant on PersistentVolumeClaims. The Job and its
  TrainJob were created and kept running, but the Workflow never marked the group
  Running, so it never observed the Job and the run never completed (#463)
- Multi-node MPI workers (the multi-node NCCL categories and WorkloadRun
  `framework.mpi`) now request `SYS_CHROOT` alongside `IPC_LOCK`. `sshd` chroots during
  privilege separation before authenticating, so on CRI-O, which does not grant the
  capability by default, every SSH session died preauth while the listener stayed up and
  `mpirun` failed with the generic ORTE banner. The worker readiness probe now opens a
  real loopback SSH session instead of a TCP connect, so a worker whose `sshd` cannot
  serve sessions stays `NotReady`, `mpirun` is never released against it, and the
  worker's events name the failure; the Job then waits on `timeoutPerJob`. An image
  without an `ssh` client keeps the previous listening check. The AWS GB200 and H100
  override blocks, which restate the worker's capability list, carry the capability for
  MPI as well (#466)
- `nvcre_job_status` now reports `failed` for a Job that timed out on `timeoutPerJob`.
  The gauge was written only by the Job tier, so the Workflow reconciler's timeout write
  left it at `in_progress` for the rest of the run, and after a controller restart
  terminal Jobs had no series at all. It is now built at scrape time from the informer
  cache, like `nvcre_certification_status` and `nvcre_workflow_status`; series of a
  deleting Job remain until the Job leaves the cache (#448)
- A Job that times out on `timeoutPerJob` no longer carries `InProgress=True` beside
  `Failed=True`. The Workflow reconciler's timeout write now sets the same exclusive
  InProgress / Succeeded / Failed shape as every other Job transition; Jobs that timed
  out before the upgrade keep their old conditions (#448)
- The GCP H100 `tcpxo-daemon` now gets `NET_ADMIN`. The capability was listed as
  `CAP_NET_ADMIN`, which containerd drops, so daemon v1.0.21 and later crash-looped on
  their NIC tuning step; their entrypoint flags now match the release, since they reject
  `--enforce_kernel_ipv6_support` (#445)
- GCP H100 training sets `TRITON_LIBCUDA_PATH` to the GKE driver directory. The NGC
  image's ldconfig cache points `libcuda.so.1` at a CUDA compat path that does not exist
  on GKE, so Megatron failed at import with `libcuda.so cannot found!` (#445)
- GCP H100 reads the GKE network names its TCPXO pod annotation attaches from the target
  nodes instead of assuming `gpu-nic0` through `gpu-nic7`. GKE resolves every name in
  `networking.gke.io/interfaces` to a Network object at admission and rejects the pod
  when one is missing, and the provisioner picks the names (AICR uses
  `<deployment-id>-gpu-nic-N`), so on such a cluster every TCPXO worker pod was
  rejected. The controller now uses the `networking.gke.io.networks/<name>` resources
  allocatable at a positive quantity on every node the job can run on, only when there
  are exactly eight of them (`default` and the `.IP` twins ignored), ordered by the host
  NIC each sits on per the `networking.gke.io/north-interfaces` and `nic-info` node
  annotations, else sorted by name. Any other count renders the `gpu-nic0..gpu-nic7`
  default and emits a Warning `GKENetworkDetection` event naming what was found.
  `certification render --dry-run` runs the same detection; offline render is unchanged
  (#437)
- GCP H100 NCCL and training entries set `NCCL_PROTO=Simple,LL128` and
  `NCCL_NVLSTREE_MAX_CHUNKSIZE=131072` and no longer set `NCCL_ALGO`. The TCPXO NCCL
  shim checks the job's environment against the node's `a3plus_guest_config.textproto`
  (`NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE`) and aborts on an enforced mismatch;
  current installers enforce both values, so the previous `NCCL_PROTO=Simple` made the
  shim abort, and the file recommends leaving `NCCL_ALGO` unset so the tuner plugin
  chooses (#437)
- `communication/nccl-loopback`, `training/nemotron5-8b`, and `training/nemotron5-56b`
  now render `timeoutPerJob`. The loopback entry rendered `execution: {}` and the two
  training entries no `execution` block at all, so their Jobs had no bound and an
  explicit `timeoutPerJob` on the category was discarded. An explicit value still wins;
  otherwise the loopback entry takes the catalog default of `1h` and the training
  entries take `24h` from their `meta.yaml`. `certification run --wait` derives its
  default `--timeout` from that budget, so a training category with no explicit timeout
  now derives a 36h wait (24h times the 3/2 margin) (#467)
- A group retry is no longer lost to a stale cache read. On a retry the Workflow
  controller deletes the failed Job and resets the group to Pending in one reconcile,
  but the Job deletion is a watch event of its own, and the reconcile it triggered could
  read the Workflow from the informer cache before that status write landed. It then saw
  a Running group on a deleting Job, treated it as an external deletion, marked the
  group Failed, and the Workflow failed while the relaunched Job ran to completion with
  nothing observing it. Both Job-gone branches now confirm against the API server that
  the group is still Running on that Job, at the same retry count and iteration, before
  failing it, and leave the group alone when the live view has moved on. The status
  write carries only the groups the pass changed, and `createOrAdoptJob` no longer
  adopts the Workflow's own Job while it is still terminating, retrying with backoff
  until the name is free (#454)
- `nvcrectl cluster info` counts GPUs on DRA-only nodes. It read each node's count only
  from allocatable `nvidia.com/gpu`, so on a stack such as Nscale NKS, where no device
  plugin advertises that resource, it reported zero GPUs per node and warned that
  nothing would schedule. A node with no allocatable `nvidia.com/gpu` now counts the
  full GPUs (`type: gpu`) in the newest generation of its `gpu.nvidia.com` ResourceSlice
  pool; MIG and VFIO devices and slices without a `nodeName` are left out, and the
  allocatable count still wins on any node that has one. A cluster where every node has
  the allocatable makes no ResourceSlice List; when the List fails the command prints a
  warning to stderr, so `-o json` and `-o yaml` stay parseable, and the affected nodes
  stay at 0 (#419)

### Security

- The manager image and the `nvcrectl` binaries are built with Go 1.27.2 and
  `golang.org/x/net` v0.60.0, picking up the fixes from the Go 1.27.2 security release
  that `govulncheck` reported as reachable from this code. The `go` directive, which
  every workflow builds from through `go-version-file: go.mod`, and the Dockerfile
  builder image both move to 1.27.2; `x/net` v0.60.0 raises `golang.org/x/sys` to
  v0.48.0 and `golang.org/x/term` to v0.46.0 (#459)
- `attest.yml` refuses a `cosign_version` below v3.1.3, the first v3 release that
  patches GHSA-fx35-mq7g-6g98, a verification bypass on the legacy blob path. The input
  was only matched against `vMAJOR.MINOR.PATCH`, so a pin of v3.1.2 reached the signing
  job. The three components are compared as integers, each capped at six digits so the
  comparison cannot error out and fail open; the default was already v3.1.3, so the
  floor stops a caller from pinning lower. A release policy test runs the extracted
  validate step against pins on both sides of the floor, fails if the floor is removed,
  and holds every `cosign-installer` pin in the workflows to the same floor (#460)
- The release verify gate refuses legacy signature formats rather than letting cosign
  fall back to reading them. Every detached `.sigstore.json` must be a protobuf Sigstore
  bundle with no sibling `.sig` before `verify-blob-attestation` runs, and the image
  index, both platform digests, the `${IMAGE}:${TAG}` reference, and the Helm chart are
  probed with `crane manifest` for the old `sha256-<digest>.sig` / `.att` sidecar tags,
  using a `crane` v0.20.6 the gate installs and checks against a recorded SHA-256. Only
  a `MANIFEST_UNKNOWN` or `NAME_UNKNOWN` answer counts as absent; a hard inspect failure
  fails the gate, and each `cosign tree` and `crane` attempt is bounded at 120s (#369)

## [0.6.0] - 2026-10-05

### Added

- `nvcrectl mcp serve`, a read-only MCP server over stdio with four tools
  (`list_categories`, `get_certification_status`, `get_certification_report`,
  `list_failed_nodes`). Every verdict is projected from the same code that backs
  `nvcrectl certification report`, so the two cannot disagree (#372)
- `nvcre_certification_status` and `nvcre_workflow_status` gauges, with the same
  0/1-per-status encoding as `nvcre_job_status`. A scrape-time collector reads them
  from the manager's informer cache on the elected leader, so no reconciler write is
  needed to keep them current (#414)
- GPU architecture is resolved from `gpu.nvidia.com` DRA ResourceSlices when nodes
  carry no `nvidia.com/gpu.product` label, as on DRA-only stacks that run no GPU
  Feature Discovery. The manager role gains `get`/`list`/`watch` on `resource.k8s.io`
  resourceslices; a missing grant degrades to leaving those nodes unlabeled rather
  than failing the reconcile. `--gpu-arch` supplies the architecture to the offline
  `render` commands, which have no cluster to read slices from (#379)
- GCP RTX PRO 6000 Blackwell Server Edition overlays for NCCL all-reduce, all-gather,
  all-to-all, and Nemotron 5 8B, with an eight-GPU architecture default. Smaller G4
  node shapes require an explicit `gpusPerNode` (#396)
- `metrics.serviceMonitor.labels`, `.interval`, and `.scrapeTimeout` Helm values. The
  ServiceMonitor's discovery label was previously hardcoded to `release: prometheus`
  and the scrape cadence was not configurable (#413)

### Changed

- NCCL bandwidth gauges are now `nvcre_nccl_algbw_gbs` and `nvcre_nccl_busbw_gbs`
  (gigabytes per second). The previous `_gbps` names incorrectly implied gigabits;
  values have always been GB/s from nccl-tests. The old names remain dual-registered
  as deprecated aliases for one minor release and will be removed in the following
  minor (#412)
- The ServiceMonitor sets `honorLabels: true` by default, so the controller's own
  `namespace` and `job` labels survive the scrape instead of being renamed to
  `exported_*`. Dashboards built on `exported_namespace` / `exported_job` should set
  `metrics.serviceMonitor.honorLabels=false` (#413)
- `--timeout` is validated only when `--wait` is set, and a value below `1s` is
  rejected before the run is created. Previously a non-positive or tiny timeout was
  accepted and cancelled the watch immediately, which looked like a stalled run (#411)
- `nvcrectl certification run --category` no longer persists the GPU product it
  detected into `spec.target.nodeSelector`. Only `nvidia.com/gpu.present: "true"` is
  stored, so node discovery is not tied to what nodes reported at creation time (#378)
- `nodeHealthMonitor`, `goodputMeasurement`, and `bandwidthMeasurement` on a Job cannot
  be added or removed after creation. Each was already immutable once set; the
  presence rule closes the absent-to-present and present-to-absent gap (#402)
- Under KAI Scheduler the MPI launcher gets no `dependsOn` gate and the JobSet no
  `startupPolicy`, so both sub-groups are populated and the gang is admitted. The
  ordering barrier moves into the launcher pod, which waits for every worker to answer
  sshd. Non-KAI paths render byte-identically (#395)

### Fixed

- AWS H100 and GB200 keep the EFA userspace stack. Two override paths matched
  `platform: aws` with no GPU guard, so every AWS architecture removed `/opt/amazon`
  and unset `NCCL_NET_PLUGIN` before the workload started. The EFA devices stayed
  attached and unused while NCCL fell back to TCP over `eth0`. The cleanup is now
  scoped to `aws` + `gb300`, which is RoCE and has no EFA devices, and H100 and GB200
  get `FI_PROVIDER=efa`, `FI_EFA_USE_DEVICE_RDMA=1`, `FI_EFA_FORK_SAFE=1`, and
  `IPC_LOCK` on both the catalog and WorkloadRun paths (#434)
- Bandwidth threshold verdicts are judged on the launcher's complete log, read once
  when the Job ends, rather than on a partial NCCL size sweep. The rows missed were
  the largest message sizes, where peak bandwidth is measured, so a healthy group
  could fail `ThresholdViolation` or send adaptive fault isolation down its healthy
  half (#418)
- Four controllers treated a cache miss as proof a child was deleted and acted on it
  destructively. Each tier now confirms absence against an uncached reader first, so a
  Workflow that is still running and still holding GPUs is not written off (#387, #370)
- `WorkloadRun.spec.env` values survive platform overrides that set
  `TrainJob.trainer.env`, restoring the documented user-wins precedence (#422)
- `cleanupPVForPVC` reads the PVC through the uncached reader. The cached read started
  a cluster-wide PVC informer the manager role has no `watch` for, so every PVC in the
  cluster was relisted on a loop after the first cleanup (#425)

### Security

- The release dispatch policy tests exercise the release-tag resolver against a
  matching tag, a branch ref, and a different tag before crediting its shell guard,
  and follow local reusable-workflow calls when checking dispatch callers. A guard
  written as `: # exit 1`, or an attest call hidden one hop away, is no longer
  accepted (#359)
- CodeQL analyzes `pull_request`, so fork PRs are scanned; the Fern docs CLI is
  installed from a lockfile pinned by integrity hash rather than `latest`, which the
  release publish path resolved at tag time (#393)

## [0.5.0] - 2026-09-28

### Added

- Lifecycle Events for phase transitions on Certification, Workflow, Job, and
  WorkloadRun, so `kubectl describe` shows each transition; Failed transitions emit
  Warnings (#349)
- `workloadMetadata.labels` sets labels on the generated workload object, globally at
  `spec.workloadMetadata` and per category at
  `spec.categories[].options.workloadMetadata` (#353)
- The gang-scheduler queue label is written to the submitted workload object, not only
  the runtime Job and pod templates, so Kueue and KAI Scheduler read it where they
  expect it (#353)
- Cordoned nodes can be certified by adding a `target.taintSelectors` entry for the
  `node.kubernetes.io/unschedulable` taint, which also drops the hardware-failure
  check for the run (#358)

### Fixed

- `nvcrectl setup init` no longer rejects read-only JobSet consumer roles such as
  GKE's `system:clustermetrics` as a leftover JobSet controller, which blocked
  Kubeflow Trainer installation (#368)

### Security

- The reusable-workflow signing boundary is enforced by release-path tests: weakening
  `attest.yml`'s sole-signer boundary, its trusted-context provenance predicate, or
  the builder-identity guard now fails `make test` (#308)

## [0.4.0] - 2026-09-21

### Added

- Admission-policy samples for Kyverno and Sigstore policy-controller that require the
  release signature and SLSA provenance on the controller image (#334)
- Runnable `Certification` and `WorkloadRun` examples under `examples/` (#355)

### Changed

- `nvcrectl setup init` and `setup reset` reject unknown `--skip-phases` values (#346)
- `nvcrectl setup init` stops before mutating the cluster when JobSet ownership is
  ambiguous and prints the manual Kubeflow Trainer install path (#346)

### Fixed

- `nvcrectl setup init` installs Kubeflow Trainer without its bundled JobSet
  controller when a verified external JobSet controller is present, and reset and
  recovery retain the shared JobSet CRD (#346)
- `--skip-phases=helm` on `setup init` and `setup reset` skips the Helm phase instead
  of being silently ignored; reset previously removed the NVCRE release and CRDs
  anyway (#346)
- A Kubeflow Trainer uninstall failure during `setup reset` stops before CRD cleanup
  and returns non-zero (#346)
- CRD descriptions for `spec.target.nodeNames` no longer claim `nodeSelector` is
  ignored; both filters have always applied (#356)

### Security

- A `workflow_dispatch` at any tag cut from `v0.4.0` onward can no longer mint the
  release signing identity: the attest self-test holds its ref to `main`, and
  `attest.yml` refuses `allow_untagged` on a release tag. `v0.3.0` and the `v0.2.0`
  series still ship the unguarded copy (#341)

## [0.3.0] - 2026-09-14

### Added

- An sshd install guard and a workload image override for air-gapped clusters (#326)
- Mirrorable training source repositories via `sourceRepo` in catalog entries (#327)
- `--chart-ref` and `--trainer-chart-ref` flags on `nvcrectl setup init` (#328)
- An on-prem platform override for GB200 and GB300 (#331)
- A configurable gang scheduler queue label key (#332)
- Controller disruption protection in the Helm chart (#330)

### Changed

- The Kubernetes dependency group is held at 0.36.x until kubeflow/trainer supports
  1.37 (#315)

### Fixed

- Reports include captured failure logs (#316)

## [0.2.0] - 2026-09-07

### Added

- Gang-scheduled certification runs: `spec.gangScheduler` opts every category's
  workload pods into a gang-aware scheduler such as KAI Scheduler (#303)
- Signatures and SLSA provenance for everything a release publishes: the container
  image with per-platform CycloneDX SBOMs, the Helm chart, the `nvcrectl` binaries,
  and the installer (#281, #286, #289, #290)
- A release verification gate: the GitHub Release stays a draft until every published
  artifact verifies against the exact signing identity (#293)
- The installer verifies each binary against that release's Sigstore bundle before
  installing it (#298)
- `manager.image.digest` Helm value to pin the controller image by digest instead of
  a mutable tag (#292)
- A weekly vulnerability scan of already-published images, for CVEs disclosed after
  those images were built (#305)
- NCCL path-spread tuning for OCI GB300 (#287)

### Changed

- Go 1.27 (#285)
- Versioned documentation is built from the frozen content the version registry
  points at, and publishes on release tags (#297, #260)

### Fixed

- `maxBytes` values are accepted again; the CRD pattern marker was quoted such that
  valid values were rejected at admission (#284)

## [0.1.0] - 2026-09-01

First stable release: a Kubernetes controller that certifies GPU clusters before
production use by running real training and communication workloads across
topology-aware node groups, measuring goodput and bandwidth, and reporting every
failed node with a reason.

### Added

- Catalog training entry CPU and memory are overridable via `CategoryOptions` (#232)
- `workloadrun run --cleanup` is implemented instead of silently discarded (#228)

### Changed

- The Helm chart is renamed to `cluster-readiness-engine` and published to the
  repo-linked GHCR package (#257)
- Install and release instructions use public download URLs (#255)

### Fixed

- Unknown threshold keys fail loudly instead of skipping validation (#227)
- The majority GPU architecture is certified on every path, not the first node's
  (#251, #254)
- Nodes with insufficient allocatable GPUs are excluded from partitioning (#231)
- A suspended TrainJob is treated as pending, not running (#238)
- Ownership is verified before adopting or deleting child resources (#239)
- Chart CRDs are applied on every `init` run to prevent schema drift on upgrade (#236)
- Certification and WorkloadRun specs are immutable after creation (#240)
- `spec.env` is passed to MPI containers instead of silently dropped (#230)

[0.7.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.7.0
[0.6.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.6.0
[0.5.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.5.0
[0.4.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.4.0
[0.3.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.3.0
[0.2.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.2.0
[0.1.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.1.0
