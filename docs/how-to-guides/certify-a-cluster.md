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
