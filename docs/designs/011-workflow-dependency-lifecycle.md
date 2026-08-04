# ADR-011: Feature — Workflow Dependency Lifecycle Management

> **Superseded by:** [ADR-034](034-inferred-dependency-lifecycle.md) — Infer Dependency Scope and Ordering from References. The explicit `LifecycleSpec` (scope, createBefore, cleanup, crossRefNames) has been removed. Scope is now inferred from job template reachability, ordering from topological sort, and cross-references from shared strings. All dependencies are auto-cleaned up.

## Context

Training workloads often require prerequisite resources — PersistentVolumeClaims for checkpoint storage, ConfigMaps for training configuration, TrainingRuntimes for Kubeflow Trainer, ComputeDomains for multi-node GPU fabric scheduling. These resources must exist before the workload starts and may need to be cleaned up when the Workflow completes.

The question is how to manage these prerequisite resources: externally (user creates them), inline (Workflow spec embeds them), or as a separate dependency concept.

Options considered:
1. User creates prerequisites manually before submitting the Workflow
2. Init containers in the workload pods
3. Workflow-level dependency specs with lifecycle management

## Decision

Add a `dependencies` field to WorkflowSpec that accepts arbitrary Kubernetes resources as inline YAML. The Workflow controller creates these resources before the first Job starts and manages their lifecycle based on a configurable scope: `workflow` (delete on completion), `iteration` (delete between iterations), or `job` (scoped to individual Job lifecycle).

## Implementation

- **DependencySpec** (`api/v1alpha1/workflow_types.go`): `resource` (runtime.RawExtension containing any Kubernetes resource), `lifecycle` enum (workflow/iteration/job), and optional `name` override.
- **Custom JSON marshaling** (`api/v1alpha1/dependency_json.go`): `runtime.RawExtension` with `json:",inline"` loses sibling fields during marshal/unmarshal because the embedded type's MarshalJSON takes over. Custom `MarshalJSON`/`UnmarshalJSON` on DependencySpec merges the resource JSON with sibling fields.
- **Workflow controller** (`pkg/controller/workflow_controller.go`):
  1. Before creating the first Job, iterates over `spec.dependencies`
  2. Deserializes each resource using `runtime.Decode`
  3. Sets owner reference (with lifecycle-appropriate owner) and labels
  4. Creates via `client.Create()` with AlreadyExists handling
  5. On completion/deletion, cleans up resources based on lifecycle scope
- **Job-scoped dependencies**: Dependencies with `lifecycle: job` are owned by the Job resource. Their names are propagated into the Job's namespace with deterministic naming.

Supported dependency types include any Kubernetes resource — PVCs, ConfigMaps, Secrets, CRDs (ComputeDomain, ClusterTrainingRuntime), etc. The controller uses the dynamic client for creation, so it doesn't need to know the resource type at compile time.

## Rationale

- **Self-contained Workflows.** All prerequisite resources are declared in one place. A single `kubectl apply` sets up everything — no separate steps to create PVCs or ConfigMaps before submitting the Workflow.
- **Lifecycle scoping prevents resource leaks.** `workflow`-scoped dependencies are cleaned up automatically. `iteration`-scoped dependencies are recreated fresh each iteration (useful for ephemeral storage). `job`-scoped dependencies are tied to individual Job lifecycle.
- **Any Kubernetes resource.** Using `runtime.RawExtension` accepts any valid Kubernetes resource. No need to enumerate supported types — if it has a GVK, it can be a dependency.
- **Deterministic and idempotent.** AlreadyExists handling means re-reconciling a Workflow doesn't fail if dependencies already exist.

## Consequences

### Positive
- Single resource (Workflow) declares everything needed for a burn-in run
- Automatic cleanup prevents resource leaks (orphaned PVCs, stale ConfigMaps)
- Works with any Kubernetes resource type (including custom CRDs)
- Lifecycle scoping gives fine-grained control over resource lifespan

### Negative
- `runtime.RawExtension` with `json:",inline"` required custom JSON handling to preserve sibling fields
- Dependency creation adds latency before the first Job starts
- Dynamic client creation means type errors are caught at runtime, not compile time
- Owner reference-based cleanup doesn't work in envtest (no GC controller)

### Mitigations
- Custom `MarshalJSON`/`UnmarshalJSON` is tested and documented as a common pitfall
- handleDeletion explicitly deletes dependencies in tests (envtest workaround)
- AlreadyExists handling makes dependency creation idempotent
- Validation errors from the API server surface immediately on Create

## Alternatives Considered

### User creates prerequisites manually
**Rejected** because: Breaks the "single resource" model. Users must remember to create PVCs, ConfigMaps, etc. before submitting the Workflow. Cleanup is manual. Multi-iteration Workflows that need fresh resources per iteration would require external scripting.

### Init containers in workload pods
**Rejected** because: Init containers run per-pod, not per-Workflow. Creating a PVC in an init container means every pod tries to create it (race condition). Init containers can't create cluster-scoped resources. Lifecycle management (cleanup on completion) is not possible from within a pod.

### Separate PrerequisiteSet CRD
**Rejected** because: Adds another CRD to the hierarchy (already three tiers). The dependency list is tightly coupled to the Workflow spec (different workloads need different prerequisites). Separating them into a different resource adds indirection without clear benefit.

## Notes

- `runtime.RawExtension` with `json:",inline"` loses sibling fields during marshal/unmarshal — this is a known Go JSON limitation and requires custom `MarshalJSON`/`UnmarshalJSON` on the parent struct
- PV reclaim policy patching (Retain → Delete) is needed when cleaning up PVCs to prevent orphaned PVs
- Job-scoped dependency names must be propagated into the Workflow-scoped naming scheme to avoid collisions across groups

## References

- `api/v1alpha1/workflow_types.go` — DependencySpec type
- `api/v1alpha1/dependency_json.go` — custom JSON marshaling
- `pkg/controller/workflow_controller.go` — dependency creation and cleanup
