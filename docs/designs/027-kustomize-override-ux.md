# ADR-026: Kustomize-like Override UX

## Context

The Workflow override mechanism (ADR-012) allows conditional patching of `jobTemplate` and `dependencies` based on auto-detected platform and GPU architecture. The implementation works but has UX gaps compared to kustomize, the established standard for Kubernetes resource patching:

1. **Custom merge code.** The controller implements its own `mergeMaps()` and `mergeNamedSlices()` (~90 lines) instead of using Kubernetes' battle-tested `strategicpatch` library. This means merge behavior doesn't follow Kubernetes conventions (e.g., `patchStrategy:"merge" patchMergeKey:"name"` struct tags on `corev1.Container.Env` are ignored).

2. **No JSON Patch support.** Kustomize offers both `patchesStrategicMerge` (declarative merge) and `patchesJson6902` (precise operations like remove, add-at-index, test-before-replace). The controller only supports strategic merge. Users who need to remove a specific env var or conditionally replace a field based on its current value have no mechanism.

3. **No dry-run preview.** `kustomize build` renders the resolved output without applying. The controller applies overrides and creates Jobs in the same reconciliation cycle. Users cannot inspect the resolved spec before expensive GPU workloads start running.

4. **Zero observability.** Override application produces no logs, no events, and no status feedback. Users are blind to which overrides matched, which were skipped, and what changed.

5. **Incomplete WhenSpec.** The `WhenSpec` struct defines 6 condition fields but `matchesWhen()` only evaluates 2 (`Platform`, `GPUArchitecture`). The remaining 4 (`WorkloadKind`, `Topology`, `Config`, `Expression`) are silently ignored.

6. **First-node-only detection.** Platform and GPU architecture are detected from `nodes[0]` only. In heterogeneous clusters, the first node may not represent the majority.

## Decision

Adopt Kubernetes-standard libraries for merge operations, add RFC 6902 JSON Patch support, build an offline CLI tool for dry-run rendering, add comprehensive observability, complete the WhenSpec implementation, and add multi-node detection consistency.

## Implementation

### Standard Merge Libraries

Replace custom merge with `k8s.io/apimachinery/pkg/util/strategicpatch` (already a direct dependency via `k8s.io/apimachinery v0.35.0`) and `github.com/evanphx/json-patch/v5` (already an indirect dependency, promoted to direct).

**`mergeJobTemplate`** uses `strategicpatch.StrategicMergePatch(baseJSON, patchJSON, &JobTemplateSpec{})`. The library reads `patchStrategy` and `patchMergeKey` struct tags from embedded types: `corev1.Container.Env` merges by `name`, `VolumeMounts` by `mountPath`. Fields without tags (e.g., Kubeflow `Trainer.Env`) fall back to JSON merge patch behavior (lists replace), matching ADR-012's documented semantics.

**Dependency merging** uses scheme-aware strategic merge. `WorkflowReconciler.Scheme` (already available, with corev1, Kubeflow Trainer v2, and Training Operator types registered) looks up typed structs via `scheme.New(gvk)`. Known types get strategic merge with proper tags; unknown types fall back to `jsonpatch.MergePatch` (RFC 7386).

Custom `mergeMaps()` and `mergeNamedSlices()` (~90 lines) are deleted.

### RFC 6902 JSON Patch

Add `jobTemplatePatch` to `OverrideSpec`:

```go
type OverrideSpec struct {
    When             WhenSpec              `json:"when"`
    JobTemplate      *apiextensionsv1.JSON `json:"jobTemplate,omitempty"`
    JobTemplatePatch *apiextensionsv1.JSON `json:"jobTemplatePatch,omitempty"`
    Dependencies     []DependencySpec      `json:"dependencies,omitempty"`
}
```

`jobTemplatePatch` contains RFC 6902 operations applied AFTER the strategic merge. This mirrors kustomize's two-patch model. Example: test that `numNodes` is 8, then replace it with 16.

### CLI Dry-Run Tool

`tools/workflow-render/main.go` — a Cobra CLI that reads a Workflow YAML, loads mock nodes (by `--platform` + `--gpu-arch` flags or custom `--nodes` file), applies overrides using the same library code as the controller, and prints the resolved spec. Fully offline, no cluster access required.

Pre-built mock node files in `tools/nodes/` (aws-h100, aws-gb200, aws-gb300) are embedded via `//go:embed` for a self-contained binary. Each file contains 8 nodes with appropriate `spec.providerID`, `nvidia.com/gpu.product`, and `nvidia.com/gpu.clique` labels.

### Observability

**Events.** Add `record.EventRecorder` to `WorkflowReconciler`. Emit events for: override applied (Normal), override error (Warning), heterogeneous platform (Warning), no-op override (Warning). RBAC marker added for event creation.

**Status.** `AppliedOverride` struct (index, when summary, patches summary) stored in `OrchestrationStatus.AppliedOverrides`. Only populated on the authoritative `applyOverrides` call in `discoverAndPartition`.

**Logging.** Structured logs at Info level for matched overrides, V(1) for skipped overrides, summary line with match count.

