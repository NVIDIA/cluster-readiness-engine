# ADR-065: ncrectl Helm Install

## Context

ADR-064 added a Helm chart at `helm/cluster-readiness-engine/` that bundles Kubeflow Trainer, CRE CRDs, the controller, and LogProfiles. `ncrectl setup init` still applies four separate embedded YAML phases (Trainer, CRDs, controller, LogProfiles) generated from kustomize. Operators now maintain two install paths with the same components.

## Decision

| # | Topic | Decision |
|---|--------|----------|
| 1 | **Default install path** | `ncrectl setup init` applies embedded YAML phases (deps, CRDs, controller, LogProfiles). **YAML is default.** |
| 2 | **`--helm` flag** | Opt-in. `ncrectl setup init --helm` / `setup reset --helm` install/uninstall via the Helm 3 Go SDK, pulling the chart from `oci://ghcr.io/nvidia/cluster-readiness-engine` at the CLI version (or `--version`). Helm becomes default once validated in production. |
| 3 | **Chart source** | No embedded chart. The Helm path pulls from NGC OCI (requires network and NGC auth via `--image-pull-secret` or prior `helm registry login`). Dev builds require `--version`. Legacy YAML phases remain embedded for the default path. |
| 4 | **Helm SDK** | The opt-in path uses `helm.sh/helm/v3` (not v4 — see comment in `setup_helm.go`); the `helm` CLI is not required for `ncrectl setup init --helm`. Manual `helm install` still needs Helm 3+. |
| 5 | **`--skip-phases=deps`** | On the Helm path: `trainer.enabled=false` on init; reset uninstalls the release without touching Trainer installed out-of-band. |
| 6 | **Partial `--skip-phases`** | Skipping crds, controller, or logprofiles forces the legacy YAML path — Helm installs all chart components in one release. |
| 7 | **Reset** | With `--helm`, Helm SDK uninstall removes the release; CRDs under `crds/` are deleted explicitly (Helm retains CRDs on uninstall). Without `--helm`, YAML phases run in reverse order. |
| 8 | **Namespace** | No `templates/namespace.yaml`. ncrectl sets `CreateNamespace=true`; manual/GitOps installs use `helm --create-namespace` or ArgoCD `CreateNamespace`. |
| 9 | **Pull secret** | **`--image-pull-secret`** creates the NGC pull secret via the Kubernetes API on both YAML and `--helm` paths (`setupControllerSecret`); Helm values reference the secret name via `manager.imagePullSecrets`. The chart does not render pull secrets. |
| 10 | **Init/reset symmetry** | `--skip-phases=deps` uses the Helm path for both init and reset. |

## Implementation

- `pkg/setup/helm.go` — NGC OCI chart pull, Helm SDK install/uninstall, CRD cleanup, release values.
- `runInit` / `runReset` — branch on `useHelmInstall(useHelm, skip)` vs YAML path.
- `make embed-ncrectl` — legacy YAML only (CRDs, controller, LogProfiles, Trainer); no chart embed.

## Rationale

- One install artifact matches GitOps and NGC distribution (ADR-064).
- No chart duplication at build time; released binaries pull the matching NGC chart version.
- YAML default preserves existing automation; `--helm` opt-in allows production validation before flipping the default.
- Pull secrets stay out of Helm release values — ncrectl creates them client-side (same as YAML path).

## Consequences

### Positive

- `ncrectl setup init --helm` and `helm install` deploy the same manifest set.
- NGC API keys are not stored in Helm release history.

### Negative

- Two install paths until `--helm` is promoted to default.
- Partial phase skips still use the legacy YAML path until removed.

## References

- ADR-064: Helm Chart Distribution
- ADR-045: Embedded Config and Go Client Apply in ncrectl
