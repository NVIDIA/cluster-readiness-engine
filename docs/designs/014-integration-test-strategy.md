# ADR-014: Testing — envtest Integration Tests with Golden Files

## Context

The nvcre has five reconcilers, each with complex state machines. Unit tests for individual functions aren't sufficient — the interactions between reconcilers, the Kubernetes API, and external resources (nodes, workloads) need end-to-end verification.

Ginkgo/Gomega with inline assertions is the standard approach for Kubebuilder controllers. However, for this project it has drawbacks: tests are verbose (~300 lines per scenario), assertions are fragile (checking individual fields rather than full state), and adding a new test requires significant boilerplate.

Options considered:
1. Ginkgo/Gomega inline assertion tests
2. envtest with golden file comparison
3. Real cluster integration tests (kind/k3s)

## Decision

Replace Ginkgo tests with envtest-based integration tests using golden file comparison. Each test case is a directory containing input fixtures (YAML) and expected output (JSON). The test framework reconciles the inputs and compares the resulting cluster state against the golden file.

## Implementation

- **Test suite** (`cmd/integration/integration_test.go`): `IntegrationTestSuite` that:
  1. Starts envtest (API server + etcd)
  2. Installs all CRDs (burn-in + external workload types from `config/crd/external/`)
  3. Starts all five reconcilers with configurable requeue intervals (1s for tests)
  4. For each test case directory:
     - Reads `input_client_objects.yaml` (resources to create)
     - Reads `input_config.yaml` (test configuration: which resource to reconcile, timeout)
     - Optionally reads `input_crd_*.yaml` (additional CRDs to install)
     - Optionally reads `input_logs_*.txt` (fake pod logs for goodput tests)
     - Creates resources, waits for reconciliation, captures cluster state
     - Compares against `expected.json` (golden file)
  5. Supports `TESTUTIL_UPDATE_EXPECTED=true` to regenerate expected files

- **Golden file format**: JSON snapshot of all relevant resources after reconciliation. Includes the reconciled resource, any child resources created, node modifications (for Remediation), and Prometheus metrics.

- **PodLogFetcher interface** (`pkg/goodput/reader.go`): Dependency injection point. Production uses Kubernetes pod log API. Tests inject fake log data from `input_logs_*.txt` files.

- **Test case directories**: Organized by controller and scenario:
  ```
  cmd/integration/testdata/reconcile/
  ├── certification/
  ├── goodput-measurement/
  ├── job/
  ├── job-auto-goodput/
  ├── job-checkpoint-restart/
  ├── job-stall-detected/
  ├── remediation/
  ├── workflow/
  ├── workflow-overrides/
  ├── workflow-topology/
  └── workflow-with-dependencies/
  ```

## Rationale

- **Golden files are complete.** A golden file captures the entire state after reconciliation — not just the fields you remembered to assert. New fields added to types are automatically included in the snapshot, catching unintended changes.
- **Test cases are data, not code.** Adding a new scenario means creating a directory with YAML and JSON files. No Go code to write. This lowers the barrier to adding test coverage.
- **Diffs are readable.** When a golden file comparison fails, the diff shows exactly what changed in the full state. This is easier to debug than "expected X but got Y" for a single field.
- **TESTUTIL_UPDATE_EXPECTED makes refactoring safe.** When intentional changes affect test output, `TESTUTIL_UPDATE_EXPECTED=true` regenerates all golden files. The diff in the commit shows exactly what changed.

## Consequences

### Positive
- Full-state comparison catches unintended side effects
- Adding test cases is fast (YAML/JSON files, no Go code)
- Golden file diffs in PRs show the complete impact of code changes
- PodLogFetcher injection enables deterministic goodput tests
- Requeue interval of 1s keeps tests fast (10s timeout is sufficient)

### Negative
- Golden files are large (100-300 lines of JSON per scenario)
- envtest does not have a garbage collection controller — cascade deletion via OwnerReference doesn't work
- Golden files can be brittle — unrelated changes (timestamp formatting, default field values) can break multiple tests
- envtest startup adds ~5s to test execution

### Mitigations
- JSON comparison ignores timestamps, UIDs, and resource versions (sanitized before comparison)
- handleDeletion explicitly deletes child resources (envtest workaround)
- `TESTUTIL_UPDATE_EXPECTED=true` regenerates all files at once
- CRDs for external types (TrainJob, PyTorchJob, etc.) are stored in `config/crd/external/` for envtest

## Alternatives Considered

### Ginkgo/Gomega inline assertions
**Rejected** because: Tests would be 300+ lines each with individual field assertions. Adding a new field to a type wouldn't break any test (false safety). Test logic obscures what is being tested. Golden files capture full state with less code while providing broader coverage.

### Real cluster tests (kind/k3s)
**Rejected** because: Real clusters require workload framework operators to be installed. Test execution takes minutes instead of seconds. CI environments may not support nested virtualization (needed for kind). envtest provides a real API server without the overhead of a full cluster.

### Mock-based unit tests
**Rejected** because: Mocking the Kubernetes client hides real API behavior (validation rules, status subresource, field defaults). envtest uses a real API server, so tests exercise the actual API surface.

## Notes

- Field indexes for `spec.nodeName` and `metadata.labels.nvcre.nvidia.com/job` must be registered in the test suite for pod lookups to work
- Test timeout is 10s — if requeue interval > 10s, status update tests will time out
- Blank imports of `pkg/catalog` are needed in `suite_test.go` to trigger `init()` registration
- Goodput ratio comparison should sanitize floating-point values to handle non-determinism across runs

## References

- `cmd/integration/integration_test.go` — test suite and framework
- `cmd/integration/testdata/` — golden file test cases
- `config/crd/external/` — CRDs for external workload types
