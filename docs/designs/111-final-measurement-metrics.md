# ADR-111: Publish final NCCL bandwidth metrics

> **Status:** Proposed

## Context

Issue #475 reports that short NCCL runs can finish before any provisional sample is scraped. The terminal bandwidth path deletes its gauges without publishing the final results, even though the completed `BandwidthMeasurement` retains them in status.

## Decision

Keep the existing NCCL metric names and labels. While a measurement runs, its gauges remain provisional. After a successful terminal status write, replace provisional samples with the final results for `JobSucceeded`. Clear gauges for unsuccessful or empty terminal outcomes. A reconciliation of an already-complete measurement restores successful final metrics from frozen status without rereading logs or recomputing values.

Preserve the first-terminal-write-wins rule: publish the status returned after conflict handling, never the rejected candidate. A failed status write must not publish candidate metrics. A cancelled reconcile must not publish metrics after the status retry helper suppresses cancellation.

Deletion removes every series belonging to the namespace and measurement, even if its Job is gone or labels changed. Retained series are bounded by existing measurement objects, not elapsed time or Job lifetime.

## Implementation

- Replace terminal cleanup with status-derived bandwidth metric synchronization and call it from the `Complete` gate.
- Remove provisional-only message sizes before emitting the final set, including deprecated aliases.
- Document the provisional and final bandwidth metric lifecycle.
- Snapshot registry output with `TestCaseParser` fixtures for terminal success, no data, unavailable logs, restart recovery and deletion. Separate hand-written tests cover status-write failure and conflict-winner behavior.

## Rationale

Prometheus can scrape the same final per-size values used by reports and thresholds. No second metric family or unbounded history cache is needed.

## Consequences

Final series remain present until the `BandwidthMeasurement` is deleted. The existing labels do not distinguish provisional from final samples; check the measurement's completion condition when that distinction matters.

## Alternatives Considered

- Add separate final metric families: this duplicates the existing status and requires dashboard migration.
- Shorten scrape/sample intervals: this cannot guarantee a short run exports its final values.
- Keep terminal cleanup and correct the documentation only: this leaves the reported observability gap.

## Notes

This design is proposed, not a claim of maintainer acceptance. Publication and human DCO/review requirements remain separate from local implementation.

## References

- https://github.com/NVIDIA/cluster-readiness-engine/issues/475
