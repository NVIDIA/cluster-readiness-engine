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

### Added

- GCP H100 detects the TCPXO NCCL plugin release from the `nccl-tcpxo-installer` pods
  on the target nodes and picks the NCCL and training images and the paired
  `tcpxo-daemon` from it: v1.0.15 and v1.0.16 run `pytorch:25.06-py3` (CUDA 12) with
  daemon v1.0.21 and v1.0.22, v1.0.17 runs the CUDA 13 images with daemon v1.0.23.
  A build suffix is ignored, so a rebuild such as `v1.0.17-1` gets the v1.0.17 images.
  v1.0.15 is the minimum supported release. A newer release renders the latest
  mapping, and an older release or none found renders v1.0.15; each fallback emits a
  Warning `TCPXOPluginDetection` event. `certification render --dry-run` runs the
  same detection. Validated on an AICR A3 Mega cluster at plugin v1.0.15 and v1.0.16
  (#438)

### Changed

- GCP H100 training (`nemotron5-8b`, `nemotron5-56b`) runs the CUDA image that
  matches the detected TCPXO plugin, so plugin v1.0.15 and v1.0.16 now get
  `pytorch:25.06-py3` instead of `pytorch:25.08-py3`, which could not load the CUDA 12
  plugin (#439)
- The GCP H100 `tcpxo-daemon` follows the detected plugin release instead of the
  early-2024 `v1.0.8` pin (#438)

### Fixed

- `nvcre_job_status` now reports `failed` for a Job that timed out on
  `timeoutPerJob`. The gauge was written only by the Job tier, so the Workflow
  reconciler's timeout write left it at `in_progress` for the rest of the run, and
  after a controller restart terminal Jobs had no series at all. It is now built at
  scrape time from the informer cache, like `nvcre_certification_status` and
  `nvcre_workflow_status`; series of a deleting Job remain until the Job leaves the
  cache (#401)
- A Job that times out on `timeoutPerJob` no longer carries `InProgress=True` beside
  `Failed=True`. The Workflow reconciler's timeout write now sets the same exclusive
  InProgress / Succeeded / Failed shape as every other Job transition; Jobs that
  timed out before the upgrade keep their old conditions (#401)
- The GCP H100 `tcpxo-daemon` now gets `NET_ADMIN`. The capability was listed as
  `CAP_NET_ADMIN`, which containerd drops, so daemon v1.0.21 and later crash-looped
  on their NIC tuning step; their entrypoint flags now match the release, since
  they reject `--enforce_kernel_ipv6_support` (#438)
- GCP H100 training sets `TRITON_LIBCUDA_PATH` to the GKE driver directory. The NGC
  image's ldconfig cache points `libcuda.so.1` at a CUDA compat path that does not
  exist on GKE, so Megatron failed at import with `libcuda.so cannot found!` (#439)
- OCI GB200 workloads no longer request `nvidia.com/mlnxnics` or attach
  `network-operator/sriov-net` by default. The architecture default of 8 applied
  unchanged on OCI, where the shapes do not consistently advertise the resource and
  the NetworkAttachmentDefinition is absent on a stock cluster, so every worker pod
  stayed Pending on `Insufficient nvidia.com/mlnxnics`. Sites running the SR-IOV
  device plugin set `mlnxPerNode` to their own count (#350)
- `mlnxPerNode: 0` is now the opt-out the API reference has always described. No
  template branched on the count, so zero rendered as a literal
  `nvidia.com/mlnxnics: "0"` request and, on OCI, a null
  `k8s.v1.cni.cncf.io/networks` annotation. The Azure, OCI, TogetherAI, and Forge
  templates now drop the request entirely at zero, and a platform default can lower
  an architecture's count to none (#350)

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

[0.6.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.6.0
[0.5.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.5.0
[0.4.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.4.0
[0.3.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.3.0
[0.2.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.2.0
[0.1.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.1.0
