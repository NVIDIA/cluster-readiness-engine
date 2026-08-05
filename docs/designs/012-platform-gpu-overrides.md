# ADR-012: Feature — Platform and GPU Architecture Overrides

## Context

GPU clusters are heterogeneous. The same burn-in Workflow needs to run on AWS GB300 nodes with one NCCL plugin, GCP H100 nodes with another, and on-prem A100 nodes with different resource limits. Writing a separate Workflow per platform-GPU combination doesn't scale — a matrix of 5 platforms × 4 GPU architectures would require 20 Workflows for the same benchmark.

The controller needs a way to conditionally patch Workflow specs based on the runtime environment, without requiring users to detect the platform themselves.

Options considered:
1. Separate Workflow CRDs per platform
2. Helm chart values with platform-specific overlays
3. Inline overrides with auto-detected conditions

## Decision

Add an `overrides` field to WorkflowSpec. Each override has a `when` condition (platform and/or GPU architecture) and patches to `jobTemplate` and/or `dependencies`. The controller auto-detects platform from node `spec.providerID` and GPU architecture from `nvidia.com/gpu.product` labels, then applies matching overrides using JSON merge patch semantics.

## Implementation

- **Override API** (`api/v1alpha1/workflow_types.go`): `OverrideSpec` with:
  - `when`: `PlatformMatcher` (equals/in/notIn) and `GPUArchitectureMatcher` (equals/in/notIn)
  - `jobTemplate`: partial JobTemplateSpec to merge
  - `dependencies`: additional dependencies to append
- **Detection** (`pkg/controller/workflow_detect.go`):
  - Platform: Parse `node.Spec.ProviderID` prefix (e.g., `aws://...` → `aws`, `gce://...` → `gcp`, `azure://...` → `azure`). Nodes without providerID default to `onprem`.
  - GPU architecture: Read `nvidia.com/gpu.product` label, normalize to base model (e.g., `NVIDIA-H100-80GB-HBM3` → `h100`, `NVIDIA-L40S` → `l40s`, `NVIDIA-GB200` → `gb200`).
- **Override application** (`pkg/controller/workflow_controller.go`): Multiple overrides can match. They are applied in order — later overrides take precedence for conflicting fields. Maps merge recursively, lists replace entirely (JSON merge patch semantics).
- **Node filtering**: Only GPU-equipped nodes (`nvidia.com/gpu.present=true`) are considered for detection. This prevents non-GPU nodes from diluting the detected platform/architecture.

Golden file tests cover: single match, no match, multiple matches, combined platform+GPU conditions, each matcher type (equals, in, notIn), and all GPU architecture normalizations.

Example:
```yaml
overrides:
  - when:
      platform: { equals: "aws" }
      gpuArchitecture: { equals: "gb200" }
    jobTemplate:
      spec:
        workload:
          trainJob:
            trainer:
              env:
                - name: NCCL_NET_PLUGIN
                  value: "aws"
    dependencies:
      - resource:
          apiVersion: v1
          kind: ConfigMap
          metadata:
            name: aws-nccl-config
          data:
            plugin: "aws-ofi-nccl"
```

## Rationale

- **One Workflow, many environments.** A single Workflow definition can adapt to AWS, GCP, Azure, OCI, and on-prem deployments. Catalog entries (ADR-010) use overrides to ship one benchmark that works everywhere.
- **Auto-detection eliminates manual configuration.** Users don't need to know the platform or GPU architecture. The controller reads it from node metadata that's already present (providerID from kubelet, GPU labels from GPU Operator/device plugin).
- **JSON merge patch is predictable.** The patching behavior is well-defined — maps merge, lists replace. Users can reason about what the final spec will look like.
- **Composable with catalog.** Catalog entries define base specs with overrides. The Certification controller doesn't need platform awareness — the Workflow controller handles it.

## Consequences

### Positive
- Single Workflow definition works across heterogeneous clusters
- Platform and GPU detection is automatic (no user configuration)
- Override application order is explicit (array order = priority order)
- Dependency overrides can add platform-specific prerequisites

### Negative
- Multiple matching overrides can produce unexpected results if not carefully ordered
- GPU product label normalization may not cover all GPU models
- Auto-detection requires nodes to have providerID and GPU labels (not always present in dev environments)

### Mitigations
- Golden file tests for all matcher types and override combinations
- Unknown GPU products fall through to base spec (no override applied)
- Nodes without providerID default to `onprem`
- Documentation provides override examples for common platform-GPU combinations

## Alternatives Considered

### Separate Workflows per platform
**Rejected** because: Combinatorial explosion. 5 platforms × 4 GPU architectures = 20 Workflows for one benchmark. Maintaining 20 nearly-identical Workflows is error-prone. Changes to the base spec must be replicated across all variants.

### Helm chart platform overlays
**Rejected** because: Helm is a deployment-time tool. The platform is known at workload-time (when the Workflow is created), not at controller deployment time. A single controller deployment should handle any platform — the Workflow spec should be self-contained.

### Kustomize overlays
**Rejected** because: Same problem as Helm — overlays are applied before submission, not at runtime. The controller can detect the platform dynamically from node metadata, which is simpler than requiring users to select the right overlay.

## Notes

- GPU architecture normalization handles dashes and suffixes: `NVIDIA-H100-80GB-HBM3` → `h100`, `NVIDIA-H100-NVL` → `h100`, `NVIDIA-GB200` → `gb200`
- Override application happens before Job creation — the controller patches the spec, then creates the Job from the patched spec
- All matcher operations (equals, in, notIn) are case-sensitive

## References

- `api/v1alpha1/workflow_types.go` — OverrideSpec, PlatformMatcher, GPUArchitectureMatcher
- `pkg/controller/workflow_detect.go` — platform and GPU detection, matcher evaluation
- `pkg/controller/workflow_detect_test.go` — golden file tests
