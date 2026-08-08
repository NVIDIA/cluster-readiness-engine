---
name: cre-test
description: Use when writing, running, or debugging tests in this repo. Covers the testutil.TestCaseParser golden-file pattern, integration tests, golden file regeneration, and the full verification suite.
---

# CRE Test Skill

## When to Use
- Writing new unit or integration tests
- Deciding whether to use testutil.TestCaseParser or plain table tests
- Regenerating golden files after an intentional change
- Running a specific test or the full suite
- Debugging a failing integration test

## Test Pattern Decision

**Always use `testutil.TestCaseParser`** (defined in `pkg/testutil/`) for tests in these packages — they already follow the pattern:
- `pkg/catalog/`, `pkg/controller/`, `pkg/report/`, `pkg/render/`, `pkg/goodput/`, `pkg/orchestration/`, `pkg/workload/`, `pkg/nodemonitor/cel/`, `pkg/certification/`, `pkg/platform/`

**Plain table-driven tests are acceptable only for genuinely trivial helpers** with no structured output worth snapshotting:
- `pkg/gpu/`, `pkg/naming/`, `pkg/nccl/`, `pkg/threshold/`, `pkg/numstr/`, `pkg/noderesults/`, `pkg/setup/`
- Nil-safety pins, concurrency guards, and single-value assertions anywhere

When in doubt: use `testutil.TestCaseParser`. It makes failures readable and lets you update expectations in one command instead of editing inline structs.

## testutil.TestCaseParser Pattern

Each test case is a directory under `pkg/<package>/testdata/<subdir>/`:
```
testdata/
  my-feature/
    my-case/
      input.yaml        # or input_*.yaml
      expected.json     # golden output
```

Minimal test:
```go
func TestMyFeature(t *testing.T) {
    p := &testutil.TestCaseParser{Subdir: "my-feature"}
    p.TestDir(t, func(tc *testutil.TestCase) error {
        // parse tc.Inputs["input.yaml"]
        // run your function
        b, err := json.MarshalIndent(result, "", "  ")
        if err != nil {
            return err
        }
        tc.Actual = string(b) + "\n"
        return nil
    })
}
```

See `pkg/catalog/gpu_defaults_test.go` as the canonical reference.

## Running Tests

```bash
# Full suite (unit + integration)
make test

# Single unit test
go test ./pkg/workload/ -run TestAdapterForSpec -v

# Single integration test case
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./cmd/integration/ -v -timeout 300s -count=1 \
  -run TestIntegration/reconcile/job-checkpoint-restart
```

## Golden File Regeneration

**Never regenerate blindly.** Before touching golden files:
1. Read the existing `expected.json`
2. Understand exactly which fields will change and why
3. Confirm the change is an expected consequence of the code change
4. Get explicit user permission

Then regenerate:
```bash
TESTUTIL_UPDATE_EXPECTED=true make test-integration
```

Or for a specific case:
```bash
TESTUTIL_UPDATE_EXPECTED=true KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./cmd/integration/ -v -timeout 300s -count=1 \
  -run TestIntegration/reconcile/<case-name>
```

After regeneration, **always review the diff** before considering the test complete. A golden file that auto-passes is not a test.

## Integration Test Structure

Each case under `cmd/integration/testdata/reconcile/<case-name>/`:
```
input_client_objects.yaml   # k8s objects to pre-populate
input_config.yaml           # waitFor condition + collect list
expected.json               # golden snapshot of collected resources
```

`input_config.yaml` shape:
```yaml
waitFor:
  kind: Workflow
  name: my-workflow
  namespace: default
  condition: InProgress
  reason: JobRunning
collect:
  - kind: Workflow
    name: my-workflow
    namespace: default
```

Use `generateNodes` in `input_config.yaml` instead of explicit Node objects when you only need uniform nodes:
```yaml
generateNodes:
  count: 2
  providerID: "gce://my-project/us-central1-a/instance-1"
  labels:
    nvidia.com/gpu.product: NVIDIA-GB200-NVL72
```

## Verification Suite (run in order)

```bash
make manifests generate   # after *_types.go changes
make lint-fix
make build
make test
```

Do not present failing tests to the user. Fix and re-run. Ask for help after two failed attempts.
