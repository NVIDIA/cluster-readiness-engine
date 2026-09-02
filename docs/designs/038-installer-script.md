# ADR-038: Shell Installer Script for nvcrectl

## Context

The nvcrectl CLI binary is cross-compiled and published to GitHub Releases by CI on each tagged release. However, there is no discoverable way for operators to install it — they must manually navigate the package registry, find the correct binary for their platform, download it, and place it in PATH. This is a poor first-run experience.

A prior internal NVIDIA CLI solved this with a shell installer script served from a stable URL, enabling a one-liner: `curl -sSL <url> | bash`. We adopt the same pattern for nvcrectl.

### Requirements

1. **One-liner install**: `curl -sSL <url> | bash` must work.
2. **Platform detection**: Automatically detect OS (linux/darwin) and architecture (amd64/arm64).
3. **Version discovery**: Fetch the latest release tag from GitHub API.
4. **No authentication**: The repository is public; no token required.
5. **Configurable install directory**: Default `/usr/local/bin`, override with `-d <dir>`.
6. **Sudo fallback**: Use sudo for system directories if needed.

## Decision

Create an `installer` bash script at the repository root, adapted from the prior internal CLI's installer pattern. The script downloads the pre-built binary from the GitHub Releases and installs it.

The script is served via:
```
https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer
```

### Download URL Pattern

Binaries are published by CI to:
```
https://github.com/NVIDIA/cluster-readiness-engine/releases/download/{version}/nvcrectl-{os}-{arch}
```

Binary naming follows the existing CI convention: `nvcrectl-{os}-{arch}` (e.g., `nvcrectl-linux-amd64`).

### Version Discovery

The latest version is fetched from the GitHub Releases API:
```
GET https://api.github.com/repos/NVIDIA/cluster-readiness-engine/releases/latest
```

The first entry (most recent tag) is used as the version.

## Rationale

- **Shell script over Go binary**: The installer must bootstrap nvcrectl from nothing — a Go binary would have the same distribution problem it's trying to solve.
- **GitHub release asset URL**: Provides a stable, versionable URL without requiring a separate hosting service.
- **No checksum verification**: Kept simple for initial implementation. Can be added later when CI generates checksum files.
- **Prior internal CLI pattern**: Proven pattern, familiar to NVIDIA operators.

## Consequences

### Positive
- One-command installation from anywhere.
- Works in CI pipelines and developer workstations.
- No external dependencies beyond `curl`.

### Negative
- No checksum verification (to be added in a future iteration).
- Requires network access to github.com and api.github.com.
