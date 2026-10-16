# ADR-099: Relay the Failure Attribution NVCRE Already Has, and Do Not Classify Causes from Log Text

> **Status:** Accepted

## Context

An on-prem HGX B300 cluster ran `communication/nccl-all-reduce` across two nodes. Every
attempt reached full 16-rank init, printed `Connected all trees`, then died on the first
16-byte handshake send with `IBV_WC_RETRY_EXC_ERR`, `vendor_err=129`, on HCA `mlx5_1`,
between two macvlan GIDs, bidirectionally. Zero cross-node bytes moved on any rail.

NVCRE recorded `WorkloadFailed` on both nodes with an identical generic message. Issue #505
asks whether that is the correct stopping point, or whether a certification product ought to
name the failing component: a new `NodeFailureReason` for a fabric fault, a device field on
`FailedNode`, and a pattern source so something can key on the signature.

The question is a product question before it is an implementation question, and it has to be
answered before any API change, because both the reason enum (`api/v1alpha1/job_types.go:75`)
and `FailedNode` (`:87-103`) are CRD surface.

Investigating it surfaced a second, narrower finding: NVCRE already computes more per-node
attribution than it reports, and loses it at three identifiable places.

1. **The timeout path records no failed nodes at all.** `failTimedOutJob`
   (`pkg/controller/workflow_controller.go:1760-1801`) applies the Failed condition and calls
   `r.Status().Update` directly, bypassing `setJobFailed` and therefore bypassing the only
   site on that path that seeds `FailedNodes`. It captures a `FailureLog` and writes zero
   `FailedNodes`. A certification that times out reports no failed nodes.

2. **The CEL node-health detector computes per-node attribution and discards it.**
   `checkNodeHealth` (`pkg/controller/job_controller.go:1731-1845`) receives a per-node
   `result.Reason` and `result.Message` from the detector, logs both, flattens the results to
   a `[]string` of names (`:1820-1827`), and substitutes the constant
   `"Hardware failure detected by node health check"` (`:1845`) for every node.

3. **The certification report carries no per-node reason or message.**
   `CertFailedNodeDetails` (`pkg/report/report.go:272`) already resolves the full
   `{name, reason, message}` triple from the failed-nodes ConfigMap; `CertFailedNodes`
   (`:303`) reduces it to names, and `CertReport.FailedNodes` is `[]string` (`:68`).
   `NodeResultReport` is not an alternative landing site: `report.Build` (`:338`) never
   assigns `.NodeResults`, which is set only on the WorkloadRun path
   (`pkg/workloadrun/workloadrun.go:1659`). Today the triple is reachable only through the
   MCP `list_failed_nodes` tool (`pkg/mcpserver/server.go:214`).

## Decision

1. **NVCRE relays failure evidence and does not classify failure causes from log text.** No
   new `NodeFailureReason` value, no device or signature field on `FailedNode`, no new
   `LogPatternSet` pattern, and no regex over `failureLog.tail` anywhere in the controller.
   This ADR makes no CRD change.

2. **`failTimedOutJob` records the group's nodes.** A timed-out Job seeds `FailedNodes` from
   the group it ran on, with reason `WorkloadFailed`, before its status update. A Job that
   timed out while its pods were unschedulable is excluded: nothing ran on those nodes.

3. **The CEL detector's per-node message survives.** `checkNodeHealth` builds `FailedNode`
   entries carrying each node's own detector message, falling back to the existing constant
   when the detector supplies none. The reason stays `HardwareFailureDetected`.

4. **The report carries the per-node triple.** `CertReport` gains a `failedNodeDetails`
   field alongside the existing `failedNodes`, and the human report prints each failed node's
   reason next to its name. `failedNodes` keeps its `[]string` shape.

5. **A failed collective with no bandwidth recorded points at `diagnose`.** The report
   renders a next-step hint naming `testScale: diagnose`, which can separate a node-local
   fault from a link fault by experiment.

## Implementation

### Timeout attribution

In `failTimedOutJob`, after the exclusive conditions are applied and before
`r.Status().Update`, seed `job.Status.FailedNodes` from `g.Nodes` when the list is empty and
`SchedulingBlockedSince` is nil, reusing the `timeoutMsg` already built there. The
`*GroupStatus` is a parameter, so this needs no lookup and no `groupNodeNames` call from the
Workflow tier.

The unschedulable branch records nothing. The workload never ran on those nodes, and the Job
tier already treats scheduling-blocked time as not-running: `resumeFromSchedulingBlock` shifts
`WorkloadStartTime` forward by the paused interval, and the blocked branch returns before
stall detection. Recording them would place them in `failedNodes` and `CertFailedNodes`
indistinguishable from nodes that ran and crashed, since only `failedNodeDetails` carries the
message that separates the two, and ADR-061 designates that list as the signal external
systems consume. The Job still fails and its condition still explains why.

