---
title: Validation Results
description: Cluster certification results across validated platform and GPU combinations, and the machine-readable results format.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


The Cluster Readiness Engine (CRE) has been validated on the following platform and GPU combinations. Each certification ran NCCL communication tests (all-reduce, all-gather, alltoall) and NeMo training. GB200/GB300 configurations include both MNNVL-enabled and MNNVL-disabled variants.

## Summary

| Platform | GPU   | Nodes | Interconnect | Categories | Result   |
|----------|-------|-------|--------------|------------|----------|
| AWS      | H100  | 2     | EFA          | 4/4        | PASSED   |
| AWS      | GB200 | 4     | EFA          | 8/8        | PASSED   |
| AWS      | GB300 | 8     | RoCE         | 8/8        | PASSED   |
| GCP      | H100  | 2     | TCPXO        | 4/4        | PASSED   |
| GCP      | GB200 | 2     | RoCE         | 8/8        | PASSED   |

Catalog variant names evolve between releases. The training entry used during these validation runs (`training/nemotron4-15b`) has since been superseded — the current catalog offers `training/nemotron5-8b` and `training/nemotron5-56b`. List the categories available in your installed version with `nvcrectl certification list-categories`.

## Detailed Reports

### AWS H100

```
╔════════════════════════════════════════════════════════════════╗
║                      Certification Report                      ║
╚════════════════════════════════════════════════════════════════╝

  Name:      gpu-cluster-cert
  Platform:  aws
  GPU:       h100
  Nodes:     2

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  11m 0s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      260.20 GB/s  487.87 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  7m 5s                                              │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      361.14 GB/s  338.57 GB/s  67                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  22m 10s                                            │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      84.40 GB/s   79.11 GB/s   66                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  4m 49s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Avg Runtime Goodput:  0.90 (90%)                              │
│  Avg TFLOPs/GPU:  539.8                                        │
│  Avg Train Time:  3m 40s                                       │
│  Avg Step Time:  2.85s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Summary                                                       │
├────────────────────────────────────────────────────────────────┤
│  Categories:   4/4 passed                                      │
│  Failed Nodes: none                                            │
│  Result:       PASSED                                          │
└────────────────────────────────────────────────────────────────┘
```

### AWS GB200

```
╔════════════════════════════════════════════════════════════════╗
║                      Certification Report                      ║
╚════════════════════════════════════════════════════════════════╝

  Name:      gpu-cluster-cert
  Platform:  aws
  GPU:       gb200
  Nodes:     4

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  6m 47s                                             │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      470.68 GB/s  882.51 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  4m 3s                                              │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      649.90 GB/s  609.28 GB/s  67                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  5m 12s                                             │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      606.58 GB/s  568.66 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  20m 20s                                            │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      102.08 GB/s  191.40 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  10m 23s                                            │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      204.01 GB/s  191.26 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  28m 31s                                            │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      62.02 GB/s   58.15 GB/s   63                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  3m 43s                                             │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Avg Runtime Goodput:  0.68 (68%)                              │
│  Avg TFLOPs/GPU:  1186.5                                       │
│  Avg Train Time:  2m 38s                                       │
│  Avg Step Time:  1.30s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  3m 49s                                             │
│  Nodes/Job: 4                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Avg Runtime Goodput:  0.66 (66%)                              │
│  Avg TFLOPs/GPU:  1118.5                                       │
│  Avg Train Time:  3m 10s                                       │
│  Avg Step Time:  1.38s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Summary                                                       │
├────────────────────────────────────────────────────────────────┤
│  Categories:   8/8 passed                                      │
│  Failed Nodes: none                                            │
│  Result:       PASSED                                          │
└────────────────────────────────────────────────────────────────┘
```

### AWS GB300

```
╔════════════════════════════════════════════════════════════════╗
║                      Certification Report                      ║
╚════════════════════════════════════════════════════════════════╝

  Name:      gpu-cluster-cert
  Platform:  aws
  GPU:       gb300
  Nodes:     8

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  7m 32s                                             │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      476.55 GB/s  923.32 GB/s  67                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  5m 3s                                              │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      721.28 GB/s  698.74 GB/s  69                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  6m 40s                                             │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      700.45 GB/s  678.57 GB/s  68                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  13m 39s                                            │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      194.07 GB/s  376.01 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  6m 19s                                             │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      377.48 GB/s  365.68 GB/s  63                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  24m 26s                                            │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      88.38 GB/s   85.62 GB/s   68                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  3m 49s                                             │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Avg Runtime Goodput:  0.57 (57%)                              │
│  Avg TFLOPs/GPU:  1388.5                                       │
│  Avg Train Time:  3m 26s                                       │
│  Avg Step Time:  1.11s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  3m 54s                                             │
│  Nodes/Job: 8                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Avg Runtime Goodput:  0.57 (57%)                              │
│  Avg TFLOPs/GPU:  1136.9                                       │
│  Avg Train Time:  3m 26s                                       │
│  Avg Step Time:  1.40s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Summary                                                       │
├────────────────────────────────────────────────────────────────┤
│  Categories:   8/8 passed                                      │
│  Failed Nodes: none                                            │
│  Result:       PASSED                                          │
└────────────────────────────────────────────────────────────────┘
```

### GCP H100

