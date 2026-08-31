# ADR-072: Freeze GoodputMeasurement Status at Job Terminal State

> **Status:** Accepted

## Context

The goodput reported for a completed certification changes between report reads (issue #177). `nvcrectl certification report` reads `GoodputMeasurement.status.result` live from the cluster on every invocation ([report.go:758](../../pkg/report/report.go#L758), `buildDomainReports`), and GoodputMeasurements are deliberately owned by the Workflow and preserved until teardown so the report can read them after Jobs are gone ([workflow_controller.go:1710-1713](../../pkg/controller/workflow_controller.go#L1710)). Nothing snapshots the measurement when the measured Job reaches a terminal condition, so any post-success status churn shows up as report-to-report drift.

The terminal path in [goodputmeasurement_controller.go](../../pkg/controller/goodputmeasurement_controller.go) has three distinct sources of that churn:

1. **Wall-clock training time.** `handleSucceeded` computes `t_w = time.Since(cm.TrainingStartTime)` at *reconcile* time, not at the Job's terminal time; `handleFailed` falls back to `time.Now()` when no log-derived termination time exists. `CalculateGoodput` ([calculator.go](../../pkg/goodput/calculator.go)) is a pure function — the nondeterminism is entirely in the `t_w` fed to it. Every pass through the terminal path produces a different `trainingTimeSec` and therefore a different `result`.

2. **Two-write terminal sequence.** On `JobSucceeded`/`JobFailed`, `finalSample` first runs the full `handleRunning` path (a last log read, then `Status().Update`) to capture the trailing sample window, and `handleSucceeded`/`handleFailed` then writes a second time via `setComplete` with a later "now". A report read between the two writes sees a different goodput than one after. The first write also re-asserts `Measuring=True/"JobRunning"` on a Job that has already succeeded.

3. **Re-entry is not idempotent.** The controller watches only GoodputMeasurement (`SetupWithManager` has no Job watch), so every status write triggers another reconcile. If the cached measurement does not yet show `Complete=True` — stale informer cache, or a manager restart between Job success and the `Complete` write landing — the whole terminal path runs again with a fresh wall clock. `finalSample` clears its own sampling throttle, so each re-entry re-reads logs and rewrites `result`. Worse, `handleSucceeded` recomputes `t_w` only when in-memory `JobState` survived (`r.getState(key)`), so a restarted controller takes a different code path than a long-lived one and lands on a different value. A restart hours after Job success recomputes `time.Since(TrainingStartTime)` across those hours.

The drift is visible beyond the report: `collectJobMeasuredValues` ([job_threshold_helpers.go:64](../../pkg/controller/job_threshold_helpers.go#L64)) reads `status.result` live for `goodputRatio` threshold evaluation, so a Job's pass/fail can depend on *when* the Job controller happens to evaluate.

The existing `Complete=True` gate at [goodputmeasurement_controller.go:122](../../pkg/controller/goodputmeasurement_controller.go#L122) already stops reconciliation once the condition lands — the problem is everything that happens (and can happen again) before it lands, and that the values written are not a deterministic function of their inputs.

## Decision

1. **Anchor all terminal-time computation to the measured Job's terminal condition timestamp.** Define `anchor = LastTransitionTime` of the Job's `Succeeded` (or `Failed`) condition. Terminal `t_w = anchor - TrainingStartTime`. `handleFailed`'s termination-time fallback chain ends at `anchor` instead of `time.Now()`. `status.completionTime` is set to `anchor`, not `metav1.Now()`. The terminal status payload becomes a pure function of (persisted GoodputMeasurement status, Job object, final log parse) — the same inputs always produce the same bytes.

2. **Collapse the terminal path into a single atomic status write.** The final log sample is parsed in memory and folded into the same `setComplete` write that records the terminal metrics, `completionTime`, `Complete=True`, and `Measuring=False`. No intermediate `Status().Update` from the final-sample path, and no transient `Measuring=True` after Job success. Readers can never observe a half-final state: before the write the values are explicitly provisional (`Measuring=True`), after it they are final.

3. **`Complete=True` is the freeze marker and the reconcile gate.** No new `final` field or marker is added — the existing condition already carries exactly this meaning, the gate already exists, and after `Complete` the controller returns without a requeue, so reconciliation stops naturally. **No CRD status addition is needed.**

4. **Deterministic handling of in-flight parsing at the boundary.** The final log read remains best-effort: on failure, the terminal write proceeds from the last persisted values (today's behavior, minus the second write). `ApplicationStopTime` and the log read window are capped at `anchor` so late-arriving log timestamps cannot push terminal metrics past the Job's terminal time. Because re-entry recomputes from the same anchored inputs, a duplicate terminal write (stale cache, conflict retry, controller restart) is byte-identical rather than a new value.

5. **Threshold evaluation consumes only frozen values.** `collectJobMeasuredValues` skips goodput-derived keys (`goodputRatio`, `avgTFLOPsPerGPU`, `avgStepTimeSec`) while the GoodputMeasurement's `Complete` condition is not `True`. The existing missing-key requeue and `measurementTimeout` machinery in `checkPerformanceThresholds` already handles the wait.

6. **Do not copy final values into Job/Workflow/Certification status.** The report aggregates GoodputMeasurements directly, and they already outlive their Jobs by design.

## Implementation

- **`pkg/controller/goodputmeasurement_controller.go`**
  - New `terminalAnchor(job) time.Time` helper returning the `LastTransitionTime` of the Job's terminal condition (with a `time.Now()` last-resort fallback for a condition that somehow lacks one).
  - `finalSample` becomes `collectFinalSample`, returning the parsed `ParseResult`/state deltas in memory instead of calling `handleRunning`; it no longer writes status, sets conditions, or touches the sampling throttle map.
  - `handleSucceeded`/`handleFailed` compute `t_w` from `anchor` (dropping the `time.Since` calls and the in-memory-state-dependent recompute), merge the final sample, and hand the complete payload to `setComplete`, which stamps `completionTime = anchor`.
  - Reconcile gate unchanged; after `Complete=True` the measurement is inert.
- **`pkg/controller/job_threshold_helpers.go`** — `collectJobMeasuredValues` gates goodput keys on the GM's `Complete` condition.
- **`pkg/goodput/calculator.go`** — no change; it is already pure.
- **No API change.** `Complete` condition and `status.completionTime` already exist in `GoodputMeasurementStatus`. If review concludes an explicit `status.final` marker is wanted after all, that is a CRD schema change requiring user approval and `make manifests generate` — flagged here, and rejected in Alternatives.
- **Integration tests** (`cmd/integration/testdata/reconcile/`):
  - Existing cases `goodput-measurement`, `goodput-measurement-job-succeed`, `goodput-measurement-job-fail`, `goodput-measurement-restart-missed`, `goodput-measurement-state-recovery`, `job-goodput-threshold-pass/fail`, and `job-auto-goodput` exercise the changed paths. Their golden files collect only the post-`Complete` state, so diffs are expected to be nil or confined to condition reasons/messages; any regeneration happens only after diagnosis and with explicit user approval per repo policy.
  - New case `goodput-measurement-frozen-after-complete`: drive a measurement to `Complete`, then touch the Job and the measurement, and assert the collected `status` (including `result`) is unchanged.
  - The harness's GM sanitization (`integration_test.go` clears `trainingTimeSec`, `result`, `startTime`, `completionTime`, …) **stays**: anchoring makes values stable across reads within a run, but they still depend on envtest wall-clock spacing between training start and Job terminal transition, so they remain non-comparable across runs.
- **Docs** — update the goodput page under `docs/` (and `site/content/docs/` if present) to state that measurement values are provisional while `Measuring=True` and frozen once `Complete=True`.

## Rationale

- **Purity is the idempotency mechanism.** Anchoring to the Job's terminal condition timestamp makes the terminal computation replay-safe across conflict retries, stale-cache re-entries, and controller restarts of any duration — the classes of nondeterminism observed in #177. Gating alone ("stop reconciling") cannot achieve this because the gate itself is subject to cache staleness.
- **One write, one observable transition.** Merging the final sample into the terminal write removes the window where a report read returns a value that the very next read contradicts, and stops `Measuring=True` from flapping on an already-succeeded Job.
- **The `Complete` condition is already the contract.** Introducing a parallel `final` marker creates two sources of truth that can disagree; every consumer (report, thresholds, the gate) can key off the condition that already exists.
- **Keeping the final sample (rather than freezing at the last periodic sample)** preserves the fix documented in `finalSample`'s comment: without the trailing read, up to one sample interval of data is lost — observed on hardware as step 90 recorded for a run that reached step 100.
- **Not copying into Job/Workflow status** keeps a single source of truth. The Workflow-ownership design exists precisely so measurements survive Job deletion for the report; duplicating values across tiers reintroduces the synchronization problem this ADR removes.

## Consequences

- **Positive:** report output for a completed certification is byte-stable across reads; threshold pass/fail is computed on the frozen value regardless of Job-controller timing; controller restarts around the terminal boundary no longer change results; the transient `Measuring=True`-after-success flap disappears.
- `t_w` is measured to the Job's terminal transition, not to the last observed log line. For a workload that idles between its last step and Job success, terminal `t_w` is slightly larger than "active training time". This matches the wall-clock definition of `t_w` in the goodput formula, and — unlike today — it is one number, not a family of numbers indexed by reconcile time.
- Goodput threshold evaluation waits for the GM's `Complete` condition — at most roughly one sample interval of added latency on the Job success path, bounded by the existing `measurementTimeout`.
- The terminal reconcile does more in a single pass (final log read + terminal metrics + conditions), but performs one API write where today there are two.
- Anything relying on post-success intermediate status states (none known in-tree; the harness collects only final state) would need updating.

## Alternatives Considered

- **Gate-only fix: keep the two-write sequence, rely on `Complete` to stop reconciling.** Rejected — the inter-write window still yields contradictory report reads, and re-entry before the gate lands still rewrites values; the gate already exists and demonstrably does not fix #177 alone.
- **Add `status.final` bool (or `status.finalResult`).** Rejected — redundant with the `Complete` condition, requires a CRD schema change and regeneration, and two markers can disagree. Flagged in Implementation in case review overrides.
- **Copy final goodput into Job/Workflow status.** Rejected — duplicates metrics across tiers, needs CRD changes at two tiers, and creates a second source of truth the report would have to reconcile with the GM it already reads.
- **Freeze at the last periodic sample (skip the final log read).** Rejected — deterministic but discards up to one sample interval of trailing data, regressing the documented hardware-observed loss `finalSample` was added to fix.
- **Anchor to worker-pod container termination time.** Rejected — pods may already be garbage-collected when the terminal reconcile (or its restart replay) runs, so it is not restart-idempotent; the Job condition timestamp is always available on the object being watched. It remains a *preferred* source where present (via `ApplicationStopTime`), with `anchor` as the deterministic floor/ceiling.

## Notes

- The Job controller's checkpoint-restart stall reset ([job_controller.go:733](../../pkg/controller/job_controller.go#L733)) writes GM status (`lastStepTimestamp`, `avgStepTimeSec`, `startTime`) strictly pre-terminal and is unaffected. Convention after this ADR: the GM controller is the only writer of GM status after the measured Job is terminal, and nothing writes to a GM whose `Complete=True`.
- That stall reset moves `status.startTime`, which is also the `t_w` baseline via `buildCumulativeFromStatus` — a pre-existing interaction that is out of scope here; this ADR only guarantees the terminal computation over that baseline is deterministic.
- Prometheus gauges emit one final sample with the frozen values at terminal handling; operational-metric cleanup is unchanged.
- Iteration reuse is safe: per-iteration Jobs get distinct names (`<workflow>-<group>-iter-<n>`), so each iteration's GM (`<job>-goodput`) freezes independently.

## References

- Issue #177 — goodput of a completed certification changes between report reads
- [`pkg/controller/goodputmeasurement_controller.go`](../../pkg/controller/goodputmeasurement_controller.go) — `reconcileMeasurement`, `finalSample`, `handleSucceeded`, `handleFailed`, `setComplete`
- [`pkg/goodput/calculator.go`](../../pkg/goodput/calculator.go) — `CalculateGoodput`
- [`pkg/report/report.go`](../../pkg/report/report.go) — `buildDomainReports` reads `status.result` live
- [`pkg/controller/job_threshold_helpers.go`](../../pkg/controller/job_threshold_helpers.go) — `collectJobMeasuredValues`
- ADR-005: LogProfile-based goodput measurement
- ADR-008: Checkpoint restart
- ADR-009: Adaptive stall detection — the other consumer of live GM status fields
- ADR-013: Prometheus observability
- ADR-014: Integration test strategy — golden-file sanitization of wall-clock-dependent GM fields
