# ADR-051: Tolerate All Taints and Avoid GPU Nodes for Controllers

> **Status:** Proposed

## Context

Customers have custom taints on their nodes (e.g., maintenance windows, security policies, compliance requirements) that prevent CRE and Kubeflow Trainer controller pods from scheduling. The controllers currently tolerate only a specific `dedicated=system-workload` taint, causing them to remain in Pending state on clusters with arbitrary taints.

Controller pods do not consume GPU resources and should run on infrastructure (non-GPU) nodes. Workload pods (TrainJob) already tolerate all taints via `operator: Exists` injected unconditionally by the Workflow controller (`workflow_controller.go:640-642`).

## Decision

Apply two scheduling rules to all controller Deployments (CRE, Kubeflow Trainer, JobSet):

1. **Tolerate all taints** — `operator: Exists` (no key specified) so controllers schedule on any node regardless of taints.
2. **Require non-GPU nodes** — `requiredDuringSchedulingIgnoredDuringExecution` node affinity with `DoesNotExist` on `nvidia.com/gpu.present`, ensuring controllers never consume GPU node capacity.

```yaml
tolerations:
  - operator: Exists
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: nvidia.com/gpu.present
              operator: DoesNotExist
```

## Implementation

Three deployment manifests are updated:

1. **`config/manager/manager.yaml`** — CRE controller (kustomize path)
2. **`pkg/setup/embedded/controller.yaml`** — CRE controller (nvcrectl embedded path)
3. **`pkg/setup/embedded/trainer.yaml`** — Kubeflow Trainer and JobSet controllers

Changes per manifest:
- Replace specific toleration entries with single `operator: Exists`
- Remove `nodeSelector: kubernetes.io/arch: amd64` (the nodeAffinity subsumes architecture concerns — GPU nodes are the only nodes to avoid)
- Add `affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution`

No changes to workloads — the Workflow controller already injects `operator: Exists` tolerations on all workload pods.

## Rationale

1. **`operator: Exists` without a key** is the standard Kubernetes pattern for "tolerate everything" — used by DaemonSets and critical system components. It eliminates the need to enumerate customer-specific taints.

2. **Node affinity with `DoesNotExist`** (not pod anti-affinity) is the correct primitive for "avoid nodes with a specific label." Pod anti-affinity is for spreading replicas, not for node type exclusion.

3. **`requiredDuringScheduling`** (hard requirement) was chosen over `preferredDuringScheduling` because controllers have no reason to run on GPU nodes and doing so would waste GPU capacity. If no non-GPU nodes exist, the controller pod stays Pending — which is the correct behavior (the cluster needs infrastructure nodes for control plane components).

## Consequences

- Controllers schedule on any node with arbitrary taints, resolving customer-reported scheduling failures.
- Controllers are guaranteed to never run on GPU nodes, preserving GPU capacity for workloads.
- In clusters with no non-GPU nodes, controllers will remain Pending. This is intentional — such clusters need at least one non-GPU node for infrastructure workloads.
- The `dedicated=system-workload` tolerations are removed. Clusters relying on this specific taint to schedule the controller continue to work because `operator: Exists` is a superset.

## Alternatives Considered

1. **`preferredDuringScheduling` instead of `required`** — Would allow controllers on GPU nodes as fallback. Rejected: wastes GPU capacity unnecessarily and indicates a misconfigured cluster.

2. **Keep specific tolerations + add more** — Enumerate known customer taints. Rejected: fragile, requires updates for each new customer taint.

3. **Use `nodeSelector` instead of `nodeAffinity`** — Cannot express "DoesNotExist" with nodeSelector. nodeAffinity is required for negative matching.

## References

- [Kubernetes Taints and Tolerations](https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/)
- [Assigning Pods to Nodes](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)
- `pkg/controller/workflow_controller.go:640-642` — existing workload toleration injection

## Change Log

| Date | Change |
|------|--------|
| 2026-02-25 | Initial proposal |
