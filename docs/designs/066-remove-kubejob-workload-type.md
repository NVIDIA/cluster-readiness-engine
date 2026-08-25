# ADR-066: Remove the `kubeJob` Workload Type

## Context

ADR-042 added `kubeJob` (native `batch/v1` Job) as a second arm of the `WorkloadSpec`
discriminated union, alongside `trainJob`. It targeted single-node workloads that need no
training-framework runtime — GPU diagnostics, conformance tests, validation suites.

In practice no catalog entry ever adopted it: every entry under `pkg/catalog/entries/`
uses `trainJob`. The unused arm carries ongoing cost:

- It doubles the workload-adapter surface — `KubeJobAdapter` implements every method of the
  ADR-003 `Adapter` interface, with its own tests and golden files.
- It embeds the entire `batchv1.JobSpec` schema into the Job CRD for a path nothing exercises.
- It must be accounted for in every workload-type switch, doc page, and CRD-validation change.

## Decision

Remove `kubeJob` from `WorkloadSpec`, delete `KubeJobAdapter`, and drop the
`Owns(&batchv1.Job{})` watch from the Job controller. `trainJob` becomes the sole workload type.
`ForSpec()` returns an error for any spec without `trainJob`.

## Implementation

- **API**: Remove the `KubeJob *batchv1.JobSpec` field and the `batchv1` import from
  `api/v1alpha1/job_types.go`. The `MinProperties=1`/`MaxProperties=1` markers continue to
  enforce the (now single-arm) union.
- **Adapter**: Delete `pkg/workload/kubejob.go` and the `case spec.KubeJob != nil` arm of
  `ForSpec()` in `pkg/workload/adapter.go`.
- **Controller**: Remove the `case spec.KubeJob != nil` arm of `detectWorkloadKind()` and drop
  `Owns(&batchv1.Job{})` (plus the `batchv1` import) from `job_controller.go`.
- **Regenerate**: deepcopy (`make generate`), CRDs + Helm sync (`make manifests`), and the
  nvcrectl-embedded CRDs (`make embed-nvcrectl`).
- **Tests/docs**: delete all `TestKubeJob*` cases and their testdata, the `job-kubejob`
  integration case, and remove KubeJob references from documentation.

### `batch/v1` handling retained for the TrainJob path

Not all `batch/v1` handling belonged to KubeJob. JobSet — which backs Kubeflow TrainJob —
creates real `batch/v1` Jobs and, on failure, emits a condition message of the form
`"... (first failed job: <name>)"`. `nvcrectl`'s `batchJobFailureReason`
(`pkg/report/report.go`) parses that message and reads the JobSet-created `batch/v1` Job to
surface the root-cause failure reason in reports. This serves TrainJob, not KubeJob, and is
retained, along with the `batchv1` scheme registration in the UAT test helpers.

By contrast, `Owns(&batchv1.Job{})` only ever fired for KubeJobs: JobSet's batch Jobs are owned
by JobSet (not the CRE Job), so the watch never observed them. It is removed.

## Consequences

### Positive
- Smaller Job CRD — the embedded `batchv1.JobSpec` schema is gone.
- A single workload adapter to reason about, test, and document.
- Fewer workload-type switches and doc surfaces to keep in sync.

### Negative
- Breaking `v1alpha1` API change: a `Job` CR with `spec.workload.kubeJob` set fails validation
  after upgrade. Acceptable for an alpha API; no catalog entry or sample produces such CRs.
- Re-introducing a runtime-free single-node workload type later would mean reviving ADR-042's
  approach (or a lighter variant).

## Alternatives Considered

### Keep `kubeJob` unused
Rejected: ongoing schema, adapter, test, and documentation cost with no consumer.

### Replace it with a lighter custom single-node spec
Rejected: no current demand. Revisit if a concrete runtime-free single-node use case appears.

## Notes
- ADR-042 is marked Superseded rather than deleted, preserving the decision trail.

## References
- Supersedes ADR-042 (Native batch/v1 Job Support in the Workload Adapter Pattern).
- ADR-003: Strongly-Typed Workload Adapter Pattern.
- `api/v1alpha1/job_types.go` — `WorkloadSpec` discriminated union.
- `pkg/workload/adapter.go` — `Adapter` interface and `ForSpec()` factory.
