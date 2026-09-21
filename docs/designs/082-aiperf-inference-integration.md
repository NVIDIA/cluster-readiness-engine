# ADR-082: Inference Certification and the AIPerf Measurement Contract

> **Status:** Proposed — requires approval before implementation.
> **Date:** 2026-09-21

## Context

CRE needs to certify GPU clusters for inference as well as training. Its
existing training and communication workloads measure training goodput and
NCCL bandwidth, but leave the inference serving path untested. Inference adds
another requirement: the deployed model must sustain a defined request load
while meeting latency, throughput, and error criteria on the selected GPU nodes.
Adding this capability lets CRE evaluate clusters against the workloads they
will serve in production.

Existing benchmark tools provide a foundation for this work.
[Exemplar Performance](https://github.com/NVIDIA/exemplar-performance) provides
model and hardware recipes, while
[InferenceX](https://github.com/SemiAnalysisAI/InferenceX) explores inference
performance through scenarios, configuration sweeps, and published comparisons.
These projects inform what CRE should test and how to make experiments
reproducible. CRE still needs a measurement engine that it can run within its
own node selection, workload lifecycle, and certification process.

[AIPerf](https://github.com/ai-dynamo/aiperf) fits that role. It generates
traffic against inference endpoints, handles streaming responses and warmup,
and exports latency distributions, throughput, errors, and inference goodput.
Its use in InferenceX's AgentX scenario demonstrates its relevance to broader
inference performance work; InferenceX uses a separate client for fixed-sequence
benchmarks. Integrating AIPerf gives CRE these specialized measurement
capabilities without maintaining its own load generator and streaming metrics
implementation. Its structured exports also provide a concrete interface for
bringing measurements into CRE's threshold evaluation and reports.

The design therefore introduces inference as a CRE workload, with AIPerf as
its benchmark client. CRE will deploy the serving workload, establish which
nodes were exercised, run the benchmark, and evaluate the resulting evidence.
The first integration uses the `aiperf profile` executable and structured
exports from a pinned runner image. AIPerf's Kubernetes operator remains an
option for future distributed load generation.

The contract below defines how configuration reaches AIPerf, how results return
to CRE, and how both components participate in a finite test with explicit
completion and failure semantics. The initial scope is one model, one streaming
chat endpoint, synthetic token lengths, a fixed concurrency, and a
request-count-bounded profiling phase.

### Current CRE constraints

- [WorkloadSpec](../../api/v1alpha1/job_types.go) currently has only `trainJob`.
  [ADR-066](066-remove-kubejob-workload-type.md) removed the unused generic
  `kubeJob` arm. A batch Job example is not an already-supported CRE workload.
- The [workload adapter](../../pkg/workload/adapter.go) builds and observes one
  typed resource. An inference server is long-lived; server readiness cannot
  map to `WorkloadSucceeded`.
- The [Job controller](../../pkg/controller/job_controller.go) distinguishes
  execution success from the `ValidationFailed` verdict. Workflow waits for
  threshold evaluation before accepting a successful group.
- [Measured-value collection](../../pkg/controller/job_threshold_helpers.go)
  currently reads GoodputMeasurement and BandwidthMeasurement. The
  [threshold registry](../../pkg/threshold/evaluator.go) has no inference keys.
- [Training goodput](../../pkg/goodput/calculator.go) is a runtime efficiency
  ratio. AIPerf inference goodput is SLO-compliant requests per second. They are
  different quantities and cannot share `goodputRatio`.

## Decision

**Integrate AIPerf as an external benchmark executable behind a CRE-owned,
versioned request/result contract. CRE owns the finite inference test and the
certification verdict.**

The first integration runs `aiperf profile` inside a pinned runner image. It
does not embed Python in the manager, import AIPerf internals into controller
code, fork its load generator, or shell out to `aiperf kube` from a reconciler.

### 1. Ownership boundary

| Concern | Authority |
|---|---|
| Model/engine recipe and supported hardware combinations | CRE catalog, optionally adapted from a pinned external recipe |
| Serving resources, placement, readiness, deadlines, cleanup | CRE inference reconciler; a future serving operator retains ownership of its descendants |
| Request scheduling, streaming parsing, native metrics | AIPerf |
| Translation to/from the pinned AIPerf interface | CRE request translator, inside the runner image |
| Result identity, completeness, units, coverage validation | CRE result ingestion |
| Performance thresholds and certification verdict | Existing CRE Job → Workflow → Certification path |
| Node hardware failure attribution | CRE node-health evidence, scoped to serving nodes |

The runner returns measurements and execution evidence. It does not decide
whether a Certification passes, create serving resources, select target GPU
nodes, or retry an entire experiment.

The catalog holds the recipe. InferenceRun runs one finite attempt. The frozen
result ConfigMap is what thresholds and reports read. AIPerf fills the client
slot inside the attempt, the way `all_reduce_perf` fills the binary slot
inside a TrainJob.

### 2. A finite workload resource

Add a typed `WorkloadSpec.inferenceRun` arm and a namespaced
`nvcre.nvidia.com/v1alpha1 InferenceRun` CRD. Its adapter implements the existing
single-resource contract. A new reconciler manages the composite experiment.
This is the concrete cost of supporting a server plus a separate finite client;
it is not a generic workflow engine or a second public certification tier.

```text
Certification → Workflow → CRE Job → InferenceRun
                                      ├── serving Deployment + Service
                                      ├── benchmark batch/v1 Job
                                      │     ├── AIPerf runner
                                      │     └── result publisher
                                      ├── immutable request ConfigMap
                                      ├── publisher result ConfigMap (transport)
                                      └── per-attempt artifacts PVC

Workflow owns one frozen result ConfigMap per attempt.
Reports and thresholds read that ConfigMap.
```

All children are in the InferenceRun namespace. CRE Job owns InferenceRun;
InferenceRun owns the transient children. The frozen result ConfigMap is in
the same namespace and is owned by the Workflow, not by InferenceRun. The
artifacts PVC has the explicit
retention policy described below. Existing caller-owned model PVCs and Secrets
are referenced, never adopted or deleted.

The serving Deployment is not a Workflow dependency.
[ADR-034](034-inferred-dependency-lifecycle.md) dependencies are static
resources named by the job template, created before the Job, and deleted with
their scope. The server is attempt-scoped coverage evidence: its Pod UID and
node are part of the result, and it must release the GPU before another attempt
reuses the group. That lifecycle belongs to InferenceRun.

The initial serving arm is typed `vllm`: one Deployment replica on one selected
GPU node, a Service selecting only that attempt, and a pinned model revision.
It supports tensor parallelism within that node. The initial benchmark arm is
typed `aiperf`. Exactly one serving arm and one benchmark arm must be set.
Do not expose arbitrary resource YAML, arbitrary shell commands, or the entire
upstream AIPerf configuration as CRE's public API.

`WorkloadSpec` stays a one-arm union: `trainJob` or `inferenceRun`, never both.
This does not restore `kubeJob`. [ADR-066](066-remove-kubejob-workload-type.md)
removed that public arm, and the benchmark `batch/v1` Job is only an
InferenceRun child. The CRE Job controller does not `Owns()` batch Jobs; the
InferenceRun reconciler does. The in-image component that turns
`cre.inference.request/v1` into an AIPerf argument vector is a request
translator. It is not a `pkg/workload` Adapter, and it does not live in
`ForSpec()`.

`NodesRequired` reports **serving nodes**, not load-generator nodes.
`SetNodeSelector`, `SetNodeAffinity`, and `SetTolerations` constrain the serving
spec. Load-generator scheduling is a separate field. v1 accepts only one
serving node and does not resize tensor parallelism to match a group.

`SetNumNodes` cannot carry that rejection. The method returns no error, and
`createJobForGroup` always calls `SetNumNodes(len(group.Nodes))` so bisection
can shrink a TrainJob. V1 does not change that signature. Render, the catalog
entry (`nodesPerJob: 1`), and Workflow preflight fail the submission when the
group size is not `NodesRequired()` before that call. `SetNumNodes(1)` leaves
tensor parallelism alone. Diagnose and bisection stay disabled for inference:
both depend on `SetNumNodes` changing the group.

`InjectPodLabel` writes `nvcre.nvidia.com/job`. `DiscoverNodesForJob`, node
health checks, and failure-log capture treat every pod with that label as part
of the tested set. The InferenceRun reconciler copies it onto serving pods
only. Benchmark and publisher pods do not get it. Failure-log selection still
reads those pods by role label, so a client crash is captured without adding
the client node to hardware attribution.

The load generator must not consume the selected GPU node. `clientScheduling`
requires a non-GPU node, the same placement rule [ADR-051](051-tolerate-all-taints.md)
uses for controllers (`nvidia.com/gpu.present` DoesNotExist), unless a later
recipe explicitly documents a different client placement.

[ADR-077](077-workload-image-override.md) rewrites `trainJob.trainer.image`
only. An inference category has two pinned images, serving and runner, and
neither is that field. V1 rejects `options.image` on an inference category
instead of applying it to one digest. Platform NCCL, EFA, RoCE, and
ComputeDomain patches are TrainJob and TrainingRuntime rewrites; the inference
catalog entry does not include them. `workloadMetadata.labels` remain labels
on the InferenceRun object, per [ADR-079](079-workload-object-labels.md). V1
rejects gang and queue settings until child propagation and admission are
tested. Plain Kubernetes scheduling is the initial path.

### 3. Input contract

The immutable InferenceRun spec contains the following typed groups. These
names describe the proposed API; they are not fields available in CRE today.

| Group | Required meaning |
|---|---|
| `serving.vllm` | Image digest, model identifier/revision, local model/tokenizer references, dtype/quantization, tensor parallel size, GPU/CPU/memory requests, validated engine options |
| `benchmark.aiperf` | Qualified runner image digest, streaming chat endpoint type, synthetic input/output token profile, seed, concurrency, warmup request count, profiling request count, request timeout, optional latency SLOs |
| `validity` | Minimum successful profiling requests and minimum observations for each required latency statistic |
| `timeouts` | Admission/scheduling, serving readiness, total benchmark, collection, and cleanup deadlines |
| `clientScheduling` | CPU/memory requests and placement for the load generator, separate from GPU targets |
| `artifacts` | Storage class/size, retention policy, and bounded output settings |

CRE derives the internal endpoint from the owned Service. The runner verifies
model availability and completes a small streamed generation before warmup.
Pod `Running`, Deployment availability, or a TCP-open check alone is not model
readiness. Readiness and warmup requests are excluded from profiling statistics.

V1 uses one model, one streaming chat endpoint, one fixed concurrency, one
warmup phase, and one request-count-bounded profiling phase per attempt. It
rejects sweeps, adaptive search, multi-turn requests, intentional cancellation,
and automatic request retries. These restrictions make the validity and error
denominators explicit. Later traffic modes require contract fixtures and a
documented interpretation of offered versus achieved load.

The runner's internal input is `cre.inference.request/v1` JSON, rendered into
an immutable ConfigMap. It includes the resolved spec, CRE Job UID,
InferenceRun UID, attempt ID, and a SHA-256 digest of canonical JSON for the
effective non-secret configuration. It also records Secret/PVC reference
identities; secret values are never serialized into this document or its hash.
Secrets are mounted only into the containers that need them.

The request translator turns that document into the pinned AIPerf CLI/config
format and invokes the executable with an argument vector. AIPerf flags,
environment defaults, output directories, and supported schema versions are
implementation details of this translator. Endpoint, output path, seed, and stop conditions cannot
be overridden by an unvalidated options bag. Missing assets must fail preflight;
model/tokenizer downloads are outside the measured phase.

Target output length must be enforced through qualified engine settings, and
actual token-length distributions must be retained. A requested length alone
does not establish that the server generated that length. Prefix caching,
sampling, precision, and token-counting mode are recorded parts of the recipe.

### 4. Process and artifact contract

The benchmark Job runs once (`backoffLimit: 0`, `restartPolicy: Never`) with
a finite `activeDeadlineSeconds`. A CRE supervisor runs AIPerf in a process
group, enforces the deadline, forwards termination, and reaps descendants.
It writes native output to a fresh attempt directory on an artifacts PVC.

After the child exits, the supervisor closes output files, validates and
normalizes them, and atomically renames a completion manifest into place.
The manifest contains execution outcome, file sizes, hashes, and the normalized
result location. An absent manifest is incomplete, even if some JSON exists.
A forced termination or OOM can leave partial diagnostic artifacts but cannot
produce an accepted result. Normalization never turns a nonzero AIPerf exit
into execution success.

The second, finite publisher container waits for that manifest, verifies file
hashes, and publishes `result.json` into a transport ConfigMap pre-created
by the InferenceRun reconciler. It exits after publication; publication retries
are bounded and do not rerun AIPerf. The Job deadline also bounds a publisher
waiting for a manifest that will never arrive. The publisher mounts artifacts
read-only. It accepts an identical already-published result after a retry, and
rejects conflicting content. The reconciler verifies that transport ConfigMap's
UID and owner before ingestion, then writes the Workflow-owned result ConfigMap
from section 7. Later publisher writes cannot change that frozen object.

The publisher uses an explicit projected service-account token and a Role
granting `get`/`update` only on that transport ConfigMap name. The benchmark and
server containers receive no Kubernetes API token. The publisher has no rights
to create workloads or change CRE status. The manager reads results through the
Kubernetes API; it never execs into benchmark pods or parses console tables.

This is not a LogProfile measurement. [ADR-005](005-logprofile-goodput-measurement.md)
and [ADR-017](017-nccl-bandwidth-measurement.md) tail pod logs and parse lines
while the workload runs. AIPerf's contract is a closed set of files plus a
completion manifest. The publisher exists to place the bounded normalized
result on the API; native files stay on the PVC. Do not add an inference
LogProfile that scrapes the AIPerf console.

The normalized result is limited to 64 KiB. Full native JSON/JSONL, input data,
logs, and resolved configuration remain on the PVC with size limits and a
manifest. Exceeding a required artifact limit fails collection rather than
silently truncating accepted evidence. The retained PVC is not owned by a
short-lived Pod and does not require the completed runner to stay alive.

### 5. Result contract

The publisher submits `cre.inference.result/v1`. Ingestion requires:

| Field | Contract |
|---|---|
| `identity` | CRE Job UID, InferenceRun UID, attempt ID, runner Job/Pod UID, effective-config digest; all match controller-held values |
| `producer` | Runner image digest/version, AIPerf version, native export schema version, normalizer version |
| `execution` | Child exit code, cancellation/completeness flags, start/end timestamps, stop reason |
| `population` | Profiling attempted/successful/failed requests, observation counts, measurement interval, achieved input/output lengths |
| `metrics` | Explicit values and units under the CRE metric names below; absence is distinct from zero |
| `artifacts` | PVC namespace/name/UID, attempt-relative directory, manifest hash and native artifact hashes |

Serving placement is independently observed and attached by the reconciler:
server resource UID, Pod UID, node name/UID, image ID, model revision, and
Service/EndpointSlice membership. Client-supplied node names do not establish
which GPU nodes were tested. The reconciler rejects a result if serving identity
or placement changed during the measured attempt.

The request translator reads the single-run `profile_export_aiperf.json` and
records in `profile_export.jsonl`. Schema 1.4 keeps top-level JSON metrics
profiling-only; warmup lives in `warmup_metrics`, which v1 does not read.
The JSONL writer emits every record that has displayable metrics or an error,
and each record's `metadata.benchmark_phase` is `warmup` or `profiling`. The
translator counts only `benchmark_phase == "profiling"`. It verifies those
counts against the summary population. It does not infer completeness from a
nonempty summary, average percentiles from separate runs, or consume the
differently shaped multi-run aggregate file as a single-run export.

An accepted native export must explicitly establish completion and lack of
cancellation under the qualified schema. The request translator checks the resolved native
input against the CRE request, confirms the requested stop condition was met,
and compares producer metadata with the qualified runner's actual image ID.
The published contract version and producer tuple are checked independently;
an upstream schema string alone does not establish compatibility.

For v1, every attempted profiling request must have exactly one terminal
profiling record. Successful requests have no request error and valid
completion/token evidence. HTTP, transport, timeout, and invalid responses
count as failures. With no request retries or deliberate cancellations,
`attempted = successful + failed`. An unaccounted request makes the result
incomplete, not a zero-error run. The pinned request translator must qualify
this accounting against actual AIPerf output.

`request_count` is successful requests. `error_request_count` carries
`ERROR_ONLY` and is omitted on a clean run; absence there is zero, not missing
data, and only when `is_complete` is true and `was_cancelled` is false.
Otherwise a missing error count stays incomplete. On that same complete,
uncancelled run:

- `attempted = request_count + error_request_count`, treating either omitted
  counter as zero when the other is present.
- `completed_request_count` must equal `attempted`.
- `request_error_rate` is a percent. It must equal
  `100 * error_request_count / attempted`. The CRE key below is the ratio
  `error_request_count / attempted`, not the percent.

`time_to_first_token`, `inter_token_latency`, and `request_latency` carry
`PERCENTILE_INCLUDES_FAILED_REQUESTS`. Their base percentiles are the
successful requests only. When the run has failed requests, AIPerf also emits `adj_<tag>`
blocks that append one `+inf` sample per failure. Those blocks are absent on a
clean run. v1 certifies the base percentiles. The translator accepts a result
that has no `adj_*` keys, and it does not promote `adj_*` into threshold keys.
Failures are certified by `inferenceErrorRatio`.

`inter_token_latency` for one request is
`(request_latency - time_to_first_token) / (output_sequence_length - 1)`
when `--per-chunk-usage` is off, which v1 leaves off. That value is the
request's mean decode interval. `p99` is the 99th percentile of those
per-request means, not a percentile of individual token gaps. The CRE name
stays `inferenceItlP99Ms`. It is not inter-chunk latency. AIPerf raises
`NoMetricValue` when output sequence length is below 2, so the metric is
omitted. A recipe that thresholds ITL uses an output length of at least 2.
A one-token response leaves ITL absent; absence is not zero latency.

`request_throughput` is `request_count / benchmark_duration`, and
`output_token_throughput` is total successful output tokens over that same
duration. Both exclude failed requests. A high error rate is an
`inferenceErrorRatio` failure. It is not expressed by reinterpreting
throughput as if failed requests had been counted in the numerator.

| CRE threshold key | Source / statistic | Unit | Population |
|---|---|---|---|
| `inferenceTtftP99Ms` | `time_to_first_token.p99` | ms | Successful requests. `adj_time_to_first_token` is ignored |
| `inferenceItlP99Ms` | `inter_token_latency.p99` | ms | Per-request mean decode interval, successful requests with output length ≥ 2 |
| `inferenceRequestLatencyP99Ms` | `request_latency.p99` | ms | Successful requests. `adj_request_latency` is ignored |
| `inferenceOutputTokensPerSec` | `output_token_throughput.avg` | tokens/s | Successful output tokens / benchmark duration |
| `inferenceRequestsPerSec` | `request_throughput.avg` | requests/s | Successful requests / benchmark duration |
| `inferenceGoodputRequestsPerSec` | `goodput.avg` | requests/s | SLO-compliant requests / duration, only when explicit SLOs were configured |
| `inferenceGoodRequestFraction` | `good_request_fraction.avg` | ratio 0–1 | `good_request_count / attempted`, only when explicit SLOs were configured. Errors stay in the denominator, so the fraction stays comparable across concurrency |
| `inferenceErrorRatio` | `error_request_count / attempted` | ratio 0–1 | Profiling requests. Zero is a present value on a complete clean run |

`goodput` and `good_request_fraction` are `GOODPUT` metrics. They are absent
unless SLOs were configured, and that absence is not zero. When they are
present, zero is a real measurement.

Absence stays distinct from zero for every other key. The `ERROR_ONLY` error
counter on a complete, uncancelled run is the exception above.
`collectJobMeasuredValues` today emits a goodput-derived value only when it is
greater than zero (`job_threshold_helpers.go`). An inference collector that
copies that test turns a perfect run's zero error ratio into a missing key,
and the missing-key path then fails the Job with `MeasurementTimeout`.
Inference keys are emitted whenever the frozen result contains them, including
zero. A key that the result does not contain stays missing.

Reject unsupported schema/producer versions, unit mismatches, non-finite or
out-of-range values, stale identities, inconsistent counts, incomplete runs,
disagreement with `completed_request_count` or `request_error_rate`, and
missing required metrics. Unknown optional upstream fields may be ignored
within a qualified schema, but do not become threshold keys automatically.
Zero errors are valid; zero successful requests cannot pass validity checks.

### 6. Lifecycle, verdict, and failure contract

| Stage | Exit condition / persisted evidence |
|---|---|
| Pending | Spec/preflight valid; serving resources admitted and scheduled; waiting has a separate deadline |
| Preparing | Server image/model ready; serving identity and target placement recorded; client scheduled |
| Benchmarking | Streaming readiness probe, warmup, then profiling within the benchmark deadline |
| Collecting | Publisher finishes; Job completion and matching result envelope observed; ingestion validates and freezes evidence |
| Cleaning | Stop owned serving/client resources; confirm their deletion before releasing the group |
| Succeeded / Failed | Durable execution result and bounded cleanup outcome; no later result replacement |

Pending maps to adapter `WorkloadPending`; Preparing, Benchmarking, Collecting,
and Cleaning map to `WorkloadRunning`. InferenceRun becomes Succeeded only after
valid evidence is persisted and owned serving and client pods are gone. A
persisted execution-start timestamp anchors the overall execution timeout;
warmup and profiling timestamps are separate and cannot reset that clock.
Admission wait does not count as measured runtime, but cannot wait forever.

That Succeeded rule is stricter than TrainJob. A TrainJob can be Succeeded
while its pods still exist. A serving pod holds the GPUs under test, so
inference `Succeeded` means those pods are already gone.

Workflow does not read InferenceRun. It reads the Job.
`GetStatus` stays the [ADR-003](003-workload-adapter-pattern.md) phase
contract: phase, reason, and message. It does not grow a cleanup field, and
v1 does not add an adapter method for one. `GetStatus` stays
`WorkloadRunning` through Cleaning and returns `WorkloadSucceeded` only after
serving and client pods are gone, so the Job becomes Succeeded only then. On
failure, `GetStatus` returns `WorkloadFailed` while cleanup is still in
progress.

The Job controller already `Get`s the object named by
`job.status.workloadRef` before it calls `GetStatus`. When that object is an
InferenceRun, the same reconcile reads its `CleanupComplete` condition and
copies it onto the Job. That read is allowed because the controller owns the
child and already fetched it. It is not a Workflow watch and not a change to
`WorkloadStatus`. Workflow retry and group release check the Job condition.
Generated manager RBAC includes get, list, and watch on InferenceRun. The
publisher Role still cannot write Job status.

`shouldWaitForPodDrain` lists pods by `nvcre.nvidia.com/job`. That set is the
serving pods. Benchmark and publisher pods do not carry the label, so the
drain barrier does not see them. `CleanupComplete=True` on the Job means those
pods are gone as well. Training drain behavior is unchanged.

The InferenceRun owns its stage deadlines; the CRE Job's configured execution
timeout remains an outer cap. Expiring either cancels the attempt. Reconcile
restarts resume from persisted timestamps and resource UIDs, never restart a
timer or launch a second benchmark for the same attempt.

InferenceRun reports execution and measurement validity, not threshold pass.
The CRE Job reads the frozen result ConfigMap from section 7 through
`collectJobMeasuredValues`, evaluates the registered inference keys with the
existing CEL threshold path, and writes `ValidationFailed=False` only when all
required criteria pass. It does not copy those values onto Job status. A
performance miss keeps the existing separation: Job execution may be Succeeded
while Workflow fails validation. Missing or malformed evidence makes
InferenceRun fail with an explicit result reason before successful execution
is advertised. Training keys (`goodputRatio`, bandwidth, step time) are not
collected for an inference Job.

Certification catalog recipes must supply nonempty performance thresholds,
including an error-ratio ceiling, plus minimum sample requirements. A direct
benchmark without thresholds may report execution/measurements, but must be
labeled **unvalidated** and must not generate a certification-pass claim.

| Failure | Classification / action |
|---|---|
| Unschedulable client, missing model/Secret, image pull, readiness timeout | Execution/setup failure; preserve cause; no hardware attribution |
| AIPerf crash/OOM/deadline, incomplete population, publisher failure, unsupported schema | Benchmark/collection failure; never pass or blame server nodes from client failure alone |
| Valid result exceeds TTFT/ITL/error or throughput criterion | Performance validation failure for the tested group; not proof of a defective individual node |
| Serving Pod replacement, endpoint membership drift, unverified coverage | Coverage failure; invalidate attempt and retain evidence |
| Health detector identifies a serving-node hardware fault | Existing hardware-failure path with node-specific evidence; stop the attempt |
| Forbidden/timeout while reading or deleting children | Preserve API error and retry within the relevant deadline; never interpret as absence |

Role labels (`server`, `benchmark`, `publisher`) sit beside the association
labels. The tested-node set is still exactly the pods that carry
`nvcre.nvidia.com/job`, which section 2 limits to serving pods. Failure-log
capture learns the role labels so it can read a crashed benchmark container
without treating that pod's node as a tested GPU node. Generic
workload-failure attribution needs an inference-specific audit before an
inference catalog entry ships.

Workflow alone owns whole-attempt retries. Each retry has a fresh attempt ID,
resources, output directory, and result ConfigMap. On the failure path it
waits for the Job's `CleanupComplete=True` before reusing the node group.
There is no checkpoint restart for inference. Failed-attempt evidence is kept;
a later success does not replace it.

Deletion/cancellation first stops the benchmark, then the owned server, then
transient configuration/RBAC resources. Finalizers verify cleanup, including
in envtest without garbage collection. A cleanup timeout is surfaced; the group
is not declared safely released and a finalizer is not silently removed.
The Job keeps `CleanupComplete=False` with the cause while cleanup is blocked,
even when `Failed` is already true. Workflow retry and group release read that
Job condition. Terminal `Failed` is not evidence that the GPU is free.

### 7. Retention and reporting

The reconciler freezes accepted metrics, provenance, and artifact references
before deleting compute. Readers use one object, and it is not Job status.

[ADR-072](072-goodput-terminal-freeze.md) keeps frozen goodput on the
GoodputMeasurement and refuses a second copy on Job, Workflow, or Certification
status. The report and `collectJobMeasuredValues` both read that object, and
only after `Complete=True`. GoodputMeasurement and BandwidthMeasurement are
created by the Job controller and owned by the Workflow when the Job has one,
so they survive the Job deletion in `handleIterationComplete`. A direct Job
with no Workflow owner keeps the measurement itself.

Inference follows that ownership rule with a different birth time. The result
does not exist at Job creation, so nothing is created up front to sample. When
ingestion accepts the publisher document, the InferenceRun reconciler writes
one immutable result ConfigMap and sets a freeze condition plus the ConfigMap
reference on InferenceRun status. The ConfigMap is owned by the Workflow, or
by the CRE Job when there is no Workflow owner. Ownership decides retention,
not lookup: both modes are found through the same InferenceRun status
reference. The publisher ConfigMap stays transport owned by InferenceRun;
thresholds and reports do not read it.

A measurement CRD is not added. Nothing reconciles the result after the freeze
write. The ConfigMap is a bounded document with a status reference, the same
retention shape as the node-results ConfigMaps in
[ADR-062](062-node-detail-propagation.md). `collectJobMeasuredValues` receives
the Job. It follows `job.status.workloadRef` to the InferenceRun the
controller already fetched, and only after that object's freeze condition is
true does it `Get` the ConfigMap named by the reference on InferenceRun
status, checking the ConfigMap UID against the reference. It does not list
ConfigMaps by name or label, and `GetStatus` does not carry the reference.
The manager role already grants get, list, and watch on ConfigMaps. The
report reads the same object once orchestration status records the reference.
Neither path reads a live InferenceRun pod, and neither stores the metric map
on Job status. Inference keys present in that object are emitted even when
the value is zero, as section 5 requires.

Failed attempts get an evidence ConfigMap under the same ownership when no
valid metrics exist. Per-attempt results stay separate populations. Reports
show execution, validity, the threshold verdict, and cleanup state as separate
fields. Orchestration status records the ConfigMap reference for the current
attempt and an index reference for history; it does not grow an attempt array.
Report snapshots are exported before the owning Workflow or Job is deleted.
Rendered reports say when raw artifacts have been deleted or are unavailable.
Retained raw PVCs follow their own policy.

Artifacts default to **Retain**: the per-attempt PVC has no owner reference that
would garbage-collect it with the run, and carries origin UID/attempt labels.
CRE reports how to retrieve and explicitly delete it. Opt-in **DeleteWithRun**
removes it on run deletion, after writers stop; deleting a run with this policy
also deletes its raw evidence. Missing required retained storage is a preflight
failure. Automatic TTL and external object-store export are future work.

### 8. Compatibility and upstream upgrades

Publish a tested compatibility tuple: CRE contract version, runner digest,
AIPerf package/source version, input format, and native export schema. An image
override outside that tuple is rejected for certification. Pin serving images,
model/tokenizer revisions, recipe revision, and dataset seed/hash as well.

The reviewed upstream source is commit
[`ab7c8ee87c77b4848f853d7b38dc2c2f0b796157`](https://github.com/ai-dynamo/aiperf/tree/ab7c8ee87c77b4848f853d7b38dc2c2f0b796157).
Its `JsonExportData.SCHEMA_VERSION` is `1.4`; its schema documentation also
describes a `1.5` entry. This is evidence for checking produced artifacts and
source together, not accepting versions from documentation alone. This review
pin is **not** a qualified runner release or an assertion that a GPU test passed.

Before shipping the first catalog recipe, choose an immutable runner build and
capture real success, request-error, cancelled, and incomplete exports from it.
The request translator supports only tuples backed by those fixtures and an end-to-end
run. Every upgrade repeats that qualification. No floating `latest` tag, silent
schema fallback, or assumption that all `1.x` exports are equivalent.

## Implementation

Implementation follows approval of this ADR. No new API or controller is
implemented by the document change itself.

### Delivery order

1. **Qualify the process/file boundary.** Build the pinned runner and normalizer;
   capture upstream fixtures against a deterministic streaming test server.
   Establish native counts, stop conditions, warmup exclusion, token accounting,
   and metric units. Block unsupported semantics rather than guessing mappings.
2. **Add the typed finite workload.** Implement InferenceRun API, reconciler,
   workload adapter, watches, RBAC, publisher, deadlines, retention, and the
   vLLM serving arm. Follow the placement, label, image-override, and
   scheduling rules in section 2. Reject `goodputMeasurement`,
   `bandwidthMeasurement`, checkpoint, and stall settings on an inference Job.
   The runner binary is `cmd/<runner>/main.go` per
   [ADR-069](069-cmd-layout.md); translation and normalization stay in
   `pkg/inference/`. WorkloadRun does not grow an inference framework.
3. **Connect verdicts and reports.** Register the inference keys in
   `pkg/threshold.Registry`. Extend `collectJobMeasuredValues` to read the
   frozen result ConfigMap through `job.status.workloadRef` after the freeze
   condition, under either owner, so a direct inference Job collects the same
   keys as one under a Workflow. No metric copy on Job status, and emit present
   zeros. Mirror `CleanupComplete` from that same fetched InferenceRun onto
   the Job and make Workflow's retry guard read that Job condition. Render and
   Workflow preflight reject a group
   size other than one serving node, and reject `options.image` on an
   inference category.
4. **Add one catalog recipe.** A pinned single-node vLLM model with synthetic
   streaming traffic, sample minimums, and workload-specific thresholds. Run it
   across node groups to establish per-node coverage. Add API docs, a sample,
   public navigation, and artifact retrieval instructions with this feature.
5. **Qualify on GPUs.** Verify placement and achieved traffic, repeatability,
   resource sufficiency of the CPU client, deliberate failures, and cleanup.
   A mock HTTP server or Kind/KWOK result alone is not GPU/fabric validation.

Expected code areas are `api/v1alpha1/`, `pkg/workload/`, a new `pkg/inference/`
for contract validation and normalization, `pkg/controller/`, `pkg/threshold/`,
`pkg/catalog/entries/inference/`, `pkg/report/`, `pkg/render/`, and a thin
runner entrypoint under `cmd/`. CRDs, RBAC, and deepcopy are generated through
the existing build targets. The manager does not acquire a Python dependency.

### Acceptance criteria

- Structured input/output fixtures cover valid exports, unknown schemas,
  absent/null/zero values, wrong units, invalid numbers, insufficient samples,
  warmup exclusion, partial records, and count disagreement.
- Identity fixtures reject results from another UID/attempt/config, stale
  ConfigMaps, changed serving Pods, and foreign Service endpoints. Conflicting
  existing resource ownership fails closed rather than adopting resources.
- Reconcile tests cover restart at every stage, idempotent creation, publication
  conflicts, timeout/cancellation, health failure, API read errors, and cleanup
  without garbage collection. New attempts cannot overlap old compute.
- Verdict tests distinguish execution success, valid measurements, passing
  thresholds, failing thresholds, and missing evidence. Client failures never
  create serving-node hardware accusations or successful node coverage.
- RBAC/render tests prove only the publisher can update its transport
  ConfigMap, serving and client placement stay separate, `options.image` and
  platform TrainJob image rewrites do not move either pinned digest, and queue
  settings are rejected rather than ignored.
- End-to-end tests exercise the actual pinned AIPerf executable and publisher:
  streamed success, server errors, missing exports, runner kill, and retained
  artifact retrieval after compute deletion. GPU qualification verifies the
  actual server node identity and a failing performance threshold.
- Tests follow the repository's TestCaseParser/golden conventions and
  `.claude/skills/cre-test/SKILL.md`. Golden generation requires explicit approval.

## Rationale

This boundary reuses AIPerf's specialized measurement implementation while
keeping CRE's API, conditions, failure attribution, and verdicts independent of
upstream flags and export changes. A versioned normalizer is a smaller ongoing
commitment than maintaining traffic generation and streaming metric semantics.

InferenceRun preserves the one-resource workload adapter while making the
server and client lifecycle explicit. A public `kubeJob` arm would still leave
serving placement, readiness, evidence, and teardown unspecified. The frozen
result is a ConfigMap owned by the Workflow, or by the Job when there is no
Workflow, not a new measurement controller: the run produces one terminal
document, and Job status is not a second copy of it.

## Consequences

- CRE gains a CRD/reconciler, runner image, publisher, artifact-storage lifecycle,
  and compatibility fixtures. The integration is more than a catalog command.
- Inference needs persistent artifact storage and CPU capacity for load
  generation. Client saturation can hide server capacity; qualify the supported
  load range and retain achieved-load evidence.
- V1 certifies a declared single-node inference profile. It makes no claim about
  cross-node tensor parallelism, expert parallelism, or KV-cache-transfer fabric.
- Results are comparable only under equivalent model, precision, topology,
  request distributions, caching, and measurement definitions. External
  leaderboard numbers are contextual references, not universal pass thresholds.

## Alternatives Considered

Building a load generator inside CRE would duplicate request scheduling,
streaming parsing, token accounting, and statistical measurement. AIPerf
provides that capability through an executable and structured exports, with
upstream configuration isolated in the request translator. Running it inside a
TrainJob with shell-managed servers would reuse today's workload arm, but leave
serving readiness, ownership, and cleanup implicit. InferenceRun makes those
responsibilities part of the controller contract.

Exemplar recipes and InferenceX scenarios can inform catalog entries, with
source revisions and license attribution recorded. Their launchers and research
pipelines do not become CRE runtime dependencies. Published results require
matching experimental conditions before comparison; they are not automatic
acceptance thresholds for a customer cluster, and a CRE run is not an official
InferenceX result.

`vllm bench serve` is another endpoint benchmark, particularly useful alongside
vLLM development. AIPerf is the initial choice for a shared measurement interface
across future serving backends. MLPerf Inference and LoadGen serve standardized
benchmark methodology and submission requirements; supporting that methodology
would require a separate profile. This integration makes no MLPerf compliance
claim.

AIPerf's operator is a real alternative, not a missing upstream capability.
If a qualified single client cannot supply the required load, evaluate it before
building a distributed client coordinator. A future operator backend must
preserve CRE's request/result contract and give the AIPerf operator sole
ownership of its JobSet/Pods. That change requires an explicit lifecycle and
compatibility extension; CRE must not also reconcile those descendants.

## Notes

- This record decides the inference workload and the AIPerf contract together.
  The finite InferenceRun is what makes the contract enforceable. A later
  record can add a serving backend without reopening the measurement contract.
- Exemplar here means NVIDIA's `exemplar-performance` project.
- InferenceX uses AIPerf for AgentX (`utils/aiperf`, `aiperf profile`). Its
  fixed-sequence path uses `infx.bench_serving` and does not invoke AIPerf.
  CRE v1 uses AIPerf for a single synthetic streaming profile, so it matches
  neither InferenceX client contract.
- Deferred scope: external user-owned endpoints, Dynamo/llm-d serving operators,
  multi-node and disaggregated serving, distributed load generation, sweeps,
  trace replay, accuracy evaluation, and automatic artifact expiry. External
  endpoints especially need a separate coverage contract before they can
  certify selected CRE nodes.
- Next approval is of this design and its initial scope. Selecting a runner
  release/digest and qualifying actual artifacts are implementation gates,
  not evidence already established by this ADR.

## References

- [ADR-003: Typed workload adapters](003-workload-adapter-pattern.md)
- [ADR-015: Auto-created GoodputMeasurement](015-auto-created-goodput-measurement.md)
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
- [AIPerf Kubernetes integration](https://github.com/ai-dynamo/aiperf/blob/main/docs/kubernetes/getting-started.md)
- [Exemplar Performance](https://github.com/NVIDIA/exemplar-performance)
- [InferenceX overview](https://github.com/SemiAnalysisAI/InferenceX/blob/main/README.md)
- [InferenceX pipeline architecture](https://github.com/SemiAnalysisAI/InferenceX/blob/main/docs/architecture.md)
- [InferenceX benchmark library](https://github.com/SemiAnalysisAI/InferenceX/blob/main/benchmarks/benchmark_lib.sh) (`run_benchmark_serving`, AgentX `aiperf profile`)
- [InferenceX on AIPerf / AgentX](https://inferencex.semianalysis.com/about)
- [vLLM bench serve](https://docs.vllm.ai/en/latest/cli/bench/serve/)
- [MLPerf Inference](https://github.com/mlcommons/inference)