The timeout branch also re-runs `backfillGroupNodes` with `terminal` true before attributing.
The surrounding dispatch calls it with the Job's own terminal state, which is false here, so
under Unpinned a partially placed group would otherwise reach attribution with no nodes
recorded at all. The timeout ends the group, so this is the last chance to record where it ran.

The condition reason stays `JobTimedOut`. The node reason is `WorkloadFailed`, mirroring the
existing asymmetry at `job_controller.go:1374`, where the condition reason varies across
callers and the node reason is a constant.

`completeTerminalGroup` already guards on `len(job.Status.FailedNodes) > 0`
(`workflow_controller.go:2300`), so the ConfigMap write follows with no second call site.

### Per-node messages

`pkg/noderesults` gains `NodesWithPerNodeDetails(names, reason, msgFor func(string) string)`.
The existing scalar `NodesWithFailureDetails` delegates to it, so the four call sites that
genuinely attribute a whole group are untouched and the fan-out guarantee stays legible in
the signature.

`checkNodeHealth` collects `[]FailedNode` instead of `[]string`, and `mergeFailedNodes`
(`pkg/controller/job_controller.go:1864`) takes the entries directly rather than re-deriving
them from names and a constant. It has one production caller, so the signature change does
not ripple; `sortMergedFailedNodes` (`pkg/controller/node_results.go:51`) already operates on
two `[]FailedNode`.

`FailedNodes` is a `+listType=map` keyed on `name` and `reason`
(`api/v1alpha1/job_types.go:375-377`), so a node holds one message per reason. Two distinct
`HardwareFailureDetected` messages for one node collapse, and `sortMergedFailedNodes` decides
which deterministically.

### Report

`CertReport` gains `FailedNodeDetails []FailedNode`, populated in `report.Build` from
`CertFailedNodeDetails`, which `Build` already reaches through `CertFailedNodes`. No new
fetch. The summary block prints the reason after each failed node name.

`CategoryReport` gains a `hint` field, set when the category measures bandwidth, it failed,
and some failed group of two or more nodes recorded no bandwidth of its own. Those together
are the signature of a collective that never moved bytes. The hint names `testScale: diagnose`
and says what bisection can distinguish; it asserts nothing about the fault.

The bandwidth test is per failed group rather than category-wide, in both directions.
`buildGroupBandwidthRows` emits a row for every failed group even when nothing was measured,
so any category with more than one group has a non-empty `GroupBandwidth` and a category-wide
emptiness test would never fire. A row counts as measured only when it carries a `BusBW`.
The aggregate row is built from every group's measurements, so it is consulted only when there
are no per-group rows to judge; otherwise a healthy group's bandwidth would suppress the hint
for a failed sibling. Both halves protect the same case: one bad pair among several healthy
groups, which is the common fleet shape.

## Rationale

**A fourth reason value would not have helped, for mechanical reasons before charter ones.**
`NodesWithFailureDetails` takes a scalar reason and copies it across every name, so a
`FabricTransportError` value would have landed on both B300 nodes identically, reproducing
the exact over-attribution the issue reports. It would then reach `CertFailedNodes`, which
keeps only the name. A new enum value is an API-breaking change that buys a field fanned out
at one hop and discarded at the next.

**The evidence does not support a causal claim.** The issue states the limit precisely: NCCL
logs only failed completions, and `mlx5_1` is first in NCCL's channel order, so it is the
device that went first, not provably the one that is bad. A reason enum is an assertion about
cause. Recording `FabricTransportError` against a node would claim more than the run knows.

**The charter already answers this.** ADR-061 scopes NVCRE to "run certification tests and
clearly report which nodes failed which tests," and ties each of its three reasons to the
code path that fired rather than to evidence content. ADR-001 lists GPU monitoring and
diagnostics as a non-goal. ADR-004 has NVCRE consume hardware signals through CEL rather than
re-implement diagnosis. ADR-083 states the principle in one line: relay the diagnosis, do not
re-derive it.

**The three defects are the same shape ADR-071 fixed.** Its framing applies without
modification here: the data survives, and only the last hop is missing. Fixing those hops is
what changes this incident's output. Adding a taxonomy on top of them would have been a
second floor over a missing one.

**`failedNodes` keeps its shape.** ADR-071 rejected promoting it to objects because that
breaks existing consumers of `results.json`, and recorded that a structured sibling remains
an additive, non-breaking follow-up. `failedNodeDetails` is that sibling.

**The hint is the only part that changes what an operator does.** `printFailureLog` already
renders the pod, node, exit code and log tail inside the failed group's card, and the excerpt
is end-biased, so for this incident the operator very likely already saw the `IBV_` line.
What was missing was not the text but a next step. Bisection (ADR-055, ADR-056) is the
in-charter mechanism that turns this evidence shape into a structured record derived from a
controlled experiment, and it is the only thing available that can separate "went first" from
"is bad".

## Consequences

