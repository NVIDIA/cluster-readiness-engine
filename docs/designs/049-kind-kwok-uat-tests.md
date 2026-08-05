# ADR-049: Kind + KWOK End-to-End UAT Tests with e2e-framework

> **Status:** Proposed

## Context

CRE has 78+ envtest-based integration tests with golden file comparison (ADR-014). These tests are fast (~2-3s startup) and thorough for controller logic. However, they have a critical gap: **workloads (TrainJob) are pre-created in test inputs, never verified as created by the Job controller**. The code path at `pkg/controller/job_controller.go:264-333` (`createWorkloadFromSpec()`) — which calls `adapter.Build()`, sets labels, sets OwnerReference, then `r.Create()` — is untested in integration. This has caused 2 regressions in the past sprint.

envtest lacks:

- **Garbage collection controller**: OwnerReference cascade deletion does not work, requiring explicit `handleDeletion()` workarounds.
- **Kubeflow Trainer and JobSet controllers**: No external controllers run, so even if a TrainJob is created, nothing processes it into Pods.
- **Kubelet / Scheduler**: Pods never run. Status must be pre-set in test inputs.

The existing `test/e2e/` directory has 2 Ginkgo smoke tests (controller pod running + metrics endpoint). These verify deployment health but test zero reconciliation logic. The existing `test/uat/` directory has manual YAML manifests for human-driven UAT.

## Decision

Add automated UAT tests in `test/uat/` using **Kind** (real cluster with GC and scheduler), **KWOK** (simulated GPU nodes with pod lifecycle), **Tilt** (infrastructure orchestration via `ncrectl`), **Kubeflow Trainer v2 + JobSet** (real controllers that create actual Pods from TrainJobs), and **`sigs.k8s.io/e2e-framework`** (programmatic Go tests).

Key design choices:

1. **Tilt orchestrates all infrastructure** — builds the controller image, loads it into Kind, builds ncrectl, installs KWOK + Prometheus CRDs, then runs `ncrectl setup init --image <local> --auto-approve` to install Trainer+JobSet, CRDs, controller, and LogProfiles.
2. **KWOK simulates GPU nodes** with `nvidia.com/gpu.product` labels and `spec.providerID` for platform detection. Nodes are defined in YAML and created per test — easy to update without code changes.
3. **KWOK auto-completes pods after 30s** via a custom Stage CRD that overrides the default `pod-complete` stage. No manual status updates needed.
4. **`ncrectl certification run --namespace default`** creates Certifications in tests — dogfoods the CLI for the full creation flow.
5. **YAML golden file comparison** — pod specs are compared against `expected_pods.yaml` and certification state against `expected_certification.yaml`. Volatile fields (timestamps, UIDs) are stripped. No `TESTUTIL_UPDATE_EXPECTED` — golden files are hand-authored and reviewed.
6. **Two-stage assessment** — Stage 1 captures pods while Certification is InProgress (before KWOK completes them). Stage 2 waits for Certification Succeeded and compares final state.
7. **CSP/HW directory hierarchy** — each test is colocated with its data at `test/uat/<csp>/<hw>/<workload>/`.
8. **`//go:build uat`** build tag separates these tests from the existing `e2e` tag.

## Implementation

### Full lifecycle under test

```
ncrectl certification run --category communication/nccl-all-reduce --namespace default
  └─ CRE: Certification → Workflow → Job → TrainJob (createWorkloadFromSpec)
      └─ Trainer: TrainJob → Secret + ConfigMap + JobSet
          └─ JobSet controller: JobSet → batch/v1 Jobs (launcher + node)
              └─ K8s Job controller: Jobs → Pods
                  └─ Scheduler: Pods → KWOK nodes
                      └─ KWOK: Running (instant) → Succeeded (30s)
                          └─ Status: Pods → Jobs → JobSet → TrainJob → Job → Workflow → Certification
```

### Directory structure

Tests are colocated with their data, organized by CSP, hardware, and workload:

```
test/uat/
    Tiltfile                                  # Tilt orchestration
    tilt/
        kwok-stage-pod-complete.yaml          # KWOK Stage: pods complete after 30s

    util/
        helpers.go                            # Shared: wait, compare, ncrectl, YAML apply

    aws/h100/nccl/
        nccl_test.go                          # TestMain + TestAWSH100NCCL
        testdata/
            nodes.yaml                        # Input: 2 KWOK AWS H100 GPU nodes
            expected_pods.yaml                # Golden: expected pod specs (YAML)
            expected_certification.yaml       # Golden: expected cert final state (YAML)
```

