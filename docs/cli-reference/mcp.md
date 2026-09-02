---
title: nvcrectl mcp
description: Serve read-only NVCRE certification state to MCP agents.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl mcp serve

Exposes NVCRE certification state to [Model Context Protocol](https://modelcontextprotocol.io) (MCP) agents over the stdio transport, so an agent can answer "did this certification pass, and which nodes failed?" without scraping CLI output.

```bash
nvcrectl mcp serve [flags]
```

### Tools

The server is strictly read-only: no tool creates, mutates, or deletes a resource, and nothing triggers a run — runs consume real GPU time. Every tool carries the MCP `readOnlyHint` annotation.

| Tool | Description |
|------|-------------|
| `list_categories` | List the certification catalog: every registered `domain/variant` category a Certification can run |
| `get_certification_status` | Overall result (`PASSED`/`FAILED`/`RUNNING`), conditions, per-category state, and failed node names for one Certification |
| `get_certification_report` | The full report that `nvcrectl certification report` prints — categories with metrics, bandwidth, cliques, diagnose results, and per-node results |
| `list_failed_nodes` | Failed nodes for one Certification with per-node failure reason and message |

The three certification-scoped tools accept `name` and `namespace` (default `default`).

### Authentication

All cluster access uses the kubeconfig of whoever launches the server, resolved with the standard client-go rules: the `--kubeconfig`/`--context` flags, then the `KUBECONFIG` environment variable, then `~/.kube/config`. The server holds no credentials of its own, never reads in-cluster service account tokens, and can therefore never exceed the permissions of the user who runs it.

### Client configuration

Most MCP clients spawn the server themselves over stdio. For example, in Claude Desktop or any client with a similar config format:

```json
{
  "mcpServers": {
    "nvcre": {
      "command": "nvcrectl",
      "args": ["mcp", "serve"]
    }
  }
}
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--kubeconfig` | `~/.kube/config` | Path to the kubeconfig file to use for requests |
| `--context` | current context | Name of the kubeconfig context to use |

### Example

```bash
# Serve with the default kubeconfig (typical: launched by the MCP client)
nvcrectl mcp serve

# Serve against a specific kubeconfig and context
nvcrectl mcp serve --kubeconfig /path/to/kubeconfig --context gpu-cluster
```
