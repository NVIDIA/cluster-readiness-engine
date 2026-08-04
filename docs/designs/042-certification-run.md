# ADR-042: CLI Command for Running Certifications

## Context

Operators today must manually author Certification YAML files to run burn-in tests. This requires knowing the exact category domain/variant strings, the correct nodeSelector format, and the CRD schema. After applying, there is no way to wait for completion from the CLI — operators must poll with `kubectl get` or watch events.

`kubectl run` provides a well-known pattern for programmatically creating resources from CLI flags. We adopt the same approach for certifications: `ncrectl certification run` constructs a Certification object from flags and applies it to the cluster, with an optional `--wait` for completion.

### Requirements

1. **Category selection**: Specify one or more categories via `--category domain/variant` (repeatable flag).
2. **Validation**: Fail fast if a category doesn't exist in the catalog.
3. **Auto-naming**: Generate a unique name by default (`ncrectl-<timestamp>`), with `--name` override.
4. **Default target**: `nvidia.com/gpu.present=true` as the node selector.
5. **Wait mode**: `--wait` polls the Certification status and prints progress updates.
6. **No new dependencies**: Use existing `newK8sClient()` and standard `time.Ticker` for polling.

## Decision

Add `ncrectl certification run` that programmatically builds and creates a Certification resource in the cluster. The `--wait` flag polls every 5 seconds, prints category status changes, and exits on terminal condition (Succeeded/Failed).

### Category Validation

Before creating the Certification, each `--category` value is validated against `catalog.Lookup()`. On failure, the error message lists available categories from `catalog.List()`.

### Wait Implementation

Simple polling with change detection — only print a status line when a category's status changes. No spinner library — just `[wait]` bracketed output to stderr matching the kubeadm style from `setup init`.

Terminal conditions checked via `meta.IsStatusConditionTrue()`:
- `CertificationSucceeded` → exit 0
- `CertificationFailed` → print failed nodes, exit 1

## Rationale

- **Repeatable `--category` flag**: More ergonomic than comma-separated. Matches `kubectl run --env` pattern.
- **Auto-generated name**: Reduces friction for ad-hoc runs. Timestamp ensures uniqueness.
- **`nvidia.com/gpu.present=true` default**: The most common selector — targets all GPU nodes.
- **Polling over watch**: Simpler implementation. 5-second polling is acceptable for certifications that run for minutes to hours. Avoids client-go watch complexity and reconnection handling.
- **No `--dry-run`**: The existing `ncrectl certification render` already serves this purpose.

## Consequences

### Positive
- One-command certification run without writing YAML.
- Built-in wait with progress tracking.
- Category validation prevents typos.

### Negative
- Limited to catalog categories (custom Workflow specs still require YAML).
- Polling every 5s adds minor API server load (negligible for one resource).
