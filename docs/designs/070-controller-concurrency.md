# ADR-070: Bounded Concurrent Reconciliation

## Context

CRE registers six controllers: Job, Workflow, Certification,
GoodputMeasurement, BandwidthMeasurement, and WorkloadRun. None currently sets
controller-runtime's `MaxConcurrentReconciles` option, so each controller uses
the default of one worker. Reconcile requests for different objects handled by
the same controller are therefore processed sequentially.

This becomes a fleet-scale bottleneck. A certification can create many
Workflows and Jobs at once, but one slow reconcile delays every request queued
behind it. Tail latency consequently grows with the number of active objects
even when the controller Pod has spare CPU and memory.

CRE needs bounded concurrency with production-safe defaults. Operators also
need to tune those bounds for the API-server capacity and controller resources
available in a particular cluster. The configuration surface should remain
small: there is no current evidence that every controller needs an independent
setting.

## Decision

Configure two concurrency tiers:

| Tier | Controllers | Default |
|---|---|---:|
| Core | Job, Workflow, Certification, WorkloadRun | 10 |
| Measurement | GoodputMeasurement, BandwidthMeasurement | 5 |

The controller manager exposes two CLI flags:

- `--max-concurrent-reconciles`, default `10`, for core controllers.
- `--measurement-max-concurrent-reconciles`, default `5`, for measurement
  controllers.

Both values must be greater than zero. The manager validates them before
creating any controllers and exits with an actionable error when either value
is invalid.

The Helm chart exposes the same settings:

```yaml
manager:
  maxConcurrentReconciles: 10
  measurementMaxConcurrentReconciles: 5
```

The Deployment template renders both values as manager CLI arguments. This
keeps Helm and direct binary invocation on the same configuration path.

## Implementation

1. Add the two integer flags and positive-value validation in
   `cmd/manager/main.go`.
2. Add a `MaxConcurrentReconciles int` field to each reconciler. The manager
   passes the appropriate tier value when constructing each reconciler.
3. In every `SetupWithManager`, call `WithOptions` with
   `controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}`.
   A zero value on a reconciler constructed outside the production manager
   retains controller-runtime's normal manager-level/default behavior, while
   the production manager always supplies a validated positive value.
4. Add the two Helm defaults to `values.yaml` and render the matching flags in
   `templates/deployment.yaml`.
5. Add tests for defaults and invalid values, and verify that default and
   overridden Helm values render the expected Deployment arguments.
6. Document how operators tune concurrency and the API-server/resource tradeoff.

This change does not alter CRE CRDs, catalog output, or reconciliation
semantics for an individual object. controller-runtime's work queue continues
to serialize processing for the same object key while allowing different keys
to be processed concurrently.

## Rationale

- **Two tiers match the workload.** Core reconcilers mostly read and mutate
  Kubernetes objects. Measurement reconcilers additionally fetch and parse Pod
  logs, so a lower default limits log-server and API-server pressure.
- **Bounded workers reduce queue latency.** Multiple independent objects can
  progress without making concurrency unbounded.
- **One configuration path avoids drift.** Helm values become CLI arguments;
  the binary remains usable and tunable without Helm.
- **Explicit per-controller options are local and visible.** Each
  `SetupWithManager` declares its worker bound rather than relying on implicit
  controller-runtime defaults.
- **Positive validation prevents silent fallback.** controller-runtime treats
  non-positive values as unset. Rejecting them at the manager boundary avoids
  an operator believing a disabled or negative worker count took effect.

## Consequences

### Positive

- Independent Workflows, Jobs, Certifications, and WorkloadRuns can reconcile
  concurrently, reducing fleet-scale queue and tail latency.
- Measurement processing is concurrent but deliberately more constrained.
- Operators can lower concurrency on small control planes or raise it after
  observing sufficient controller and API-server headroom.
- Direct binary users and Helm users receive the same defaults and controls.

### Negative

- Higher concurrency increases burst load on the Kubernetes API server and the
  controller Pod. Operators must tune the values together with manager CPU and
  memory resources.
- Concurrent status updates across related resources may produce more
  optimistic-conflict retries. Existing reconciliation logic must continue to
  treat those conflicts as retryable.
- Two tiers cannot tune one controller independently. A future change may add
  per-controller controls if production evidence shows that this is necessary.

## Alternatives Considered

### Six independent CLI flags and Helm values

This offers maximum control but exposes six knobs without evidence that the
controllers within a tier need different worker counts. It increases chart,
CLI, documentation, and validation complexity. Two tiers provide the expected
operational distinction with a smaller interface.

### One concurrency value for all controllers

This is simpler, but measurement reconciliation performs Pod log reads and
parsing and should have a lower default than the core object controllers. A
single value either underutilizes core controllers or applies unnecessary load
through measurement controllers.

### Configure manager-level GroupKind concurrency

controller-runtime can select concurrency from manager configuration by
GroupKind. That relies on indirect string-keyed mapping and makes the setting
less visible at each controller registration. Explicit `WithOptions` calls are
type-local, follow the issue's proposed fix, and are easier to audit.

### Increase manager replicas

Leader election permits only one active manager, so additional replicas improve
availability but do not increase active reconcile throughput. Running multiple
active managers would introduce a much broader coordination design and is not
needed for this problem.

## Notes

- The existing mutexes protecting Job detector caches and measurement parser,
  sampling, and state maps remain required once multiple worker goroutines are
  active.
- Verification should include the Go race detector for controller packages in
  addition to the repository's standard generation, lint, build, and test
  commands.
- No integration golden-file regeneration is expected.

## References

- [Issue #138: No MaxConcurrentReconciles on any controller](https://github.com/NVIDIA/cluster-readiness-engine/issues/138)
- [controller-runtime controller.Options](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/controller#Options)
- [ADR-013: Prometheus Observability](013-prometheus-observability.md)
- [ADR-064: Helm Chart Distribution](064-helm-chart-distribution.md)
