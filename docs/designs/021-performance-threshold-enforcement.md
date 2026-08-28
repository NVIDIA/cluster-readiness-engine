# ADR-021: Performance Threshold Enforcement (Pass/Fail Criteria)

## Context

The nvcre measures bandwidth (`BandwidthMeasurement`) and goodput (`GoodputMeasurement`) for every Job that configures them, but has no mechanism to enforce minimum performance thresholds. Measurements are recorded, conditions are set (`Measuring` -> `Complete`), and results are stored in status — but the controller never compares those results against acceptable minimums. Operators must manually inspect `BandwidthMeasurement.status.results` and `GoodputMeasurement.status.result` to determine whether hardware meets performance requirements.

This manual inspection does not scale. A certification run across a 10,000-GPU cluster with topology-aware orchestration produces hundreds of Jobs, each with its own measurement resources. Without automated pass/fail evaluation, the entire certification pipeline stops at "results collected" and requires human judgment to proceed.

Industry references establish clear expectations for automated threshold enforcement:

- **Together AI** uses an acceptance threshold of "around 92% of theoretical maximum" (~370 GB/s on a 400 GB/s fabric) for NCCL all-reduce bandwidth, applied automatically during Phase 3 Network Validation.
- **ClusterMAX 2.0** requires NCCL tests to run "at full bandwidth" and flags nodes that fall below expected performance, with automated quarantine of underperforming hardware.

The controller already has a pattern for orthogonal quality signals: `HardwareFailed` is set independently of `InProgress`/`Succeeded`/`Failed` and propagates up through the Workflow and Certification tiers. Performance threshold violations need the same treatment — the workload succeeded (it ran to completion), but the measured performance is below acceptable levels.

## Decision

Add a `ThresholdSpec` struct with `minBusBandwidthGBps` and `minGoodputRatio` fields, configured on the existing `PerformanceValidationSpec` at the Workflow level. The Workflow controller propagates thresholds to each Job's measurement configs during `createJobForGroup()`. The Job controller evaluates thresholds after workload success and sets a `ValidationFailed` condition — independent of execution state, following the same pattern as `HardwareFailed`. The Workflow controller aggregates `ValidationFailed` from Jobs, and the existing Certification propagation handles the rest.

The feature starts disabled: catalog entries do not set thresholds initially, and code comments show how to enable them. This allows the API and controller logic to land without changing default behavior.

## Implementation

### API Changes

**`ThresholdSpec` on `PerformanceValidationSpec`** (`api/v1alpha1/workflow_types.go`):

```go
type PerformanceValidationSpec struct {
    Enabled    bool                   `json:"enabled"`
    Plugin     string                 `json:"plugin,omitempty"`
    Config     *apiextensionsv1.JSON  `json:"config,omitempty"`
    Tracking   TrackingSpec           `json:"tracking,omitempty"`
    Thresholds *ThresholdSpec         `json:"thresholds,omitempty"` // NEW
}

type ThresholdSpec struct {
    // Minimum acceptable bus bandwidth in GB/s. Compared against max(BusBW)
    // across all message sizes in the Job's BandwidthMeasurement results.
    // Example: "370" for 370 GB/s (92% of a 400 GB/s fabric).
    MinBusBandwidthGBps *string `json:"minBusBandwidthGBps,omitempty"`

    // Minimum acceptable goodput ratio (0.0 to 1.0). Compared against the
    // measured goodput ratio in the Job's GoodputMeasurement status.result.
    // Example: "0.85" for 85% goodput.
    MinGoodputRatio *string `json:"minGoodputRatio,omitempty"`
}
```

**Threshold fields on measurement configs** (`api/v1alpha1/job_types.go`):

```go
type BandwidthMeasurementConfig struct {
    LogProfileRef        string           `json:"logProfileRef"`
    SampleInterval       *metav1.Duration `json:"sampleInterval,omitempty"`
    TestType             string           `json:"testType,omitempty"`
    MinBusBandwidthGBps  *string          `json:"minBusBandwidthGBps,omitempty"` // NEW — set by Workflow controller
}

type GoodputMeasurementConfig struct {
    LogProfileRef    string           `json:"logProfileRef"`
    SampleInterval   *metav1.Duration `json:"sampleInterval,omitempty"`
    MinGoodputRatio  *string          `json:"minGoodputRatio,omitempty"`         // NEW — set by Workflow controller
}
```

