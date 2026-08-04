# ADR-017: NCCL Bandwidth Measurement

## Context

The certification catalog now includes NCCL all-reduce communication benchmarks (ADR-016). These benchmarks produce bandwidth metrics (algorithmic bandwidth and bus bandwidth) per message size via the `all_reduce_perf` binary. The existing GoodputMeasurement system (ADR-005, ADR-015) is designed for training workloads — it tracks training steps, checkpoints, interruptions, and computes a goodput ratio. NCCL bandwidth data is structurally different: tabular (message size → bandwidth) rather than time-series (step → timing).

We need to:
1. Parse NCCL `all_reduce_perf` output from launcher pod logs
2. Export every result as a Prometheus metric with `message_size_bytes` as a label
3. Store only per-size averages in the CR status (the raw output is too large to store entirely)

## Decision

Add a new `BandwidthMeasurement` CRD and controller following the same architectural patterns as GoodputMeasurement, but in a separate package (`pkg/nccl/`). Extract generic pod log fetching and pod discovery utilities from `pkg/goodput/` into shared packages (`pkg/podlogs/`, `pkg/podutil/`) so both systems depend on common infrastructure without coupling to each other.

## Implementation

### Package Refactoring

- **`pkg/podlogs/`**: `PodLogFetcher` interface, `LogOptions`, `NewKubernetesLogFetcher()` — extracted from `pkg/goodput/reader.go`
- **`pkg/podutil/`**: `WorkerDiscoverer`, `GetWorkerPods()`, `GetReplicatedJobPod()`, pod status helpers — extracted from `pkg/goodput/worker.go`
- **`pkg/goodput/`**: Retains `LogReader`, `ProfileParser`, `Calculator` — imports shared packages
- **`pkg/nccl/`**: New. `NCCLParser` compiles `bandwidthResult` regex from LogProfile, `ParseBandwidthLogs()` returns `[]BandwidthDataPoint`

### BandwidthMeasurement CRD

- **Spec**: `jobRef`, `logProfileRef`, `sampleInterval`, `tailLines` — mirrors GoodputMeasurementSpec
- **Status**: `results []BandwidthResult` (per-size averages: sizeBytes, algBW, busBW, samples), `conditions` (Measuring/Complete), `startTime`, `completionTime`
- **Auto-creation**: Optional `bandwidthMeasurement` field on JobSpec, auto-creates child resource (same pattern as ADR-015)

### LogProfile Extension

- New pattern type `bandwidthResult` in `LogPatternSet` with captures: `size` (int), `algBW` (float), `busBW` (float)
- New `replicatedJobName` field on `WorkerStrategySpec` to select which JobSet replicatedJob to read (default: "trainer", MPI uses "launcher")

### Controller

- `BandwidthMeasurementReconciler` follows GoodputMeasurementReconciler patterns: reconcile loop, sample throttling, condition management
- Finds launcher pod via `podutil.GetReplicatedJobPod()`, reads logs via `podlogs.PodLogFetcher`, parses with `nccl.NCCLParser`
- Aggregates per-size running averages in status
- Emits `burnin_nccl_algbw_gbps` and `burnin_nccl_busbw_gbps` Prometheus gauges with `message_size_bytes` label

## Rationale

- **Separate CRD**: NCCL bandwidth data (tabular, per-size) is structurally incompatible with GoodputMeasurement status (time-series, per-step). Merging them would bloat the GoodputMeasurement status and complicate the controller.
- **Shared packages**: `PodLogFetcher` and `WorkerDiscoverer` are generic Kubernetes pod utilities. Extracting them prevents coupling `pkg/nccl/` to `pkg/goodput/`.
- **`replicatedJobName` on WorkerStrategy**: MPI output goes to the launcher pod, not worker pods. The existing `WorkerDiscoverer` finds "trainer" pods by default; this field selects "launcher" instead.
- **Per-size averages in status**: The NCCL test produces 32 sizes × 20 iterations. Storing every data point (640+ entries) would exceed practical CR size limits. Averaging per size keeps 32 entries.
- **Prometheus for full data**: Each parsed line updates the gauge for that message size. Prometheus time-series retention captures the history.

## Consequences

### Positive
- Clean separation: `pkg/goodput/` and `pkg/nccl/` don't depend on each other
- Shared infrastructure (`podlogs/`, `podutil/`) benefits future measurement types
- NCCL bandwidth data is properly modeled, not force-fit into training types
- Prometheus metrics enable Grafana dashboards with per-size bandwidth plots

### Negative
- New CRD adds operational complexity (one more resource type to manage)
- Package refactoring touches existing code (risk of regression, mitigated by existing tests)
- Controller boilerplate duplicated from GoodputMeasurement (acceptable for clean separation)

## Alternatives Considered

### Extend GoodputMeasurement CRD
**Rejected**: Would add bandwidth-specific fields to a training-focused status, confuse users, and complicate the controller with conditional logic based on measurement type.

### Put NCCL parsing in `pkg/goodput/`
**Rejected**: Couples NCCL concerns with training concerns. Adding more measurement types would bloat the package.

## References

- ADR-005 — LogProfile goodput measurement
- ADR-015 — auto-created GoodputMeasurement
- ADR-016 — NCCL all-reduce catalog entry
- `test/uat/aws-gb300-all-reduce.yaml` — NCCL UAT example
- `nccl.log` — sample NCCL all_reduce_perf output
