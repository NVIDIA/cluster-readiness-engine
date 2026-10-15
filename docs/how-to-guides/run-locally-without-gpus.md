---
title: Run NVCRE Locally Without GPUs
description: Bring up a Kind cluster with simulated GPUs, install NVCRE from your working tree, and run a real Certification end to end on a laptop.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---

[Mokka](https://github.com/NVIDIA/k8s-test-infra) simulates the NVIDIA driver's management interfaces (NVML, `nvidia-smi`, the PCI and fabric views) on ordinary Kind nodes. The real device plugin, GPU Feature Discovery (GFD) and DRA driver run against it, so the cluster looks like a GPU cluster to Kubernetes and to NVCRE. Nothing in it runs CUDA. The design is in [ADR-096](../designs/096-mokka-local-environment-and-uat-runtime-lane.md).

Use this environment to develop and debug the controller against real scheduling, real GPU allocation, real MPI bootstrap and, on GB200 and GB300, a real ComputeDomain lifecycle.

## Before you begin

- Docker with 8 GB of memory or more. One cluster uses about 2.5 GiB while idle.
- [Kind](https://kind.sigs.k8s.io/), `kubectl`, Helm 3, Go, and `make`.
- For `tilt-mokka`, [Tilt](https://tilt.dev/).
- Network access to `ghcr.io`, `nvcr.io`, `helm.ngc.nvidia.com`, and GitHub. The GB200 and GB300 profiles also build an image from Ubuntu's package archive.

## Bring up a cluster

From the repository root:

```bash
make mokka-up PROFILE=h100
```

This creates the Kind cluster `nvcre-mokka`: one control plane and two simulated GPU workers. It installs Mokka, the GPU stack and NVCRE built from your working tree, then prints `nvcrectl cluster info`. The first run takes several minutes; later runs reuse the cluster and the images.

`kind` switches your current kubectl context to `kind-nvcre-mokka` when it creates the cluster. The make targets themselves always name that context explicitly, so they never act on another cluster.

| `PROFILE` | GPUs per node | Allocation | What NVCRE exercises |
|---|---|---|---|
| `h100` | 8 | Device plugin | On-prem HGX path, MPI |
| `b200` | 8 | Device plugin | On-prem HGX B200 path |
| `gb200` | 4 | Device plugin for GPUs, DRA driver for ComputeDomains | On-prem NVL72 path: ComputeDomain, MNNVL, clique topology |
| `gb300` | 4 | Same as `gb200` | Same as `gb200` |

Kind gives every node the provider ID `kind://`, so NVCRE detects the platform as `onprem`. Cloud-specific overrides, such as AWS EFA or GKE TCPXO, never apply here.

A cluster holds one profile. To switch profiles, run `make mokka-down` first, or keep both clusters by naming the second one:

```bash
make mokka-up PROFILE=gb200 MOKKA_CLUSTER=nvcre-mokka-gb200
```

## Run a Certification

Real NCCL needs CUDA, so the environment provides a stub workload image instead. OpenMPI, `sshd` and the launcher are real; only `all_reduce_perf_mpi` is replaced, by a script that replays a recorded nccl-tests log on rank 0. Select it with `spec.image`:

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: mokka-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  nodesPerJob: 2
  image: nvcre-nccl-stub:pass
  categories:
    - domain: communication
      variant: nccl-all-reduce
```

```bash
bin/nvcrectl --context kind-nvcre-mokka certification run --cert-file mokka-cert.yaml --wait
```

The Certification should reach `Succeeded` in under a minute, with a BandwidthMeasurement holding the replayed results. On `gb200` and `gb300` the same Certification also creates a ComputeDomain, waits for the DRA driver to make it ready, and removes it afterwards.

Four stub images are loaded into the cluster. Use the others to drive NVCRE's failure paths:

| Image | Behaviour |
|---|---|
| `nvcre-nccl-stub:pass` | Replays the log and exits 0 |
| `nvcre-nccl-stub:fail` | Prints part of the log and exits 1 |
| `nvcre-nccl-stub:empty` | Replays the log without any result rows |
| `nvcre-nccl-stub:slow` | Waits five minutes before replaying, so you can delete a Certification mid-run |

## Iterate on the controller

```bash
make tilt-mokka PROFILE=h100
```

This prepares the same cluster, then starts Tilt on port 10351, so it can run beside `make tilt-uat`. Saving a change under `api/`, `cmd/`, `pkg/` or the Helm chart rebuilds the manager image, loads it into Kind and restarts the controller. Changes to tests and `testdata` are ignored.

Without Tilt, rerun only the NVCRE step after a change:

```bash
PROFILE=h100 test/uat/mokka/setup.sh nvcre
```

## Clean up

```bash
make mokka-down
```

This deletes only the cluster named by `MOKKA_CLUSTER`.

## What this does not prove

The simulation covers the contracts between Kubernetes, the GPU stack and NVCRE. It does not cover the GPUs themselves. A Certification that passes here says nothing about:

- CUDA, NCCL, or any kernel running on a GPU.
- Bandwidth. The numbers in the BandwidthMeasurement are the recorded log's, not a measurement.
- Goodput of a real training run.
- DCGM diagnostics levels 2 to 4.

Those still need hardware.
