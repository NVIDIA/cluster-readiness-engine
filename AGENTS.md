# cluster-readiness-engine — AI Agent Guide

## Project Overview

Kubebuilder-based Kubernetes controller for GPU cluster burn-in certification. Single binary controller (`cmd/manager/`) and a CLI tool (`cmd/ncrectl/`). Distributed as a Helm chart at `oci://ghcr.io/nvidia/cluster-readiness-engine`.

## Repository Layout

```
cmd/
  manager/main.go       Controller manager entry point
  ncrectl/main.go       CLI entry point (thin — wires cobra commands)

api/v1alpha1/           CRD types and kubebuilder markers
  *_types.go            Schema definitions (+kubebuilder markers)
  zz_generated.*.go     Auto-generated deepcopy — DO NOT EDIT

pkg/
  controller/           Kubernetes reconcilers (6 controllers)
  catalog/              Test-category registry (init()-based)
  certification/        ncrectl certification commands
  cluster/              Cluster info discovery
  render/               Workflow offline/online render
  report/               Report data model and rendering
  setup/                ncrectl setup init/reset/status
  upgrade/              ncrectl self-upgrade
  workloadrun/          ncrectl workloadrun commands
  goodput/              Goodput log parsing
  nccl/                 NCCL log parsing
  orchestration/        Node-partitioning algorithms
  platform/             Platform-specific runtime config
  nodemonitor/          Node failure detectors
  workload/             TrainJob adapter
  gpu/ naming/ podlogs/ podutil/ threshold/

helm/cluster-readiness-engine/
  Chart.yaml            Chart metadata (version injected at release)
  values.yaml           Default values
  crds/                 Generated CRDs — DO NOT EDIT (make manifests)
  templates/            Generated RBAC + authored Helm templates

config/samples/         Example CRs for each resource type
docs/designs/           Architecture Decision Records (ADR-000–ADR-069)
```

## Never Edit (Auto-Generated)

- `helm/cluster-readiness-engine/crds/*.yaml` — from `make manifests`
- `helm/cluster-readiness-engine/templates/role*.yaml` — from `make manifests`
- `helm/cluster-readiness-engine/templates/service_account.yaml` — from `make manifests`
- `**/zz_generated.*.go` — from `make generate`
- `PROJECT` — kubebuilder metadata

Never remove `// +kubebuilder:scaffold:*` markers.

## Key Commands

```bash
make manifests generate   # After editing *_types.go or kubebuilder markers
make test                 # Unit + integration tests
make lint                 # golangci-lint
make lint-fix             # Auto-fix lint issues
make build                # Build bin/manager
make build-ncrectl        # Build bin/ncrectl
make helm-package         # Package Helm chart
make helm-push            # Push Helm chart to oci://ghcr.io/nvidia
make deploy IMG=<img>     # Deploy via Helm (wraps helm-deploy)
make undeploy             # Remove via Helm (wraps helm-uninstall)
make install              # Apply CRDs only
make uninstall            # Remove CRDs only
```

## Feature Development Workflow

Every feature follows this mandatory sequence:

1. **Plan** — Explore codebase, design approach, get user approval (`EnterPlanMode`)
2. **ADR** — Write `docs/designs/NNN-short-description.md` (Context → Decision → Implementation → Rationale → Consequences → Alternatives → References), get user approval
3. **Implement** — Follow existing patterns; `setExclusiveCondition()`, `Owns()` watches, reason constants with tier prefix
4. **Update** — Integration tests (`cmd/integration/testdata/`), golden files, docs, samples
5. **Verify** — `make manifests generate && make lint-fix && make build && make test` — all must pass

## Critical Pitfalls

- **After `*_types.go` changes**: run `make manifests generate` before building
- **`internal/catalog`** must be blank-imported in `cmd/manager/main.go`, `cmd/ncrectl/main.go`, and integration test suites
- **envtest has no GC**: cascade deletion via OwnerReference won't work in tests
- **Golden files**: never regenerate blindly — understand what changed first, then ask user for permission
- **Test timeout is 10s** — requeue intervals > 10s will break status-update tests
- **`Trainer.NumProcPerNode`** is `*intstr.IntOrString` — use `intstr.FromInt32()`, not `*int32`

## Testing Catalog Changes with ncrectl

```bash
go build -ldflags "-s -w" -o bin/ncrectl ./cmd/ncrectl/
bin/ncrectl certification render --platform aws /tmp/cert.yaml
```

Verify: `ncrectl.nvidia.com/applied-overrides` annotation, architecture-specific env vars (EFA only for GB200/H100 on AWS, not GB300).

## References

- Architecture Decision Records: `docs/designs/`
- Controller patterns: `pkg/controller/`
- CRD hierarchy: Certification → Workflow → Job
- Kubebuilder Book: https://book.kubebuilder.io
