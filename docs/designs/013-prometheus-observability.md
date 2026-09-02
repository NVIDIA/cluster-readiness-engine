# ADR-013: Architecture — Prometheus Metrics and Observability

## Context

The nvcre manages long-running certification campaigns. Operators need visibility into what's happening: how many nodes are being tested, which jobs are failing, what's the goodput trend, how long are reconciliations taking. This information must be available without reading CRD status fields via kubectl.

Options considered:
1. Status fields only (kubectl-based monitoring)
2. Structured logging with log-based alerting
3. Prometheus metrics on a standard endpoint

## Decision

Expose Prometheus metrics on the controller's HTTPS endpoint (`:8443`). Metrics cover job lifecycle, node health, goodput measurement, topology validation, and controller internals (reconciliation counts and duration). A standard `ServiceMonitor` enables Prometheus scraping.

## Implementation

- **Metrics registry** (`pkg/controller/metrics.go`): All metrics defined in one file using `prometheus.NewGaugeVec`, `prometheus.NewCounterVec`, and `prometheus.NewHistogramVec`. Registered with controller-runtime's metrics registry.
- **Job metrics**: `burnin_job_status` (gauge, labels: namespace, job, workflow, status), `burnin_job_failed_nodes` (gauge), `burnin_reconcile_total` (counter), `burnin_reconcile_duration_seconds` (histogram).
- **Goodput metrics**: `burnin_goodput_ratio` (gauge), `burnin_goodput_current_step` (gauge), `burnin_goodput_avg_step_time_seconds` (gauge, split into warmup-exclusive and global), `burnin_goodput_avg_tflops_per_gpu` (gauge).
- **Topology metrics**: `burnin_topology_validated_nodes` (gauge, per-workflow count of nodes in validated topology groups).
- **Metric cleanup**: On Job deletion, all associated metrics are removed using `DeletePartialMatch()` to prevent stale data from accumulating.
- **Workflow label**: All job-level metrics include a `workflow` label for grouping by Workflow.

The `ServiceMonitor` is enabled by default in the Helm chart (`config/prometheus/`). Operators with existing Prometheus installations get scraping automatically.

## Rationale

- **Prometheus is the Kubernetes standard.** Every production Kubernetes cluster has Prometheus or a compatible metrics backend. Using Prometheus means no custom monitoring infrastructure.
- **Gauge metrics for current state.** Burn-in is about current state (is this job running? what's the goodput now?), not historical counts. Gauges are the right metric type.
- **Single endpoint, no sidecars.** Metrics are served from the controller pod's existing HTTPS endpoint. No exporter sidecar, no additional deployment.
- **Metric cleanup prevents staleness.** Long-running controllers accumulate metrics from completed jobs. Cleanup on deletion keeps the metric set clean.

## Consequences

### Positive
- Standard Prometheus alerting and Grafana dashboards work out of the box
- Operators can alert on failed nodes, low goodput, stalled reconciliation, etc.
- ServiceMonitor auto-discovery means zero configuration for Prometheus Operator users
- Workflow label enables per-certification drill-down in dashboards

### Negative
- High cardinality risk if many jobs run with many node labels
- Metrics add memory overhead proportional to active jobs/nodes
- Prometheus endpoint requires TLS configuration (controller-runtime default)

### Mitigations
- Metric cleanup on deletion bounds cardinality to active resources
- Label set is bounded (namespace, job, workflow, status — not per-node metrics)
- TLS is handled by controller-runtime's default cert manager

## Alternatives Considered

### Status fields only
**Rejected** because: Requires kubectl access and scripting to monitor. No alerting. No historical trends. Operators managing large clusters need dashboards, not terminal commands.

### Structured logging with log-based alerting
**Rejected** because: Log-based monitoring requires a log aggregation pipeline (ELK, Loki). Not all clusters have this. Prometheus is more universally available in Kubernetes environments. Structured logging is still used for debugging but is not the primary monitoring surface.

### OpenTelemetry
**Rejected** because: OTel is a comprehensive observability framework but adds SDK dependencies and collector infrastructure. Prometheus metrics are simpler and sufficient for the controller's monitoring needs. OTel can be added later if needed (Prometheus metrics can be exported to OTel collectors).

## Notes

- Goodput metrics include both warmup-exclusive and global average step times — warmup-exclusive is the useful one for steady-state performance assessment
- The `burnin_goodput_ratio` metric accumulates across restarts to provide a holistic view of training efficiency
- Test golden files include metric output to catch metric regressions

## References

- `pkg/controller/metrics.go` — metric definitions
- `config/prometheus/` — ServiceMonitor configuration
