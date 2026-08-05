# ADR-019: Sequential Workflow Execution in Certification Controller

## Context

The Certification controller currently creates all Workflows simultaneously when a Certification is created. On GPU clusters with limited resources, running multiple heavy workloads (NCCL bandwidth tests, Nemotron pre-training) concurrently creates resource contention. Concurrent Workflows also compete for the same GPU nodes, leading to scheduling failures or interference between workloads that are supposed to stress-test hardware independently.

Downstream scheduling to handle concurrent workloads targeting the same nodes is complex and error-prone. Sequential execution is simpler to reason about, easier to debug (only one Workflow is active at a time), and avoids the need for workload isolation between concurrent burn-in categories targeting the same nodes.

## Decision

Modify the Certification controller to create Workflows one at a time, in the order categories appear in `spec.categories[]`. Only create the next Workflow after the current one reaches a terminal state (Succeeded or Failed).

Key behaviors:

- All category statuses are initialized as "Pending" on first reconcile.
- One Workflow is active at a time; its category is "InProgress" with a WorkflowRef.
- On terminal state: update category status, immediately advance to next Pending category.
- Continue on failure: all categories execute regardless of prior failures.
- Remediation is created after all categories complete (same as before).
- Category ordering is deterministic: `spec.categories[]` array order.

## Implementation

### State Machine

```
Reconcile:
  1. If categoryStatuses empty:
     → Validate all categories in catalog (fail-fast on unknown)
     → Initialize all as "Pending"
     → Create Workflow for first category, set to "InProgress"
     → Set Certification InProgress/WorkflowCreated
     → Requeue

  2. Find first non-terminal category (not Succeeded/Failed):
     a. If "InProgress":
        → Fetch Workflow, check conditions
        → If Succeeded/Failed: update category status, collect failedNodes, requeue immediately
        → If still running: requeue after interval
     b. If "Pending":
        → Create Workflow, set to "InProgress"
        → Set Certification InProgress/WorkflowCreated
        → Requeue

  3. If all categories terminal:
     → If any Failed: create Remediation, set Certification Failed
     → If all Succeeded: set Certification Succeeded
```

### Controller Changes

- Replace `createWorkflowsForCategories` with `initializeCategoryStatuses` (validates all categories, creates first Workflow only).
- Replace `updateStatusFromWorkflows` with `processNextCategory` (finds next non-terminal category, creates or polls Workflow).
- Extract `createWorkflowForCategory` helper (single Workflow creation logic).
- Add `checkActiveWorkflow` (polls active Workflow status).
- Add `finalizeCertification` (aggregates results when all categories terminal).
- No new API types or reason constants needed.

### Backward Compatibility

- Single-category Certifications behave identically.
- The API types are unchanged; only the controller behavior changes.

## Rationale

- **Resource efficiency**: Only one Workflow's pods compete for GPU resources at a time.
- **Simpler debugging**: Only one active Workflow to inspect at a time.
- **Deterministic ordering**: Categories run in the user-specified order.
- **Continue-on-failure**: Preserves diagnostic coverage (same as concurrent mode).
- **No API changes**: The existing status model already supports Pending without WorkflowRef.

## Consequences

### Positive

- Reduced resource contention on GPU clusters.
- Clearer status messages ("Created Workflow for category X (2 of 4)").
- Simpler mental model for operators.
- No API changes needed.

### Negative

- Total certification time increases (sequential vs concurrent).
- If categories are truly independent and the cluster has capacity, parallelism could have been faster.

### Mitigations

- Categories that need parallelism can be run as separate Certifications.
- A future `spec.execution: parallel | sequential` field could opt back into concurrent mode.

## Alternatives Considered

### Configurable execution mode (parallel vs sequential)

Add `spec.execution: "sequential" | "parallel"`. Rejected for initial implementation because it adds API complexity. Can be added later if concurrent mode is needed.

### Fail-fast on first failure

Stop creating subsequent Workflows when one fails. Rejected because continue-on-failure maximizes diagnostic coverage — if NCCL fails but Nemotron succeeds, that narrows the failure to a specific hardware dimension.

## References

- ADR-002 — layered CRD hierarchy (Certification → Workflow → Job)
- ADR-010 — certification catalog
- ADR-018 — NCCL test suite catalog entries
