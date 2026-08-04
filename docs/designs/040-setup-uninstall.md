# ADR-040: CLI Commands for Uninstalling Controller and Dependencies

## Context

ADR-036 and ADR-037 added `ncrectl setup install-deps` and `ncrectl setup install` for installing the Kubeflow Trainer dependency and the CRE controller respectively. However, there is no way to reverse these operations. Operators need clean uninstall commands for teardown, environment cleanup, and migration scenarios.

### Requirements

1. **Symmetric with install**: Mirror the install commands with the same safeguards.
2. **Reverse order**: Uninstall in reverse order of install (LogProfiles → Controller → CRDs) to avoid dangling references.
3. **Idempotent**: Use `--ignore-not-found` so running uninstall on an already-clean cluster succeeds.
4. **Same safeguards**: Terraform-style confirmation prompt with `--auto-approve` for CI.

## Decision

Add two commands:

- `ncrectl setup uninstall` — removes LogProfiles, controller (Deployment/RBAC/metrics), and CRDs in reverse order.
- `ncrectl setup uninstall-deps` — removes Kubeflow Trainer.

Both use `kubectl delete --ignore-not-found -k <url>` mirroring the install commands' `kubectl apply --server-side -k <url>`.

## Implementation

### `runKubectlDelete` helper

Mirrors `runKubectlApply` but uses `delete --ignore-not-found` instead of `apply --server-side`:

```go
func runKubectlDelete(kubectlPath, kustomizeURL, kubeconfig, kubeContext string, out io.Writer) error
```

### Uninstall order (reverse of install)

Install: CRDs → Controller → LogProfiles
Uninstall: LogProfiles → Controller → CRDs

The controller must be stopped before CRDs are removed, otherwise the API server rejects requests for CRD-defined types while the controller is still running.

### Controller uninstall via temp overlay

Same as install — `kubectl delete -k` requires a kustomization.yaml to resolve what to delete, so the same temp overlay (remote base + image override) is created. The image value doesn't affect deletion but the kustomization must be valid.

## Rationale

- **`--ignore-not-found`**: Makes uninstall idempotent. Running it twice or on a partially-uninstalled cluster succeeds without errors.
- **Reverse order**: Prevents the controller from crash-looping on missing CRDs during teardown.
- **Temp overlay for controller**: `kubectl delete -k` needs to resolve the kustomization to know which resources to delete. The overlay approach is the same as install.

## Consequences

### Positive
- Clean teardown with a single command.
- Safe by default with confirmation prompt.
- Idempotent — safe to run multiple times.

### Negative
- Requires network access to the git repo (same as install) to resolve the kustomization.
- CRD deletion removes all custom resources of those types — this is intentional but destructive.
