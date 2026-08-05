# ADR-048: Embedded Trainer Manifests

## Context

ADR-045 embedded all CRE manifests (CRDs, controller, LogProfiles) into the ncrectl binary and applied them via the Go Kubernetes client. The Kubeflow Trainer `[deps]` phase was intentionally kept remote ("it is an external dependency with its own release cadence") and continues to shell out to `kubectl apply -k <github-url>`.

In practice, this has three problems:

1. **The Trainer version is pinned.** The kustomize URL always points at `v2.1.0`. There is no dynamic version discovery — bumping the version requires a code change regardless.
2. **GitHub fetch timeouts.** The remote kustomize resolution depends on `github.com` being reachable. Transient timeouts cause `setup init` and `setup reset` to fail entirely, even after the retry loop.
3. **kubectl is only needed for `[deps]`.** All other phases use the embedded Go client path. The sole remaining reason to require kubectl in the default install flow is this one phase.

The force-cleanup fallback added for network failures (deleting known resources individually) is a workaround, not a fix. Embedding the manifests eliminates the root cause.

## Decision

1. **Pre-render** the Kubeflow Trainer kustomize overlay (`manifests/overlays/manager?ref=v2.1.0`) into `pkg/setup/embedded/trainer.yaml` and commit it.
2. **Embed** the file via `go:embed embedded/trainer.yaml` alongside the existing CRE embeds.
3. **Apply/delete via Go client** using the existing `applyManifests`/`deleteManifests` functions — no kubectl needed in the default path.
4. **Retain kubectl** only for the `--version` override path, which still fetches from remote URLs.
5. **Add a Makefile target** `embed-trainer` for re-rendering on version bumps. This is NOT a dependency of `embed-ncrectl` — it runs on-demand when the Trainer version changes.

### Embedded File

```
pkg/setup/embedded/
├── crds/                   # (existing) CRE CRDs
├── logprofiles/            # (existing) CRE LogProfiles
├── controller.yaml         # (existing) CRE controller stack
└── trainer.yaml            # (new) Pre-rendered Kubeflow Trainer manifests
```

### Modified runInit Flow (default embedded path)

```
1. [preflight]    getClusterInfo (kubectl NOT required)
2. [deps]         applyManifests(ctx, c, embeddedTrainer, out)
                  → waitForDeploymentReady("kubeflow-system", "kubeflow-trainer-controller-manager")
3. [crds]         applyEmbeddedDir (unchanged)
4. [controller]   patchControllerImage + applyManifests (unchanged)
5. [logprofiles]  applyEmbeddedDir (unchanged)
```

### Modified runReset Flow (default embedded path)

```
1. [logprofiles]  deleteEmbeddedDir (unchanged)
2. [controller]   deleteManifests (unchanged)
3. [crds]         deleteEmbeddedDir (unchanged)
4. [deps]         deleteManifests(ctx, c, embeddedTrainer, out)
```

### `--version` Override

When `--version` is set, the `[deps]` phase falls back to the existing kubectl + remote kustomize URL approach. The `forceCleanupTrainer` fallback is retained for the `--version` reset path.

### cleanupLifecycle (certification run)

The separate `[deps]` cleanup block (which required kubectl and had its own force-cleanup logic) is folded into the existing `embeddedPhase` loop alongside LogProfiles, controller, and CRDs.

## Rationale

- **The version is pinned.** Embedding a pinned dependency trades a rare version-bump task for reliable offline install. The coupling concern from ADR-045 does not apply when the version is already statically defined.
- **Eliminates the last kubectl dependency** in the default install/reset path. The Go client is more reliable and does not require a separate binary.
- **Force-cleanup becomes unnecessary** in the default path. The embedded manifests are always available for clean deletion.
- **Offline install is now complete.** All four phases (deps, CRDs, controller, LogProfiles) work without network access.

## Consequences

### Positive

- `ncrectl setup init` and `setup reset` work fully offline.
- kubectl is no longer required in the default install flow.
- No more GitHub timeout failures during install.
- Binary size increases by ~150KB (Trainer manifests).

### Negative

- Bumping the Kubeflow Trainer version requires running `make embed-trainer` and committing the updated `trainer.yaml`. This is the same workflow as `make embed-ncrectl` for CRE manifests.
- The `trainer.yaml` file is large (~4000 lines) and checked into the repository.

## Alternatives Considered

1. **Keep the status quo with more retries.** Does not fix air-gapped environments. Rejected.
2. **Cache trainer.yaml at first successful fetch.** Adds complexity (where to store, cache invalidation). Rejected.
3. **Vendor the Kubeflow kustomize sources.** More files to maintain than a single pre-rendered YAML. Rejected.

## Notes

- Supersedes ADR-045 decision #3 ("Keep kubectl only for the `[deps]` phase") and alternative #2 ("Embed everything including Kubeflow: Couples ncrectl releases to Kubeflow releases. Rejected.").
- `forceCleanupTrainer` is retained for the `--version` reset fallback path.

## References

- ADR-045: Embedded Config and Go Client Apply in ncrectl
- ADR-041: Kubeadm-style Init/Reset