- A certification whose workload ran and then timed out now reports failed nodes where it
  previously reported none. Consumers that read failed nodes to make a pass/fail or exclusion
  decision will see a non-empty list for a case that was previously silent. This is the
  intended correction, but it is a behavioural change for anyone keying on the old emptiness.
  A run that timed out without ever being scheduled stays silent, so the two cases are
  distinguishable from the node list alone rather than only from a message.
- A node flagged by a CEL detector now carries that detector's own message. Operators writing
  CEL expressions get their message surfaced, which makes expression wording
  operator-visible in a way it was not before.
- `results.json` gains `failedNodeDetails` and an optional per-category `hint`. Both are
  additive; existing keys are unchanged.
- A machine still cannot distinguish a RoCE fabric fault from a bad container image. Both
  remain `WorkloadFailed`. This is the deliberate consequence of decision 1, and it is the
  thing to revisit if a downstream consumer is ever named that must branch on the difference
  and will not parse a message.
- No CRD change, so no migration and no API review. `make manifests generate` producing no
  CRD diff is the check that this held.

## Alternatives Considered

**A new `NodeFailureReason` for a fabric or transport fault.** Rejected. It is an
API-breaking CRD enum change; it would be written through a scalar fan-out and so would
attribute every node in the group identically; it would be discarded by the report; and it
asserts a cause the logs cannot establish.

**A record-only `failureSignature` pattern on `LogPatternSet`, plus an observed-errors list
on `FailureLog`.** Rejected, but on narrower grounds than the first alternative, and worth
stating honestly. `networkTransport` is a live precedent for exactly this shape: a
string-capturing pattern field added after the fact, with graceful degradation for profiles
that predate it, documented as record-only and never used for pass/fail. So the claim that
the API surface forbids another pattern field is not true. ADR-091 rejected a `gemmResult`
pattern as "the fixed-field regex approach the customer asked to move away from," and
ADR-090 records the customer's direction as declaring results in the spec rather than
regexing a fixed set of fields, with the declaration vocabulary deferred to a later ADR.
Minting a tenth fixed pattern field now risks creating a legacy field that vocabulary has to
carry. There is also a concrete evidence problem: the device name appears in the NET/IB init
banner near the start of the log, while the captured tail is the last 32 KB, so a single-line
pattern may well capture a signature with no device on the very incident that motivated it.

**A `signature` and `device` field on `FailedNode`.** Rejected. This is the only option that
satisfies "nothing can key on it" literally, and it is also the largest commitment: it puts a
log-derived string on the object ADR-061 designates as the contract for external consumers,
where every other field is derived from a code path that fired. It touches four CRDs, the
gzipped ConfigMap format, and four ConfigMap consumers. No downstream consumer has been named
that needs it.

**Enriching the `WorkloadFailed` message with the failing pod's name, exit code and
termination reason.** Rejected as redundant and fragile. `captureFailureLog` runs immediately
before `FailedNodes` is built in the same closure (`pkg/controller/job_controller.go:1504-1515`),
so that detail is in hand, but `printFailureLog` (`pkg/report/report.go:1390`) already prints
all of it a few rows up the same card. It also interacts badly with `failedNodeReason`
(`pkg/report/report.go:644-668`), which returns the first in-group entry with a non-empty
message over a list sorted by name, so a one-node enrichment surfaces only when that node
happens to sort first.

## Notes

Three questions are left open deliberately, because no amount of further code reading settles
them:

1. Is there a named downstream consumer that must branch on fabric-versus-other and will not
   parse a message string? ADR-061 designs for such a consumer; none has been named. If one
   exists and refuses free text, the enum question reopens on its merits.
2. Does ADR-090's deferred declaration vocabulary admit a row with no numeric value, that is,
   error events and not only results? That decides whether a signature field would be
   reasonable or legacy on the day it ships.
3. If a signature pattern is ever added, the regex should be validated against a real failing
   run's full log before the field is minted, for the banner-versus-tail reason above.

## References

- [ADR-004: Feature — CEL-Based Node Health Monitoring](004-cel-node-health-monitoring.md)
- [ADR-055: Adaptive Fault Isolation for NCCL Diagnostics](055-adaptive-fault-isolation.md)
- [ADR-056: Cross-Boundary Probing for Infrastructure Fault Detection](056-cross-boundary-probing.md)
- [ADR-061: Remove Remediation Controller — Failed Node Attribution via Certification CR](061-nvcre-nvsentinel-remediation-decoupling.md)
- [ADR-071: Threshold Violation Reason Propagation into Report Surfaces](071-threshold-reason-propagation.md)
- [ADR-083: Surface Runtime Scheduling Stalls as a First-Class Job Condition](083-scheduling-stall-visibility.md)
- [ADR-090: Sharded Component Results and the Report Bundle](090-component-result-sharding.md)
- [ADR-091: CUTLASS GEMM Per-GPU Burn-In as a `compute` Catalog Entry](091-compute-gemm-catalog-entry.md)
- Issue #505