These threshold fields on the measurement configs are "internal" — set by the Workflow controller during Job creation, not by users directly. Users configure thresholds on `PerformanceValidationSpec` (at the Workflow level), and the Workflow controller copies them down.

**`ValidationFailed` condition constants** at each tier:

```go
// Job tier (api/v1alpha1/job_types.go)
JobValidationFailed string = "ValidationFailed"

// Workflow tier (api/v1alpha1/workflow_types.go) — already exists
WorkflowValidationFailed = "ValidationFailed"

// Certification tier (api/v1alpha1/certification_types.go)
CertificationValidationFailed = "ValidationFailed"
```

### Workflow Controller: Threshold Propagation

In `createJobForGroup()`, after deep-copying `workflow.Spec.JobTemplate.Spec`, propagate thresholds from `PerformanceValidationSpec` to the Job's measurement configs:

```go
// Propagate performance thresholds to measurement configs
if workflow.Spec.Validation != nil &&
    workflow.Spec.Validation.Performance != nil &&
    workflow.Spec.Validation.Performance.Thresholds != nil {

    thresholds := workflow.Spec.Validation.Performance.Thresholds

    if thresholds.MinBusBandwidthGBps != nil && specCopy.BandwidthMeasurement != nil {
        specCopy.BandwidthMeasurement.MinBusBandwidthGBps = thresholds.MinBusBandwidthGBps
    }
    if thresholds.MinGoodputRatio != nil && specCopy.GoodputMeasurement != nil {
        specCopy.GoodputMeasurement.MinGoodputRatio = thresholds.MinGoodputRatio
    }
}
```

This runs after `DeepCopy()` and before `job := &nvcrev1alpha1.Job{...}`, so the threshold fields are part of the Job spec from creation.

### Workflow Controller: ValidationFailed Aggregation

In the group evaluation logic and `setFinalStatus()`, aggregate `ValidationFailed` from child Jobs:

```go
// In group evaluation (after checking Job Succeeded/Failed/HardwareFailed):
validationCond := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobValidationFailed)
if validationCond != nil && validationCond.Status == metav1.ConditionTrue {
    // Mark group as Failed with reason JobValidationFailed
}
```

A new reason constant `reasonJobValidationFailed = "JobValidationFailed"` is added alongside the existing `reasonJobHardwareFailed`.

In `setFinalStatus()`, if any group failed due to validation, the Workflow is marked Failed with reason `JobValidationFailed`. The existing `WorkflowValidationFailed` condition is also set independently (like `HardwareFailed`), providing a specific quality signal that the Certification tier can inspect.

### Job Controller: Threshold Evaluation

After the workload reaches `Succeeded`, the Job controller calls `checkPerformanceThresholds()`:

```go
func (r *JobReconciler) checkPerformanceThresholds(ctx context.Context, job *nvcrev1alpha1.Job) error {
    // Check bandwidth threshold
    if job.Spec.BandwidthMeasurement != nil && job.Spec.BandwidthMeasurement.MinBusBandwidthGBps != nil {
        if err := r.checkBandwidthThreshold(ctx, job); err != nil {
            return err
        }
    }

    // Check goodput threshold
    if job.Spec.GoodputMeasurement != nil && job.Spec.GoodputMeasurement.MinGoodputRatio != nil {
        if err := r.checkGoodputThreshold(ctx, job); err != nil {
            return err
        }
    }
    return nil
}
```

**Bandwidth evaluation** (`checkBandwidthThreshold`):

1. List `BandwidthMeasurement` resources owned by this Job (via owner reference).
2. If no measurement found or measurement has no results, **fail safe**: set `ValidationFailed` with reason `MeasurementNotFound`.
3. Find `max(BusBW)` across all message sizes in `status.results[]`.
4. Parse the threshold and measured value as floats. If `maxBusBW < minBusBandwidthGBps`, set `ValidationFailed`.

