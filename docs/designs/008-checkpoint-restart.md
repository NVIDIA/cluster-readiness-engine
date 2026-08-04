# ADR-008: Feature — Checkpoint-Based Restart and State Recovery

## Context

Burn-in workloads can fail mid-run due to transient hardware issues — a GPU thermal event that resolves after cooling, a network hiccup that causes a NCCL timeout, or a node that becomes temporarily unresponsive. Not all failures indicate permanent hardware problems.

Without restart support, any transient failure terminates the entire burn-in run. For long-running workloads (hours of pre-training), this wastes significant compute. The controller should be able to restart a failed workload from its last checkpoint rather than starting from scratch.

Options considered:
1. No restart — fail and report (user re-runs manually)
2. Simple restart from scratch (no checkpoint awareness)
3. Checkpoint-aware restart with goodput state recovery

## Decision

Implement checkpoint-aware restart in the Job controller. When a workload fails and the GoodputMeasurement has recorded a checkpoint, the controller deletes the failed workload and creates a new one (up to `maxRestarts`). Goodput state (pending interruption timing) is persisted to GoodputMeasurement status to survive across restarts and controller restarts.

## Implementation

- **Job API** (`api/v1alpha1/job_types.go`): `maxRestarts` field (default 0 = no restart). `checkpointPVC` field specifying the PVC name used for checkpoint storage.
- **Job controller** (`pkg/controller/job_controller.go`): On workload failure:
  1. Check if `maxRestarts > 0` and restarts remaining
  2. Check if GoodputMeasurement has recorded at least one checkpoint
  3. If yes: record a `PendingInterruption` in GoodputMeasurement status, delete the failed workload, increment restart count, and let the next reconcile create a new workload (which picks up from checkpoint via shared PVC)
  4. If no checkpoint: fail terminally
- **GoodputMeasurement state recovery** (`pkg/controller/goodputmeasurement_controller.go`): `PendingInterruption` is persisted to CRD status. On next reconcile after restart, the interruption timing is incorporated into goodput calculation. This survives controller restarts — state is in the API server, not in memory.
- **Workflow validation** (`pkg/controller/workflow_controller.go`): Before creating a Job with `checkpointPVC`, the Workflow controller verifies the PVC exists in dependencies. This catches configuration errors early.

Restart flow:
```
Workload fails → Job controller checks checkpoint → records PendingInterruption →
deletes workload → next reconcile creates new workload → new workload resumes from PVC →
GoodputMeasurement accounts for interruption in goodput ratio
```

## Rationale

- **Checkpoint awareness prevents false restarts.** Restarting without a checkpoint means repeating all work from scratch. If the workload failed early (before any checkpoint), restarting is pointless — the same failure will likely recur. Requiring a checkpoint ensures there's meaningful progress to preserve.
- **Goodput state must survive restarts.** The interruption between failure and restart is "lost time" that affects goodput. Persisting `PendingInterruption` to CRD status ensures this timing is captured even if the controller itself restarts during the gap.
- **maxRestarts bounds retry storms.** Without a limit, a persistently failing workload would restart forever. `maxRestarts` ensures terminal failure after a bounded number of attempts.
- **Shared PVC is the standard checkpoint mechanism.** Training frameworks (NeMo, Megatron) already support checkpoint-to-PVC. The controller doesn't need to understand checkpoint formats — it just ensures the PVC persists across workload recreations.

## Consequences

### Positive
- Transient failures don't waste hours of burn-in work
- Goodput measurement remains accurate across restarts (interruptions are tracked)
- No changes to training code — checkpoint/resume is handled by the framework
- Configurable per-Job — some workloads may not need restart support

### Negative
- Restart adds complexity to the Job controller state machine
- The controller cannot distinguish transient failures from permanent ones — it restarts up to `maxRestarts` regardless
- PVC must be configured correctly (shared filesystem, correct mount paths)

### Mitigations
- Health monitoring (ADR-004) can detect permanent hardware failures and fail the Job immediately (no restart for hardware failures)
- Workflow-level `checkpointPVC` validation catches PVC misconfiguration before the Job starts
- Integration tests cover restart with checkpoint, restart without checkpoint, and max restarts exceeded

## Alternatives Considered

### No restart support
**Rejected** because: Long-running burn-in workloads (multi-hour pre-training) are expensive. A transient failure 4 hours into a 6-hour run wastes 4 hours of compute. Restart from checkpoint recovers most of that investment.

### Restart from scratch (no checkpoint awareness)
**Rejected** because: Restarting a multi-hour workload from the beginning is nearly as wasteful as not restarting at all. Checkpoint awareness ensures only incremental work is repeated.

### External checkpoint manager
**Rejected** because: Adds an external dependency. The controller already knows the workload's lifecycle (it created it) and already monitors goodput (it has the GoodputMeasurement). Adding checkpoint-aware restart is a natural extension, not a separate concern.

## Notes

- `PendingInterruption` is written to GoodputMeasurement status, not Job status — the interruption affects goodput calculation, which is the GoodputMeasurement controller's responsibility
- PVC cleanup must use `Delete PVC → Patch Released PV` ordering to prevent CSI from reverting the PV to Bound state

## References

- `api/v1alpha1/job_types.go` — `maxRestarts`, `checkpointPVC` fields
- `pkg/controller/job_controller.go` — restart logic
- `pkg/controller/goodputmeasurement_controller.go` — PendingInterruption persistence
