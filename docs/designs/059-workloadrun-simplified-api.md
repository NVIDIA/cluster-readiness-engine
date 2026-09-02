# ADR-059: WorkloadRun — Simplified Workload Execution API

> **Status:** Proposed

## Context

Running workloads on NVCRE today requires creating a Certification with catalog domain/variant pairs. The catalog system is powerful for burn-in campaigns but too complex for ad-hoc workloads: users must know catalog entry names, all model configs are baked in, and there is no way to run custom containers with NVCRE's platform auto-detection.

An internal benchmark tool supports submitting arbitrary training jobs and NCCL tests with a simple interface — platform, container image, node count, and command. We need an equivalent Kubernetes-native path that:

1. Accepts a container image, framework (torch/MPI/exec), command, and node count.
2. Auto-detects CSP platform and GPU architecture from target nodes.
3. Auto-injects NCCL env vars, networking resources (EFA/RoCE/IB), and ComputeDomain+DRA.
4. Supports the same orchestration strategies as Certification (intra-node, intra-rack, full-scale).
5. Supports performance thresholds, goodput/bandwidth measurement, and per-node result tracking.

## Decision

Introduce a new CRD, **WorkloadRun**, that creates Workflows directly — bypassing Certification and the catalog. It is a peer resource to Certification in the composition hierarchy:

```
WorkloadRun  ──creates──>  Workflow  ──creates──>  Job  ──creates──>  TrainJob
Certification ──creates──>  Workflow  (unchanged)
```

### Framework Discriminator

WorkloadRun uses a `framework` discriminated union instead of a raw `command` field:

- **`torch`** — distributed training via torchrun. Auto-generates a PyTorch `TrainingRuntime` with `torch` mlPolicy, `numProcPerNode`, GPU resources, shared memory.
- **`mpi`** — MPI-based workloads (NCCL tests). Auto-generates an MPI `TrainingRuntime` with launcher+worker pattern, SSH auth, `IPC_LOCK`, readiness probes, and success policy.
- **`exec`** — arbitrary commands. Auto-generates a simple `TrainingRuntime` with a single replicatedJob.

### Platform Override Library

Extract model-independent, CSP/GPU-arch-only overrides from the existing `_lib/` templates into a new `pkg/platform/` Go package. These overrides cover:

- GB200/GB300 ComputeDomain + DRA
- GB200/GB300 topology key (`nvidia.com/gpu.clique`)
- AWS EFA resources, hugepages
- AWS GB300 RoCE (`roce.networking.k8s.aws`)
- AWS NCCL plugin cleanup
- GCP TCPxO/FastRak RDMA resources and annotations
- GCP GB200/GB300 RoCE
- Azure InfiniBand
- Mistral InfiniBand
- TogetherAI InfiniBand
- Auto-injected NCCL env vars per GPU architecture

Model-dependent settings (CUDA_DEVICE_MAX_CONNECTIONS, NVTE_*, framework-specific vars, parallelism) are the user's responsibility via the `env` field.

### nvcrectl Support

- `nvcrectl workloadrun render <file.yaml>` — offline or `--dry-run` rendering of the generated Workflow.
- `nvcrectl workloadrun run <file.yaml>` — create on cluster with `--setup`, `--wait`, `--cleanup`, `--image-pull-secret`.

## Implementation

### New Files

- `api/v1alpha1/workloadrun_types.go` — CRD types with kubebuilder markers.
- `pkg/platform/overrides.go` — `BuildOverrides()` returning `[]OverrideSpec`.
- `pkg/platform/nccl.go` — `NCCLEnvVars()` per GPU architecture.
- `pkg/platform/torch_runtime.go` — PyTorch `TrainingRuntime` builder.
- `pkg/platform/mpi_runtime.go` — MPI `TrainingRuntime` + SSH auth builder.
- `pkg/platform/exec_runtime.go` — Exec runtime builder.
- `pkg/controller/workloadrun_controller.go` — `WorkloadRunReconciler`.
- `pkg/workloadrun/workloadrun.go` — CLI render + run subcommands.