```
╔════════════════════════════════════════════════════════════════╗
║                      Certification Report                      ║
╚════════════════════════════════════════════════════════════════╝

  Name:      gpu-cluster-cert
  Platform:  gcp
  GPU:       h100
  Nodes:     2

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  13m 38s                                            │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      179.78 GB/s  337.09 GB/s  67                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  11m 19s                                            │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      201.88 GB/s  189.27 GB/s  66                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  40m 40s                                            │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      42.84 GB/s   40.16 GB/s   66                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  4m 37s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│                                                                │
│  Avg Runtime Goodput:  0.92 (92%)                              │
│  Avg TFLOPs/GPU:  511.7                                        │
│  Avg Train Time:  3m 33s                                       │
│  Avg Step Time:  3.01s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Summary                                                       │
├────────────────────────────────────────────────────────────────┤
│  Categories:   4/4 passed                                      │
│  Failed Nodes: none                                            │
│  Result:       PASSED                                          │
└────────────────────────────────────────────────────────────────┘
```

### GCP GB200

```
╔════════════════════════════════════════════════════════════════╗
║                      Certification Report                      ║
╚════════════════════════════════════════════════════════════════╝

  Name:      gpu-cluster-cert
  Platform:  gcp
  GPU:       gb200
  Nodes:     2

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  6m 25s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      478.90 GB/s  838.07 GB/s  64                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  2m 56s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      777.35 GB/s  680.18 GB/s  58                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  3m 7s                                              │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      781.51 GB/s  683.82 GB/s  67                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  6m 50s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      478.72 GB/s  837.76 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-gather                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  3m 24s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      777.33 GB/s  680.16 GB/s  58                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-alltoall                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  3m 8s                                              │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      781.54 GB/s  683.84 GB/s  65                     │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  5m 40s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Avg Runtime Goodput:  0.68 (68%)                              │
│  Avg TFLOPs/GPU:  1175.0                                       │
│  Avg Train Time:  2m 16s                                       │
│  Avg Step Time:  1.31s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  training/nemotron4-15b                                        │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Duration:  2m 44s                                             │
│  Nodes/Job: 2                                                  │
│  Jobs:      1                                                  │
│  MNNVL:     Disabled                                           │
│                                                                │
│  Avg Runtime Goodput:  0.69 (69%)                              │
│  Avg TFLOPs/GPU:  1122.0                                       │
│  Avg Train Time:  2m 21s                                       │
│  Avg Step Time:  1.37s                                         │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Summary                                                       │
├────────────────────────────────────────────────────────────────┤
│  Categories:   8/8 passed                                      │
│  Failed Nodes: none                                            │
│  Result:       PASSED                                          │
└────────────────────────────────────────────────────────────────┘
```

## Machine-readable results

`nvcrectl certification run --wait --results-file <path>` and `nvcrectl certification report <name> --results-file <path>` write the same report as JSON. The file contains a single report object (or an array when multiple certifications are combined into one report):

```json
{
  "name": "gpu-cluster-cert",
  "platform": "aws",
  "gpu": "gb200",
  "totalNodes": 4,
  "categories": [
    {
      "domain": "communication",
      "variant": "nccl-all-reduce",
      "status": "Succeeded",
      "runtime": "6m 47s",
      "nodesPerJob": 4,
      "jobs": 1,
      "mnnvl": "Enabled",
      "bandwidth": [
        { "size": "16 GB", "algBW": "470.68 GB/s", "busBW": "882.51 GB/s", "samples": 65 }
      ]
    }
  ],
  "failedNodes": [],
  "result": "PASSED"
}
```

Top-level fields:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Certification name |
| `platform` | string | Detected platform (`aws`, `gcp`, `azure`, `oci`, `onprem`, ...) |
| `gpu` | string | Detected GPU architecture |
| `totalNodes` | int | Number of nodes targeted by the certification |
| `excludedNodes` | []string | Nodes that matched the target but were left untested (omitted when empty) |
| `exclusionReason` | string | Why nodes were excluded (omitted when empty) |
| `categories` | []object | Per-category results (see below) |
| `failedNodes` | []string | Deduped union of failed node names across all categories |
| `result` | string | `PASSED`, `INCOMPLETE`, `FAILED`, or `RUNNING` |
| `nodeResults` | []object | Per-node pass/fail entries: `name`, `group`, `rack` (optional), `status` (`Passed` or `Failed`) |

Each entry in `categories`:

| Field | Type | Description |
|-------|------|-------------|
| `domain` / `variant` | string | Catalog category, e.g. `communication` / `nccl-all-reduce` |
| `status` | string | Category status (`Succeeded`, `Failed`, ...) |
| `failureReason` | string | Populated from the Workflow `Failed` condition message (omitted when empty) |
| `runtime` | string | Total runtime across all iterations |
| `testScale` | string | Test scale, when set |
| `nodesPerJob` | int | Nodes per job group |
| `jobs` | int | Number of jobs run |
| `mnnvl` | string | `Enabled`, `Disabled`, or omitted when unknown |
| `bandwidth` | []object | NCCL results per message size: `size`, `algBW`, `busBW`, `samples` |
| `domains` | []object | Training results per topology domain: `name`, `nodeCount`, `goodput`, `tflops`, `stepTime` |
| `failedGroups` | []object | Failed orchestration groups: `name`, `nodeCount`, `nodes`, `reason` |
| `iterations` | []object | Per-iteration results: `number`, `status`, `duration` |

## See also

- [Certification API](./certification.md) — the resource these reports are generated from
- [Interpret Results](../how-to-guides/interpret-results.md) — how to read a certification report
