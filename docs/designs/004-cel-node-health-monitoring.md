# ADR-004: Feature — CEL-Based Node Health Monitoring

## Context

Burn-in workloads run for hours or days. Hardware failures — GPU faults, NVLink degradation, thermal events — can occur mid-run. The controller needs to detect these failures during workload execution, not just at completion.

GPU health monitoring is already handled by specialized tools like NVSentinel (DCGM telemetry, Xid events) and gpud (GPU metrics daemon). These tools write signals to Kubernetes node objects as conditions, taints, or labels. The cluster-readiness-engine should consume these signals rather than re-implementing GPU diagnostics.

The challenge: different deployments use different health tools with different signal formats. NVSentinel sets `NVSentinelGPUHealthy` conditions, gpud might set custom labels, and AKS NPD sets GPU-specific conditions. The detection logic must be configurable without code changes.

Options considered:
1. Hardcoded checks for specific condition types
2. Webhook-based external health evaluation
3. CEL expressions evaluated against node objects

## Decision

Use CEL (Common Expression Language) expressions evaluated against the full Kubernetes Node object. Users define a single `celExpression` in the Job spec that returns true when a node is unhealthy. The expression has access to the entire node (metadata, spec, status).

## Implementation

- **API** (`api/v1alpha1/job_types.go`): `HealthMonitoringSpec` with `celExpression` string field
- **Detector interface** (`pkg/nodemonitor/interface.go`): `NodeFailureDetector` with `Detect(ctx, node) → DetectionResult`
- **Registry** (`pkg/nodemonitor/registry.go`): Pluggable detector factory registry. Built-in `cel` detector registered by default.
- **CEL detector** (`pkg/nodemonitor/cel/detector.go`): Compiles CEL expression once at setup, evaluates against each node on every reconcile cycle
- **Node discovery** (`pkg/nodemonitor/nodes.go`): Finds nodes running Job pods via field index on `spec.nodeName` and label index on `cre.nvidia.com/job`

The Job controller:
1. On each reconcile, looks up pods with the `cre.nvidia.com/job` label
2. Finds the nodes those pods are running on (via `spec.nodeName` field index)
3. Evaluates the CEL expression against each node
4. If any node fails, sets `HardwareFailed` condition on the Job (independent of workload execution state)
5. Reports failed node names in the condition message

Node watches use a predicate that filters for health-relevant changes (taints, conditions, labels, cordoning) to avoid unnecessary reconciliation on unrelated node updates.

Example CEL expressions:
```yaml
# Detect NVSentinel GPU health condition
celExpression: >-
  node.status.conditions.exists(c,
    c.type == 'NVSentinelGPUHealthy' && c.status == 'False')

# Detect cordon state
celExpression: "node.spec.unschedulable == true"

# Detect specific taint
celExpression: >-
  node.spec.taints.exists(t,
    t.key == 'gpu.health/error' && t.effect == 'NoSchedule')
```

## Rationale

- **Kubernetes-native integration.** The node object is the shared interface between health tools and the controller. No custom APIs, no direct tool integration, no coupling.
- **CEL is a Kubernetes standard.** Used in ValidatingAdmissionPolicy, Gateway API, and other Kubernetes projects. Operators familiar with Kubernetes already know CEL.
- **Single expression, full node access.** One field covers conditions, taints, labels, and cordoning. No need for separate configuration for each signal type.
- **Pluggable registry.** The detector interface allows adding non-CEL detectors (e.g., direct DCGM API calls) without changing the controller.

## Consequences

### Positive
- Works with any health tool that writes to node objects (NVSentinel, gpud, NPD, custom DaemonSets)
- No coupling between the cluster-readiness-engine and health tools
- CEL expressions are sandboxed — cannot execute arbitrary code
- Expressions can be tested independently against node YAML
- HardwareFailed condition is independent of workload state (a node can fail while the workload is still running)

### Negative
- CEL expression syntax has a learning curve
- Expression errors are only caught at runtime (though `Validate()` is called at setup)
- Evaluating expressions against every node on every reconcile adds CPU overhead
- No built-in library of common expressions (users must write their own)

### Mitigations
- Documentation includes a library of common CEL patterns for NVSentinel, cordon detection, and taint matching
- CEL compilation happens once at detector creation; evaluation is fast
- Node watch predicates filter out irrelevant changes, reducing reconciliation frequency
- Field indexes on `spec.nodeName` and pod labels make node discovery efficient

## Alternatives Considered

### Hardcoded condition checks
**Rejected** because: Different deployments use different health tools with different condition types. Hardcoded checks require code changes for each new tool. This approach works for a single integration but doesn't scale.

### Webhook-based external evaluation
**Rejected** because: Adds an external service dependency and network latency for every health check. The webhook must be highly available (a down webhook blocks failure detection). CEL evaluation is local and fast.

### Polling external APIs (DCGM, vendor metrics)
**Rejected** because: Requires direct integration with each health tool. The node object already aggregates health signals from all tools. Reading the node is simpler and more composable than polling multiple APIs.

## Notes

- The `cre.nvidia.com/job` pod label is auto-injected by the workload adapter (ADR-003), so users don't need to configure it manually
- Field indexes for `spec.nodeName` and the pod label must be set up in `main.go` for efficient lookups
- The HardwareFailed condition is set alongside InProgress/Succeeded/Failed — it's an orthogonal signal, not a replacement for workload status

## References

- `pkg/nodemonitor/interface.go` — detector interface
- `pkg/nodemonitor/cel/detector.go` — CEL implementation
- `pkg/nodemonitor/registry.go` — detector factory registry
- `pkg/nodemonitor/nodes.go` — node discovery via field indexes
- CEL specification: https://github.com/google/cel-spec