**Goodput evaluation** (`checkGoodputThreshold`):

1. List `GoodputMeasurement` resources owned by this Job.
2. If no measurement found or `status.result` is empty, **fail safe**: set `ValidationFailed` with reason `MeasurementNotFound`.
3. Parse `status.result` and the threshold as floats. If `result < minGoodputRatio`, set `ValidationFailed`.

**`setJobValidationFailed()`** mirrors `setJobHardwareFailed()`:

```go
func (r *JobReconciler) setJobValidationFailed(ctx context.Context, job *nvcrev1alpha1.Job, reason, message string) error {
    changed := meta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
        Type:               nvcrev1alpha1.JobValidationFailed,
        Status:             metav1.ConditionTrue,
        Reason:             reason,
        Message:            message,
        ObservedGeneration: job.Generation,
    })
    if changed {
        if err := r.Status().Update(ctx, job); err != nil {
            return fmt.Errorf("failed to update status: %w", err)
        }
    }
    return nil
}
```

The `ValidationFailed` condition is independent of `InProgress`/`Succeeded`/`Failed` — it is not part of the exclusive set. A Job can be both `Succeeded=True` and `ValidationFailed=True`, meaning the workload ran to completion but measured performance was below the threshold.

### Reason Constants

Following the existing tier-specific prefix convention:

| Tier | Constant | Value | Meaning |
|------|----------|-------|---------|
| Job | `reasonValidationFailed` | `"ValidationFailed"` | Threshold violated |
| Job | `reasonMeasurementNotFound` | `"MeasurementNotFound"` | Measurement missing (fail-safe) |
| Workflow | `reasonJobValidationFailed` | `"JobValidationFailed"` | Job had validation failure |
| Certification | `reasonWorkflowValidationFailed` | `"WorkflowValidationFailed"` | Workflow had validation failure |

### Fail-Safe Behavior

If a threshold is configured but the corresponding measurement resource is not found (or has no results), the Job controller sets `ValidationFailed` with reason `MeasurementNotFound`. This prevents a misconfigured Job from silently passing certification. The message includes which measurement type was expected, making the configuration error easy to diagnose.

### Catalog Integration

The feature starts disabled. Catalog entries that produce `BandwidthMeasurement` or `GoodputMeasurement` results do not set thresholds. Code comments in the catalog files show how to enable thresholds:

```go
// To enforce minimum bandwidth thresholds, add to the Workflow's
// PerformanceValidationSpec:
//   Thresholds: &nvcrev1alpha1.ThresholdSpec{
//       MinBusBandwidthGBps: ptr("370"), // 92% of 400 GB/s fabric
//   },
```

Operators can also set thresholds directly on Workflow resources without catalog changes.

## Rationale

- **Separate condition (not overloading Failed)**: The workload DID succeed — it ran to completion and produced results. A threshold violation is a quality signal, not an execution failure. Overloading `Failed` would conflate "workload crashed" with "workload ran fine but hardware is slow," making triage impossible. The `HardwareFailed` pattern already established this separation for hardware health signals.

- **`PerformanceValidationSpec` as single source of truth**: Users configure thresholds in one place (`spec.validation.performance.thresholds`), at the Workflow level. The Workflow controller copies them into each Job's measurement configs during creation. This keeps the user-facing API clean — users do not need to understand the internal measurement config structure.

- **Job-level evaluation**: Threshold checking happens in the Job controller because measurements are Job children. The Job controller already owns the measurement lifecycle (creating `BandwidthMeasurement` and `GoodputMeasurement` resources). Evaluating thresholds at the same tier preserves the strict tier separation that defines the controller architecture (ADR-002).

- **Propagation via measurement config fields**: The Workflow controller copies threshold values from `PerformanceValidationSpec` into the Job's `BandwidthMeasurementConfig` and `GoodputMeasurementConfig` during `createJobForGroup()`. This means the Job controller reads thresholds from its own spec — it never reaches up to the parent Workflow. This preserves the Deployment -> ReplicaSet -> Pod composition pattern where each tier is self-contained.

