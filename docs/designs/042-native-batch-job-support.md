# ADR-035: Native batch/v1 Job Support in the Workload Adapter Pattern

> **Status: Superseded by [ADR-066](066-remove-kubejob-workload-type.md).** The `kubeJob`
> workload type described below was removed — no catalog entry ever adopted it. This document is
> retained as a record of the original decision.

## Context

CRE's `WorkloadSpec` is a discriminated union of five training framework types (TrainJob, PyTorchJob, MPIJob, TFJob, JAXJob). All catalog entries must use one of these types. This forces simple single-node workloads — such as GPU diagnostics, conformance tests, and validation suites — to be wrapped in a TrainJob with a TrainingRuntime dependency, adding unnecessary CRD overhead and authoring friction.

Operators running validation as plain Kubernetes `batch/v1` Jobs (e.g., custom GPU diagnostics or conformance tests) cannot plug into the catalog today without rewriting their workloads as training framework wrappers.

`batch/v1 Job` is a core Kubernetes primitive available on every cluster. It has a direct pod template (`spec.template`), well-defined completion semantics (`Complete`/`Failed` conditions), and requires no CRD installation.

## Decision

Add a `kubeJob` field to `WorkloadSpec` backed by `batchv1.JobSpec`, and implement a `KubeJobAdapter` that satisfies the existing eight-method `Adapter` interface (ADR-003). batch/v1 Jobs in cluster-readiness-engine are always single-node workloads.

## Implementation

- **API**: Add `KubeJob *batchv1.JobSpec` to `WorkloadSpec` in `api/v1alpha1/job_types.go`. The existing `MinProperties=1` / `MaxProperties=1` markers enforce the discriminated union.
- **Adapter**: `KubeJobAdapter` in `pkg/workload/kubejob.go` implementing all eight `Adapter` methods. Direct pod template access makes this simpler than Kubeflow adapters (no ReplicaSpecs iteration).
- **Factory**: Add `case spec.KubeJob != nil` to `ForSpec()` in `pkg/workload/adapter.go`.
- **Controller**: Add `Owns(&batchv1.Job{})` to `SetupWithManager()` in `pkg/controller/job_controller.go` for event-driven reconciliation.

Status normalization:
- `batchv1.JobFailed` with `ConditionTrue` maps to `WorkloadFailed` (checked first — failure takes precedence)
- `batchv1.JobComplete` with `ConditionTrue` maps to `WorkloadSucceeded`
- No matching condition maps to `WorkloadRunning`

Key constraints:
- `NodesRequired()` always returns 1 — batch/v1 Jobs are single-node in cluster-readiness-engine's model
- `SetNumNodes()` is a no-op — bisection is not meaningful for single-node workloads
- Checkpoint restart is not applicable — `shouldRestart()` in the Job controller already checks `job.Spec.Checkpoint == nil` first

Catalog authors bring their own RBAC, ConfigMaps, and other prerequisites as workflow dependencies, exactly as they do today for TrainingRuntime resources.

## Rationale

- **Eliminates CRD overhead.** Single-node workloads no longer need a TrainingRuntime dependency to run a simple container.
- **Standard Kubernetes primitive.** `batch/v1 Job` is native to every cluster — no operator installation required.
- **Follows established pattern.** The implementation is a straightforward extension of the adapter pattern defined in ADR-003. Zero changes to the Job controller's reconciliation logic.
- **Enables external integration.** Operators with existing validation Jobs (custom diagnostics, conformance suites) can plug into the catalog by authoring a YAML template that references `kubeJob` instead of `trainJob`.

## Consequences

### Positive
- Single-node catalog entries are simpler to author (no TrainingRuntime boilerplate)
- External validation tools can integrate without rewriting their workloads
- All existing orchestration features (health monitoring, iterations, orchestration groups) work unchanged
- No new CRD installation requirements — batch/v1 is built into Kubernetes

### Negative
- CRD schema grows (embeds `batchv1.JobSpec` — same trade-off as existing adapters)
- `batch/v1 Job` shares the kind name "Job" with cluster-readiness-engine's CRD (mitigated by GVK differentiation)

## Alternatives Considered

### Continue using TrainJob wrappers for single-node workloads
Rejected: requires a TrainingRuntime CRD and dependency for what is fundamentally `docker run`. Adds authoring friction for no benefit.

### Custom lightweight spec (e.g., `SimpleJobSpec`)
Rejected: `batch/v1 Job` already is the lightweight spec. Inventing a new type adds no value and breaks compatibility with existing tooling.

### `runtime.RawExtension` for arbitrary workloads
Rejected: loses compile-time type safety, CRD validation, and status normalization. ADR-003 explicitly moved away from this approach.

## Notes
- Field named `kubeJob` (not `job`) to avoid confusion with cluster-readiness-engine's `Job` CRD in code and YAML
- `batchv1` is already registered in the scheme via `clientgoscheme.AddToScheme()` — no additional scheme registration needed
- Existing wildcard RBAC marker (`groups="*",resources="*"`) covers batch/v1 permissions

## References
- ADR-003: Strongly-Typed Workload Adapter Pattern
- `pkg/workload/adapter.go` — Adapter interface definition
- `api/v1alpha1/job_types.go` — WorkloadSpec discriminated union
