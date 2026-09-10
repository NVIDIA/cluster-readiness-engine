---
title: FAQ
description: Frequently asked questions — common gotchas, design choices, and tips from real-world usage.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## General

### How often should I run burn-in?

Recurring cluster-wide burn-in every 3–4 weeks with real ML training workloads is recommended to catch infant mortality failures and hardware degradation over time. Synthetic benchmarks alone are not enough — faults like NVLink bandwidth degradation and NCCL collective hangs only surface under sustained distributed load.

You can declare a Certification once and re-run it on a schedule. The NVIDIA Cluster Readiness Engine (NVCRE) handles orchestration, measurement, and failure detection each time.

### Can I run NVCRE on non-NVIDIA GPUs?

No. NVCRE is built for NVIDIA GPU clusters and depends on NVIDIA-specific health signals (`nvidia.com/gpu.product` labels, DCGM diagnostics, NVLink topology). The catalog entries target NeMo training, NCCL collectives, and DCGM diagnostics — all NVIDIA tooling.

### Can I use my own training workloads instead of the catalog?

Yes. The catalog is a convenience layer that provides pre-configured WorkflowSpecs. You can bypass it entirely by creating Workflow or Job resources directly with any workload spec (`trainJob`), or run a one-off workload with [WorkloadRun](../getting-started/workloadrun-quick-start.md). See [Custom Catalog Entries](../how-to-guides/custom-catalog-entries.md) for adding your own catalog categories.

### What is the difference between a gray failure and a hardware failure?

A **hardware failure** is detected by the node health monitor — the CEL expression evaluates to `true`, indicating a clear signal (for example, a GPU-related taint or node condition set by your cluster's health monitoring stack, or a node marked unschedulable). The controller reports this immediately in the Job's `HardwareFailed` condition and records the node with reason `HardwareFailureDetected`.

A **gray failure** is when hardware is degraded but no health monitor fires. The workload runs but underperforms — reduced NCCL bandwidth, lower goodput, or intermittent hangs. NVCRE detects these through performance thresholds (goodput ratio, bus bandwidth) and adaptive fault isolation. See [Adaptive Fault Isolation](../how-to-guides/adaptive-fault-isolation.md).

### What happens to failed nodes after repair?

Failed nodes are recorded in the Certification status with a reason (`HardwareFailureDetected`, `ThresholdViolation`, or `WorkloadFailed`). NVCRE never modifies nodes — it does not taint, cordon, or patch them. Node quarantine and repair are handled by your platform's own tooling. After repairing or replacing the failed hardware, re-run the Certification to verify the fix.

### Can MPI workloads run in air-gapped or restricted-egress clusters?

Only when the workload image already contains `/usr/sbin/sshd`. Multi-node MPI workloads (the multi-node NCCL catalog entries and WorkloadRun's `framework.mpi`) start each worker container with a bootstrap command that installs `openssh-server` via `apt-get` unless `/usr/sbin/sshd` is already present (`test -x /usr/sbin/sshd || (apt-get update && apt-get install ...)`). When the image ships `sshd`, the install is a no-op and the workers start with zero package-manager egress. The loopback NCCL entries run single-node and have no sshd bootstrap.

Prebake `sshd` into the workload image:

```dockerfile
FROM <workload base image>
RUN apt-get update && \
    apt-get install -y --no-install-recommends openssh-server && \
    rm -rf /var/lib/apt/lists/*
```

Then point the workload at it, on any platform:

- **WorkloadRun**: set `spec.image` to the prebaked image.
- **Certification**: set `image` at the spec level to apply it to every category, or `categories[].options.image` to override one category (the per-category value wins). The override replaces the trainer image, the primary workload container of every replicated job in the resolved runtimes, and the workload-derived init containers (those whose image equals the workload's, such as `fix-ssh-permissions` and `megatron-clone`); infrastructure init containers with their own images, such as GCP's `tcpxo-daemon`, keep them. See [Certification](../api-reference/certification.md). For the training categories the override alone does not remove all egress: the `megatron-clone` init container follows the override, but it still clones Megatron-LM from GitHub at pod start unless the workspace volume already holds a checkout at `/mnt/workspace/megatron-lm`. In-image source resolution is tracked in [#327](https://github.com/NVIDIA/cluster-readiness-engine/pull/327); once it lands, `/opt/megatron-lm` becomes the documented in-image location the training entries fall back to before any network fetch, so a prebaked image built with `RUN git clone --depth 1 -b core_v0.15.2 https://github.com/NVIDIA/Megatron-LM.git /opt/megatron-lm` will carry the source with no runtime clone.

<Warning>
On AWS H100 and GB200 the platform overrides run the NCCL workers on `public.ecr.aws/hpc-cloud/nccl-tests`, which ships both `sshd` and the aws-ofi-nccl (EFA) plugin. Setting `image` there replaces nccl-tests, and you then own the EFA OFI plugin being present in your replacement image; without it, NCCL cannot use EFA. Base your prebaked image on the nccl-tests image, or install aws-ofi-nccl in it.
</Warning>

Two AWS configurations need no override at all:

- **AWS H100 and GB200**: the catalog already lands the workers on the nccl-tests image, which ships `sshd`, so the guarded install no-ops.
- **AWS GB300**: the platform override replaces the worker command entirely with one that starts the preinstalled `sshd`, so no install runs.

A [custom catalog entry](../how-to-guides/custom-catalog-entries.md) remains an option when a category needs deeper changes than the image.

## Development

### Why do I need to run `make manifests generate` after editing types?

NVCRE uses kubebuilder markers in `*_types.go` files to auto-generate CRD manifests (`helm/cluster-readiness-engine/crds/`) and DeepCopy methods (`zz_generated.deepcopy.go`). If you modify a types file and skip this step, the generated files become stale — the controller binary won't compile because the DeepCopy methods reference the old struct shape, and the CRD YAML won't match the new fields.

Always run `make manifests generate` immediately after any change to `api/v1alpha1/*_types.go`.

### Why doesn't my custom catalog entry appear?

The catalog uses Go `init()` functions for registration. `pkg/catalog` (which loads the entries in `pkg/catalog/entries/`) must be imported by every binary and test suite that resolves catalog lookups — it is blank-imported in `cmd/nvcrectl/main.go` and imported by the controller in `cmd/manager/main.go`.

If you created a new catalog entry but the `init()` registration never runs in your binary or test suite, `catalog.Lookup()` returns "not found". See [Custom Catalog Entries](../how-to-guides/custom-catalog-entries.md).

### Why doesn't cascade deletion work in my integration tests?

Kubernetes envtest (used by the integration test suite) does not run a garbage collection controller. This means `OwnerReference`-based cascade deletion does not work — child resources are not automatically deleted when the parent is deleted.

Controllers must explicitly delete child resources in their `handleDeletion()` methods. This is by design in envtest and is documented as a critical pitfall in the project's contributor docs.

## See also

- [Troubleshooting](./troubleshooting.md) — problem-solution pairs for specific issues
- [Architecture](../concepts/architecture.md) — understand the Certification, Workflow, Job model