- **`max(BusBW)` for bandwidth evaluation**: NCCL test suites sweep across multiple message sizes (1B to 8GB). The maximum bus bandwidth — typically observed at large message sizes — is the relevant figure for training performance. Small message sizes are bandwidth-limited by latency and are not representative of training communication patterns.

- **Fail-safe on missing measurements**: If a threshold is configured but no measurement exists, the safe default is to fail. Silently passing would undermine the purpose of threshold enforcement. The `MeasurementNotFound` reason makes the root cause immediately clear.

## Consequences

### Positive

- Automated pass/fail for certifications — no manual inspection needed at any scale
- Per-group threshold evaluation catches slow topology domains that would be hidden in cluster-wide averages
- Cleanly composable with existing `HardwareFailed` and retry mechanisms — a Job can be `Succeeded + HardwareFailed + ValidationFailed` simultaneously, each signal independent
- Thresholds are optional and default to disabled — zero behavioral change for existing deployments
- Fail-safe behavior prevents misconfigured pipelines from silently passing

### Negative

- Threshold fields on measurement configs (`MinBusBandwidthGBps`, `MinGoodputRatio`) are "internal" — set by the Workflow controller, not users. This is a leaky abstraction: the fields are visible in the Job spec but meaningless to set directly. Documentation must clarify this.
- Thresholds are static per-Workflow — no support for per-GPU-architecture thresholds within a single Workflow. Operators targeting mixed hardware must create separate Workflows or use catalog overrides (ADR-012).
- String-based threshold values (`*string`) require float parsing at evaluation time. This avoids CRD validation complexity with `resource.Quantity` but means invalid values (e.g., "abc") are caught at runtime, not admission.

## Alternatives Considered

### 1. Overload Job Failed condition

Set `Failed=True` when thresholds are violated, using a distinct reason like `ValidationThresholdViolated`. Rejected because this conflates workload execution failure with performance quality. The Workflow controller treats `Failed` Jobs as execution failures and may trigger retries or escalation paths that are inappropriate for a performance issue. A separate condition keeps the signals orthogonal.

### 2. Workflow-level threshold evaluation

Have the Workflow controller read measurement results from child Jobs' measurement resources and evaluate thresholds directly. Rejected because it breaks tier separation: the Workflow controller would need to understand `BandwidthMeasurement` and `GoodputMeasurement` types, know how to find them, and contain evaluation logic that belongs at the Job tier. The existing pattern is that each tier evaluates its own children — the Job controller evaluates measurements, the Workflow controller evaluates Jobs.

### 3. Thresholds only on measurement configs (no PerformanceValidationSpec)

Let users set `minBusBandwidthGBps` directly on `BandwidthMeasurementConfig` in the Job template. Rejected because this scatters threshold configuration across multiple fields in the Job template spec, making it harder to discover and configure. `PerformanceValidationSpec` provides a single, discoverable location for all performance validation settings. The Workflow controller handles the propagation.

### 4. Dedicated ThresholdEvaluation CRD

Create a new CRD that references a Job and its measurements, with thresholds as spec fields. Rejected as over-engineering: the evaluation is a simple comparison that runs once after workload success. It does not need its own reconciliation loop, status, or lifecycle. A condition on the existing Job resource is sufficient.

## References

- ADR-002 — layered CRD hierarchy (tier separation pattern)
- ADR-004 — CEL node health monitoring (`HardwareFailed` condition pattern)
- ADR-005 — LogProfile and GoodputMeasurement
- ADR-017 — NCCL bandwidth measurement (`BandwidthMeasurement` CRD)
- [ClusterMAX 2.0](https://newsletter.semianalysis.com/p/clustermax-20-the-industry-standard) — NCCL tests at full bandwidth
- [Together AI](https://www.together.ai/blog/a-practitioners-guide-to-testing-and-running-large-gpu-clusters-for-training-generative-ai-models) — ~92% of theoretical maximum acceptance threshold
