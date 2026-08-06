# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
make manifests generate   # MUST run after editing *_types.go or kubebuilder markers
make test                 # Unit tests + integration tests (requires envtest)
make test-integration     # Integration tests only (envtest + golden files)
make lint                 # golangci-lint
make lint-fix             # Auto-fix lint issues
make build                # Build binary to bin/manager
make docker-build         # Build Docker image
make helm-package         # Package the Helm chart
```

Run a single unit test:
```bash
go test ./pkg/workload/ -run TestAdapterForSpec -v
```

Run a single integration test case:
```bash
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./cmd/integration/ -v -timeout 300s -count=1 -run TestReconcile/job-checkpoint-restart
```

Update golden files after intentional changes:
```bash
TESTUTIL_UPDATE_EXPECTED=true make test-integration
```

### UAT Tests (Kind + KWOK)

UAT tests validate rendered catalog output against golden files using a Kind cluster with KWOK-simulated GPU nodes. Requires: Docker, Kind, Tilt, Go 1.24+.

```bash
# Full lifecycle (CI):
make setup-test-uat     # Create Kind cluster
make tilt-uat-ci        # Deploy infra (Kubeflow Trainer, KWOK, controller) — headless
make test-uat-run       # Run UAT tests against deployed cluster
make cleanup-test-uat   # Delete Kind cluster

# One-shot (handles everything):
make test-uat
```

For development iteration (faster feedback loop):
```bash
make setup-test-uat              # Once
make tilt-uat                    # Interactive Tilt (keep in another terminal, hot-reloads)
make test-uat-run                # Run as needed
```

Run a single UAT test:
```bash
kind get kubeconfig --name cre-test-uat > /tmp/kind-uat.kubeconfig
KUBECONFIG=/tmp/kind-uat.kubeconfig NCRECTL=bin/ncrectl \
  go test -tags=uat ./test/uat/ -v -timeout 900s -count=1 -run TestAWSGB200NCCL
```

UAT golden files are in `test/uat/testdata/<csp>/<gpu>/nccl/expected_pods.yaml`. When catalog entries change, tests write `.actual` files on failure — review the diff, then copy `.actual` over the golden file if correct.

**Note**: If `tilt-uat-ci` fails with Kubeflow Trainer timeout, retry — it's a race condition on first deploy. The second run typically succeeds since Kubeflow is already running.

## Architecture

Kubebuilder-based Kubernetes controller for GPU cluster burn-in certification. Single binary, six reconcilers.

### CRD Hierarchy (Certification → Workflow → Job)

Follows the Deployment → ReplicaSet → Pod composition pattern:

- **Certification** creates one Workflow per category from the catalog. Auto-creates Remediation on failure. Workflow named `<certName>-<domain>-<variant>`.
- **Workflow** creates a single Job from `spec.jobTemplate`. Applies orchestration target, manages iterations, creates dependency resources. Job named `<workflowName>-job`.
- **Job** creates a workload (TrainJob) via the adapter pattern. Manages health monitoring, goodput measurement, checkpoint restart.
- **GoodputMeasurement** watches a Job, parses pod logs via LogProfile regex patterns, computes goodput ratio.
- **BandwidthMeasurement** watches a Job, parses NCCL log output, and computes per-bus bandwidth metrics.
- **Remediation** taints (`cre.nvidia.com/preflight-failed:NoExecute`), cordons, and sets conditions on failed nodes. Reverses on deletion.
- **LogProfile** (cluster-scoped) defines regex patterns with named capture groups for parsing training framework logs.

### Key Packages

- `pkg/workload/` — Adapter interface normalizing five training frameworks to `WorkloadPhase` (Running/Succeeded/Failed). `ForSpec()` factory selects adapter based on which `WorkloadSpec` field is set.
- `pkg/catalog/` — Maps `{domain, variant}` → WorkflowSpec builder via `init()` registration. Adding a category = one new Go file.
- `pkg/nodemonitor/` — `NodeFailureDetector` interface; the CEL implementation evaluates expressions against Node objects. Detectors are selected by which typed field is set on `Job.spec.nodeHealthMonitor` (same pattern as workload adapters), not by name lookup.
- `pkg/goodput/` — Log parsing (`parser.go`), goodput calculation (`calculator.go`), pod log reading (`reader.go` with `PodLogFetcher` interface for test injection).
- `pkg/orchestration/` — Node partitioning: simple (name-sorted chunking), topology-aware (greedy domain-based allocation), and bisection (used internally by `diagnose` for adaptive fault isolation).
- `pkg/gpu/` — GPU architecture detection and defaults (gpusPerNode, MNNVL).
- `pkg/naming/` — Resource naming conventions and helpers.
- `pkg/nccl/` — NCCL log parsing for bandwidth measurement results.
- `pkg/podlogs/` — Pod log fetching abstraction (`PodLogFetcher` interface).
- `pkg/podutil/` — Pod status inspection and readiness utilities.
- `pkg/controller/workflow_detect.go` — Auto-detects platform from `spec.providerID` and GPU architecture from `nvidia.com/gpu.product` label for override matching.

### Controller Patterns

- `setExclusiveCondition()` makes InProgress/Succeeded/Failed mutually exclusive at every tier
- Each tier uses `Owns()` for event-driven reconciliation; polling as safety net
- Configurable requeue intervals: tests use 1s, production uses 15s
- Child resource creation: deep-copy spec → set labels → `SetControllerReference()` → Create()
- Reason constants use tier-specific prefixes: `reasonWorkload*` (Job), `reasonJob*` (Workflow), `reasonWorkflow*` (Certification), `reasonRemediation*` (Remediation)

### Testing

Integration tests use envtest with golden file comparison in `cmd/integration/testdata/reconcile/`. Each test case is a directory with `input_client_objects.yaml`, `input_config.yaml`, and `expected.json`. `PodLogFetcher` interface enables deterministic goodput tests via `input_logs_*.txt` files.

## Critical Pitfalls

- **After modifying `*_types.go`**: Must run `make manifests generate` before anything else compiles (stale deepcopy). `make manifests` writes CRDs directly to `helm/cluster-readiness-engine/crds/` and RBAC to `helm/cluster-readiness-engine/templates/`.
- **Blank imports required**: `pkg/catalog` must be blank-imported in `cmd/ncrectl/main.go` and test suites to trigger `init()` registration.
- **envtest has no GC controller**: Cascade deletion via OwnerReference won't work in tests. Controllers must explicitly delete child resources in `handleDeletion()`.
- **`runtime.RawExtension` with `json:",inline"`**: Loses sibling fields during marshal/unmarshal. Requires custom `MarshalJSON`/`UnmarshalJSON` on the parent struct (see `api/v1alpha1/dependency_json.go`).
- **Test timeout is 10s**: If requeue interval > 10s, status update tests will time out.
- **`Trainer.NumProcPerNode`** is `*intstr.IntOrString` — use `intstr.FromInt32()`, not `*int32`.
- **Never edit auto-generated files**: `helm/cluster-readiness-engine/crds/*.yaml`, `helm/cluster-readiness-engine/templates/role*.yaml`, `helm/cluster-readiness-engine/templates/*_role*.yaml`, `helm/cluster-readiness-engine/templates/service_account.yaml`, `**/zz_generated.*.go`, `PROJECT`.
- **Never remove scaffold markers**: `// +kubebuilder:scaffold:*` comments are used by the CLI.

