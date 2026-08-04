# ADR-037: CLI Command for Controller Installation

## Context

After installing external dependencies (ADR-036), operators must install the CRE controller itself — CRDs, RBAC, the controller Deployment, and LogProfiles. Today this requires cloning the repository and running three separate Makefile targets (`make install`, `make deploy IMG=...`, `kubectl apply -k config/logprofiles/`), each with different arguments. This process is error-prone, undiscoverable, and requires Git access to the repo source.

Since the repository is public and kustomize supports remote bases via HTTPS, the CLI can install directly from the repository without cloning. The controller image is versioned together with the CLI (same repo, same tags), so the CLI can derive the correct image automatically.

### Requirements

1. **Single command**: Install CRDs, controller, and LogProfiles in one operation.
2. **Version-locked image**: Default image derived from the CLI's own compiled-in version.
3. **Image override**: Allow custom registries via `--image` flag.
4. **Same safeguards**: Terraform-style confirmation prompt, `--auto-approve` for CI, fail-fast kubectl check.
5. **Idempotent**: Safe to run multiple times (server-side apply).

## Decision

Add `ncrectl setup install` that performs three sequential kubectl operations:

1. **CRDs**: `kubectl apply --server-side -k <repo>/config/crd?ref=<version>` — applied directly (CRDs are too large for piped rendering).
2. **Controller**: Create a temporary kustomize overlay that references the remote `config/default` as a base and overrides the container image. Apply via `kubectl apply --server-side -k <temp-dir>`.
3. **LogProfiles**: `kubectl apply --server-side -k <repo>/config/logprofiles?ref=<version>` — applied directly.

The controller image defaults to `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>` where `<version>` is the CLI's build-time version. The `--image` flag overrides the full image string for custom registries.

## Implementation

### Image Override via Local Kustomize Overlay

Kustomize has no CLI flag for inline image overrides. The community-standard pattern (used by Flux and ArgoCD) is a local overlay that references a remote base:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://github.com/NVIDIA/cluster-readiness-engine/archive/refs/tags/v1.0.0.tar.gz
images:
  - name: controller
    newName: ghcr.io/nvidia/cluster-readiness-engine/manager
    newTag: "v1.0.0"
```

The CLI creates this in a temporary directory, runs `kubectl apply --server-side -k <temp-dir>`, then cleans up via `defer os.RemoveAll`. This avoids cloning the repo while allowing arbitrary image overrides.

### Image Parsing

The `--image` value (or default) must be split into kustomize's `newName` (registry/repository) and `newTag` components. The parser handles:
- Standard tags: `registry/repo:tag` → newName=`registry/repo`, newTag=`tag`
- Digests: `registry/repo@sha256:abc` → newName=`registry/repo`, newTag=`@sha256:abc`
- Registry ports: `localhost:5000/repo:tag` → newName=`localhost:5000/repo`, newTag=`tag`
- No tag: `registry/repo` → newName=`registry/repo`, newTag=`latest`

### Version Locking

The `version` variable (set via `-ldflags` at build time) serves dual purpose:
1. **Git ref**: `?ref=<version>` in remote kustomize URLs — ensures manifests match the CLI.
2. **Image tag**: Default image tag — ensures the controller binary matches the manifests.

This means `ncrectl v1.2.3` always installs v1.2.3 manifests with the v1.2.3 image by default.

### Files Modified

- `pkg/setup/setup.go` — add `newInstallCommand()` and supporting functions
- `internal/setup_test.go` — add unit tests for parseImage, writeTempOverlay, URL construction

## Rationale

- **Default image from version**: The CLI and controller are versioned together. Requiring operators to specify the image for every install would be redundant and error-prone. The default ensures version consistency.
- **`--image` override**: Custom registries, air-gapped environments, and development all require image flexibility. Making it optional (not required) optimizes for the common case.
- **Temp overlay over sed**: `sed`-based image replacement is fragile. The kustomize overlay pattern is the community standard, type-safe, and handles edge cases (multiple containers, init containers, etc.).
- **Three sequential steps**: CRDs must exist before the controller can start (the Deployment references CRD types). LogProfiles are cluster-scoped resources that the controller reads at runtime.

## Consequences

### Positive

- Single-command installation from anywhere — no repo clone needed.
- Version-locked by default — prevents accidental version mismatches.
- Same UX pattern as `install-deps` — consistent operator experience.

### Negative

- Requires network access to the git repository at install time.
- Temporary directory creation adds a small amount of complexity.
- `dev` version (no ldflags) will fail for remote refs — acceptable for development.
