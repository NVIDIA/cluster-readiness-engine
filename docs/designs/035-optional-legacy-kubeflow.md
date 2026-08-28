# ADR-035: Optional Legacy Kubeflow Training Operator Support

## Context

NVCRE supports five training workload types via the adapter pattern (ADR-003): TrainJob (Kubeflow Trainer v2), PyTorchJob, MPIJob, TFJob, and JAXJob (Kubeflow Training Operator v1). The Job controller's `SetupWithManager()` registers `Owns()` watches on all five types. Controller-runtime requires the corresponding CRDs to exist in the cluster at startup — if any CRD is missing, the controller crashes with "no matches for kind".

This creates a hard dependency: operators must install **both** the Kubeflow Trainer v2 CRDs and the Training Operator v1 CRDs even if they only use TrainJob. All built-in catalog entries (communication, diagnostics, stress, training) have been migrated to TrainJob, making the v1 CRDs an installation burden with no production benefit for most deployments.

### Requirements

1. **Default to v2 only**: Out-of-the-box, the controller must start without Training Operator v1 CRDs installed.
2. **Opt-in v1 support**: Operators who still use PyTorchJob/MPIJob/TFJob/JAXJob can enable support via a flag.
3. **Clear failure**: If a Job references a v1 workload type while the feature is disabled, the Job must fail immediately with an actionable error message.
4. **No adapter removal**: The v1 adapter code stays compiled in — this is a runtime gate, not a code deletion.
5. **Backward-compatible tests**: All existing integration tests must pass without modification.

## Decision

Add a `--enable-legacy-kubeflow` CLI flag (default: `false`) that controls:

1. Whether `Owns()` watches for PyTorchJob, MPIJob, TFJob, and JAXJob are registered in `SetupWithManager()`.
2. Whether Jobs with v1 workload specs are allowed to proceed or are immediately failed.

Scheme registration (`kubeflowv1.AddToScheme`) remains unconditional — Go types must be registered for CRD field validation, deep copy, and serialization regardless of whether watches are active.

## Implementation

### 1. CLI Flag

Add to `cmd/manager/main.go` following the same `cobra` flag-registration pattern used by the controller's other CLI flags (e.g. `--enable-http2`, `--leader-elect`):

```go
var enableLegacyKubeflow bool

cmd.Flags().BoolVar(&enableLegacyKubeflow, "enable-legacy-kubeflow", false,
    "Enable support for legacy Kubeflow Training Operator v1 workloads "+
        "(PyTorchJob, MPIJob, TFJob, JAXJob). Requires the Training Operator CRDs "+
        "to be installed. When disabled (default), only TrainJob is supported.")
```

Pass to `JobReconciler` via a new `EnableLegacyKubeflow bool` field.

### 2. Conditional Watches

In `JobReconciler.SetupWithManager()`, split the builder chain:

```go
b := ctrl.NewControllerManagedBy(mgr).
    For(&crev1alpha1.Job{}).
    Owns(&trainerv1alpha1.TrainJob{}).
    Owns(&crev1alpha1.GoodputMeasurement{})

if r.EnableLegacyKubeflow {
    b = b.Owns(&kubeflowv1.PyTorchJob{}).
        Owns(&kubeflowv1.MPIJob{}).
        Owns(&kubeflowv1.TFJob{}).
        Owns(&kubeflowv1.JAXJob{})
}

return b.Watches(
    &corev1.Node{},
    handler.EnqueueRequestsFromMapFunc(r.nodeToJobRequests),
    builder.WithPredicates(r.nodeHealthChangePredicate()),
).Named("job").Complete(r)
```

When `EnableLegacyKubeflow` is false, controller-runtime never attempts to list/watch v1 CRDs, so the controller starts without them.

### 3. Reconcile-Time Gate

At the start of `createWorkloadFromSpec()`, before calling `workload.ForSpec()`:

```go
if !r.EnableLegacyKubeflow && isLegacyKubeflowSpec(&job.Spec.Workload) {
    msg := "Legacy Kubeflow Training Operator workloads (PyTorchJob, MPIJob, TFJob, JAXJob) " +
        "are disabled. Set --enable-legacy-kubeflow=true to enable them, or migrate to TrainJob."
    r.setJobFailed(ctx, job, reasonLegacyKubeflowDisabled, msg)
    return ctrl.Result{}, nil
}
```

The `isLegacyKubeflowSpec()` helper checks `spec.PyTorchJob != nil || spec.MPIJob != nil || spec.TFJob != nil || spec.JAXJob != nil`.

Returning `ctrl.Result{}, nil` (not an error) is intentional: this is a terminal policy failure, not a transient error. Returning an error would trigger exponential backoff retries for a condition that cannot change at runtime.

### 4. Integration Testing

Extend the test framework's `waitConfig` with a `controllerConfig` struct:

```go
type controllerConfig struct {
    EnableLegacyKubeflow *bool `json:"enableLegacyKubeflow,omitempty"`
}
```

When omitted (nil), the test defaults to `EnableLegacyKubeflow=true` for backward compatibility with all existing v1 tests.

New test case `job-legacy-kubeflow-disabled`: a Job with a PyTorchJob spec and `controllerConfig.enableLegacyKubeflow: false` — expects the Job to reach the `Failed` condition with reason `LegacyKubeflowDisabled`.

## Rationale

- **CLI flag over env var**: Consistent with the controller's other CLI flags (`--enable-http2`, `--leader-elect`, `--metrics-bind-address`). CLI flags are self-documenting via `--help` and can still be set via env vars in Deployment manifests.
- **Default disabled**: All catalog entries use TrainJob. New installations should not require Training Operator v1 CRDs.
- **Fail-fast at reconcile time**: Rather than silently ignoring v1 specs, an explicit failure with an actionable message helps operators understand what to do.
- **Scheme registration unchanged**: Removing `kubeflowv1.AddToScheme()` would break CRD field validation for the `WorkloadSpec` type which still declares v1 fields. The Go types must be registered even if no watches are active.
- **No adapter code removal**: The adapters remain for operators who enable the flag. This is a minimal, low-risk change that doesn't refactor the adapter layer.

## Consequences

### Positive

- Operators no longer need to install Training Operator v1 CRDs for standard deployments.
- Reduces installation complexity and potential CRD version conflicts.
- Clear migration path for operators still using v1 types.

### Negative

- The `kubeflowv1` Go dependency remains in `go.mod` (scheme registration + compiled adapters). A future ADR could address full removal if v1 support is eventually deprecated.
- Operators who restart the controller with the flag disabled while v1 Jobs are in-flight will see those Jobs error-loop. The mitigation is to delete v1 Jobs before disabling the flag.

## Alternatives Considered

1. **Remove v1 support entirely**: Too disruptive — some operators may still use v1 types directly via the Job CRD. The flag provides a migration period.
2. **Runtime CRD discovery**: Check at startup whether v1 CRDs exist and auto-enable. Adds complexity and makes behavior non-deterministic (CRD installed after controller start wouldn't be detected).
3. **Environment variable**: Would work but breaks the established CLI flag pattern used by all other controller configuration.
