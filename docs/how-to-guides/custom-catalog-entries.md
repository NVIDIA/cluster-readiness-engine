---
title: Custom Catalog Entries
description: Add a new domain/variant pair to the certification catalog.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


The catalog is extensible — add a new Go file to `internal/catalog/entries/` to register a custom certification category.

## File layout

```
internal/catalog/entries/
  my-domain/
    my-variant/
      entry.go     ← new file
```

## Minimal entry

```go
package myvariant

import "github.com/NVIDIA/cluster-readiness-engine/internal/catalog"

func init() {
    catalog.Register("my-domain", "my-variant", buildSpec)
}

func buildSpec(opts catalog.Options) (*catalog.WorkflowSpec, error) {
    return &catalog.WorkflowSpec{
        // ... workload definition
    }, nil
}
```

The `init()` registration is picked up automatically as long as the package is blank-imported in `cmd/main.go`.

## Verify

```bash
go build -o bin/xcalctl ./tools/xcalctl/
./bin/xcalctl certification render --platform aws /tmp/my-cert.yaml
```

Check the rendered Workflow for correct resource requests, env vars, and override annotations.

_Full `catalog.WorkflowSpec` API reference coming soon._
