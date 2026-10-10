# ADR-093: Preserve Category Identity in Certification Watch Output

> **Status:** Accepted

## Context

`nvcrectl certification run --wait` prints category status changes from
`Certification.status.categoryStatuses`. It currently uses `domain/variant` as
the display label and uses that same label as the key in its last-status map.
When a Certification contains duplicate categories that differ by options other
than MNNVL, both labels are identical. Their status changes can therefore
suppress one another, and the output cannot identify which category changed.

The Certification spec is immutable after creation, and category statuses are
initialised and updated in spec order. The category index is consequently a
stable identity for the lifetime of a run. The watch output is a CLI reporting
surface; it does not change the API or controller's Workflow naming.

## Decision

Keep the existing `domain/variant` label for categories that are already
distinguishable. When multiple statuses have the same current display label,
append their one-based position in the Certification category list. Use the
zero-based category index, rather than the display text, as the last-status map
key.

## Implementation

- Determine duplicate display labels across the full category-status slice.
- Preserve the existing MNNVL suffix for duplicate domain/variant pairs.
- Add a stable category ordinal when labels still collide, including when
  categories differ by options such as `maxSteps` or `testScale`.
- Key remembered status by category index so output-label formatting cannot
  conflate transitions.
- Add focused package-level regression coverage for duplicate categories and
  status transitions; retain existing output for unique labels and MNNVL pairs.

## Rationale

The controller already stores statuses in immutable spec order, so the index is
stable across watch events and reconnects without relying on parsing or
reconstructing Workflow names. An ordinal is available before a queued category
has a Workflow reference and still makes each line attributable. Keeping the
existing base label avoids changing output for ordinary certifications.

## Consequences

Duplicate category lines gain a category ordinal, for example
`communication/nccl-all-reduce (MNNVL Enabled) [category 1]`. The CLI's output
changes only where the current label would be ambiguous. No API, persisted
status, Workflow name, or controller behaviour changes.

## Alternatives Considered

- **Use the Workflow name as the label:** names are available only after a
  category starts, and this would make output depend on controller state while
  changing the established label format more substantially.
- **Use category options as a suffix:** this duplicates option-formatting and
  risks drifting from the controller's Workflow naming logic as options evolve.
- **Change only the status-map key:** this would print both transitions but
  leave the lines indistinguishable to the user.

## Notes

This decision applies only to `nvcrectl certification run --wait` progress
output. The final report's category rows are outside issue #468's reported
status-conflation path.

## References

- [Issue #468](https://github.com/NVIDIA/cluster-readiness-engine/issues/468)
- [ADR-044: Full Certification Lifecycle in nvcrectl](044-nvcrectl-certification-lifecycle.md)
