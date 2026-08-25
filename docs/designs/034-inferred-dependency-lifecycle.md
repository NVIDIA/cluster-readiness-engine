# ADR-034: Eliminate LifecycleSpec — Infer Dependency Scope and Ordering from References

> **Supersedes:** ADR-011 (internal; predates the public repository) (Workflow Dependency Lifecycle Management)

## Context

ADR-011 introduced `LifecycleSpec` with explicit `scope`, `createBefore`, `cleanup`, and `crossRefNames` fields on each dependency. In practice, this produces boilerplate: every job-scoped dependency must declare `scope: job`, `createBefore: job`, `cleanup: auto`, and `crossRefNames`. The information is redundant — the controller can infer all of it from the dependency graph itself.

Observations:
- **Scope**: A dependency is job-scoped if and only if it is reachable from the job template (directly or transitively through other deps). Everything else is workflow-scoped.
- **Cross-references**: Internal names shared across dependencies (e.g., a ComputeDomain channel name referenced by a TrainingRuntime) can be auto-detected by finding strings that appear in 2+ deps but aren't any dep's `metadata.name`.
- **Ordering**: A safe creation order is a topological sort of the inter-dependency reference graph.
- **Cleanup**: All dependencies should be auto-cleaned up. Users who want persistent resources should create them externally.

## Decision

Remove `LifecycleSpec` entirely from the `DependencySpec` API. Replace explicit declarations with reference-based inference using three algorithms:

1. **Classify**: Walk the job template's string values to find seed deps, then transitively promote deps that reference seeds.
2. **Detect cross-refs**: Find resource-name-shaped strings that appear in 2+ job-scoped deps but aren't any dep's `metadata.name`.
3. **Order**: Build a directed graph from inter-dependency references and topologically sort.

Always auto-cleanup all dependencies.

## Implementation

### Algorithm: Classify Dependencies

```
Seeds = deps whose metadata.name appears as a string value in jobTemplate.spec
Queue = Seeds
Visited = set(Seeds)

while queue not empty:
  dep = dequeue()
  for each string s in collectAllStrings(dep.Raw):
    if s is a dep metadata.name AND s not in visited:
      visited.add(s)
      enqueue(dep-for-s)

JobScoped = visited
WorkflowScoped = AllDeps - visited
```

Cluster-scoped resources (kind starting with "Cluster") are never classified as job-scoped because per-job copies would collide globally.

### Algorithm: Detect Cross-References

For all job-scoped deps, collect every string value that matches `resourceNameRegex`. Strings appearing in 2+ deps that aren't any dep's `metadata.name` are cross-references needing per-job suffixing.

### Algorithm: Order Dependencies

Build a directed graph: dep A depends on dep B if A's JSON contains B's `metadata.name`. Topologically sort to determine creation order. Independent deps share the same tier and can be created in any order.

### API Changes

- Remove `LifecycleSpec` struct from `workflow_types.go`
- Remove `Lifecycle` field from `DependencySpec`
- Remove `Cleanup` field from `DependencyResourceRef` (always auto)
- Keep `Scope`, `GroupName`, `Iteration` in `DependencyResourceRef` (internal tracking)

### Controller Changes

- `workflow_deps.go`: Replace `scopedDependencies()` and `promoteWorkflowDeps()` with `classifyDependencies()`, `orderDependencies()`, `detectCrossRefs()`
- `workflow_controller.go`: Update `ensureWorkflowDependencies()`, `ensureJobDependencies()`, cleanup functions, `handleDeletion()`, and `createDependencyResource()` to use inference instead of explicit lifecycle fields
- `workflow_detect.go`: Remove lifecycle field merging from `mergeOrAppendDependency()`
- `dependency_json.go`: Remove lifecycle from marshal/unmarshal

### Catalog Changes

Remove all `lifecycle` blocks from catalog YAML entries. The inference engine determines scope and ordering automatically.

## Rationale

- **Less boilerplate**: Catalog authors no longer need to declare `scope: job`, `createBefore: job`, `cleanup: auto`, and `crossRefNames` on every dependency. The graph structure encodes all of this.
- **Correct by construction**: The inference algorithm follows the actual reference graph, so cross-references are never missed and scope is always consistent with usage.
- **Simpler API surface**: Fewer fields means less documentation, fewer validation rules, and less surface area for misconfiguration.
- **Single behavior**: Always auto-cleanup eliminates the ambiguity of `cleanup: never` vs `cleanup: manual` and prevents resource leaks.

## Consequences

### Positive
- Simpler dependency declarations — just the resource YAML and optional `when` conditions
- No risk of mismatched scope/crossRefNames declarations
- Topological ordering prevents race conditions from unordered creation
- Consistent cleanup behavior (always auto)

### Negative
- False positive cross-refs for shared label values (e.g., `app: nemotron6-8b`). Harmless — per-group label values provide better isolation.
- Users who previously used `cleanup: never` must create those resources externally. Only one test case used this; no catalog entries do.
- Slightly more computation per reconcile (graph traversal). Negligible for typical dependency counts (< 10).

## Alternatives Considered

### Keep LifecycleSpec but make inference the default
**Rejected** because: Maintaining two code paths (explicit and inferred) doubles complexity. If inference is correct, the explicit fields add no value.

### Remove only crossRefNames but keep scope
**Rejected** because: Scope inference is the primary simplification. Cross-ref detection without scope inference would still require users to declare `scope: job` on each dep.

## Notes

- `cleanup: never` removal affects one integration test (`workflow-with-dependencies` where a TrainingRuntime had `cleanup: never`). The test is updated to always auto-cleanup.
- Namespace strings cannot cause false promotion because `metadata.namespace` values would need to match another dep's `metadata.name`, which is unlikely in practice.

## References

- ADR-011 (internal; predates the public repository) — original dependency lifecycle design (superseded)
- `api/v1alpha1/workflow_types.go` — DependencySpec type changes
- `pkg/controller/workflow_deps.go` — classification, ordering, and cross-ref detection
