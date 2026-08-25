# ADR-052: Forced CLI Upgrade Check for Release Builds

> **Status:** Proposed

## Context

Version sprawl across customer deployments causes bug reports against outdated nvcrectl versions. Customers run old binaries for weeks after new releases, leading to wasted debugging time on already-fixed issues. The CLI needs a mechanism to force users onto the latest version.

The check must only apply to release-pipeline builds (clean semver like `1.20.0`), not development builds (`1.20.0-4-gHASH-dirty` or `dev`). When the GitHub API is unreachable (air-gapped clusters), the CLI should warn and proceed rather than blocking.

## Decision

Add a `PersistentPreRunE` hook to the root command that, for release builds only, calls the GitHub Releases API to check if a newer version exists. If outdated, the CLI prints an upgrade message and exits with code 1. There is no skip mechanism — the check is mandatory for release builds.

### Release Build Detection

A version string is a release build if `parseSemanticVersion` succeeds AND the original string equals the reconstructed `major.minor.patch` (no suffix):

- `1.20.0` → release (original matches `String()`)
- `1.20.0-4-gHASH` → dev (suffix stripped during parse, original differs)
- `dev` → dev (parse fails)

### Behavior Matrix

| Scenario | Action |
|----------|--------|
| Dev build (`1.20.0-dirty`) | Skip check, proceed normally |
| Release build, up to date | Proceed normally |
| Release build, outdated | Print upgrade message, exit 1 |
| Release build, GitLab unreachable | Print warning, proceed |
| `upgrade` command | Skip check (it IS the upgrade) |
| `--version` / `--help` | Skip check |

## Implementation

**`pkg/upgrade/upgrade.go`**: Add `isReleaseBuild(v string) bool` and `enforceUpgrade(v string, out io.Writer) error`. The `enforceUpgrade` function uses a dedicated HTTP client with 5-second timeout to avoid blocking on slow networks. Reuses existing `fetchLatestVersion()`, `parseSemanticVersion()`, and `isNewer()`.

**`cmd/nvcrectl/main.go`**: Add `PersistentPreRunE` to the root command that calls `enforceUpgrade` for release builds, skipping for `upgrade`, `version`, `help`, and `completion` commands.

## Rationale

1. **No skip mechanism** — the entire point is to force upgrades. An env var escape hatch defeats the purpose and creates a support burden ("just set SKIP=1").

2. **Release builds only** — developers need to iterate without API calls blocking every invocation. The `git describe` suffix naturally distinguishes dev from release.

3. **Warn-and-proceed on network failure** — customers in air-gapped or restricted environments should not be blocked entirely. The check is best-effort.

4. **5-second timeout** — short enough to not frustrate users, long enough for typical corporate network latency.

## Consequences

- Release builds older than the latest tag will refuse to run (exit 1).
- Customers must upgrade to continue using the CLI. This is intentional.
- Air-gapped deployments will see a warning but can still operate.
- Dev builds are completely unaffected.

## References

- `pkg/upgrade/upgrade.go` — existing upgrade infrastructure
- `cmd/nvcrectl/main.go` — root command setup
- `.github/workflows/release.yml` — release pipeline (semantic-release + publish)

## Change Log

| Date | Change |
|------|--------|
| 2026-02-25 | Initial proposal |