### Complete WhenSpec

`OverrideContext` struct bundles all detected runtime values. `matchesWhen` evaluates all 6 fields:

- `Platform` and `GPUArchitecture`: existing (unchanged)
- `WorkloadKind`: string match against detected workload type (TrainJob, PyTorchJob, etc.)
- `Topology`: `matchesTopology()` checking mode (none/topology-aware) and domain count via `matchesIntSpec`
- `Config`: `matchesConfig()` doing recursive subset matching against `validation.performance.config`
- `Expression`: CEL evaluation using `cel-go` (already in go.mod) with variables for all context fields

### Multi-Node Detection Consistency

**Platform:** Fail the Workflow if target nodes report different platforms. Emit `HeterogeneousPlatform` warning event with per-platform counts.

**GPU Architecture:** Warn if heterogeneous, then filter the node list to only the primary architecture (first node's value). The filtered list is used for partitioning, ensuring Jobs only run on nodes with matching GPUs.

## Rationale

- **Standard libraries over custom code.** `strategicpatch` is the same library `kubectl apply` uses. It's thoroughly tested across the Kubernetes ecosystem. Our custom merge code handles the same cases but without the benefit of struct tag conventions.
- **Two-patch model matches kustomize.** Users familiar with kustomize will recognize `jobTemplate` (strategic merge) + `jobTemplatePatch` (JSON Patch) as the same pattern as `patchesStrategicMerge` + `patchesJson6902`.
- **Offline CLI tool, not controller status.** `kustomize build` is a CLI tool, not a controller feature. A controller-side dry-run would require pausing reconciliation, adding complexity. An offline tool is simpler and follows the established pattern.
- **Events for operator visibility.** Kubernetes events are the standard mechanism for reporting operational status. They appear in `kubectl describe`, integrate with monitoring systems, and auto-expire.
- **Fail on heterogeneous platform, filter on heterogeneous GPU.** Platform heterogeneity (e.g., AWS + on-prem nodes in same target set) is likely a misconfiguration and should fail fast. GPU heterogeneity is more common (mixed H100/A100 racks) and should be handled gracefully by filtering.

## Consequences

### Positive
- Merge behavior aligns with Kubernetes conventions (struct tags respected)
- Users can do precise array operations via JSON Patch
- Override application is visible through events, status, and logs
- All 6 WhenSpec fields work as documented
- CLI tool enables pre-flight validation of override configurations
- Heterogeneous clusters are detected and handled explicitly

### Negative
- `Trainer.Env` (Kubeflow type without struct tags) changes from named-merge to list-replacement. This matches ADR-012's documented "lists replace" semantics but differs from the custom implementation's undocumented named-merge behavior.
- CLI tool adds a build target and embedded files
- EventRecorder adds RBAC requirements and integration test setup

### Mitigations
- Existing catalog entries (Nemotron 15B) already replace the entire env array, so the merge behavior change is compatible
- Mock node files are small YAML files (~2KB each)
- FakeRecorder is available for integration tests

## Alternatives Considered

### Keep custom merge, only add observability
**Rejected** because the merge behavior should follow Kubernetes conventions. Using `strategicpatch` reduces maintenance burden and ensures predictable behavior for users familiar with `kubectl apply`.

### Use kustomize directly as a library
**Rejected** because kustomize operates on YAML files with overlay directories. Integrating it into a controller that patches in-memory structs would require converting to/from YAML, managing temporary overlay directories, and handling kustomize's file-based model. The underlying libraries (`strategicpatch`, `jsonpatch`) are the right level of abstraction.

### Add platform/gpuArchitecture fields to WorkflowSpec
**Rejected** for now. Auto-detection is a core design principle (ADR-012). The CLI tool addresses the dry-run use case with mock node files instead of requiring users to specify platform in the Workflow spec.

## Notes

- `k8s.io/apimachinery/pkg/util/strategicpatch` uses reflection to read struct tags at runtime. For the override use case (called 3 times per reconcile, not in a hot loop), this is negligible.
- `applyOverrides` is called 3 times per reconcile (lines 129, 164, 215 in `workflow_controller.go`). Only the 3rd call (in `discoverAndPartition`) has full detection context and emits events/populates status. The first two calls use partial context and only log.
- CEL expressions in `WhenSpec.Expression` are compiled on each evaluation. For the override use case (small expressions, low frequency), this is acceptable. Caching can be added if profiling shows a bottleneck.

## References

- ADR-012: Platform and GPU Architecture Overrides
- [Kubernetes Strategic Merge Patch](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/update-api-object-kubectl-patch/#use-a-strategic-merge-patch-to-update-a-deployment)
- [RFC 6902: JSON Patch](https://datatracker.ietf.org/doc/html/rfc6902)
- [RFC 7386: JSON Merge Patch](https://datatracker.ietf.org/doc/html/rfc7386)
- [kustomize patchesJson6902](https://kubectl.docs.kubernetes.io/references/kustomize/kustomization/patchesjson6902/)
- `k8s.io/apimachinery/pkg/util/strategicpatch`
- `github.com/evanphx/json-patch/v5`
