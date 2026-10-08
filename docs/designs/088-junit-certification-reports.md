# ADR-088: JUnit certification reports

## Context

CI systems display individual test results from JUnit XML.
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
- Include category runtime and failure diagnostics, with XML escaping handled by Go.
- Failure to write a requested JUnit report fails the command.
- Exercise the writer with golden fixtures and verify the CLI's output flags.

## Rationale

The existing report already contains category results, durations, and failure
diagnostics. A native writer removes consumer-side conversion without adding
dependencies or changing Kubernetes resources.

## Consequences

Consumers can publish test results directly without converting JSON.
Existing certification exit behavior is unchanged.

## Alternatives Considered

Keep a consumer-side JSON converter: duplicates reporting policy and XML logic.
Add a reporting dependency: unnecessary for this small XML format.

## References

- `pkg/report/report.go`
- `docs/cli-reference/certification.md`
- https://github.com/NVIDIA/cluster-readiness-engine/issues/446
