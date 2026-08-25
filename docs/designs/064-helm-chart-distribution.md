# ADR-064: Helm Chart Distribution

## Context

CRE is installed today via `nvcrectl setup init`, which applies four phases in order: Kubeflow Trainer v2.1.0, CRE CRDs, the controller stack (`config/default`), and LogProfiles (`config/logprofiles`). Platform and GitOps teams need a versioned Helm install path with the same components bundled in one release artifact, published alongside the controller image on NGC.

Kubeflow Trainer publishes an official Helm chart at `oci://ghcr.io/kubeflow/charts` (chart name `kubeflow-trainer`, v2.1.0). Internal reference implementation: [NVSentinel](https://github.com/NVIDIA/NVSentinel) — Helm-native chart under `distros/kubernetes/nvsentinel/`, `controller-gen` emits CRDs into chart `crds/`, templates authored for Helm (not converted from kustomize).

## Decision

1. **Single Helm chart** at `helm/cluster-readiness-engine/` (Helm-native, not kustomize→Helm conversion):
   - **Kubeflow Trainer** as an OCI chart dependency (`trainer.enabled` condition).
   - **CRDs** in `helm/cluster-readiness-engine/crds/` — synced from `make manifests` (`controller-gen` → `config/crd/bases` → copy to chart).
   - **Manager ClusterRole** in `helm/cluster-readiness-engine/templates/manager-role.yaml` — copied from `config/rbac/role.yaml` in `make manifests` (same loop as static RBAC).
   - **Static RBAC** — one file per manifest in `helm/cluster-readiness-engine/templates/`, copied from `config/rbac/` via `cp` + `sed` in `make manifests` (kustomize parity: `cre-` name prefix, Helm namespace labels).
   - **Namespace** — no `templates/namespace.yaml`; use `helm --create-namespace`, ArgoCD `CreateNamespace`, or nvcrectl `CreateNamespace=true`.
   - **Controller Deployment, metrics Service, ServiceMonitor** — flat Helm templates (`deployment.yaml`, `metrics-service.yaml`, `metrics-monitor.yaml`).
   - **LogProfiles** — one file per CR in `helm/cluster-readiness-engine/templates/`, copied from `config/logprofiles/` (always installed; not optional).
2. **Dual emit from generators** — `make manifests` updates `config/` and syncs into `helm/cluster-readiness-engine/` (CRDs, RBAC, LogProfiles). Kustomize remains for `make deploy` and `nvcrectl` until Helm is default; then deprecate.
3. **`nvcrectl` Helm path** pulls the chart from NGC OCI at install time (see ADR-065). Legacy YAML manifests remain embedded for the default `--skip-phases` YAML path.
4. **Publish** to `oci://ghcr.io/nvidia/cluster-readiness-engine` on release tags.

## Implementation

### Chart layout

```
helm/cluster-readiness-engine/
├── Chart.yaml              # dep: kubeflow-trainer (OCI)
├── values.yaml
├── crds/                   # synced from make manifests
├── templates/              # flat layout (one resource per file)
│   ├── _helpers.tpl
│   ├── deployment.yaml
│   ├── manager-role.yaml   # rules synced from controller-gen
│   ├── *-role.yaml         # static RBAC synced from config/rbac/
│   └── *.yaml              # LogProfiles synced from config/logprofiles/
```

### Makefile targets

| Target | Action |
|--------|--------|
| `manifests` | `controller-gen` → `config/crd/bases` + `config/rbac/role.yaml`; copies CRDs, RBAC, LogProfiles into chart (`cp` + `sed` in Makefile) |
| `embed-trainer` | `helm template` official Kubeflow chart → `nvcrectl` embed |
| `build-installer` | `kustomize build config/default` → `nvcrectl` embed (legacy) |
| `render-helm` | alias for `manifests` (regenerates all synced chart artifacts) |
| `helm-lint` / `helm-package` | Standard Helm workflow |

### Key values

```yaml
trainer:
  enabled: true
kubeflow-trainer:
  jobset:
    install: true
manager:
  image:
    repository: ghcr.io/nvidia/cluster-readiness-engine/manager
    tag: ""
metrics:
  port: 8443
```

## Rationale

- **Helm-first chart** matches NVSentinel and ArgoCD operator expectations: `helm install` works without conversion scripts.
- **Official Kubeflow chart** as OCI dependency — no vendored third-party YAML or license exceptions.
- **Dual emit** keeps `config/` working while the chart becomes the long-term install surface.

## Consequences

### Positive

- No kubebuilder `helm/v2-alpha` conversion or patch scripts.
- CRD and manager RBAC stay in sync with API changes via `make manifests`.
- Published chart is self-contained after `helm dependency build`.

### Negative

- Controller templates must be maintained in Helm until kustomize is fully deprecated (Deployment/RBAC helpers are not auto-generated except manager role rules).
- `helm dependency build` requires network when packaging; published NGC chart bundles dependencies.
- Kubeflow subchart installs into `kubeflow-system` by default; umbrella release namespace is typically `cluster-readiness-engine`.

## Alternatives Considered

1. **kustomize→Helm conversion** (kubebuilder `helm/v2-alpha`, patch scripts) — rejected after team review; fragile and not NVSentinel-aligned.
2. **Flattened vendored Trainer YAML** — rejected; use OCI dependency.
3. **Helm-only, drop kustomize immediately** — deferred; dual path until `nvcrectl` Helm install is validated behind a feature flag.

## References

- ADR-048: Embedded Trainer Manifests
- [NVSentinel Helm chart](https://github.com/NVIDIA/NVSentinel/tree/main/distros/kubernetes/nvsentinel)
- [Kubeflow Trainer Helm install](https://www.kubeflow.org/docs/components/trainer/operator-guides/installation/#install-with-helm-charts)
