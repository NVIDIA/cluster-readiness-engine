# ADR-006: Feature — Remediation Lifecycle

## Context

When burn-in workloads fail due to hardware issues, the affected nodes must be quarantined to prevent production workloads from scheduling on bad hardware. The quarantine must be reversible — when hardware is repaired and the node passes re-certification, it should return to the schedulable pool.

The Certification controller knows which nodes failed (from Workflow status). The question is how to manage the quarantine lifecycle: inline in the Certification controller, or as a separate resource.

Options considered:
1. Certification controller directly taints/cordons nodes
2. Separate Remediation CRD with its own controller and lifecycle
3. External webhook that receives failure events and manages nodes

## Decision

Implement a `Remediation` CRD with its own controller. The Certification controller creates a Remediation resource when Workflows fail with FailedNodes. The Remediation controller applies taints, cordons, and conditions to each listed node. All changes are reversed when the Remediation resource is deleted.

## Implementation

- **Remediation CRD** (`api/v1alpha1/remediation_types.go`): `spec.nodes` lists the nodes to quarantine. `status.nodeStatuses` tracks per-node remediation results.
- **Remediation controller** (`pkg/controller/remediation_controller.go`): For each node in `spec.nodes`:
  1. Applies taint `cre.nvidia.com/preflight-failed:NoExecute` (evicts existing pods)
  2. Sets `spec.unschedulable = true` (cordons the node)
  3. Sets node condition `cre.nvidia.com/PreflightCheckFailed`
  4. Records result in `status.nodeStatuses`
- **Finalizer-based cleanup**: On deletion, the controller reverses all changes — removes the taint, uncordons, removes the condition. The finalizer prevents the CR from being deleted before cleanup completes.
- **Certification integration**: Certification auto-creates Remediation named `<certName>-remediation`. When the Remediation is deleted (e.g., after hardware repair), the operator can re-run certification.

Node modifications use `client.MergeFrom()` for spec changes (taints, unschedulable) and `r.Status().Patch()` for condition changes, ensuring atomic updates that don't conflict with other controllers modifying the same node.

## Rationale

- **Separation of concerns.** Detection (Certification/Workflow/Job) and remediation are different problems with different owners. A hardware team might want to inspect Remediation resources without understanding Certification. A platform team might want to create Remediations manually for known-bad nodes.
- **Reversible by design.** Deleting the Remediation resource restores the node. This is simpler and more auditable than a manual "un-quarantine" API.
- **Per-node tracking.** `status.nodeStatuses` shows exactly what was done to each node, useful for debugging and audit.
- **NoExecute taint.** Evicts existing pods from the node immediately, not just preventing new scheduling. This is the strongest quarantine signal.

## Consequences

### Positive
- Remediation is a first-class Kubernetes resource — visible via `kubectl get remediations`, auditable, versionable
- Deletion reversal makes the repair workflow intuitive: fix hardware → delete Remediation → re-certify
- Other systems (NVSentinel, platform validators) can create Remediations independently
- Per-node status tracking provides clear audit trail

### Negative
- Three separate node modifications per node (taint, cordon, condition) — partially applied state if the controller crashes mid-operation
- The Remediation controller modifies node objects, which is a cluster-wide permission
- Deleting a Remediation while hardware is still faulty will return a bad node to the pool

### Mitigations
- Finalizer ensures cleanup runs before deletion completes
- Each node modification is idempotent — retrying a partially applied remediation converges to the correct state
- Documentation warns that Remediation deletion should follow hardware repair, not precede it
- RBAC can restrict who can delete Remediation resources

## Alternatives Considered

### Inline remediation in Certification controller
**Rejected** because: Mixes orchestration concerns with node management. The Certification controller would need node modification permissions, cleanup logic, and per-node status tracking — doubling its complexity. Remediation couldn't be used independently of Certification.

### External webhook
**Rejected** because: Adds an external service dependency. The webhook must be highly available (a down webhook means no quarantine). Kubernetes-native CRDs are simpler, more reliable, and work with standard tooling (kubectl, GitOps, RBAC).

### Taint-only (no cordon or condition)
**Rejected** because: Taints alone don't prevent DaemonSet pods from scheduling (DaemonSets tolerate taints by default). Cordoning adds a second layer. The node condition provides a human-readable signal visible in `kubectl describe node`.

## Notes

- Reason constants use `reasonRemediation*` prefix to avoid collisions with other controllers
- Node conditions are patched via `r.Status().Patch()`, not `r.Patch()` — conditions live in node status, not spec
- envtest can create node objects — tests use `makeNode(name)` helper

## References

- `api/v1alpha1/remediation_types.go`
- `pkg/controller/remediation_controller.go`