### Modified Files

- `cmd/manager/main.go` — register `WorkloadRunReconciler`.
- `cmd/nvcrectl/main.go` — add `workloadrun` subcommand group.
- `pkg/setup/setup.go` — WorkloadRun CRD in embedded CRDs.

### Controller Flow

1. Check for terminal state or existing Workflow; mirror status if present.
2. Discover target nodes via `discoverTargetNodes()` (filters to `nvidia.com/gpu.present=true`).
3. Detect platform and GPU architecture via existing detection functions.
4. Resolve `gpusPerNode` from user override or `gpu.DefaultGpusPerNode()`.
5. Build framework-specific dependencies (TrainingRuntime, SSH auth for MPI).
6. Build common dependencies (ConfigMap from inline config, PVC for checkpoint).
7. Build `WorkflowSpec` with orchestration, thresholds, measurement configs.
8. Attach platform overrides from `pkg/platform/`.
9. Append user's custom overrides.
10. Create Workflow with owner reference; update WorkloadRun status.

### Image Pull Secret Handling

Secret data never touches the CRD — only `[]LocalObjectReference` name references. Three paths:

- `nvcrectl --image-pull-secret=<NGC_KEY>` creates a docker-registry secret for nvcr.io.
- `nvcrectl --image-pull-secret-from-env=NGC_API_KEY` reads from environment variable.
- Pre-created secrets referenced by name in `spec.imagePullSecrets`.

## Rationale

- **Bypasses catalog, not Workflow**: WorkloadRun creates Workflows directly because the Workflow controller already handles node discovery, platform detection, override application, dependency creation, and job lifecycle. Duplicating that logic would be wasteful.
- **Framework discriminator over raw command**: The MPI launcher+worker pattern requires fundamentally different runtime generation (SSH, IPC_LOCK, success policy) than torchrun. A framework discriminator lets the controller generate the right runtime shape automatically.
- **Separate platform library over catalog reuse**: The catalog's `_lib/` templates are tightly coupled to the template engine and `TemplateData` shape. Extracting overrides into Go code is more testable, doesn't require template rendering, and avoids coupling WorkloadRun to the catalog system.
- **Peer to Certification**: Making WorkloadRun a peer (not a wrapper) to Certification keeps both resources simple and avoids circular dependencies.

## Consequences

### Positive

- Users can submit workloads in ~20 lines of YAML instead of ~200+ lines of catalog configuration.
- CSP/GPU adaptation is fully automatic — same override coverage as catalog entries.
- MPI workloads (NCCL tests) are first-class with auto-generated SSH auth infrastructure.
- Composes with existing Workflow/Job/measurement pipeline — no changes to existing controllers.
- Other benchmark tools can invoke `nvcrectl workloadrun run` for Kubernetes-based benchmarks.

### Negative

- Platform override library is a second source of truth alongside `_lib/` YAML templates. Changes to platform overrides must be made in both places until a future unification.
- Two paths to create Workflows (Certification and WorkloadRun) may require documentation clarity.

## Alternatives Considered

1. **"Custom" catalog entry** — A catalog entry that takes user-provided image/command but applies standard overrides. Rejected: couples WorkloadRun to the catalog template engine and `TemplateData` shape.
2. **Raw mode on Certification** — Add a `rawWorkload` field to CertificationSpec. Rejected: overloads Certification's purpose (burn-in validation, not ad-hoc execution).
3. **Direct Job creation bypassing Workflow** — Rejected: loses orchestration, health monitoring, override system, and measurement infrastructure.
4. **Two separate CRDs** (TrainingRun + CommTestRun) — Rejected: fragmentation; the framework discriminator handles both patterns cleanly.

## References

- Existing MPI runtime pattern: `pkg/catalog/entries/communication/nccl-all-reduce.yaml`
- Existing training override pattern: `pkg/catalog/entries/training/nemotron5-56b.yaml`
- Platform detection: `pkg/controller/workflow_detect.go`
