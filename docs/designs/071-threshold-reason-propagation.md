# ADR-071: Threshold Violation Reason Propagation into Report Surfaces

> **Status:** Accepted
> **Issue:** #176

## Context

ADR-021 introduced performance threshold enforcement: after a workload succeeds, the Job controller evaluates each `spec.thresholds` CEL expression against measured values via [`threshold.EvaluateAll`](../../pkg/threshold/evaluator.go#L75). Every `Violation` carries the full diagnostic triple — `Key`, `Measured`, `Expression` — pre-formatted into a single message:

```
Threshold "goodputRatio" violated: measured 0.8800, expression: value >= 0.95
```

That detail is already recorded in two places that outlive the run:

1. **The Job CR.** [`setJobValidationStatus`](../../pkg/controller/job_controller.go#L684) sets the `ValidationFailed` condition with reason `ThresholdViolated` and the full message, and populates `job.status.failedNodes` with `{name, reason: ThresholdViolation, message}` entries for every group node ([job_controller.go:688](../../pkg/controller/job_controller.go#L688)). Jobs are deliberately **not** deleted at Workflow terminal state — [`setFinalStatus`](../../pkg/controller/workflow_controller.go#L1709) documents that Jobs survive until `handleDeletion()` runs when the Certification is torn down, precisely so the report can read them.
2. **The failed-nodes ConfigMap.** When a group's Job goes terminal, the Workflow controller copies `job.status.failedNodes` into the gzip-compressed `failed-nodes.json.gz` ConfigMap ([`recordFailedNodes`](../../pkg/controller/node_results.go#L102), per ADR-061/062), referenced by `workflow.status.failedNodesRef` and mirrored to `certification.status.categoryStatuses[].failedNodesRef` ([certification_controller.go:295](../../pkg/controller/certification_controller.go#L295)) and `workloadRun.status.failedNodesRef`. Reason and message are preserved verbatim.

The report generator drops all of it:

- [`buildFailedGroups`](../../pkg/report/report.go#L477) fetches the NVCRE Job for each failed group but reads **only the `Failed` condition**. A threshold violation leaves the Job with `Succeeded=True` + `ValidationFailed=True` and no `Failed` condition, so `failedGroups[].reason` in the results JSON is empty — and [`printFailedGroups`](../../pkg/report/report.go#L1005) skips the reason line in the human card when the string is empty.
- `CategoryReport.FailureReason` comes from the Workflow `Failed` condition message, which is the aggregate `"N groups failed across M iterations"` — reason `JobValidationFailed` names the class of failure but not the threshold, value, or expression.
- The top-level `failedNodes` JSON field is names-only ([`CertFailedNodes`](../../pkg/report/report.go#L195) → `FailedNodeNames`); the per-node messages decoded from the ConfigMap are discarded.

An operator looking at a FAILED report today must `kubectl get job -o yaml` or gunzip the failed-nodes ConfigMap by hand to learn *which* threshold failed, *what* was measured, and *what* the expression demanded. The data survives; only the last hop into the report is missing.

## Decision

1. **No CRD schema change.** Flagged explicitly: the threshold detail already survives report generation in two durable stores (the Job CR's `ValidationFailed` condition and the failed-nodes ConfigMap). Plumbing the last hop is a pure `pkg/report` change. If a status field had been required, it would need separate schema approval — it is not.
2. **Extend the condition scan in `buildFailedGroups` to a priority order:** `Failed` → `HardwareFailed` → `ValidationFailed`. The first condition with `Status=True` supplies `failedGroups[].reason`. The existing `Failed` path keeps its `batchJobFailureReason` enrichment; the two new branches use the condition message verbatim — for threshold violations that message already contains the threshold name, measured value, and expression.
3. **Fall back to the failed-nodes ConfigMap when the Job CR is unreachable** (deleted by a group retry, or the group lives only in iteration history). `PopulateCategoryFromWorkflow` already resolves `wf.status.failedNodesRef` ([report.go:521](../../pkg/report/report.go#L521)); pass those `FailedNode` entries into `buildFailedGroups` and, when no Job condition was found, use the message of the first entry whose name is in the group's node list, preferring `reason: ThresholdViolation` entries.
4. **No renderer changes.** `printFailedGroups` already prints `fg.Reason` when non-empty, so the human report gains the threshold line automatically. Both surfaces are covered by the same fix: `nvcrectl certification report`/`run --wait` and `nvcrectl workloadrun run/report` both flow through `PopulateCategoryFromWorkflow` → `buildFailedGroups` → `Print`/`WriteJSON`.

## Implementation

- **`pkg/report/report.go`**:
  - Change `buildFailedGroups(ctx, c, wf)` to `buildFailedGroups(ctx, c, wf, failedNodes []nvcrev1alpha1.FailedNode)` — the caller (`PopulateCategoryFromWorkflow`) already holds the decoded list; today it feeds only `buildCliqueReport`.
  - Extract a `jobFailureReason(ctx, c, job, namespace) string` helper implementing the priority scan: `nvcrev1alpha1.JobFailed` (with `batchJobFailureReason` enrichment, unchanged) → `nvcrev1alpha1.JobHardwareFailed` → `nvcrev1alpha1.JobValidationFailed`, returning the condition message.
  - Add `failedNodeReason(failedNodes, groupNodes) string` for the ConfigMap fallback: first match by node name, `ThresholdViolation` entries first (the merged list is sorted by name then reason, so selection is deterministic).
- **No changes** to `pkg/controller/`, `pkg/threshold/`, `api/v1alpha1/`, or the catalog. Condition contents, `FailedNode` records, and ConfigMap payloads are already correct; this ADR only stops the report from ignoring them.
- **Tests** (testutil golden pattern, mirroring [`build_excluded_test.go`](../../pkg/report/build_excluded_test.go)): new `TestBuildFailedGroups` with `pkg/report/testdata/build-failed-groups/` cases driven through `Build` + `Print`/JSON against a fake client:
  - `threshold-violation` — Job present with `ValidationFailed=True`; reason renders the full threshold message in both JSON and the card.
  - `execution-failed-precedence` — Job with `Failed=True` keeps today's reason; `ValidationFailed` does not override it.
  - `hardware-failed` — `HardwareFailed=True` message surfaces.
  - `job-deleted-configmap-fallback` — no Job object; reason resolved from the failed-nodes ConfigMap entry.
  - `no-failure-data` — neither source available; reason stays empty (current behavior, no invented cause).
- **Integration golden impact: none.** `cmd/integration/testdata/reconcile/**/expected.json` snapshots CR state produced by controllers, which this ADR does not touch — e.g. `job-goodput-threshold-fail` and `workflow-validation-failed` goldens already pin the condition/ConfigMap contents this change consumes and stay byte-identical. Existing `pkg/report` goldens (`build-excluded-report`, `build-clique-report`, …) contain no failed groups and are unchanged. The only golden churn is the **new** `build-failed-groups` files, generated once and reviewed by hand.
- **Docs/site**: update the results-JSON reference for `failedGroups[].reason` if `site/content/docs/` documents it, noting it now carries threshold detail for validation failures.

## Rationale

- **Read state that already survives instead of adding fields.** The Job-CR-preserved-for-the-report contract in `setFinalStatus` exists exactly for this consumer, and `buildFailedGroups` already fetches the Job — the fix is reading two more conditions from an object in hand, not new plumbing.
- **The Workflow controller guarantees the condition is present before the group fails.** `isJobAwaitingThresholdEvaluation` holds the group in Running until the Job controller has written `ValidationFailed=True/False` ([workflow_controller.go:1040](../../pkg/controller/workflow_controller.go#L1040)), so a `GroupFailed` group with a live Job always carries the reason the report needs — no race.
- **Reuse the evaluator's message rather than re-formatting.** `pkg/threshold` is the single place that knows how to describe a violation; the report repeating `fmt.Sprintf` over `{Key, Measured, Expression}` would drift. The message also covers the sibling reasons (`UnknownThresholdKey`, `InvalidThresholdExpression`) for free.
- **Priority order protects existing output.** An execution failure with a stale `ValidationFailed` from a prior attempt still reports the execution cause; validation detail only fills a slot that is empty today.
- **The ConfigMap fallback covers retried and historical Jobs** without persisting anything new: retry deletes the failed Job ([workflow_controller.go:1063](../../pkg/controller/workflow_controller.go#L1063)), but its `failedNodes` were merged into the ConfigMap before deletion.

## Consequences

- `failedGroups[].reason` and the human failed-groups card show the threshold name, measured value, and expression for validation failures; hardware failures gain their detector message through the same scan.
- `reason` stays a human-readable string. Programmatic consumers wanting the triple as fields must parse the message; a structured sub-object remains an additive, non-breaking follow-up (see Alternatives).
- The human card truncates reasons at the box width (`boxWidth-12` in `printFailedGroups`); the ~70-char threshold message mostly fits, but long expressions may be elided with `...`. The JSON always carries the full string, so nothing is lost for automation.
- Failed groups from **iteration history** are still not listed — `buildFailedGroups` iterates only the current `orch.Groups`. Pre-existing limitation, unchanged and out of scope here; the ConfigMap fallback narrows its cost because per-node reasons from earlier iterations survive in `failedNodes`.
- The reason-string spelling split remains: the condition reason is `ThresholdViolated` (evaluator) while the `FailedNode`/report enum is `ThresholdViolation` ([job_types.go:82](../../api/v1alpha1/job_types.go#L82)). Renaming either is an API-visible change and is not taken on here.

## Alternatives Considered

- **Add `failureReason` to `GroupStatus`/`GroupIterationResult` in Workflow status.** Rejected — a CRD schema change (separate approval) that duplicates data already durable in the Job CR and ConfigMap, and per-group strings multiply against the etcd object budget that ADR-062/068 fought to reclaim. If report-after-teardown ever becomes a requirement, this is the field to design; it is not needed while reports run against a live Certification.
- **Structured `thresholdViolation: {key, measured, expression}` on `FailedGroupReport`.** Deferred — issue #176 asks for `failedGroups[].reason`, and the message embeds all three values. A structured field can be added later as a purely additive JSON change; doing it now would require the report to re-parse or the Job to store the triple separately.
- **Enrich the Workflow `Failed` condition message with the first violation.** Rejected — a controller-output change for a display concern, forcing regeneration of every integration golden that pins Workflow conditions, and condition messages should stay aggregate (one Workflow can hold many groups with different violations).
- **Promote top-level `failedNodes` from `[]string` to objects with reasons.** Rejected — breaks existing JSON consumers of `results.json`; the group-level reason plus the per-node ConfigMap already serve both audiences.
- **Have the report gunzip the ConfigMap as the primary source and skip the Job CR.** Rejected — the Job condition is cheaper (already fetched), carries the authoritative per-group cause, and the ConfigMap merges nodes across groups and iterations, so mapping a message back to *this* group is heuristic. ConfigMap stays the fallback, not the source of truth.

## Notes

- Exact message format consumed verbatim (from [`evaluator.go`](../../pkg/threshold/evaluator.go#L113)): `Threshold %q violated: measured %.4f, expression: %s`. The `job-goodput-threshold-fail` integration golden pins this string today, so any future format change will trip existing tests before it reaches the report.
- `job.status.failedNodes` is only populated when the `nvcre.nvidia.com/group-nodes` annotation is present (`groupNodeNames`); standalone Jobs without it still surface the reason via the condition path, just with no node attribution — same as today.
- `nvcrectl workloadrun status` (lightweight poll) is intentionally unchanged; it reports names only by design (ADR-059).

## References

- ADR-021: Performance Threshold Enforcement — origin of `ValidationFailed` and the CEL evaluator.
- ADR-059: WorkloadRun Simplified API — the second report surface covered by this change.
- ADR-061: Remove Remediation Controller — failed-node attribution via the Certification CR.
- ADR-062: Succeeded Node Attribution via a Compressed ConfigMap — the node-results ConfigMap pattern and etcd budget rationale.
- ADR-068: Group Nodes Compressed ConfigMap — precedent for keeping large per-node data out of CR status.
- Issue #176: ThresholdViolation reasons missing from report surfaces.