## Development Workflow (MANDATORY)

Every feature, behavioral change, or non-trivial modification MUST follow this process. Do not skip steps.

### 1. Plan Mode First

Always start new features or significant changes in **plan mode** (`EnterPlanMode`). Explore the codebase, understand the existing patterns, and design your approach before writing any code. Present the plan to the user for approval.

### 2. Write an ADR

Before implementing, write an Architecture Decision Record in `docs/designs/`. Follow the existing format (see ADR-002 through ADR-015):

```
docs/designs/NNN-short-description.md
```

Structure: Context → Decision → Implementation → Rationale → Consequences → Alternatives Considered → Notes → References

The ADR must be **approved by the user** before implementation begins. The ADR should read as a forward-looking design document — not a retroactive justification.

### 3. Implement

Write the code. Follow existing patterns (adapter pattern, `setExclusiveCondition()`, tier-specific reason constants, `Owns()` watches, etc.). Refer to the ADR and the codebase architecture below.

### 4. Update Everything

After implementation, update **all** affected artifacts — do not stop partway:

- **Tests**: Add/update integration test cases in `cmd/integration/testdata/reconcile/` with golden files
- **Golden files**: Run `TESTUTIL_UPDATE_EXPECTED=true make test-integration` to regenerate. **NEVER blindly overwrite golden files when tests are failing** — investigate and fix the code or test inputs first. Only regenerate golden files when the new output is intentionally correct. For new tests that require golden file generation, thoroughly review the generated `expected.json` before considering the test complete. **NEVER silently regenerate golden files during implementation** — always stop and ask the user for explicit permission before regenerating any golden file, explaining what changed and why. **Always diagnose first**: before updating any integration golden file, read the existing expected.json, understand what fields are changing and why, and confirm the change is an expected consequence of the code changes — never jump straight to regeneration.
- **Documentation**: Update `docs/` files if API or behavior changed
- **Site**: Update `site/content/docs/` pages if user-facing behavior changed
- **Samples**: Update `config/samples/` if new fields or resources were added

### 5. Run Verification Until Green

