# ADR-075: A Read-Only MCP Server for Certification State

> **Status:** Proposed

## Context

Answering "did this certification pass, and which nodes failed?" today means a person running `nvcrectl` and reading CRD status. Operators increasingly put agents in that loop, and an agent can only work against a typed interface — scraping CLI output couples it to formatting that is free to change (issue #242).

Two properties make this more than a transport question.

**Runs cost real GPU time.** A certification occupies the fleet it certifies. An interface that lets an agent start one turns a misread prompt into hours of contended hardware, so the write surface is the part that needs justifying, not the read surface.

**Two views of the same certification can disagree.** `report.Build` ([report.go](../../pkg/report/report.go)) already owns the verdict: it normalises `InProgress` to `Running`, deduplicates failed nodes by name across categories, and downgrades `PASSED` to `INCOMPLETE` when the Workflow excluded nodes from the run — a rule that exists precisely so untested nodes cannot be invisible. Any second implementation of that logic is free to drift from it, and a drifted summary is worse than no summary: an agent quoting "PASSED" for a run that left eight nodes untested is confidently wrong in the direction that gets bad hardware shipped.

## Decision

1. **Ship the server in-repo, over stdio, exposing four read-only tools**: `list_categories`, `get_certification_status`, `get_certification_report`, `list_failed_nodes`. Stdio keeps the transport local — the agent spawns the process itself — which avoids introducing a network listener and the bearer-token or OAuth design that would come with it.

2. **No tool creates, mutates, or deletes anything, and none triggers a run.** Every tool carries the MCP `readOnlyHint` annotation, and `TestListTools` pins the surface so adding a fifth or mutating tool fails the build. Whether agents may ever trigger runs is deferred; it needs its own decision about consumption and preemption, not a flag on this one.

3. **Every certification verdict is projected from `report.Build`, never re-derived from the CR.** `get_certification_status` calls the same builder `nvcrectl certification report` uses and reads its fields, rather than walking conditions itself. This is the load-bearing decision: it makes "the tools agree with the CLI" a structural property instead of a convention two code paths have to keep. `TestStatusAgreesWithReport` asserts the two tools describe the same certification identically, across every fixture.

4. **`list_failed_nodes` returns one row per distinct (node, reason, message); `get_certification_status.failedNodes` returns unique node names.** These are deliberately different: a node can fail two categories for two reasons, and both the detail and the count are useful. Because that difference is a real trap for a caller doing `len()`, each tool's description states which one it is and points at the other.

5. **Credentials are whatever client-go resolves, and the docs say so exactly.** The server holds no credentials and adds no privilege. It is not, however, true that it "never reads service account tokens": client-go's standard loading rules end in an in-cluster fallback, so running it inside a pod without a kubeconfig authenticates as that pod's ServiceAccount. The documentation states this and tells operators to bind a least-privilege role when deploying in-cluster, because a security guarantee that is only true outside a pod is worse than none.

## Consequences

An agent gets certification state without scraping, and cannot start a run through this interface.

The server inherits `pkg/report`'s error handling, which returns empty results rather than errors when a Workflow or node-results ConfigMap cannot be read. For a CLI a human reads that is a minor annoyance; for a tool an agent quotes, "no nodes failed" and "not allowed to look" become indistinguishable. This ADR does not change that shared behaviour — the docs name the RBAC the tools need and call out the failure mode. Surfacing read errors through `report.Build` is worth doing on its own merits and affects the CLI equally.

Adding the MCP Go SDK makes it the first MCP dependency in `go.mod`, and it lands in the `nvcrectl` binary rather than the controller.

## Alternatives Considered

**Re-derive status from the Certification CR.** Cheaper — it avoids the Workflow reads `report.Build` performs — and it is what the first implementation did. It produced three divergences from the report (missing `INCOMPLETE`, raw `InProgress` instead of `Running`, and a different failed-node dedupe key) before any of them were noticed, because nothing compared the two. Rejected: the saving is a few reads, and the cost is an agent being told a run passed when it did not.

**An HTTP/SSE transport.** Would let one server serve several agents, but requires deciding how it authenticates them and how it maps a caller to a kubeconfig. Deferred until something asks for it.

**MCP resources instead of, or alongside, tools.** Resources suit static documents; certification state is queried by name and namespace. Four tools cover it without a second surface to keep consistent.
