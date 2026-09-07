---
title: Certify a Cluster
description: Platform-specific guides for running a full cluster certification on AWS.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Before you begin

- [Install nvcrectl and set up the controller](../getting-started/install.md)
- Confirm your kubeconfig points at the target cluster

## AWS

### GB200 (EFA interconnect)

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gb200-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB200
  enableMNNVL: true
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron5-8b
```

```bash
nvcrectl certification run --cert-file gb200-cert.yaml --wait
```

The controller auto-detects AWS + GB200 and applies EFA-specific resources (`hugepages-2Mi`, `vpc.amazonaws.com/efa: 4`, EFA hostPath volume) automatically.

### GB300 (RoCE interconnect)

Same spec as GB200 with `nvidia.com/gpu.product: NVIDIA-GB300`. The controller detects GB300 and applies RoCE resource claims (`roce-channel`) instead of EFA — no hugepages, no EFA volumes.

### H100

```yaml
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3
  enableMNNVL: false
```

H100 on AWS uses `vpc.amazonaws.com/efa: 32`. No hugepages or ComputeDomain.

## Gang scheduling

Distributed workloads can deadlock under the default scheduler when only some of their pods fit on the cluster: the placed pods hold GPUs while waiting for peers that never arrive. Set `spec.gangScheduler` to opt the certification into a gang-aware scheduler, such as KAI Scheduler, which holds all of a job's pods until the entire gang can be placed at once.

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gang-scheduled-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB200
  enableMNNVL: true
  gangScheduler:
    schedulerName: kai-scheduler   # required
    queue: high-priority           # optional; defaults to "default-queue"
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron5-8b
```

`schedulerName` is required. `queue` is optional and defaults to `default-queue`; when set, it must be a valid Kubernetes label value (at most 63 characters, beginning and ending with an alphanumeric character, containing only alphanumerics, hyphens, underscores, or dots).

The setting is certification-wide: it applies to **every** category in `spec.categories`, not to one of them. For each category, NVCRE rewrites every pod template in the resolved `TrainingRuntime`. For the MPI-based communication categories that is both the launcher and the worker pods:

- The configured scheduler name is injected as `schedulerName` in each pod spec, so the pods bypass the default scheduler.
- The queue is applied as the `kai.scheduler/queue` label on each replicated job's template metadata, so a gang-aware scheduler can hold all pods in the gang until they can be placed together.

The rewrite runs after the catalog entry and the platform overrides have resolved, so it replaces a scheduler name a catalog entry hardcodes. `training/nemotron5-8b` and `training/nemotron5-56b` pin `schedulerName: default-scheduler`, and both pick up the configured scheduler instead. `nvcrectl certification render` applies the same rewrite, so the rendered manifests show what the controller will create.

See [API Reference: Certification](../api-reference/certification.md) for validation details.

## Monitoring progress

```bash
# Watch overall status
kubectl get certifications.nvcre.nvidia.com -w

# Watch individual workflows
kubectl get workflows.nvcre.nvidia.com -w

# Tail controller logs
kubectl logs -n nvcre deploy/nvcre-manager -f
```

## Reviewing results

```bash
nvcrectl certification report <name>
```

- **Passed** — all categories met their thresholds. Cluster is ready.
- **Failed** — one or more categories failed. See [Interpret Results](./interpret-results.md) for how to read the failed node list and act on it.
