# ADR-003: Architecture — Strongly-Typed Workload Adapter Pattern

## Context

Burn-in testing is not limited to one training framework. Different teams use different tools — Kubeflow Trainer (TrainJob), Training Operator (PyTorchJob, MPIJob, TFJob, JAXJob), and potentially custom workload types. The Job controller needs to create, inspect, and manage these workloads without being coupled to any specific framework's API.

One approach is `runtime.RawExtension` with `unstructured.Unstructured`, allowing any JSON blob as a workload spec. This has problems: no compile-time type safety, JSON parsing errors at runtime, and no way to normalize status across frameworks (each has different condition types).

Options considered:
1. Keep `runtime.RawExtension` with unstructured access
2. Strongly-typed discriminated union with per-framework adapters
3. Generic workload interface using duck typing

## Decision

Replace `runtime.RawExtension` with a strongly-typed discriminated union (`WorkloadSpec`) and an `Adapter` interface that normalizes each framework's object construction and status model to a common `WorkloadPhase`.

## Implementation

- **WorkloadSpec** (`api/v1alpha1/job_types.go`): Discriminated union with one optional field per framework (`trainJob`, `pyTorchJob`, `mpiJob`, `tfJob`, `jaxJob`). Exactly one field must be set (enforced by CRD validation).
- **Adapter interface** (`pkg/workload/adapter.go`): Eight methods — `GVK()`, `NewObject()`, `Build()`, `InjectPodLabel()`, `SetNodeSelector()`, `SetNodeAffinity()`, `SetTolerations()`, `NodesRequired()`, `GetStatus()`.
- **ForSpec() factory** (`pkg/workload/adapter.go`): Switch on which field is non-nil to select the adapter.
- **Five adapters**: `TrainJobAdapter`, `PyTorchJobAdapter`, `MPIJobAdapter`, `TFJobAdapter`, `JAXJobAdapter`.

Status normalization:
- TrainJob uses `metav1.Condition` — `TrainJobComplete` maps to Succeeded, `TrainJobFailed` maps to Failed
- Kubeflow types use `kubeflowv1.JobCondition` with `corev1.ConditionStatus` — Failed takes precedence over Succeeded when both are present
- All adapters return `WorkloadStatus{Phase, Reason, Message}`

Shared helpers for Kubeflow types live in `pkg/workload/kubeflow_helpers.go`:
- `getStatusFromJobConditions()` — common status extraction
- `injectLabelIntoReplicaSpecs()` — pod label injection across all replica types
- `setNodeSelectorOnReplicaSpecs()` / `setNodeAffinityOnReplicaSpecs()` — scheduling control

The Job controller calls `Owns()` on all supported GVKs for event-driven reconciliation when workload status changes.

## Rationale

- **Compile-time safety.** The previous `runtime.RawExtension` approach meant malformed workload specs were only caught at runtime. Strongly-typed fields catch errors at CRD validation time.
- **Normalized status.** Each framework reports status differently. The adapter layer gives the Job controller a single `WorkloadPhase` to react to, regardless of the underlying framework.
- **Pod label injection.** The controller auto-injects `cre.nvidia.com/job` labels into pod templates via the adapter, eliminating the need for users to manually configure pod labels for health monitoring.
- **Adding a framework requires zero controller changes.** Implement the interface, register in `ForSpec()`, add `Owns()` in `main.go`. The Job controller's reconciliation logic works unchanged.

## Consequences

### Positive
- Adding a new workload type is a self-contained change (~150 lines per adapter)
- CRD validation catches invalid specs before the controller runs
- All orchestration features (health monitoring, goodput, iterations) work uniformly across frameworks
- Event-driven reconciliation via `Owns()` eliminates polling for status changes

### Negative
- CRD schema grows significantly with each adapter (each framework's full type is embedded)
- Adding a new framework requires a Go code change (new adapter file + ForSpec entry)
- The discriminated union pattern is unfamiliar to some Kubernetes users

### Mitigations
- CRD size is a build-time cost, not a runtime cost
- Adapter implementations follow a clear template (copy an existing one, change the type assertions)
- Samples and documentation show usage for each framework

## Alternatives Considered

### Keep runtime.RawExtension
**Rejected** because: No compile-time safety. Status normalization required parsing unstructured JSON with string comparisons. Pod label injection required knowledge of each framework's pod template location without type assistance. Every new feature in the controller required handling unstructured data.

### Duck typing (partial object metadata)
**Rejected** because: Status conditions differ across frameworks (different condition types, different status enums). Duck typing works for metadata but not for status normalization. Building workload objects from a duck-typed spec still requires knowing the full type.

## Notes

- `Trainer.NumProcPerNode` is `*intstr.IntOrString` — use `intstr.FromInt32()`, not `*int32`
- TrainJob uses `PodTemplateOverrides` targeting the "trainer" job name for pod label injection
- The `ptr[T]()` helper is defined in test files for both the workload and controller packages

## References

- `pkg/workload/adapter.go` — interface definition and factory
- `pkg/workload/trainjob.go`, `pytorchjob.go`, `mpijob.go`, `tfjob.go`, `jaxjob.go` — implementations
- `pkg/workload/kubeflow_helpers.go` — shared Kubeflow adapter helpers
