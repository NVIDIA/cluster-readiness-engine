# ADR-069: cmd/ layout — kubernetes/kubernetes convention

## Context

Two structural problems existed in the codebase:

1. **`cmd/main.go`** (the controller manager) was a flat file directly in `cmd/`, not in its own subdirectory. The kubernetes/kubernetes convention places every binary entrypoint in its own directory (`cmd/<name>/main.go`) so that `go build ./cmd/<name>/` works consistently and the purpose of each binary is immediately visible.

2. **`tools/nvcrectl/`** placed the CLI outside `cmd/` and implemented everything in `package main`. A `package main` is a dead end — no other package can import its symbols. As a result, logic that naturally belongs in reusable internal packages (report building, cluster discovery, setup operations, self-upgrade) was inaccessible to tests via import and duplicated where needed (notably `buildWorkflowSpec` existed in both the CLI and the controller).

## Decision

Restructure the source tree to follow the kubernetes/kubernetes convention:

- Every binary lives in its own `cmd/<name>/` subdirectory with a minimal `main.go` that only wires the cobra command tree and calls `Execute()`.
- All reusable logic moves to `pkg/` packages named after their domain.
- `tools/` is deleted (it would be empty) and `pkg/` is used instead of `internal/` for exportability.

### Target layout

```
cmd/
  manager/main.go     ← controller manager (was cmd/main.go)
  nvcrectl/main.go     ← thin entry point only (was tools/nvcrectl/main.go + root.go)

pkg/
  certification/      ← certification run/watch/report commands
  cluster/            ← cluster info discovery + shared GPU node helpers
  render/             ← workflow offline/online render + embedded node fixtures
  report/             ← report data model, terminal rendering, JSON export
  setup/              ← cluster init/reset/status + embedded manifests
  upgrade/            ← binary self-upgrade + semantic versioning
  workloadrun/        ← workloadrun CLI commands + BuildWorkflowSpec (shared)
```

### Package boundaries

| Package | Exports | No internal deps |
|---|---|---|
| `pkg/upgrade/` | `NewCommand`, `EnforceUpgrade` | ✓ |
| `pkg/setup/` | `NewCommand`, `SetupStatus` | ✓ |
| `pkg/cluster/` | `NewCommand`, `DiscoverGPUNodes`, `UniformGPUProduct` | — |
| `pkg/report/` | `Build`, `Print`, `PrintMulti`, `WriteJSON`, report types | — |
| `pkg/render/` | `NewWorkflowCommand`, `SyntheticProviderID`, `LoadEmbeddedNodes` | — |
| `pkg/certification/` | `NewCommand` | — |
| `pkg/workloadrun/` | `NewCommand`, `BuildWorkflowSpec` | — |

### Deduplication

`buildWorkflowSpecFromRun` (CLI) and `(r *WorkloadRunReconciler).buildWorkflowSpec` (controller) implemented parallel logic. With `pkg/workloadrun/` now a proper package, `BuildWorkflowSpec` is extracted there. Full unification with the controller version is deferred because the controller's method depends on several reconciler-specific helpers; that work belongs in a separate ADR.

`hasCondition` (local in certification and report files) is replaced with the existing `controller.CondIsTrue`.

## Implementation

See the git history for file-by-file changes. The cobra command tree in `cmd/nvcrectl/main.go` is the single place where sub-commands are assembled.

## Rationale

- **Discoverability**: `cmd/` lists every binary. `internal/` lists every domain.
- **Testability**: Package-level functions can be unit-tested directly; `package main` cannot be imported.
- **Reuse**: `pkg/cluster.DiscoverGPUNodes` is now callable from any future package without copy-paste.
- **Deduplication**: `hasCondition` eliminated; `BuildWorkflowSpec` is now in a proper, importable package.

## Consequences

- Makefile and Dockerfile build paths change: `cmd/main.go` → `./cmd/manager/`, `./tools/nvcrectl/` → `./cmd/nvcrectl/`.
- `CLAUDE.md` and ADR docs that reference `tools/nvcrectl/` paths are updated to the new `internal/` paths.
- No behavior change to either binary.
- No API, CRD, or Kubernetes resource changes.

## Alternatives Considered

**Single `internal/nvcrectl/` package** — simpler, but one flat package for all CLI logic is an afterthought rather than a design. The domain packages make each area independently navigable and testable.

**Keep `tools/nvcrectl/` as-is** — avoids the refactor but leaves the dead-end `package main` problem and the duplicated `buildWorkflowSpec`.