Adding a new CSP/HW combination is one directory:

```
    gcp/h100/nccl/                            # Future
        nccl_test.go
        testdata/
            nodes.yaml                        # providerID: "gce://..." → platform "gcp"
            expected_pods.yaml
            expected_certification.yaml
```

### Tiltfile

Tilt uses `local_resource` for all steps (no `k8s_yaml` — ncrectl manages K8s deployments):

```python
# 1. Build controller image + load into Kind
local_resource('build-image',
    cmd='cd ../.. && make docker-build IMG=... && kind load docker-image ...')

# 2. Build ncrectl binary
local_resource('build-ncrectl', cmd='cd ../.. && make build-ncrectl')

# 3. Install KWOK (via kubectl apply -f <release-url>)
local_resource('kwok-install', cmd='kubectl apply -f .../kwok.yaml && ...')
local_resource('kwok-stage-override', cmd='kubectl apply -f tilt/kwok-stage-pod-complete.yaml')

# 4. Install Prometheus Operator CRDs (for ServiceMonitor in embedded controller.yaml)
local_resource('prometheus-crds', cmd='kubectl apply --server-side -f .../stripped-down-crds.yaml')

# 5. ncrectl setup init (installs Trainer+JobSet, CRDs, controller, LogProfiles)
local_resource('ncrectl-setup',
    cmd='../../bin/ncrectl setup init --image <img> --auto-approve',
    resource_deps=['build-image', 'build-ncrectl', 'kwok-stage-override', 'prometheus-crds'])
```

### Test structure

Each test has a `TestMain` (for `e2e-framework` environment) and a single test function with two assessment stages:

```go
func TestAWSH100NCCL(t *testing.T) {
    feature := features.New("aws/h100/nccl-all-reduce").
        Setup(func(...) {
            // 1. Create KWOK nodes from testdata/nodes.yaml
            // 2. Run ncrectl certification run --namespace default
        }).
        Assess("Pods match expected spec", func(...) {
            // Wait for InProgress, capture pods, compare against expected_pods.yaml
        }).
        Assess("Certification succeeds", func(...) {
            // Wait for Succeeded, compare against expected_certification.yaml
        }).
        Feature()
    testenv.Test(t, feature)
}
```

Stage 1 captures pods while the Certification is InProgress — before KWOK completes them and they may be garbage collected. Stage 2 waits for the full status propagation chain to complete.

### Makefile targets

```makefile
make setup-test-uat     # Create Kind cluster, wait for nodes ready
make tilt-uat           # Interactive Tilt dashboard (dev)
make tilt-uat-ci        # Headless Tilt (CI)
make test-uat           # Full CI: Kind + Tilt + tests + teardown
make test-uat-run       # Run tests against existing cluster (dev iteration)
make cleanup-test-uat   # Delete Kind cluster
```

## Rationale

- **Tests the untested gap.** The full chain from Certification to Pod creation is exercised with real controllers and a real API server. The `createWorkloadFromSpec()` code path is now covered.
- **Dogfoods ncrectl.** The Tiltfile uses `ncrectl setup init` and tests use `ncrectl certification run` — the same commands users run. This catches CLI bugs alongside controller bugs.
- **Real GC validates OwnerReference cascade.** Kind's GC controller processes OwnerReference-based deletion, catching bugs that envtest cannot surface.
- **KWOK enables GPU simulation at zero cost.** Fake H100 nodes with 8 GPUs each require zero hardware. KWOK's Stage CRDs drive pod lifecycle naturally — no manual status updates.
- **YAML golden files catch regressions.** Pod specs and certification state are compared as YAML against hand-authored expected files. Unintended changes in image, args, volumes, or env break the test immediately.
- **YAML-driven nodes.** Node definitions are in `testdata/nodes.yaml` — easy to update providerID, labels, or GPU count without touching Go code.
- **Colocated tests.** Each `test/uat/<csp>/<hw>/<workload>/` directory contains the test and all its data. Adding a new combination is self-contained.
- **Premerge viable.** Target: ~4 minutes total (60s cluster + 90s Tilt + 90s test).

