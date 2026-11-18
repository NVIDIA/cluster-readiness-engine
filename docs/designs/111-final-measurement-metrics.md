# ADR-111: Retain final measurement metrics until measurement deletion

> **Status:** Proposed

## Context

Issue #475 reports that short NCCL runs can finish before any provisional sample is scraped. The terminal bandwidth path deletes its gauges without publishing final results. Goodput publishes terminal values and immediately deletes the operational gauges. Persisted measurement status already outlives the Job, but the metrics do not.

## Decision

Keep the existing metric names and labels. While running they remain provisional. After a successful terminal status write, replace NCCL samples with the final results for `JobSucceeded`; clear unsuccessful or empty bandwidth outcomes. Retain goodput terminal values for `JobSucceeded` and `JobFailed`, including loss/overhead from failed runs, until measurement deletion. No-data goodput outcomes do not produce final measurements.

A reconciliation of an already-complete measurement restores its metrics from the frozen status without reading logs or recomputing values. Preserve ADR-072's first-terminal-write-wins rule: publish the status returned after conflict handling, never the rejected candidate. A failed status write must not publish candidate final metrics. An explicit context-cancellation guard protects publication while the shared retry helper's cancellation handling is addressed separately in upstream PR #588.

Deletion removes every series belonging to the namespace and measurement, even if its Job has gone or label values changed. Checkpoint restart still removes instantaneous goodput gauges. Metric retention is bounded by existing measurement objects, not elapsed time or Job lifetime.

## Implementation

- Replace terminal cleanup with status-derived metric synchronization in both measurement reconcilers, and call it from their Complete gates.
- Remove NCCL provisional-only sizes before emitting the final set, including deprecated aliases.
- Capture the workflow label on new GoodputMeasurements; recover it from a Workflow controller owner, then the referenced Job for older objects. If neither survives on a legacy standalone measurement, the workflow label is empty. Deletion matches measurement identity rather than depending on this label.
- Keep all CRD fields and measurement calculations unchanged.
- Document provisional/final lifecycle and restart recovery in metrics and API guides.
- Snapshot registry samples through the existing TestCaseParser fixture pattern: terminal success, failure and no data; a fresh reconciler restoring Complete status; deletion after Job removal; stale provisional sizes; conflict and failed-status-write behavior.

## Rationale

Prometheus can scrape the same final values used by reports and thresholds. No second metric family or unbounded historical cache is needed. Persisted status is the authoritative replay source.

## Consequences

Completed measurement series remain present longer, bounded by measurement retention. The metrics use the existing labels and do not alone distinguish provisional from final; use measurement completion status when that distinction matters. Restart recovery runs when the manager's initial measurement events reconcile. It does not recreate deleted measurements.

## Alternatives Considered

- Separate final metric families: redundant status and dashboard migration.
- Shorter scrape/sample intervals: cannot guarantee a short run ever exports its final values.
- Keep terminal cleanup and correct documentation only: leaves the reported observability gap.

## Notes

This design is proposed, not a claim of maintainer acceptance. ADR-093 is already assigned to the MPI worker probe design. Publication and human DCO/review requirements remain separate from local implementation approval.

## References

- https://github.com/NVIDIA/cluster-readiness-engine/issues/475
- https://github.com/NVIDIA/cluster-readiness-engine/issues/553
- ADR-072: Freeze GoodputMeasurement Status at Job Terminal State
