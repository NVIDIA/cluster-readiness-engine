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

[0.3.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.3.0
[0.2.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.2.0
[0.1.0]: https://github.com/NVIDIA/cluster-readiness-engine/releases/tag/v0.1.0
