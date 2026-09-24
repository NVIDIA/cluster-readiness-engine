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
| `get_certification_status` | Overall result (`PASSED`/`INCOMPLETE`/`FAILED`/`RUNNING`), conditions, per-category state, any nodes excluded from the run, and the unique names of failed nodes for one Certification |
| `get_certification_report` | The full report that `nvcrectl certification report` prints — categories with metrics, bandwidth, cliques, and diagnose results |
| `list_failed_nodes` | Failed nodes for one Certification with per-node failure reason and message |

The three certification-scoped tools accept `name` and `namespace` (default `default`).

`INCOMPLETE` means the run passed but left some targeted nodes untested — the Workflow excluded them, and `excludedNodes`/`exclusionReason` say which and why. Treat it as "not certified": nothing was observed about those nodes either way. Both `get_certification_status` and `get_certification_report` report it, because both derive the verdict from the same builder.

`result` is the authoritative outcome. `get_certification_status.conditions` are the Certification's raw conditions, which the `INCOMPLETE` downgrade does not touch: an `INCOMPLETE` run still carries `Succeeded=True` (`AllCategoriesSucceeded`). Never infer a pass from `conditions`.

`get_certification_status.failedNodes` is the unique node names, deduplicated across categories. `list_failed_nodes` returns one row per distinct failure reason instead, so a node that failed in several categories appears more than once — use the former for a count. Both come from the same walk over the categories' node-results ConfigMaps, so the set of node names always matches.

### Authentication

All cluster access uses the kubeconfig of whoever launches the server, resolved with the standard client-go rules: the `--kubeconfig`/`--context` flags, then the `KUBECONFIG` environment variable, then `~/.kube/config`. The server holds no credentials of its own and adds no privilege of its own — it is exactly as authorized as the identity client-go resolves.

<Warning>
Client-go's loading rules end in an in-cluster fallback. Run `nvcrectl mcp serve` inside a pod with no kubeconfig and it authenticates as that pod's ServiceAccount, which may be broader than the operator running the agent. When you deploy the server in-cluster, bind its ServiceAccount to a role that grants no more than the reads below.
</Warning>

In the target namespace the tools need:

| Verb | Resources |
|------|-----------|
| `get` | `certifications`, `workflows`, `jobs` (`nvcre.nvidia.com`), `jobs` (`batch`), `configmaps` |
| `list` | `goodputmeasurements`, `bandwidthmeasurements`, `jobs` (`nvcre.nvidia.com`) |

`certifications` is always fetched by name, so `get` is enough; it does not need `list`.

<Warning>
`report.Build` treats every one of these reads as best-effort: a read it is not permitted to make is skipped, not reported. Bind all of them, or `get_certification_report` returns a successful response with metrics, bandwidth, and diagnose data silently absent, and `get_certification_status` reports an empty `failedNodes` for what is really "not allowed to look". The missing data is not one obvious field, so a partial binding is hard to notice from the output alone.
</Warning>

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
