# ADR-088: JUnit certification reports

## Context

CI systems such as GitLab display individual test results from JUnit XML.
Certification reports currently provide console and JSON output, requiring
consumers to maintain a converter.

## Decision

Add optional `--junit-file PATH` output to `certification run --wait` and
`certification report`. Use Go's `encoding/xml` and the existing report model.
Keep JSON output and the existing certification exit behavior.

## Implementation

- One suite per certification and one testcase per category.
- Successful categories pass; failed categories include native failure details.
- Running, unknown, empty, and incomplete reports include an error testcase.
- Include category runtime and report data, with XML escaping handled by Go.
- Failure to write a requested JUnit report fails the command.
- Exercise the writer with golden fixtures and verify the CLI's output flags.

## Rationale

The existing report already contains category results, durations, and failure
diagnostics. A native writer removes consumer-side conversion without adding
dependencies or changing Kubernetes resources.

## Consequences

Consumers can upload native XML directly to GitLab. A failed category can also
represent a startup error, so CI consumers still decide when a completed failure
is advisory. This change adds no advisory mode or new exit-code contract.

## Alternatives Considered

Keep a consumer-side JSON converter: duplicates reporting policy and XML logic.
Add a reporting dependency: unnecessary for this small XML format.

## Notes

The proposed scope and test fixtures were approved before implementation.

## References

- `pkg/report/report.go`
- `docs/cli-reference/certification.md`
- https://docs.gitlab.com/ci/testing/unit_test_reports/
- https://github.com/NVIDIA/cluster-readiness-engine/issues/446
