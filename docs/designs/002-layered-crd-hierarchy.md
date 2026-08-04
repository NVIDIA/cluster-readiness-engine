# ADR-002: Architecture — Layered CRD Hierarchy

## Context

GPU cluster burn-in requires orchestrating workloads at multiple levels of abstraction. A platform team doing push-button certification needs a different interface than an engineer debugging a single node. A single CRD cannot serve both without becoming unwieldy.

The core tension: a Certification suite runs multiple benchmark categories (NCCL, Nemotron pre-training, storage stress), each with its own iteration count, topology requirements, and health checks. But individual categories also need to be runnable standalone, and individual workloads need to be runnable independently for debugging.

Options considered:
1. Single monolithic CRD with nested specs for everything
2. Flat collection of independent CRDs with no hierarchy
3. Layered CRDs following the Kubernetes Deployment → ReplicaSet → Pod composition pattern

## Decision

Implement a three-tier CRD hierarchy — Certification → Workflow → Job — where each tier manages the lifecycle of the tier below it via owner references and status propagation.

## Implementation

- **Job** (`pkg/controller/job_controller.go`): Creates a single workload (TrainJob, PyTorchJob, etc.) via the adapter pattern. Manages health monitoring, goodput measurement, and checkpoint restart. Reports InProgress/Succeeded/Failed conditions.
- **Workflow** (`pkg/controller/workflow_controller.go`): Creates a single Job from `spec.jobTemplate`. Applies orchestration target (nodeSelector or nodeNames). Manages iterations and warmup. Creates dependency resources before the first Job. Propagates Job conditions up with `reasonJob*` prefixes.
- **Certification** (`pkg/controller/certification_controller.go`): Creates one Workflow per category from the catalog. Propagates Workflow conditions up with `reasonWorkflow*` prefixes. Auto-creates Remediation when Workflows fail with FailedNodes.

Child resource naming is deterministic:
- Workflow creates Jobs named `<workflowName>-job`
- Certification creates Workflows named `<certName>-<domain>-<variant>`
- Certification creates Remediation named `<certName>-remediation`

Status propagation uses `setExclusiveCondition()` to keep InProgress/Succeeded/Failed mutually exclusive at every tier. Each tier uses `Owns()` for event-driven reconciliation from child status changes.

## Rationale

- **Each tier has exactly one job.** Job runs one workload. Workflow orchestrates one Job across iterations. Certification composes Workflows from a catalog. No tier tries to do more than it should.
- **Users operate at their level.** Platform teams create a Certification. Engineers debugging a node create a Job directly. The middle tier (Workflow) serves benchmark runners who need iteration and topology without full certification.
- **Composition matches Kubernetes patterns.** Deployment → ReplicaSet → Pod is the established model. Users and controllers already understand owner references, status propagation, and cascade deletion.
- **Status is always readable at the top.** A Certification's conditions tell you the aggregate state without needing to inspect individual Workflows or Jobs.

## Consequences

### Positive
- Three natural entry points for three different personas
- Each controller is small and testable (~300 lines of reconciliation logic)
- Owner references give you cascade deletion for free (in production; envtest requires explicit cleanup)
- Adding a new tier (e.g., Campaign above Certification) requires no changes to existing tiers

### Negative
- Three CRDs to understand instead of one
- Status propagation adds latency (child update → watch event → parent reconcile)
- Reason constants must not collide across tiers (Job uses `reasonWorkload*`, Workflow uses `reasonJob*`, Certification uses `reasonWorkflow*`)

### Mitigations
- Configurable requeue intervals per tier (tests use 1s, production uses 15s) keep propagation fast
- Documentation and samples show entry points by persona
- Naming convention for reason constants prevents collisions

## Alternatives Considered

### Single monolithic CRD
**Rejected** because: A single CRD that handles certification categories, iterations, topology, health monitoring, and workload creation becomes a god object. The reconciler would be 1000+ lines. Testing individual behaviors in isolation would require setting up the entire state machine.

### Flat independent CRDs
**Rejected** because: Without a hierarchy, there is no automated composition. A user wanting to certify a cluster would need to manually create Workflows for each category and aggregate results themselves. The value of the Certification tier is exactly this automation.

### Two tiers (Certification → Job)
**Rejected** because: Iteration management, dependency creation, and topology application don't belong in the Job controller (Job is about running one workload). They also don't belong in the Certification controller (Certification is about composing categories). The Workflow tier exists because these concerns need a home.

## Notes

- envtest does not have a garbage collection controller, so `handleDeletion()` explicitly deletes child resources in tests
- The hierarchy is intentionally shallow (3 levels). Deeper hierarchies add latency and complexity without clear benefit.

## References

- Kubernetes Deployment → ReplicaSet → Pod pattern
- `api/v1alpha1/job_types.go`, `workflow_types.go`, `certification_types.go`
- `pkg/controller/job_controller.go`, `workflow_controller.go`, `certification_controller.go`
