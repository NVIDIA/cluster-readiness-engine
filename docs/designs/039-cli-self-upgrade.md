# ADR-039: CLI Self-Upgrade Command

## Context

When a new version of ncrectl is released, operators must manually download and replace the binary. There is no built-in mechanism to check for updates or perform in-place upgrades. This leads to version drift across teams and missed improvements.

the ncrectl installer solves this with a self-update mechanism that queries GitLab for newer releases and downloads them in-place. We adopt the same pattern for ncrectl, adapted to use the Package Registry (where ncrectl binaries are published) instead of GitLab Releases.

### Requirements

1. **Version check**: Compare the running version against the latest GitLab tag.
2. **Release notes**: Show what changed in the new version.
3. **Interactive prompt**: Ask before replacing the binary.
4. **Check-only mode**: `--check` flag to just report without installing.
5. **No authentication**: Public repo, no GITLAB_TOKEN required.
6. **Sudo fallback**: Handle system directories gracefully.

## Decision

Add a top-level `ncrectl upgrade` command that:

1. Fetches the latest version tag from GitHub API.
2. Compares against the running version using semantic versioning.
3. Shows release notes from the GitHub release notes.
4. Prompts for confirmation (y/N).
5. Downloads the correct binary from the Package Registry.
6. Replaces the running binary with `os.Rename` (sudo fallback on permission error).

### GitHub API Endpoints

- **Latest tag**: `GET https://api.github.com/repos/NVIDIA/cluster-readiness-engine/releases/latest` (first entry)
- **Release notes**: `GET https://api.github.com/repos/NVIDIA/cluster-readiness-engine/releases/tags/{tag}` (description field)
- **Binary download**: `GET https://github.com/NVIDIA/cluster-readiness-engine/releases/download/{version}/ncrectl-{os}-{arch}`

### Version Comparison

Semantic versioning with optional `v` prefix: `v1.2.3` or `1.2.3`. Pre-release suffixes (e.g., `-dirty`, `-4-gabcdef`) are stripped for comparison. Major > Minor > Patch ordering.

## Implementation

### New file: `pkg/upgrade/upgrade.go`

Functions:
- `newUpgradeCommand()` — cobra command with `--check` flag
- `runUpgrade()` — orchestrates the full flow
- `fetchLatestVersion()` — GitHub Releases API
- `fetchReleaseNotes()` — GitHub Releases API
- `downloadBinary()` — Package Registry download to temp dir
- `installBinary()` — rename with sudo fallback
- `parseSemanticVersion()` — parse version string
- `isNewer()` — compare two versions
- `generateBinaryName()` — `ncrectl-{runtime.GOOS}-{runtime.GOARCH}`

Registered as top-level command in `root.go` (not under `setup` — upgrades are a different concern from cluster setup).

## Rationale

- **Top-level command**: `ncrectl upgrade` is more discoverable than `ncrectl setup upgrade`. Upgrading the CLI is not cluster setup.
- **y/N prompt (not Terraform-style)**: Self-upgrade is lower risk than cluster changes. Simple y/N is sufficient, matching the ncrectl installer's pattern.
- **No checksum**: Kept simple for initial implementation. The HTTPS transport provides integrity.
- **Package Registry**: Matches the existing CI publishing pipeline. No changes to release process needed.

## Consequences

### Positive
- Operators can upgrade with a single command.
- Version drift is eliminated — easy to stay current.
- Release notes are visible at upgrade time.

### Negative
- No checksum verification (acceptable for initial version, HTTPS provides transport security).
- Requires network access to github.com and api.github.com.
- `dev` version (no ldflags) will report as outdated against any real tag.
