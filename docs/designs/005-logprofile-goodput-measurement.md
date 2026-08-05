# ADR-005: Feature — LogProfile-Driven Goodput Measurement

## Context

Training workloads can run without errors but still be inefficient — slow checkpointing, long warmup phases, frequent interruptions, or stalled collective operations. A pass/fail result from workload completion is not enough. The controller needs to measure training quality (goodput) to detect performance degradation.

Measuring goodput requires parsing training framework logs to extract timestamps for application start, training steps, checkpoint saves, and checkpoint loads. Different frameworks (NeMo 4, NeMo 6, Megatron) use different log formats. The measurement system must support new frameworks without code changes.

Options considered:
1. Require training code to export metrics via SDK/library
2. Sidecar container that parses logs and exports metrics
3. Controller-side log parsing with configurable regex patterns (LogProfile CRD)

## Decision

Implement a cluster-scoped `LogProfile` CRD that defines regex patterns with named capture groups for parsing training logs. The `GoodputMeasurement` controller reads pod logs via the Kubernetes API, parses them using the referenced LogProfile, and computes goodput as `(t_total - t_checkpoint - t_resume - t_save) / (t_total - t_resume)`.

## Implementation

- **LogProfile CRD** (`api/v1alpha1/logprofile_types.go`): Cluster-scoped resource with regex patterns for `applicationStart`, `trainingStep`, `checkpointSave`, `checkpointLoad`, and `warmupStep`. Each pattern uses named capture groups (e.g., `(?P<timestamp>...)`, `(?P<globalStep>\d+)`).
- **GoodputMeasurement CRD** (`api/v1alpha1/goodputmeasurement_types.go`): References a Job and a LogProfile. Reports `Measuring` and `Complete` conditions. Status includes `GoodputRatio`, `AvgStepTimeSec`, `AvgTFLOPSPerGPU`, `LastStepTimestamp`, and timing breakdowns.
- **Log parser** (`pkg/goodput/parser.go`): Compiles LogProfile patterns into Go regexes. Parses log lines into structured events. Handles multi-worker log merging.
- **Goodput calculator** (`pkg/goodput/calculator.go`): Computes goodput ratio from parsed events using the formula above.
- **Log reader** (`pkg/goodput/reader.go`): Reads pod logs via `PodLogFetcher` interface. Uses `sinceTime` for incremental log fetching. The interface enables dependency injection for testing.

The GoodputMeasurement controller runs on a configurable sample interval (default 15s). On each cycle:
1. Reads logs from all workload pods (via `cre.nvidia.com/job` label)
2. Parses logs using the referenced LogProfile
3. Computes goodput metrics
4. Updates status with latest values
5. Publishes Prometheus gauge metrics

Warmup handling: The `warmupSteps` field in LogProfile specifies how many initial steps to exclude from steady-state metrics. The `warmupStep` pattern identifies framework-specific warmup markers.

## Rationale

- **Zero code changes in training code.** LogProfiles parse existing log output. No SDK, no library, no sidecar. Training code doesn't know the measurement system exists.
- **New frameworks via YAML.** Adding support for a new framework means creating a LogProfile with the right regex patterns. No Go code changes.
- **Controller-side parsing.** Log reading happens in the controller, not in sidecars. This avoids per-pod resource overhead and keeps the measurement concern out of the workload.
- **Named capture groups are self-documenting.** A regex like `step=(?P<globalStep>\d+)` makes it clear what data is being extracted.

## Consequences

### Positive
- Works with any training framework that logs to stdout
- LogProfiles are cluster-scoped — deploy once, reference from any Job
- Incremental log fetching (`sinceTime`) keeps memory bounded for long-running workloads
- PodLogFetcher interface enables deterministic testing with fake log data
- Goodput ratio provides a single number for training efficiency

### Negative
- Regex patterns are brittle — framework log format changes can break parsing
- Log parsing adds CPU and memory overhead proportional to log volume
- Pod log API has size limits and can lose data under high throughput
- The goodput formula assumes the timing components are exhaustive

### Mitigations
- Built-in LogProfiles for NeMo 4 and NeMo 6 are tested against real training logs
- Configurable `sampleInterval` and `tailLines` control resource usage
- Incremental fetching avoids re-processing old logs
- Golden file tests for the parser catch regressions from log format changes

## Alternatives Considered

### SDK/library in training code
**Rejected** because: Requires modifying training code. Different teams use different frameworks and different versions. The cluster-readiness-engine should work with unmodified workloads — this is a validation tool, not a training framework.

### Sidecar container
**Rejected** because: Adds a container to every workload pod. Requires shared volume or log forwarding. Increases pod resource requests. The sidecar must be framework-aware, moving the same regex logic to a different location without reducing complexity.

### Prometheus metrics from training code
**Rejected** because: Not all training frameworks expose Prometheus metrics. Those that do use different metric names and semantics. The controller would still need framework-specific logic to interpret the metrics.

## Notes

- `ApplicationStartTime` must be persisted to status to survive controller restarts — in-memory state is lost when the controller pod is rescheduled
- `NonWarmupTime` must be accumulated incrementally across reconcile cycles, not recomputed from scratch, to handle log window boundaries correctly
- Log fetching should use `sinceTime` to avoid re-processing old logs and to prevent missing early startup lines that fall outside the tail window in long-running workloads

## References

- `api/v1alpha1/logprofile_types.go` — LogProfile CRD
- `api/v1alpha1/goodputmeasurement_types.go` — GoodputMeasurement CRD
- `pkg/goodput/parser.go`, `calculator.go`, `reader.go`, `worker.go`, `types.go`
- `pkg/controller/goodputmeasurement_controller.go`