Run the full verification suite and **do not stop until all commands pass**:

```bash
make manifests generate    # Regenerate CRDs and DeepCopy (MUST run after *_types.go changes)
make lint-fix              # Auto-fix lint issues
make build                 # Verify compilation
make test                  # Unit + integration tests
```

If tests fail, fix the issue and re-run. Do not present failing tests to the user. If you are stuck after two attempts, stop and ask the user for guidance.

### 5b. Do Not Push Without Confirmation

**Never run `git push` unless the user explicitly asks.** After committing, wait for the user to confirm before pushing. This applies to all branches.

### 6. Ask When Unsure

If you are uncertain about:
- Which pattern to follow → Read the relevant ADR in `docs/designs/`
- How a controller works → Read the source and existing tests
- What the user wants → **Stop and ask**. Do not guess at requirements.

Never assume. A question costs seconds; a wrong implementation costs a rewrite.

## Testing Catalog Config Changes with ncrectl

When modifying catalog entries (`pkg/catalog/entries/`), use `ncrectl certification render` to verify the rendered Workflow manifests are correct before committing.

### Build ncrectl

```bash
go build -ldflags "-s -w" -o bin/ncrectl ./cmd/ncrectl/
```

### Create Test Certification YAMLs

Create a temp cert file per GPU architecture. Change `nvidia.com/gpu.product`, `enableMNNVL`, and `nodesPerJob` to match the target. Find valid `domain`/`variant` pairs from directory names under `pkg/catalog/entries/<domain>/<variant>/`.

```yaml
# /tmp/cert-gb300.yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-cluster-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB300  # NVIDIA-GB200, NVIDIA-GB300, or NVIDIA-H100-80GB-HBM3
  nodesPerJob: 8
  enableMNNVL: false  # GB200: true (MNNVL); GB300: true for intra-rack (MNNVL), false for inter-rack (RoCE); H100: false
  imagePullSecrets:
    - name: ngc-secret
  categories:
    - domain: training
      variant: nemotron5-8b
      options:
        maxSteps: 50
```

### Render and Verify

```bash
# Render only (works without a cluster, or when cluster arch doesn't match the cert)
./bin/ncrectl certification render --platform aws /tmp/cert-gb300.yaml

# Render + dry-run (validates resources against the live cluster API server;
# kubeconfig context MUST point to a cluster matching the cert's target architecture)
./bin/ncrectl certification render --platform aws /tmp/cert-gb300.yaml --dry-run
```

### What to Verify in Output

1. **Override tracking** — Check `ncrectl.nvidia.com/applied-overrides` annotation to confirm the right overrides matched, and `detected-gpu-architecture`/`detected-platform` are correct.

2. **Architecture-specific env vars** — EFA vars (`FI_EFA_USE_DEVICE_RDMA`, `LD_LIBRARY_PATH`, `PATH`, `OPAL_PREFIX`) should ONLY appear for GB200 and H100 on AWS (EFA interconnect), NOT for GB300 (RoCE).

3. **Resources by architecture**:
   - GB200: `hugepages-2Mi: 10256Mi`, `vpc.amazonaws.com/efa: 4`, `amazon-efa` hostPath volume
   - GB300: `roce-channel` resource claim with `roce.networking.k8s.aws`, NO hugepages, NO EFA
   - H100: `vpc.amazonaws.com/efa: 32`, NO hugepages, NO ComputeDomain

4. **Spot-check with grep**:
   ```bash
   # Should return NOTHING for GB300
   ./bin/ncrectl certification render --platform aws /tmp/cert-gb300.yaml 2>&1 | grep -E "FI_EFA|LD_LIBRARY|OPAL_PREFIX|hugepages"

   # Should show EFA vars and hugepages for GB200
   ./bin/ncrectl certification render --platform aws /tmp/cert-gb200.yaml 2>&1 | grep -E "FI_EFA|LD_LIBRARY|OPAL_PREFIX|hugepages"
   ```

### Override Semantics (Critical)

- **`jobTemplate`** in an override uses strategic merge patch. **Arrays (like `env`) are REPLACED entirely**, not merged by name. If an override sets `jobTemplate.spec.workload.trainJob.trainer.env`, it wipes out the base env and replaces with only the listed vars.
- **`jobTemplatePatch`** uses RFC 6902 JSON Patch. Use `op: add` with `path: /spec/workload/trainJob/trainer/env/-` to **append** env vars without replacing the existing list. The `-` means "end of array".
- Overrides are applied **in order** as listed in the YAML. A `jobTemplatePatch` must come after any `jobTemplate` override that sets the env array, otherwise the appended vars get wiped.

## Design Decisions

Architecture decision records are in `docs/designs/` (ADR-000 through ADR-069). Read these before making significant changes to understand why things are the way they are.
