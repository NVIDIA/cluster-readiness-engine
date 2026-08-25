# ADR-063: Auto-Inject Tolerations from `target.taintSelectors`

## Summary

`TargetSpec.TaintSelectors` is documented to "auto-inject matching tolerations into the workload pods,"
but the injection path is dead code: `buildTolerations()` in the Workflow controller exists and is
never called. Today, MPI/launcher workloads (NCCL tests) schedule onto tainted nodes only because the
controller applies a blanket `Operator: Exists` toleration, and non-MPI training workloads
(TrainJob, e.g. nemotron) get no tolerations at all and therefore cannot run on tainted nodes.

**Decision:** wire `buildTolerations()` into `createJobForGroup` and apply the resulting tolerations
to every workload type when `target.taintSelectors` is set. Retain the existing MPI blanket as a
fallback when no `taintSelectors` are declared, so current NCCL deployments do not regress.

## Context

- `TargetSpec.TaintSelectors` (`api/v1alpha1/workflow_types.go:147`) is set on both
  `Certification.spec.target` and `Workflow.spec.orchestration.target`. The Certification target
  flows into the Workflow via `pkg/catalog/loader.go:338`, so the plumbing from cert → workflow
  is already in place.
- `TaintSelectors` is used today **only for filtering** candidate nodes during discovery
  (`pkg/controller/workflow_controller.go:419-422`): a node must carry every listed taint to be
  selected.
- `buildTolerations()` at `pkg/controller/workflow_controller.go:512` constructs the
  corresponding `corev1.Toleration` list (`Equal` when `value` is set, `Exists` otherwise) but is
  never invoked.
- `createJobForGroup` applies a blanket `Operator: Exists` toleration only when the workload has a
  launcher target — i.e. MPI/NCCL (`workflow_controller.go:748-755`). The comment on that block
  explicitly notes "Non-MPI training workloads (e.g., nemotron) must respect node taints for proper
  scheduling," which is a deliberate (but undocumented) restriction that now blocks the use case of
  running training on tainted GPU nodes.
- Both workload adapters' `SetTolerations` methods **append** to existing tolerations
  (`pkg/workload/trainjob.go:144`), so user-declared
  tolerations in `jobTemplate` are preserved either way.

## Decision

In `createJobForGroup` (Workflow controller), the toleration-injection logic becomes:

1. If `workflow.Spec.Orchestration.Target.TaintSelectors` is non-empty
   → build tolerations from those selectors (reusing `buildTolerations`) and call
   `adapter.SetTolerations(&job.Spec.Workload, ...)` for **all** workload types (MPI and training).
2. Otherwise, if the workload has a launcher target (MPI)
   → keep the existing blanket `Operator: Exists` toleration. This preserves today's NCCL behavior
   for users who haven't declared `taintSelectors`.
3. Otherwise
   → no controller-injected tolerations (unchanged behavior for training on untainted nodes).

The precedence is: **explicit `taintSelectors` always win.** When set, even MPI workloads stop
receiving the blanket toleration and get only the tolerations matching the declared taints.

## Rationale

- **Honors the documented contract.** The field comment already promises auto-injection; this fixes
  the broken half of the feature.
- **Unblocks training on tainted nodes** without introducing a new field or YAML surface — users who
  already taint their GPU nodes (e.g., `nvidia.com/gpu=present:NoSchedule`) declare the matching
  selector in `target.taintSelectors` and training pods schedule correctly.
- **Non-breaking for NCCL.** Existing Certifications/Workflows that rely on the MPI blanket
  toleration keep working because the blanket only drops out when `taintSelectors` is explicitly
  set — a clear opt-in.
- **Single source of truth.** Node *filtering* and toleration *injection* are now driven by the same
  list, which matches the field's documented behavior and removes a class of subtle scheduling bugs
  where a node was selected but the pod could not actually land on it.

## Consequences

- Workloads now schedule onto tainted nodes whenever the operator declares the matching taints in
  `target.taintSelectors`. This includes TrainJob and MPI variants alike.
- Users currently relying on the blanket `Exists` toleration for NCCL will see no change unless they
  set `taintSelectors`; if they do, NCCL pods will only tolerate the listed taints — a tightening of
  scheduling that is the intended opt-in behavior.
- The `TaintSelectors` field comment must be updated to describe the precedence rule (declared
  selectors override the MPI fallback).

## Alternatives Considered

1. **Drop the MPI blanket entirely** — require everyone to declare taints via `taintSelectors`.
   Cleaner contract but a breaking change for any existing NCCL deployment that relies on the
   blanket. Rejected as too disruptive for a fix to a documented-but-broken feature.
2. **Add a separate `tolerations` field on TargetSpec** — explicit user-supplied tolerations
   independent of taint selection. Rejected because it duplicates information already in
   `taintSelectors` and breaks the "filter and inject from the same list" invariant.
3. **Apply tolerations from `taintSelectors` on top of the MPI blanket** — i.e. union rather than
   precedence. Rejected because the blanket already tolerates everything, making the union
   indistinguishable from the blanket alone; the value of declaring `taintSelectors` for MPI is to
   *narrow* what the pod will tolerate.

## Implementation

- `pkg/controller/workflow_controller.go` — replace the MPI-only toleration block in
  `createJobForGroup` with the three-branch logic above. `buildTolerations` is already correct and
  reused as-is.
- `pkg/controller/workflow_controller_test.go` — add a unit test covering `buildTolerations`
  (empty value → `Exists`, non-empty value → `Equal`, with/without `effect`).
- `cmd/integration/testdata/reconcile/` — add two cases following the existing golden-file pattern:
  - `taint-selectors-trainjob/` — Workflow with `target.taintSelectors` and a TrainJob; expect
    tolerations on the worker pod-template override.
  - `taint-selectors-mpi-fallback/` — Workflow with no `taintSelectors` and an MPI workload; expect
    the blanket `Operator: Exists` toleration (regression guard).
- `api/v1alpha1/workflow_types.go` — update the doc comment on `TaintSelectors` to describe the
  precedence rule. Re-run `make manifests generate` and `make embed-nvcrectl` to refresh CRDs and the
  embedded copy in `pkg/setup/embedded/`.
- `site/content/docs/` — short note on scheduling training onto tainted nodes via
  `target.taintSelectors`, if a relevant page exists.

## References

- `api/v1alpha1/workflow_types.go:143-165` — `TaintSelectors` field and `TaintSelector` struct.
- `pkg/controller/workflow_controller.go:419-422` — current node-filtering use of
  `TaintSelectors`.
- `pkg/controller/workflow_controller.go:512-530` — `buildTolerations` (dead code today).
- `pkg/controller/workflow_controller.go:748-755` — current MPI-only blanket toleration.
- `pkg/workload/trainjob.go:144-166` — adapter
  `SetTolerations` (append semantics).
- `pkg/catalog/loader.go:338` — `TargetSpec` propagation from Certification to Workflow.