## Consequences

### Positive

- Catches workload creation regressions (the primary gap)
- Real pod specs verified against YAML golden files
- Real GC tests OwnerReference cascade deletion
- Controller binary + RBAC tested in production-like conditions
- ncrectl install and run paths exercised in CI
- Adding a new CSP/HW test is one directory with YAML + one Go file
- Two-stage assessment ensures pods are captured before potential cleanup

### Negative

- Slower than envtest (~4 min vs ~5s)
- Requires Docker, Kind, and Tilt in CI environment
- KWOK nodes do not run real containers (pod logs are empty)
- Prometheus Operator CRDs must be installed separately (embedded controller.yaml includes ServiceMonitor)

## Alternatives Considered

### Kyverno Chainsaw (declarative YAML tests)

**Rejected** because: No golden file support (partial match only). Cannot verify full Pod specs. Tests must be Go for ncrectl integration.

### Mock Trainer controller in envtest

**Rejected** because: Still no GC, no real RBAC validation, no binary-level testing. Pods would not exist to compare.

### Replace existing Ginkgo e2e tests

**Rejected** because: The 2 Ginkgo smoke tests in `test/e2e/` serve a different purpose (Deployment health + metrics). They stay as-is. UAT tests complement them.

### Manual status updates (no Trainer/KWOK)

**Rejected** because: Does not test the real Trainer → JobSet → Pod chain. Pod specs would not exist to compare.

### JSON golden files

**Rejected** because: YAML is more readable for Kubernetes resources and matches the format of the input test data (nodes.yaml). Easier to review and hand-author.

### TESTUTIL_UPDATE_EXPECTED env var for auto-regeneration

**Rejected** because: Golden files should be deliberately authored and reviewed, not auto-generated. Forces reviewers to understand what the expected output should be.

## Notes

- KWOK nodes must NOT have the `kwok.x-k8s.io/node` taint — only the annotation. KWOK identifies managed nodes by annotation, but pods must schedule freely on these nodes.
- Kubeflow Trainer v2.1.0 requires the JobSet controller v0.10.1. The `ncrectl setup init` command installs both from the Trainer kustomize overlay.
- The KWOK `pod-complete` Stage selects pods with `ownerReferences[].kind == Job`. This matches the batch/v1 Jobs created by the JobSet controller. The 30s delay gives CRE's health monitoring time to observe Running pods.
- `ncrectl certification run --namespace default` is used to avoid namespace creation issues. Tests run in the `default` namespace.
- `spec.providerID: "aws://us-east-1a/i-..."` triggers AWS platform detection in `workflow_detect.go:49`, which activates the EFA override in the NCCL catalog entry.
- Tilt `deps` for build resources are narrowly scoped (`cmd/manager/main.go`, `cmd/ncrectl/main.go`) to avoid rebuild loops between `build-image` and `build-ncrectl` — both depend on `internal/` which `make build-ncrectl` regenerates via `embed-ncrectl`.
- `make setup-test-uat` waits for Kind nodes to be Ready before Tilt starts deploying.

## References

- ADR-014: envtest Integration Tests with Golden Files
- ADR-003: Strongly-Typed Workload Adapter Pattern
- [sigs.k8s.io/e2e-framework](https://github.com/kubernetes-sigs/e2e-framework)
- [KWOK — Kubernetes WithOut Kubelet](https://kwok.sigs.k8s.io/)
- [KWOK Stage Configuration](https://kwok.sigs.k8s.io/docs/user/stages-configuration/)
- [Kind](https://kind.sigs.k8s.io/)
- [Tilt](https://tilt.dev/)
- [Kubeflow Trainer v2](https://github.com/kubeflow/trainer)
- [NVSentinel e2e testing](https://github.com/NVIDIA/NVSentinel) — reference for KWOK + Kind pattern
- `pkg/controller/job_controller.go:264-333` — the untested code path
- `internal/` — CLI used by Tiltfile and tests

## Change Log

| Date       | Author | Description                                                    |
|------------|--------|----------------------------------------------------------------|
| 2026-02-24 | —      | Initial draft                                                  |
| 2026-02-24 | —      | Revised: YAML golden files, colocated test dirs, two-stage assess, ncrectl --namespace default, Prometheus CRDs in Tilt, no TESTUTIL_UPDATE_EXPECTED |
