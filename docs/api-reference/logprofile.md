---
title: LogProfile
description: CRD reference for the LogProfile resource.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`LogProfile` is a cluster-scoped resource that defines regex patterns with named capture groups for parsing training framework log output. It is referenced by `GoodputMeasurement` and `BandwidthMeasurement` resources.

## Example

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: LogProfile
metadata:
  name: nemo-training
spec:
  patterns:
    - name: step_time
      regex: 'reduced_train_loss=(?P<loss>[0-9.]+).*step_time=(?P<step_time>[0-9.]+)'
      fields:
        - name: loss
          type: float
        - name: step_time
          type: float
          unit: seconds
```

## Spec fields

_Generated from CRD schema — coming soon._

## Key spec fields (summary)

| Field | Type | Description |
|-------|------|-------------|
| `patterns` | []Pattern | List of named regex patterns with capture groups |
| `patterns[].name` | string | Unique name for this pattern |
| `patterns[].regex` | string | Go-syntax regex with named capture groups |
| `patterns[].fields` | []Field | Capture group names, types, and units |

## Built-in profiles

The following LogProfiles are installed by `ncrectl setup init`:

| Name | Framework |
|------|-----------|
| `nccl-bandwidth` | NCCL test output (all collectives) |
| `nemo-training` | NeMo framework training logs |

## Scope

LogProfiles are cluster-scoped — a single profile can be referenced by Jobs across all namespaces.
