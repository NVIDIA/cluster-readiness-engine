# ADR-082: Inference Certification and the AIPerf Measurement Contract

> **Status:** Proposed — requires approval before implementation.
> **Date:** 2026-09-21

## Context

CRE certifies GPU clusters for training: its training and communication
workloads measure training goodput and NCCL bandwidth. The inference serving
path is untested. Inference certification asks a different question: can a
deployed model on the selected GPU nodes sustain a defined request load while
meeting latency, throughput, and error criteria?

[Exemplar Performance](https://github.com/NVIDIA/exemplar-performance) provides
model and hardware recipes, and
[InferenceX](https://github.com/SemiAnalysisAI/InferenceX) explores inference
performance through scenarios and published comparisons. They inform what CRE
should test. CRE still needs a measurement engine it can run inside its own
node selection, workload lifecycle, and certification process.

[AIPerf](https://github.com/ai-dynamo/aiperf) fits that role. It generates
traffic against inference endpoints, handles streaming responses and warmup,
and exports latency distributions, throughput, errors, and inference goodput as
structured files. InferenceX uses it for its AgentX scenario. Using AIPerf means
CRE does not maintain its own load generator or streaming metric code.

The initial scope is one model on one GPU node, one streaming chat endpoint,
synthetic token lengths, a fixed concurrency, and a request-count-bounded
profiling phase.

### Current CRE constraints

- [WorkloadSpec](../../api/v1alpha1/job_types.go) has only `trainJob`.
  [ADR-066](066-remove-kubejob-workload-type.md) removed the generic `kubeJob`
  arm.
- The [workload adapter](../../pkg/workload/adapter.go) builds and observes one
  typed resource. A server is long-lived; its readiness cannot map to
  `WorkloadSucceeded`.
- The [Job controller](../../pkg/controller/job_controller.go) separates
  execution success from the `ValidationFailed` verdict.
- [Measured-value collection](../../pkg/controller/job_threshold_helpers.go)
  reads only GoodputMeasurement and BandwidthMeasurement, and the
  [threshold registry](../../pkg/threshold/evaluator.go) has no inference keys.
- [Training goodput](../../pkg/goodput/calculator.go) is a runtime efficiency
  ratio. AIPerf goodput is SLO-compliant requests per second. They cannot share
  `goodputRatio`.

## Decision

**Integrate AIPerf as an external benchmark executable behind a CRE-owned,
versioned request/result contract. CRE owns the finite inference test and the
certification verdict.**

The first integration runs `aiperf profile` in a pinned runner image. It does
not embed Python in the manager, import AIPerf internals, fork its load
generator, or call `aiperf kube` from a reconciler.

The key decisions, detailed in the sections below:

1. A new `InferenceRun` workload runs one finite attempt: a vLLM server plus an
   AIPerf client, with explicit readiness, deadlines, and cleanup (§2).
2. The server takes all of one node's GPUs. The client runs on the same node in
   its own CPU-only pod and is never counted as a tested node (§2).
3. A result is accepted only if it is complete, matches the attempt's identity,
   and shows the client kept the configured load in flight (§5).
4. The CRE Job controller freezes the accepted result into one immutable
   ConfigMap, owned like existing measurements. Thresholds and reports read
   only that object (§7).
5. `Succeeded` means the server's GPUs are already released (§6).

### Scope: what v1 certifies

V1 certifies **each GPU node for single-node serving**: that every node serves
the recipe's model within the same acceptance criteria for latency,
throughput, and error rate. It does not judge nodes against each other; two
nodes can both pass while differing widely. Reports show the spread across
nodes (median, minimum, maximum, and outliers) for information only. This matches the most common
deployment: independent replicas, one per node, behind a router, with no
traffic between nodes during inference. For a single-node model, cluster
serving capacity is then roughly N times the per-node result, provided nothing
shared becomes the bottleneck.

It does **not** certify cluster-level serving:

| Not covered by v1 | Why | Covered by |
|---|---|---|
| Compute fabric for inference (InfiniBand, RoCE, EFA) | A single-node server's GPU traffic stays on NVLink inside the node; v1 rejects the platform fabric patches (§2) | Multi-node serving |
| Cross-node tensor, pipeline, or expert parallelism; disaggregated prefill/decode KV transfer | They need a server spanning nodes | Multi-node serving |
| Rack-scale NVLink on GB200/GB300 NVL72 | A node there is one 4-GPU tray; large-model serving on these racks usually spans trays over MNNVL | Multi-node serving with ComputeDomain |
| Shared front door (router, gateway, ingress, load balancing) | Each client targets one server's Service directly | Cluster endpoint |
| Behavior with every node at full load at once (power, cooling, shared storage) | The finite default `maxConcurrent` staggers attempts (§8) | Synchronized profiling across nodes |
| Consistency between nodes (a node much slower than its peers) | Verdicts are per node against fixed thresholds; no cross-node rule, tolerance, or fleet baseline is defined | A cross-node consistency verdict |
| Latency as seen by users outside the cluster | The client is on the serving node by design | Out of scope for certification |

CRE's NCCL workloads already check that the fabric works (`nccl-alltoall` is
close to expert-parallel traffic), but not how inference latency behaves on it.

Inference certification is expected to grow in levels, as NCCL testing has
`intra-node`, `intra-rack`, and `full-scale`:

1. **Per-node serving (this ADR).** Node readiness against fixed acceptance criteria. The client
   is local, as AIPerf recommends when measuring a server.
2. **Multi-node serving.** One model spread across nodes, using the platform
   fabric patches and, on NVL72, a ComputeDomain.
3. **Cluster endpoint.** Many replicas behind one router or gateway, with load
   from the AIPerf operator. Here the client is deliberately not local.

Levels 2 and 3 each need their own ADR. Reports for v1 must describe the result
as per-node serving certification, not as cluster inference capacity.

### 1. Ownership boundary

| Concern | Authority |
|---|---|
| Model/engine recipe and supported hardware | CRE catalog, optionally adapted from a pinned external recipe |
| Serving resources, placement, readiness, deadlines, cleanup | CRE InferenceRun reconciler |
| Request scheduling, streaming parsing, native metrics | AIPerf |
| Translation to and from the pinned AIPerf interface | CRE request translator, inside the runner image |
| Result identity, completeness, units, coverage validation | CRE result ingestion |
| Thresholds and certification verdict | Existing Job → Workflow → Certification path |
| Node hardware failure attribution | CRE node-health evidence, scoped to serving nodes |

The runner returns measurements and execution evidence. It does not decide a
verdict, create serving resources, select GPU nodes, or retry an experiment.
AIPerf fills the client slot inside an attempt the way `all_reduce_perf` fills
the binary slot inside a TrainJob.

### 2. The InferenceRun workload

Add a typed `WorkloadSpec.inferenceRun` arm and a namespaced
`nvcre.nvidia.com/v1alpha1 InferenceRun` CRD. Its adapter implements the existing
single-resource contract; a new reconciler manages the server and client. It is
not a generic workflow engine or a second certification tier.

```text
Certification → Workflow → CRE Job → InferenceRun
                                      ├── serving Deployment + Service
                                      ├── benchmark batch/v1 Job
                                      │     ├── AIPerf runner (supervised)
                                      │     └── result publisher
                                      ├── immutable request ConfigMap
                                      ├── transport result ConfigMap
                                      └── artifacts PVC (only under Retain)

The CRE Job controller writes one frozen result ConfigMap per attempt,
owned by the Workflow, or by the Job when it has no Workflow.
Reports and thresholds read that ConfigMap.
```

All objects live in the InferenceRun namespace. The Job owns InferenceRun, and
InferenceRun owns its transient children. Caller-owned model PVCs and Secrets
are referenced, never adopted or deleted. The server is not an
[ADR-034](034-inferred-dependency-lifecycle.md) Workflow dependency: it is
attempt-scoped coverage evidence that must release its GPUs before the group is
reused.

`WorkloadSpec` stays a one-arm union, `trainJob` or `inferenceRun`. This does
not restore `kubeJob`; the benchmark `batch/v1` Job is only an InferenceRun
child, and the Job controller does not `Owns()` batch Jobs.

**Server.** The serving arm is typed `vllm`: one Deployment replica on one GPU
node, a Service selecting only that attempt, and a pinned model revision. The
serving pod requests the node's full `gpusPerNode`, as a training pod does, and
in v1 the tensor-parallel size equals that count. A recipe that held more GPUs
than it used would report a node covered with GPUs untested. `nodesPerJob` is
1; diagnose and bisection are disabled for inference.

**Client.** The benchmark arm is typed `aiperf`. The client runs on the serving
node, in its own pod. Once the serving pod's node is recorded, the reconciler
creates the benchmark Job with required node affinity on that node's name
(`matchFields` on `metadata.name`). The pod requests no GPU, has Guaranteed QoS
at the recipe's qualified size, and carries the workload tolerations. Traffic
goes through the attempt's Service, which with one endpoint stays on the node.

Same-node placement gives every node the same client conditions: client to
server network time is near zero everywhere, so latency differences between
nodes come from the nodes. The cluster needs no CPU-only pool, the same
prerequisites as a training benchmark. AIPerf's own guidance agrees; its
tutorials target `localhost` and its Kubernetes guide schedules clients onto
GPU nodes. The cost is CPU contention with vLLM's host-side work. The fixed
client size bounds it, the load-maintenance check (§5) catches a client that
falls behind, and GPU qualification measures the remaining interference
(delivery step 5).

**Tested nodes.** Only serving pods carry `nvcre.nvidia.com/job`, so node
discovery, health checks, and the drain barrier see only the serving node.
Benchmark and publisher pods carry role labels (`server`, `benchmark`,
`publisher`), so failure-log capture can read a crashed client without
attributing its node. Generic workload-failure attribution gets an
inference-specific audit before the first inference recipe ships.

**Rejected settings.** V1 rejects `options.image` (an inference category has
two pinned images, and [ADR-077](077-workload-image-override.md) rewrites only
the TrainJob image), gang and queue settings, and platform NCCL, EFA, RoCE, and
ComputeDomain patches, which are TrainJob rewrites. `workloadMetadata.labels`
apply to the InferenceRun object per [ADR-079](079-workload-object-labels.md).
The public API does not expose arbitrary resource YAML, shell commands, or the
full upstream AIPerf configuration.

How these rules fit the existing adapter, labeling, and drain code is in
Appendix B.

### 3. Input contract

The immutable experiment configuration has these typed groups (proposed API).
The separate write-once `spec.cancellation` control field is defined in Appendix C and
does not change the experiment or its configuration digest.

| Group | Contents |
|---|---|
| `serving.vllm` | Image digest, model identifier/revision, local model/tokenizer references, dtype/quantization, tensor parallel size, resource requests, validated engine options |
| `benchmark.aiperf` | Qualified runner image digest, streaming chat endpoint, synthetic input/output token profile, seed, concurrency, warmup and profiling request counts, request timeout, optional latency SLOs |
| `client` | Qualified CPU/memory size (Guaranteed, no GPU). V1 has no placement field |
| `validity` | Minimum successful requests, minimum observations per required statistic, minimum achieved-concurrency ratio, qualified settling interval and minimum steady-state duration |
| `timeouts` | Admission, serving readiness, benchmark, collection, and cleanup deadlines |
| `artifacts` | Retention policy (`Ephemeral` default, or `Retain`), output bounds, storage class/size under `Retain` |

V1 rejects sweeps, adaptive search, concurrency ramps, multi-turn requests, intentional
cancellation, request retries, and more than one AIPerf `--url`. Multi-URL mode
merges servers into one result, so a slow node would be averaged away. Each
serving node gets its own run and result.

The reconciler renders the spec into `cre.inference.request/v1` JSON in an
immutable ConfigMap, with the Job UID, InferenceRun UID, attempt ID, and a
SHA-256 digest of the effective non-secret configuration. Secret values are
never serialized, and Secrets are mounted only into containers that need them. Inside the runner, the request translator turns that
document into an AIPerf argument vector; endpoint, output path, seed, and stop
conditions cannot be overridden. Missing model or tokenizer assets fail
preflight, so downloads never happen in the measured phase.

Readiness means the runner completed a small streamed generation, not that the
pod is `Running` or a port is open. Readiness and warmup requests are excluded
from statistics. Output length is enforced through engine settings and the
achieved token lengths are recorded; prefix caching, sampling, precision, and
token-counting mode are recorded parts of the recipe.

### 4. Process and transport

The benchmark Job runs once (`backoffLimit: 0`, `restartPolicy: Never`) with an
`activeDeadlineSeconds`. A CRE supervisor runs AIPerf in a process group,
enforces the deadline, and reaps descendants. Native output goes to a fresh
directory on the artifacts volume.

When AIPerf exits, the supervisor validates and normalizes the output, then
atomically renames a completion manifest into place (outcome, file sizes,
hashes). No manifest means incomplete. Normalization never turns a nonzero
AIPerf exit into success.

A second container, the publisher, waits for the manifest, verifies hashes, and
writes the normalized `result.json` (at most 64 KiB) into a transport ConfigMap
the reconciler pre-created. It mounts artifacts read-only, retries a bounded
number of times without rerunning AIPerf, and is bounded by the Job deadline
if no manifest ever appears. It accepts an identical re-publish and rejects
conflicting content. It uses a projected token with a Role granting
`get`/`update` on that one ConfigMap name. No other container gets an API
token. The manager never execs into pods or parses console output, and there is
no inference LogProfile: this is a closed file set plus a manifest, not a log
stream ([ADR-005](005-logprofile-goodput-measurement.md),
[ADR-017](017-nccl-bandwidth-measurement.md)).

The InferenceRun reconciler checks the transport ConfigMap's UID and owner,
validates the document (§5), and sets `ResultAccepted` with the content hash.
Exceeding an output limit fails collection; accepted evidence is never
silently truncated.

### 5. Result contract

The publisher submits `cre.inference.result/v1`:

| Field | Contents |
|---|---|
| `identity` | Job UID, InferenceRun UID, attempt ID, runner Job/Pod UID, config digest; all must match controller-held values |
| `producer` | Runner image digest, AIPerf version, native export schema version, normalizer version |
| `execution` | Exit code, completeness and cancellation flags, timestamps, stop reason |
| `population` | Attempted, successful, and failed requests; observation counts; measurement interval; achieved token lengths; achieved concurrency; client CPU throttling |
| `metrics` | Values and units under the CRE keys below; absent is distinct from zero |
| `artifacts` | Retention policy and hashes; under `Retain`, the PVC identity and directory |

The reconciler attaches serving placement it observed itself: server and Pod
UIDs, node name/UID, image ID, model revision, and Service/EndpointSlice
membership. Client-reported node names never establish coverage. A result is
rejected if serving identity or placement changed during the attempt.

| CRE threshold key | AIPerf source | Unit | Population |
|---|---|---|---|
| `inferenceTtftP99Ms` | `time_to_first_token.p99` | ms | Successful requests |
| `inferenceItlP99Ms` | `inter_token_latency.p99` | ms | Successful requests with output length ≥ 2; per-request mean decode interval |
| `inferenceRequestLatencyP99Ms` | `request_latency.p99` | ms | Successful requests |
| `inferenceOutputTokensPerSec` | `output_token_throughput.avg` | tokens/s | Successful output tokens / duration |
| `inferenceRequestsPerSec` | `request_throughput.avg` | requests/s | Successful requests / duration |
| `inferenceGoodputRequestsPerSec` | `goodput.avg` | requests/s | SLO-compliant requests / duration; only with SLOs configured |
| `inferenceGoodRequestFraction` | `good_request_fraction.avg` | ratio 0–1 | Good requests / attempted; only with SLOs configured |
| `inferenceErrorRatio` | `error_request_count / attempted` | ratio 0–1 | Profiling requests |

Rules the ingestion enforces:

- **Every request accounted for.** Each attempted profiling request has exactly
  one terminal record, and `attempted = successful + failed`. An unaccounted
  request makes the result incomplete, not clean.
- **Latency covers successes; errors are separate.** Percentiles are over
  successful requests. Failures are certified by `inferenceErrorRatio`, not
  folded into latency or throughput.
- **Absent is not zero.** A key missing from the result stays missing. A key
  that is present is emitted even when it is zero, so a perfect run's zero
  error ratio reaches the threshold check. Training and bandwidth keys keep
  their existing `> 0` rule.
- **Reject rather than guess.** Unsupported schema or producer versions, unit
  mismatches, non-finite values, stale identities, inconsistent counts,
  incomplete or cancelled runs, and missing required metrics are rejected.
  Zero successful requests never passes validity.

Appendix A maps these rules onto AIPerf's export fields.

#### Load maintenance

This check establishes one thing: that the client kept the configured load in
flight. V1 runs a closed loop at concurrency `C`: a slow server lengthens
requests but keeps `C` in flight, while a client that falls behind leaves gaps
between one request finishing and the next starting. Achieved concurrency
measures that.

The normalizer computes the time-weighted mean of in-flight profiling requests
over a fixed window, starting `validity.loadSettlingSeconds` after the first
profiling request starts and ending at the last profiling request start.
The recipe qualifies this positive settling interval and a positive
`validity.minSteadyStateSeconds`. The window is independent of achieved
concurrency: reaching exactly `C` is not an additional requirement. Appendix A
defines the calculation. The result has one of three outcomes:

| Outcome | Condition | Classification |
|---|---|---|
| Load maintained | `achieved / C >= validity.minAchievedConcurrencyRatio` (default 0.95) | The run proceeds to threshold evaluation |
| `ClientSaturated` | Below the ratio | Benchmark failure; never attributes the serving node |
| `InsufficientEvidence` | Fewer than `k × C` profiling requests, fewer than `m` completions inside the window (defaults `k = 5`, `m = 2 × C`), or a window shorter than `minSteadyStateSeconds` | Validity failure; neither a pass nor a client or node failure |

Render rejects a recipe whose profiling request count cannot meet `k × C`. The
supervisor also records the runner's cgroup CPU throttling as evidence; it
explains a `ClientSaturated` result but does not decide it.

The check does **not** show that the client left the measurement undisturbed.
A co-located client can take CPU time from vLLM's host-side work, and a client
slow to process responses can record late timestamps, inflating TTFT and
latency while all `C` requests stay in flight. That interference is bounded
by qualification, not by a per-run check: GPU qualification (delivery step 5)
compares each recipe's same-node results with a client on another node (with
network-latency calibration) and with a client twice the qualified size, and
the recipe ships only if the latency difference stays within a recorded
tolerance. Clusters that run the kubelet CPU manager with the `static` policy
can also give the client and server exclusive cores; v1 records whether it is
active but does not require it.

### 6. Lifecycle, verdict, and failures

| Stage | Exit condition |
|---|---|
| Pending | Preflight valid; serving resources admitted and scheduled (separate admission deadline) |
| Preparing | Model ready; serving identity and node recorded; client scheduled on that node |
| Benchmarking | Streaming readiness, warmup, profiling, within the benchmark deadline |
| Collecting | Publisher done; result validated; `ResultAccepted` set |
| Cleaning | Serving and client pods confirmed gone, or cleanup deadline expired (failure only) |
| Succeeded / Failed | Terminal; the result is never replaced |

Pending maps to `WorkloadPending`; the middle stages map to `WorkloadRunning`.
InferenceRun is `Succeeded` only after the result is accepted and the serving
and client pods are gone. That is stricter than TrainJob: the serving pod holds
the GPUs under test, so success means they are released.

Failure follows the same rule: cleanup ends before anything becomes terminal.
InferenceRun reports `Failed` only after cleanup finishes or its cleanup
deadline expires, so `GetStatus` keeps returning `WorkloadRunning` through
Cleaning on both paths. `CleanupComplete` is therefore final when InferenceRun
turns terminal. The Job copies it in the same status write that sets the Job's
terminal phase. Once the Job is terminal, the controller skips
`updateStatusFromWorkload`, and nothing needs to sync afterwards (Appendix B).

- `CleanupComplete=True`: the GPUs are released, and Workflow may retry the
  attempt or reuse the node.
- `CleanupComplete=False` (the cleanup deadline expired): Workflow fails the
  group with a `CleanupIncomplete` reason and the cause, and records the node in
  orchestration status as cleanup-blocked. It does not retry on that node, and
  it skips the node for the rest of the Workflow, including later iterations
  under `repeatCount`, reporting it as not covered in each skipped iteration.
  Without that record, the next iteration would create a new serving pod on a
  node whose GPUs may still be held. There is no hardware attribution.
  Workflow never waits on a condition that can no longer change.

A result accepted before cleanup expired is still frozen, thresholds are still
evaluated on it, and reports show it. The node is **not** certified: releasing
the GPUs is part of passing, as the `Succeeded` rule above requires. The report
shows the measurement, the threshold verdict, and `CleanupIncomplete` side by
side.

InferenceRun owns its stage deadlines. Workflow's `timeoutPerJob` is an outer
execution cap, not permission to skip cleanup or result freezing. On timeout,
Workflow requests cancellation instead of failing the Job directly. The Job
passes the request to InferenceRun through a write-once `spec.cancellation`
field. InferenceRun stops the benchmark and server under its own cleanup
deadline, and the Job freezes the evidence before it turns terminal. A
Workflow-side backstop ends the wait if InferenceRun stops making progress.
Appendix C defines the protocol. Training timeout behavior is unchanged.

Restarts resume from persisted timestamps and UIDs. Deletion stops the
benchmark, then the server, then transient configuration; finalizers verify
each step, and a cleanup timeout is surfaced rather than a finalizer being
silently removed. Explicit deletion follows this finalizer path even if the
Job is terminal; the frozen cleanup verdict remains the outcome of its bounded
attempt and does not authorize skipping deletion checks.

InferenceRun reports execution and measurement validity. The Job evaluates
thresholds on the frozen result (§7) through the existing CEL path and sets
`ValidationFailed`. A performance miss keeps the usual separation: execution
succeeded, validation failed. Certification recipes must supply thresholds,
including an error-ratio ceiling, and sample minimums. A run without
thresholds is reported as **unvalidated** and never produces a pass.

| Failure | Classification |
|---|---|
| Unschedulable client, missing model or Secret, image pull, readiness timeout | Setup failure; no hardware attribution |
| AIPerf crash, OOM, or deadline; `ClientSaturated`; incomplete population; publisher failure; unsupported schema | Benchmark failure; never a pass, never blames the serving node |
| Valid result misses a threshold | Performance failure for the tested node |
| Serving pod replaced, endpoint drift, unverified coverage | Coverage failure; attempt invalidated, evidence kept |
| Health detector finds a serving-node hardware fault | Existing hardware-failure path; attempt stopped |
| API error reading or deleting children | Retried within the deadline; never read as absence |
| Too few profiling requests or window completions, or a window shorter than `minSteadyStateSeconds` | `InsufficientEvidence`; validity failure, no attribution |
| Cleanup deadline expired, or the Workflow backstop fired | `CleanupIncomplete`; node skipped for the rest of the Workflow and not covered, no hardware attribution |

Workflow alone retries whole attempts. Each retry gets a fresh attempt ID,
resources, and result ConfigMap; failed-attempt evidence is kept, and a later
success does not replace it. There is no checkpoint restart for inference.

### 7. Results, retention, and reporting

[ADR-072](072-goodput-terminal-freeze.md) keeps frozen goodput on the
measurement object, never copied onto Job, Workflow, or Certification status.
The Job controller creates GoodputMeasurement and BandwidthMeasurement and
owns them through the Workflow when there is one, so they survive the Job's
deletion.

Inference follows the same rule, including who creates the object. When
InferenceRun reports `ResultAccepted`, the Job controller, in the reconcile
where it already fetched the InferenceRun, writes one immutable result
ConfigMap:

- It copies the transport document and verifies it against the accepted hash.
- It uses the existing measurement ownership policy: the owning Workflow, or
  the Job only when no Workflow owner reference exists. Resolution is stricter
  than today's `getOwnerWorkflow` helper: validate the reference's API group,
  kind, and UID, fetch the Workflow in the Job namespace, and require its UID
  to match. Ambiguous Workflow references, lookup errors (including NotFound),
  and a same-name replacement block freezing with an explicit owner-resolution
  error and requeue; none permits fallback to Job ownership. They do not block
  InferenceRun's independent compute cleanup. A missing Workflow is transient
  in a real cluster: garbage collection either deletes the Job or, when
  orphaning, removes its owner reference. Once the Job has a
  `deletionTimestamp`, the freeze is abandoned and recorded in an event, and
  the Job's finalizer path continues rather than retrying forever.
- The name derives from the Job name and attempt ID. Creation is idempotent
  only when content and owner identity match; any conflict fails closed.
- It records the name and UID in `job.status.inferenceResultRef`, a reference,
  not the metric map.

The Job does not evaluate thresholds or report a terminal phase before that
ConfigMap exists, except through the Workflow backstop (Appendix C). A frozen result
from an attempt that ended `CleanupIncomplete` is kept and reported, but never
certifies the node. The transport copy lives until InferenceRun is deleted with
the Job, so the freeze always happens first. `collectJobMeasuredValues` `Get`s
the ConfigMap through the reference and checks its UID; nothing lists
ConfigMaps by label. No measurement CRD is added: the result is one bounded,
immutable document, the same shape as the node-results ConfigMaps in
[ADR-062](062-node-detail-propagation.md).

A failed attempt gets an evidence ConfigMap from the Job controller, under the
same owner, holding the failure reason and any transport document. Reports show
execution, validity, verdict, and cleanup state as separate fields. Orchestration
status records the current attempt's reference and an index for history.
Reports are snapshotted before the owning object is deleted.

**Artifacts** default to `Ephemeral`: native files live on a size-limited
`emptyDir` shared by the runner and publisher and go away with the benchmark
pod. Thresholds and reports lose nothing, since they read the normalized
result. This default needs no StorageClass and leaves no per-attempt PVCs
behind across N nodes and retries. Opt-in `Retain`, for debugging and audit,
creates an unowned per-attempt PVC labeled with its origin UID and attempt; CRE
documents how to retrieve and delete it. Automatic expiry is future work.

### 8. Running across many nodes

Certifying N nodes runs N attempts. Each pays its own setup: the first image
pull on the node, model load, engine startup, readiness, and warmup, then
profiling (about `profiling requests × mean latency / concurrency`) and
cleanup.

```text
wall clock ≈ ceil(N / maxConcurrent) × per-attempt time
```

Per-attempt time is measured in GPU qualification for each recipe and GPU
architecture; the catalog entry records it and derives its deadlines from it.

With each client on its own node, parallel attempts share no measurement path,
so `execution.maxConcurrent` limits setup load (registry pulls, model storage
reads), not measurement interference. Today's default of 0 (unlimited) would
load the model on every node at once, so the inference entry applies a finite
default from qualification when `options.maxConcurrent` is unset. Model storage
must allow concurrent read-only mounts from different nodes; preflight rejects
a ReadWriteOnce model PVC unless `maxConcurrent` is 1.

That default has a cost: nodes are not all under load at the same time, so v1
makes no claim about the cluster at full simultaneous load. Staggering only
setup and then running profiling on every node together would cover it, and is
future work (see Scope).

### 9. Compatibility and upgrades

CRE publishes a tested compatibility tuple: contract version, runner digest,
AIPerf version, input format, and native export schema. An image outside it is
rejected for certification. Serving images, model and tokenizer revisions,
recipe revision, and dataset seed are pinned too. The runner image is built for
linux/amd64 and linux/arm64, because the client runs on the serving node and
GB200/GB300 nodes have Arm (Grace) CPUs; each architecture is qualified.

The reviewed upstream source is commit
[`ab7c8ee8`](https://github.com/ai-dynamo/aiperf/tree/ab7c8ee87c77b4848f853d7b38dc2c2f0b796157),
export schema `1.4`. That is review evidence, not a qualified release. Before
the first recipe ships, an immutable runner build is chosen and real success,
error, cancelled, and incomplete exports are captured from it. Only tuples
backed by those fixtures and an end-to-end run are supported, and every upgrade
repeats that qualification. No floating tags or silent schema fallback.

## Implementation

Implementation follows approval of this ADR.

### Delivery order

1. **Qualify the file boundary.** Build the pinned runner and normalizer and
   capture fixtures against a deterministic streaming test server: counts, stop
   conditions, warmup exclusion, token accounting, units, and achieved
   concurrency.
2. **Add InferenceRun.** API, reconciler, adapter, watches, RBAC, publisher,
   deadlines, retention, and the vLLM arm, per §2. Reject goodput, bandwidth,
   checkpoint, and stall settings on an inference Job. The runner binary lives
   under `cmd/` per [ADR-069](069-cmd-layout.md); translation and normalization
   live in `pkg/inference/`. WorkloadRun does not grow an inference framework.
3. **Connect verdicts and reports.** Register the inference keys; have the Job
   controller freeze results and record `inferenceResultRef`; extend
   `collectJobMeasuredValues`; add strict result-owner resolution; route
   inference `timeoutPerJob` through cancellation, cleanup, and evidence freeze;
   copy final `CleanupComplete` in the terminal status write; handle
   `CleanupIncomplete` in Workflow, including the backstop and skipping
   cleanup-blocked nodes in later iterations. Add the render and preflight rejections
   from §2, §3, and §8.
4. **Add one recipe.** A pinned single-node vLLM model with thresholds, sample
   minimums, a qualified client size, and a finite default `maxConcurrent`,
   plus API docs, a sample, navigation, and `Retain` retrieval instructions.
5. **Qualify on GPUs.** Verify placement, achieved traffic, repeatability,
   deliberate failures, and cleanup. For each recipe and architecture, find the
   smallest client size that passes the load-maintenance check with vLLM on the
   same node, run the interference comparison from §5, and record the
   tolerance, settling interval, minimum steady-state duration, and per-attempt
   time. Qualification also shows, with margin, that the recipe's profiling
   request count at its typical latency produces a window longer than
   `loadSettlingSeconds + minSteadyStateSeconds`. Render cannot know latency,
   and a recipe sized too small would return `InsufficientEvidence` on every
   run. Mock servers and Kind/KWOK do
   not count as GPU validation.

Expected code areas: `api/v1alpha1/`, `pkg/workload/`, `pkg/inference/`,
`pkg/controller/`, `pkg/threshold/`, `pkg/catalog/entries/inference/`,
`pkg/report/`, `pkg/render/`, and a runner under `cmd/`. The manager gains no
Python dependency.

### Acceptance criteria

- **Contract fixtures:** valid exports, unknown schemas, absent/null/zero
  values, wrong units, invalid numbers, insufficient samples, warmup exclusion,
  partial records, count disagreement, and achieved concurrency just above and
  below the gate. Window fixtures cover a burst start with `R = C`, a single
  request, initial dispatch settling, a drained tail, and a window shorter
  than the minimum; short runs yield `InsufficientEvidence`. With enough
  evidence, sustained 97–98 in flight at `C = 100` passes a 0.95 ratio without
  ever reaching 100, while sustained 90 fails. Render rejects concurrency
  ramps and nonpositive settling/minimum-window durations.
- **Identity:** results from another UID, attempt, or config, stale
  ConfigMaps, changed serving pods, and foreign endpoints are rejected;
  conflicting ownership fails closed.
- **Reconcile:** restart at every stage, idempotent creation, publication
  conflicts, timeouts, health failure, API errors, and cleanup without garbage
  collection. New attempts never overlap old compute. A failure followed by
  successful cleanup leaves the Job terminal with `CleanupComplete=True`, and
  Workflow retries; a cleanup timeout leaves `CleanupComplete=False`, and
  Workflow fails the group with `CleanupIncomplete` instead of waiting.
  Exercise outer `timeoutPerJob` before workload creation, during profiling,
  and during cleanup, including controller restart between cancellation and
  propagation. None may terminalize the Job before evidence and final cleanup
  status exist, restart the cleanup clock, or retry a timed-out attempt.
  With an InferenceRun reconciler that never progresses, the Workflow backstop
  fails the group with `CleanupIncomplete` and does not wait forever. With
  failing API reads, the InferenceRun cleanup deadline still expires. With
  `repeatCount: 2`, a node left `CleanupIncomplete` in iteration 1 gets no
  serving pod in iteration 2 and is reported as not covered. An accepted
  result whose cleanup expires is reported with its threshold verdict and
  does not certify the node. A Job deleted while its freeze is blocked on
  owner resolution completes deletion.
- **Verdict:** execution success, valid measurements, passing and failing
  thresholds, and missing evidence are distinct. Client failures, including
  `ClientSaturated`, never produce hardware attribution or node coverage.
- **Render/RBAC:** only the publisher can update its ConfigMap; the client lands
  on the serving node with no GPU and no `nvcre.nvidia.com/job` label; image
  overrides cannot move a pinned digest; multiple URLs, queue settings, and a
  ReadWriteOnce model PVC with `maxConcurrent` above 1 are rejected.
- **Freeze:** the frozen ConfigMap under both owners, idempotent re-creation,
  conflicting content, and no terminal Job phase before it exists.
  Workflow lookup timeout, Forbidden, NotFound, and a same-name replacement
  with a different UID must block freezing without Job-owner fallback; cleanup
  must still progress. Existing matching content under a different owner is a
  conflict. Only absence of a Workflow owner reference selects Job ownership.
- **End to end:** the pinned AIPerf and publisher with streamed success, server
  errors, missing exports, runner kill, a starved client failing
  `ClientSaturated`, and `Retain` retrieval after compute deletion. GPU
  qualification verifies the server node identity and a failing threshold.
- Tests follow the TestCaseParser/golden conventions in
  `.claude/skills/cre-test/SKILL.md`; golden generation needs explicit approval.

## Rationale

AIPerf supplies the specialized measurement; the versioned request/result
contract keeps CRE's API, failure attribution, and verdicts independent of
upstream flags and export changes. Maintaining a normalizer is a smaller
commitment than maintaining a load generator.

InferenceRun keeps the one-resource adapter while making the server and client
lifecycle explicit, which a generic `kubeJob` arm would leave unspecified.
Freezing the result in the Job controller keeps the owner decision in one
place, the same place as the existing measurements, without a new measurement
controller.

Running the client on the serving node gives every node the same measurement
conditions and adds no cluster prerequisite. The load-maintenance check turns
a client that falls behind into an explicit client failure, and qualification
bounds the interference a per-run check cannot see.

## Consequences

- CRE gains a CRD, reconciler, runner image, publisher, and compatibility
  fixtures. This is more than a catalog entry.
- The client takes a fixed slice of the serving node's CPU and memory, which
  recipes must leave free.
- Raw AIPerf artifacts are discarded by default; keeping them needs `Retain`
  and a StorageClass.
- V1 certifies nodes for single-node serving, not cluster inference capacity.
  It makes no claim about the compute fabric, cross-node parallelism, KV-cache
  transfer, rack-scale NVLink, or the front door (see Scope).
- Results compare only under equivalent model, precision, topology, request
  distribution, caching, and measurement definitions. External leaderboard
  numbers are context, not pass thresholds.

## Alternatives Considered

**Build a load generator in CRE.** It would duplicate request scheduling,
streaming parsing, token accounting, and statistics that AIPerf already
provides.

**Run the server inside a TrainJob.** It reuses today's workload arm but leaves
serving readiness, ownership, and cleanup in shell scripts.

**Other benchmarks.** `vllm bench serve` is useful for vLLM development; AIPerf
is chosen as a shared interface across serving backends. MLPerf Inference
would need a separate profile, and this integration makes no MLPerf claim.
Exemplar and InferenceX can inform recipes, with revisions and licenses
recorded, but do not become runtime dependencies, and a CRE run is not an
official InferenceX result.

**Client on a dedicated non-GPU node.** It keeps client CPU off the node under
test, but adds network time that varies by node, which reads as node-to-node
latency differences, and requires a CPU-only pool in every certified cluster.
It remains a later opt-in mode that must enable AIPerf's network-latency
calibration (`--network-latency-automatic`) and record the round-trip time and
unadjusted metrics. [ADR-051](051-tolerate-all-taints.md)'s non-GPU rule is for
controllers and does not govern the load generator.

**One shared client using multi-URL mode.** It merges every server into one
result and is the configuration most likely to saturate.

**Pod-log transport instead of the publisher.** The runner would print the
normalized document as a framed final log line with its hash in the container
termination message. No workload container would need an API token, and the
manager already reads pod logs. The publisher is kept for v1 because the
result then exists as an API object before any pod is deleted, independent of
the node and kubelet log retention; long log lines are split by container
runtimes and share the stream with AIPerf's output; and the
accept-identical, reject-conflicting rule needs a single write target. The log
transport can replace the publisher later behind the same result contract.

**AIPerf Kubernetes operator.** It provides distributed load generation. The
trigger to adopt it is a recipe whose target load fails the load-maintenance
gate at the largest single client size CRE qualifies, during GPU qualification.
Such a recipe does not ship on the single-client path; it waits for an operator
backend rather than CRE building its own coordinator. That backend must keep
the request/result contract and give the operator sole ownership of its
JobSet and pods; CRE must not also reconcile them.

## Notes

- This record decides the inference workload and the AIPerf contract together.
  A later record can add a serving backend without reopening the measurement
  contract.
- InferenceX uses AIPerf for AgentX (`aiperf profile`); its fixed-sequence path
  uses `infx.bench_serving`. CRE v1 matches neither client contract.
- Deferred: multi-node and disaggregated serving (level 2), cluster-endpoint
  testing with distributed load generation (level 3), synchronized profiling
  across nodes, external endpoints (which need their own coverage contract),
  Dynamo/llm-d serving operators, sweeps, trace replay, accuracy evaluation,
  artifact expiry, and dedicated client-node placement.
- Approval covers this design and scope. Choosing and qualifying a runner
  release are implementation gates, not evidence established here.

## Appendix A: AIPerf export mapping

This appendix fixes how the §5 rules map onto AIPerf's exports at the reviewed
commit (schema 1.4). Delivery step 1 must confirm each point against captured
output from the qualified runner; anything that disagrees is blocked, not
guessed.

**Inputs.** The translator reads the single-run `profile_export_aiperf.json`
and `profile_export.jsonl`. Top-level JSON metrics are profiling-only; warmup
sits in `warmup_metrics`, which v1 ignores. JSONL records carry
`metadata.benchmark_phase` (`warmup` or `profiling`); only `profiling` records
count, and their counts must match the summary. The multi-run aggregate file
has a different shape and is never read as a single run.

**Completeness.** The export must establish `is_complete` true and
`was_cancelled` false. The translator also checks the resolved native input
against the CRE request, that the stop condition was met, and that producer
metadata matches the runner's actual image ID. The contract version and
producer tuple are checked independently; an upstream schema string alone
does not establish compatibility.

**Request accounting.** HTTP, transport, timeout, and invalid responses are
failures; successes have no request error and valid completion and token
evidence.

- `request_count` is successful requests. `error_request_count` has the
  `ERROR_ONLY` flag and is omitted on a clean run. Its absence means zero only
  on a complete, uncancelled run; otherwise the result is incomplete.
- `attempted = request_count + error_request_count`, treating one omitted
  counter as zero when the other is present.
- `completed_request_count` must equal `attempted`.
- `request_error_rate` is a percent and must equal
  `100 × error_request_count / attempted`. `inferenceErrorRatio` is the ratio,
  not the percent.

**Latency.** `time_to_first_token`, `inter_token_latency`, and
`request_latency` carry `PERCENTILE_INCLUDES_FAILED_REQUESTS`: base
percentiles cover successful requests, and when failures exist AIPerf also
emits `adj_<tag>` blocks with one `+inf` sample per failure. V1 certifies the
base percentiles, accepts results without `adj_*` keys, and never promotes
`adj_*` to threshold keys.

**ITL.** With `--per-chunk-usage` off, as v1 requires, one request's ITL is
`(request_latency − time_to_first_token) / (output_sequence_length − 1)`, its
mean decode interval. `p99` is over those per-request means, not over
individual token gaps. AIPerf raises `NoMetricValue` below two output tokens,
so the key is absent, not zero; a recipe that thresholds ITL uses an output
length of at least 2.

**Throughput.** `request_throughput` is `request_count / benchmark_duration`;
`output_token_throughput` is successful output tokens over the same duration.
Both exclude failures.

**Goodput.** `goodput` and `good_request_fraction` have the `GOODPUT` flag and
exist only when SLOs are configured. Their absence is not zero; when present,
zero is a real value.

**Achieved concurrency.** Computed from profiling JSONL records, using
`metadata.request_start_ns` and `metadata.request_end_ns`:

1. Sort the start and end events and sweep them to get in-flight count over
   time.
2. Set `windowStart = firstRequestStart + loadSettlingSeconds` and
   `windowEnd = lastRequestStart`. These boundaries exclude the qualified
   initial settling interval and the final drain. They never move based on the
   observed in-flight count, so a slow client cannot discard its own later
   underloaded interval by waiting for a favorable sample. V1 does not enable
   `--concurrency-ramp-duration`.
3. Evidence checks come first: fewer than `k × C` profiling requests, a
   nonpositive window, a window shorter than `minSteadyStateSeconds`, or fewer
   than `m` completions in the window yields `InsufficientEvidence`. No ratio
   is computed. Evaluate counts before dividing, including for a single request.
4. Clip every request interval to `[windowStart, windowEnd)` and sum its
   duration. Divide by window length to obtain achieved concurrency, then
   divide by `C` and compare with `minAchievedConcurrencyRatio`. Include all
   profiling attempts, including failures. Count a completion only when its
   end timestamp is inside that same half-open window.

A run need not ever reach exactly `C`: sustained concurrency above the ratio
passes when the evidence minimums are met. A short burst with `R = C` is
insufficient evidence, not client saturation. This window applies only to the
load-maintenance check; the exported performance metrics continue to describe
the full profiling population, as defined above.

The supervisor samples the runner's cgroup `cpu.stat`
(`nr_periods`, `nr_throttled`, `throttled_usec`) at AIPerf start and exit and
records the throttled-period ratio, or absence if cgroup accounting is not
readable. AIPerf's `replay_sched_lag_*` and `replay_sched_degraded` metrics
apply only to fixed-schedule trace replay and are inactive at fixed
concurrency; a later trace-replay mode would also gate on
`replay_sched_degraded`.

## Appendix B: Fit with existing CRE code

- **`SetNumNodes`** returns no error, and `createJobForGroup` always calls
  `SetNumNodes(len(group.Nodes))` so bisection can shrink a TrainJob. The
  signature is unchanged. Render, the catalog entry (`nodesPerJob: 1`), and
  Workflow preflight reject any group whose size differs from `NodesRequired()`
  before that call; `SetNumNodes(1)` leaves tensor parallelism alone.
- **Placement methods.** `NodesRequired` reports serving nodes.
  `SetNodeSelector`, `SetNodeAffinity`, and `SetTolerations` apply to the
  serving spec; the client follows the serving pod.
- **`InjectPodLabel`** writes `nvcre.nvidia.com/job`, which
  `DiscoverNodesForJob`, node health checks, and failure-log capture treat as
  the tested set. The reconciler copies it onto serving pods only.
- **`shouldWaitForPodDrain`** lists pods through the `nvcre.nvidia.com/job`
  index, so it sees only serving pods. `CleanupComplete=True` covers the
  client and publisher pods. Training drain behavior is unchanged.
- **`CleanupComplete` copy.** `reconcileWorkload` returns early once the Job
  is terminal (`isTerminalState`), so `updateStatusFromWorkload` never runs
  again. A condition copied there after the Job turned terminal would stay
  stale forever. That is why InferenceRun turns terminal only with a final
  `CleanupComplete` (§6). `updateStatusFromWorkload` already `Get`s the object
  in `job.status.workloadRef` before calling `GetStatus`; in the reconcile that
  sees InferenceRun terminal, it copies `CleanupComplete` in the same status
  write that sets the Job's terminal phase. No code path runs after the
  terminal guard. `GetStatus` keeps the [ADR-003](003-workload-adapter-pattern.md)
  phase/reason/message contract and gains no cleanup field. Manager RBAC gains
  get/list/watch on InferenceRun; the publisher Role cannot write Job status.
- **Outer timeout.** Workflow currently writes `JobFailed/JobTimedOut` directly
  before `completeTerminalGroup` deletes the workload. Inference must branch
  before that status write and use the nonterminal cancellation protocol in
  Appendix C. Otherwise the Job terminal guard bypasses both cleanup copying and
  evidence freezing. The Workflow backstop in Appendix C replaces the old guarantee
  that `timeoutPerJob` ends the group regardless of the child. The existing
  path remains for TrainJob.
- **Iterations.** Workflow creates a new Job for each group every iteration
  (`getGroupJobName(..., orch.CurrentIteration)`), and the pod-drain barrier
  stops waiting after `podDrainGracePeriod` (5 minutes). Neither keeps a
  cleanup-blocked node out of the next iteration, so Workflow records such
  nodes in orchestration status and skips them in group creation.
- **`collectJobMeasuredValues`** emits goodput-derived values only when `> 0`.
  Copying that test for inference would turn a perfect run's zero error ratio
  into a missing key and fail the Job with `MeasurementTimeout`, which is why
  §5 emits present zeros. Training keys (`goodputRatio`, bandwidth, step time)
  are not collected for an inference Job.
- **Result ownership.** `ensureGoodputMeasurement` and
  `ensureBandwidthMeasurement` use `getOwnerWorkflow`, which currently returns
  nil on a lookup error and does not compare the fetched Workflow UID with the
  reference. Inference reuses the ownership policy, not that helper's fallback:
  a strict resolver returns an owner or an error under §7. Existing measurement
  behavior is unchanged. The manager role already has get/list/watch on
  ConfigMaps.

## Appendix C: Timeout and cancellation protocol

Workflow's `timeoutPerJob` is an outer execution cap for inference, not
permission to skip cleanup or result freezing (§6). A persisted
execution-start timestamp anchors that cap; warmup and profiling timestamps
cannot reset it. The inference timeout path differs from today's TrainJob
path, which writes `JobFailed` directly (Appendix B):

1. Workflow records a nonterminal `CancellationRequested` condition on the Job
   with reason `JobTimedOut` and the first request timestamp. It does not write
   `JobFailed` or enter the existing terminal-group deletion path yet.
2. The Job controller honors cancellation before creating or observing work.
   For an existing InferenceRun it sets a write-once `spec.cancellation`
   control field containing the reason and timestamp. This field is outside
   the immutable experiment configuration and its digest. CRD validation
   enforces both rules: the experiment groups are immutable
   (`self == oldSelf`), and `cancellation` may go from unset to set but never
   change afterwards. Reconciliation retries propagate the same request; they
   never create a new attempt.
3. InferenceRun stops the benchmark and server and completes cleanup under its
   separate cleanup deadline. Cancellation during Cleaning records the timeout
   outcome without restarting that deadline. No further profiling is allowed.
4. The Job controller freezes the failure evidence (§7), then writes the Job's
   terminal phase and final `CleanupComplete` together. Workflow then completes
   the group. `JobTimedOut` remains non-retryable; `CleanupIncomplete` takes
   precedence when cleanup expires, with the timeout cause retained.

If cancellation arrives before a workload exists, the Job controller prevents
creation, confirms no attempt resources exist, freezes the cancellation
evidence, and records cleanup complete. An ambiguous create/read response must
be resolved against the deterministic resource identity before absence is
asserted. Training timeout behavior is unchanged.

The protocol depends on InferenceRun enforcing its cleanup deadline, so
Workflow keeps a backstop that does not. A bug, a finalizer stuck on a failing
API call, or a deadline check that runs only after a successful read would
otherwise leave the Job nonterminal forever. Once the cleanup deadline plus a
grace period has passed since the first cancellation request, which is
`timeoutPerJob + cleanup deadline + grace` after execution start, Workflow stops
waiting on the Job. It fails the group with
`CleanupIncomplete` and a cause that names the unresponsive InferenceRun, marks
the node cleanup-blocked under the §6 rules, and attributes nothing to
hardware. It does not delete the Job or InferenceRun itself; their finalizers
keep running. Independently, InferenceRun evaluates its cleanup deadline from
persisted timestamps on every reconcile, including one whose reads fail, so
the deadline expires even while the API is erroring.

## References

- [ADR-003: Typed workload adapters](003-workload-adapter-pattern.md)
- [ADR-005: LogProfile goodput measurement](005-logprofile-goodput-measurement.md)
- [ADR-015: Auto-created GoodputMeasurement](015-auto-created-goodput-measurement.md)
- [ADR-017: NCCL bandwidth measurement](017-nccl-bandwidth-measurement.md)
- [ADR-021: Performance thresholds](021-performance-threshold-enforcement.md)
- [ADR-034: Inferred dependency lifecycle](034-inferred-dependency-lifecycle.md)
- [ADR-051: Keep controllers off GPU nodes](051-tolerate-all-taints.md)
- [ADR-061: Failed node attribution](061-nvcre-nvsentinel-remediation-decoupling.md)
- [ADR-062: Node results in ConfigMaps](062-node-detail-propagation.md)
- [ADR-066: Removal of generic kubeJob](066-remove-kubejob-workload-type.md)
- [ADR-069: cmd/ layout](069-cmd-layout.md)
- [ADR-072: Terminal measurement freeze](072-goodput-terminal-freeze.md)
- [ADR-077: Workload image override](077-workload-image-override.md)
- [ADR-079: Workload-object labels](079-workload-object-labels.md)
- [AIPerf README, reviewed source](https://github.com/ai-dynamo/aiperf/blob/ab7c8ee87c77b4848f853d7b38dc2c2f0b796157/README.md)
- [AIPerf YAML configuration](https://github.com/ai-dynamo/aiperf/blob/ab7c8ee87c77b4848f853d7b38dc2c2f0b796157/docs/tutorials/yaml-config.md)
- [AIPerf export model](https://github.com/ai-dynamo/aiperf/blob/ab7c8ee87c77b4848f853d7b38dc2c2f0b796157/src/aiperf/common/models/export_models.py)
- [AIPerf JSON export schema](https://github.com/ai-dynamo/aiperf/blob/ab7c8ee87c77b4848f853d7b38dc2c2f0b796157/docs/reference/json-export-schema.md)
- [AIPerf profile exports](https://github.com/ai-dynamo/aiperf/blob/main/docs/tutorials/working-with-profile-exports.md)
- [AIPerf inference goodput](https://github.com/ai-dynamo/aiperf/blob/main/docs/tutorials/goodput.md)
- [AIPerf metrics reference](https://github.com/ai-dynamo/aiperf/blob/main/docs/metrics-reference.md)
- [AIPerf Kubernetes integration](https://github.com/ai-dynamo/aiperf/blob/main/docs/kubernetes/getting-started.md)
- [Exemplar Performance](https://github.com/NVIDIA/exemplar-performance)
- [InferenceX overview](https://github.com/SemiAnalysisAI/InferenceX/blob/main/README.md)
- [InferenceX pipeline architecture](https://github.com/SemiAnalysisAI/InferenceX/blob/main/docs/architecture.md)
- [InferenceX benchmark library](https://github.com/SemiAnalysisAI/InferenceX/blob/main/benchmarks/benchmark_lib.sh) (`run_benchmark_serving`, AgentX `aiperf profile`)
- [InferenceX on AIPerf / AgentX](https://inferencex.semianalysis.com/about)
- [vLLM bench serve](https://docs.vllm.ai/en/latest/cli/bench/serve/)
- [MLPerf Inference](https://github.com/mlcommons/inference)
