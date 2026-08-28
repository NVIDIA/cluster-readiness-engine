# ADR-009: Feature — Adaptive Stall Detection

## Context

Training workloads can stall without explicitly failing — a hung NCCL collective, a deadlocked data loader, or a GPU that stops producing results. The workload process stays alive (no OOM, no crash), but no forward progress is made. The workload framework reports it as "running" indefinitely.

Fixed timeouts are a poor solution: a workload with 30-second steps and a workload with 5-minute steps need very different timeout values. A timeout that works for one benchmark is too aggressive or too lenient for another.

Options considered:
1. Fixed timeout per workload
2. User-specified expected step time with threshold
3. Adaptive detection based on observed average step time

## Decision

Implement adaptive stall detection that compares elapsed time since the last training step against a configurable multiplier of the observed average step time. When `stallMultiplier * avgStepTime` is exceeded, the workload is marked as stalled.

## Implementation

- **LogProfile** (`api/v1alpha1/logprofile_types.go`): `warmupStep` pattern identifies initial steps to exclude from average step time calculation (warmup steps are typically 10-100x slower than steady-state steps).
- **GoodputMeasurement controller** (`pkg/controller/goodputmeasurement_controller.go`): On each sample cycle, computes and persists:
  - `LastStepTimestamp` — when the most recent training step completed
  - `AvgStepTimeSec` — average time between steady-state steps (excluding warmup)
- **Job controller** (`pkg/controller/job_controller.go`): `checkStallTimeout()` runs on each reconcile for running workloads:
  1. Finds the associated GoodputMeasurement via `findGoodputMeasurement()`
  2. Reads `LastStepTimestamp` and `AvgStepTimeSec` from status
  3. Computes `elapsed = now - LastStepTimestamp`
  4. If `elapsed > stallMultiplier * AvgStepTimeSec`, marks the Job as Failed with reason `WorkloadStalled`
- **Checkpoint integration**: If checkpoint restart is configured (ADR-008), a stalled workload is restarted from checkpoint before being failed terminally. This handles transient stalls (e.g., temporary network congestion).

The `stallMultiplier` defaults to a conservative value. A multiplier of 3 means the workload must be idle for 3x the average step time before being considered stalled. This accounts for natural variance in step times (checkpointing steps are longer, data loading can be bursty).

## Rationale

- **Adaptive to the workload.** The threshold scales automatically with the workload's actual performance. A fast benchmark (2s steps) gets a tight timeout. A slow pre-training run (5min steps) gets a proportionally longer timeout. No manual tuning required.
- **Warmup exclusion prevents false positives.** The first few training steps are typically much slower (JIT compilation, data pipeline warmup, model initialization). Including them in the average would inflate the threshold and miss real stalls. The `warmupStep` pattern in LogProfile identifies these steps.
- **Single field to configure.** The user only specifies `stallMultiplier`. The controller derives everything else from observed behavior.
- **Composes with checkpoint restart.** A stalled workload might recover if restarted. The stall → restart → checkpoint pipeline handles transient stalls automatically.

## Consequences

### Positive
- No manual timeout configuration per workload type
- Adapts to workload performance automatically
- Catches hung workloads that frameworks report as "running"
- Integrates with checkpoint restart for transient stall recovery
- Warmup exclusion prevents false positives during initialization

### Negative
- Requires GoodputMeasurement to be running (stall detection depends on step timestamps from log parsing)
- Cannot detect stalls before the first training step completes (no baseline yet)
- Average step time can be skewed by outlier steps (checkpointing, evaluation)
- Stall detection adds a dependency between the Job controller and GoodputMeasurement status

### Mitigations
- Stall detection is opt-in (only active when `stallMultiplier` is set and GoodputMeasurement exists)
- The conservative default multiplier provides tolerance for step time variance
- Warmup step exclusion removes the largest source of skew
- The pre-stall period (before first step) is bounded by the workload framework's own timeouts

## Alternatives Considered

### Fixed timeout
**Rejected** because: A timeout that works for a fast NCCL benchmark (minutes) would kill a slow pre-training workload after the first step. A timeout for pre-training (hours) would let a stalled NCCL test run for hours before detection. The operator would need to set per-workload timeouts, adding configuration burden.

### User-specified expected step time
**Rejected** because: Users would need to know the expected step time for each workload on each hardware configuration. This information often isn't available until the workload runs. Adaptive detection derives the threshold from observation, eliminating this guesswork.

### Heartbeat-based detection (workload pushes signal)
**Rejected** because: Requires modifying training code to emit heartbeats. The nvcre operates on unmodified workloads (ADR-005). Log-based step detection achieves the same result without code changes.

## Notes

- NeMo 6 warmup steps are identified by the "is warmup iteration" suffix in log lines — the `warmupStep` pattern must match this framework-specific marker
- `AvgStepTimeSec` and `LastStepTimestamp` are persisted to GoodputMeasurement status, not computed on-the-fly, so they survive controller restarts

## References

- `api/v1alpha1/job_types.go` — `stallMultiplier` field
- `api/v1alpha1/logprofile_types.go` — `warmupStep` pattern
- `pkg/controller/job_controller.go` — `checkStallTimeout()`, `findGoodputMeasurement()`
- `pkg/controller/goodputmeasurement_controller.go` — step time tracking
