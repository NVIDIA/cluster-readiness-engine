# ADR-015: Feature — Auto-Created GoodputMeasurement from Job Spec

## Context

GoodputMeasurement is a separate CRD from Job (ADR-005). Users who want goodput tracking must create both a Job and a GoodputMeasurement, with the measurement referencing the Job. This two-resource pattern is error-prone — users forget to create the measurement, or create it with a wrong jobRef, or miss setting the logProfileRef.

The Workflow controller already creates Jobs from templates. Adding automatic GoodputMeasurement creation at the Workflow level would couple the Workflow controller to measurement concerns. The measurement configuration belongs with the Job spec, not the Workflow spec.

Options considered:
1. Keep the two-resource pattern (user creates both)
2. Auto-create GoodputMeasurement from Workflow spec
3. Auto-create GoodputMeasurement from Job spec

## Decision

Add an optional `goodputMeasurement` field to JobSpec. When set, the Job controller auto-creates a GoodputMeasurement child resource owned by the Job. The measurement is configured with the specified `logProfileRef`, `sampleInterval`, and `tailLines`.

## Implementation

- **Job API** (`api/v1alpha1/job_types.go`): `GoodputMeasurementSpec` with `logProfileRef` (required), `sampleInterval` (optional, default from controller), and `tailLines` (optional, default from controller).
- **Job controller** (`pkg/controller/job_controller.go`): After creating the workload, checks if `spec.goodputMeasurement` is set. If so:
  1. Constructs a `GoodputMeasurement` with the Job as `spec.jobRef`
  2. Sets `SetControllerReference()` (Job owns the measurement)
  3. Creates via `client.Create()` with AlreadyExists handling
  4. The measurement is garbage-collected when the Job is deleted

The auto-created GoodputMeasurement has the same name as the Job (with `-goodput` suffix) and lives in the same namespace. It is created idempotently — if it already exists (from a previous reconcile), the controller proceeds without error.

Users who need custom measurement configurations can still create GoodputMeasurements manually (the auto-create is opt-in via the Job spec field).

## Rationale

- **Single resource for common case.** Most Job users who want goodput tracking just need to specify the LogProfile. The Job spec is the natural place for this — it's where the workload and health monitoring are configured.
- **Job controller is the right owner.** The Job knows when the workload is created (measurement depends on pod logs). The Job's lifecycle bounds the measurement's lifecycle (owner reference). The Job controller already has the workload reference needed for `spec.jobRef`.
- **Preserves standalone GoodputMeasurement.** The auto-create is additive. Users who need standalone measurements (e.g., measuring an externally created Job) can still create GoodputMeasurements directly.

## Consequences

### Positive
- One resource (Job) instead of two for the common case
- Measurement is correctly configured by construction (jobRef, namespace, ownership)
- Garbage collected with the Job — no orphaned measurements
- Stall detection (ADR-009) works automatically when goodput measurement is configured

### Negative
- Job controller has a dependency on the GoodputMeasurement type (mild coupling)
- Auto-created measurement can't be customized after creation (must be recreated)
- Job spec grows with measurement fields

### Mitigations
- The dependency is one-directional (Job creates measurement, but doesn't watch it for status — the stall detection reads status but doesn't control the measurement lifecycle)
- Measurement configuration fields are minimal (logProfileRef, sampleInterval, tailLines)
- For advanced customization, users can create measurements manually

## Alternatives Considered

### Keep two-resource pattern
**Rejected** because: Error-prone in practice. During development, the most common issue was forgetting to create the GoodputMeasurement or misconfiguring the jobRef. The auto-create eliminates this class of errors.

### Auto-create from Workflow spec
**Rejected** because: The Workflow controller shouldn't know about measurement concerns. Measurement is a per-Job configuration — different Jobs in a certification might use different LogProfiles or not need measurement at all. Putting it in the Job spec keeps the responsibility at the right level.

### Embed goodput computation in Job controller
**Rejected** because: Goodput computation (log parsing, metric calculation) is complex and independent of Job orchestration. The separation between Job controller (orchestration) and GoodputMeasurement controller (measurement) keeps both focused. Auto-creation just removes the manual wiring step.

## Notes

- The auto-created measurement uses the same name as the Job with `-goodput` suffix — this must not exceed the 253-character Kubernetes name limit
- AlreadyExists handling ensures idempotency when the controller is restarted

## References

- `api/v1alpha1/job_types.go` — `GoodputMeasurementSpec` in JobSpec
- `pkg/controller/job_controller.go` — auto-creation logic
