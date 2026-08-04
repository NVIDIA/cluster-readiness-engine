---
title: Certify a Cluster
description: Platform-specific guides for running a full cluster certification on AWS, GCP, and Azure.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## Before you begin

- [Install ncrectl and set up the controller](../getting-started/install.md)
- Confirm your kubeconfig points at the target cluster
- Have an NGC API key available for image pulls

## AWS

### GB200 (EFA interconnect)

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gb200-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB200
  enableMNNVL: true
  imagePullSecrets:
    - name: ngc-secret
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron4-15b
```

```bash
ncrectl certification run --cert-file gb200-cert.yaml --wait
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

## GCP

_Content coming soon._

## Azure

_Content coming soon._

## Monitoring progress

```bash
# Watch overall status
kubectl get certifications.cre.nvidia.com -w

# Watch individual workflows
kubectl get workflows.cre.nvidia.com -w

# Tail controller logs
kubectl logs -n cluster-readiness-engine deploy/cluster-readiness-engine-controller -f
```

## Reviewing results

```bash
ncrectl certification report <name>
```

- **Passed** — all categories met their thresholds. Cluster is ready.
- **Failed** — one or more categories failed. Affected nodes are tainted and cordoned automatically. See [Interpret Results](./interpret-results.md).
