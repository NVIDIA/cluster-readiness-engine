# ADR-108: JUnit certification reports

> **Status:** Accepted

## Context

CI systems display individual test results from JUnit XML.
Certification reports currently provide console and JSON output, requiring
consumers to maintain a converter.

Once pipelines consume the XML, the mapping from certification state to JUnit
outcome is a compatibility surface. Changing which cases fail or error later
turns a green pipeline red, or the reverse. So the mapping has to agree with
the CLI's exit code from the start.

## Decision

Add optional `--junit-file PATH` output to `certification run --wait` and
`certification report`. Use Go's `encoding/xml` and the existing report model.
Keep JSON output and the existing certification exit behavior.

A category's testcase reports that category's own outcome. A certification-level
testcase appears only when the categories cannot carry the result on their own.
The XML never fails a run that the CLI exits 0 for.

## Implementation

One suite per certification and one testcase per category. Repeated categories
get `[2]`, `[3]` suffixes so CI keeps every result.

| Certification result | Category status | Testcase |
|---|---|---|
| any | `Succeeded` | passed |
| any | `Failed` | `<failure>` with the category's failure reason |
| `RUNNING` | not terminal | `<error>`, "Category did not complete" |
| `FAILED` | not terminal | `<failure>` with the Certification's own Failed condition message |

At most one certification-level testcase, named `certification`, is added:

- `FAILED` with no failed testcase: `<failure>` with the Certification's
  Failed condition message. This happens when the controller fails the
  certification before a category runs, for example when a Workflow fails
  validation or a category finds no matching nodes.
- `RUNNING` with no errored testcase: `<error>`. The categories have all
  finished and the Certification is finalizing, or a `--wait` timed out at
  that point.
- No categories and any other result: `<error>`.

`INCOMPLETE` adds no error. `report.Build` sets it when every category passed
but some targeted nodes were excluded, and the CLI exits 0, because the
excluded nodes were never tested. Each suite instead carries `<properties>`:
`result` always, and `excludedNodes` and `exclusionReason` when nodes were
excluded.

`CertReport` gains `failureReason`, the Certification's Failed condition
message, and `CategoryReport` gains `runtimeSeconds`. Both are additive in
`results.json`. A testcase's `time` comes from `runtimeSeconds`, not from the
`runtime` display string, which drops the seconds once a category runs past an
hour.

Testcase details hold the category's own JSON, not the whole report, so the
file grows linearly with the category count. The certification-level case
holds the full report once.

`certification run` rejects `--junit-file` or `--results-file` without
`--wait`, since without it the command returns before any report exists. It
rejects `--results-file` and `--junit-file` naming the same file before
creating the Certification. It checks again after writing the JSON and before
writing the XML, with `os.SameFile`, which catches two spellings of one file on
a case-insensitive filesystem or through a symlinked directory.

Failing to write either requested file fails the command, so a zero exit means
every requested file is there. `certification report` already failed on a JSON
write error; `certification run --wait` only warned, and now fails the same
way. A failed JSON write does not skip the JUnit write.

## Rationale

The existing report already contains category results, durations, and failure
diagnostics. A native writer removes consumer-side conversion without adding
dependencies or changing Kubernetes resources.

Marking a finished category as an error because a different category or the
certification as a whole did not finish attributes the problem to work that
succeeded. CI dashboards then show the wrong test as broken. Keeping each case
to its own category's outcome, and putting the certification-level outcome in
one case, says the same thing without relabelling.

A non-terminal category under a `FAILED` certification is a failure, not an
error. The certification stopped and that category will never run, and the
reason is a configuration problem the user has to fix. As an error with no
message, CI would show an infrastructure problem and hide the reason.

## Consequences

Consumers can publish test results directly without converting JSON.
Existing certification exit behavior is unchanged. The XML agrees with it:
`PASSED` and `INCOMPLETE` with at least one category have no failures or
errors, and `FAILED` and `RUNNING` always have at least one.

An `INCOMPLETE` run is green in CI. A consumer that wants exclusions to fail a
build reads the `result` or `excludedNodes` suite property.

## Alternatives Considered

Keep a consumer-side JSON converter: duplicates reporting policy and XML logic.

Add a reporting dependency: unnecessary for this small XML format.

Mark every category of an `INCOMPLETE` or `RUNNING` certification as an error:
the first version of this change did so. It failed CI for runs the CLI passes,
and attributed incompleteness to categories that succeeded.

Make `INCOMPLETE` an error in JUnit only: makes the XML stricter than the exit
code with no way for a user to see why the two disagree.

## References

- `pkg/report/junit.go`
- `pkg/report/report.go` (`Build`, the `INCOMPLETE` rule)
- `docs/cli-reference/certification.md`
- https://github.com/NVIDIA/cluster-readiness-engine/issues/446
