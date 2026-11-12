# ADR-090: Sharded Component Results and the Report Bundle

> **Status:** Accepted

## Context

A customer runs custom workloads on NVCRE and reports that the workloads themselves work well, but that they cannot get detailed enough results out of them. Their asks, in their words, are to report results per GPU and per link rather than only per node or per job; to read those results without knowing how NVCRE's objects relate or when they are cleaned up; to make it explicit when a result is final, when a test failed, and when a result is missing; and to declare in the spec what results a workload produces instead of regex over a fixed set of fields.

That feature is larger than one record. This ADR settles only the two questions that every other part of it depends on: **where per-component result rows are stored**, and **how they leave the cluster**. A later ADR covers the declaration vocabulary, the ingestion controller, and the verdict rules.

The case this design was proved against is a per-GPU CUTLASS GEMM burn-in: one achieved-TFLOPS number per GPU is the simplest result that cannot be expressed per node, since a node average passes while one thermally throttled GPU drags it. [ADR-091](091-compute-gemm-catalog-entry.md) proposes that entry and is the first producer of the rows described here. The two records are deliberately separate: this one is the storage and egress mechanism, which outlives any single workload, and that one is a catalog entry, which the repository records one per ADR (ADR-016, ADR-018, ADR-057).

These two questions get conflated, so it is worth separating them plainly.

**Storage is a per-object size problem.** Kubernetes caps an object's size, so a fleet-scale result set does not fit in one object. **Egress is a reachability problem.** The customer runs no Prometheus, so a file is the only way results reach them, and they should not have to walk Workflow to Job to Measurement or decode a ConfigMap to get it.

Neither mechanism solves the other's problem. Writing a bundle to disk does nothing about the object cap, because the rows still have to be stored somewhere before anything can read them. Sharding does nothing about egress, because sharded ConfigMaps are exactly the graph walk the customer asked to be spared.

### What the size limit actually is

`ValidateConfigMap` sums `len(value)` across `Data` and `BinaryData` and rejects the object when the total exceeds `MaxSecretSize`, which is 1 MiB:

```go
for key, value := range cfg.BinaryData {
    ...
    totalSize += len(value)
}
if totalSize > core.MaxSecretSize {
    allErrs = append(allErrs, field.TooLong(field.NewPath(""), cfg, core.MaxSecretSize))
}
```

That is `k8s.io/kubernetes@v1.34.1 pkg/apis/core/validation/validation.go` and `k8s.io/api@v0.36.5 core/v1/types.go:7970`. Two consequences follow, and both are easy to get backwards:

1. **The limit is per object, not per run.** A run may hold far more than 1 MiB of results, as long as no single object does.
2. **It counts decoded bytes.** `BinaryData` is `map[string][]byte`, serialized as base64 on the JSON wire, but validation sums the decoded length. Base64 inflation costs request-body bytes, not budget. A 768 KiB shard is about 1 MiB of request body against a 3 MiB default `MaxRequestBodyBytes`, so it fits with room to spare.

The `--max-request-bytes` figure in ADR-068 is about a different boundary and does not bind here, since etcd stores protobuf rather than the JSON the client sends.

### Why no CR status can hold this

ADR-068 already moved node lists off the Workflow CR into compressed ConfigMaps for exactly this reason. Per-GPU and per-link rows are one to three orders of magnitude larger than node lists: an NVL72 rack's GPU-to-GPU pairs alone are 72 x 71 directed pairs per metric, per rack. Status fields are the wrong place for any of it.

## Decision

**1. Result rows never enter a CR status.** Status carries a sealed count, a total row count, and a label selector value. Rows live in gzipped NDJSON inside ConfigMap `binaryData`, following the ADR-068 precedent.

**2. Shards roll on compressed output bytes.** A counting writer wraps the gzip stream and the writer starts a new shard when the output crosses 768 KiB. There is no row-count bound. A row bound cannot be correct here: validation counts compressed bytes, so any bound derived from a raw row size is wrong by whatever the compression ratio happens to be. An earlier draft of this design carried a 3,674-row bound derived that way, which would have rolled roughly 20x early. The measured ratio spread below is the second reason: it runs from 2.8x to 34x on the same row schema, so a row bound is not merely imprecise, it is wrong by a factor that moves with the corpus.

A single row still has to fit. The roll bound is an aggregate, and a row whose own compressed form exceeds the remaining budget in an empty shard can never be written, so retrying it would hold the Workflow open forever. One row gets 256 KiB of compressed output, a third of the shard bound and three orders of magnitude above the 204 B measured below. A row over that is dropped, counted in `truncation.json` with its component reference, and does not requeue. The row schema ADR sets the raw-field limits that keep producers away from this edge; this is the backstop, not the contract.

**3. Status holds a sealed count and a label selector, not a list of references.** Every shard is labelled; the reader resolves the set by selector and checks it against the sealed count.

**The seal is per Job and per attempt, and the Workflow total is a running sum until the Workflow is terminal.** Shards are produced per Job, so a Workflow-level total alone cannot distinguish "complete so far" from "missing": with 1000 `dcgm-level4` Jobs finishing at different times, a reader polling mid-run sees a smaller number either way. Each Job seals its own shard count and row count on `JobStatus` for the attempt that produced them, before its first shard is created. The Workflow sums the seals of its terminal Jobs and marks the total final only when the Workflow itself reaches a terminal condition. A reader that finds a non-final total knows the run is still producing; a reader that finds a final total can check it. Per-shard index and total annotations are not a substitute: if every shard from one Job fails to write, nothing in the annotation scheme records that the Job owed any rows at all, and only a seal written ahead of the first shard closes that.

**4. `nvcrectl` assembles the bundle.** `--results-file PATH` writes a single document. `--results-bundle DIR` writes `results.json`, `components/*.ndjson`, `truncation.json`, `schema/`, and `manifest.json` last with per-file sha256. The CLI does the shard walk and the gunzip, so the customer never sees a ConfigMap.

**5. No object store and no PVC sink.** See *Alternatives Considered*.

### Shard count tracks Job count, which tracks the catalog entry

This is the part that defeats intuition, and any sizing claim that does not name the entry is meaningless.

Shards are named per Job. Job count comes from the entry's `numNodes`, read by `TrainJobAdapter.NodesRequired` (`pkg/workload/trainjob.go:124-132`, which returns `Trainer.NumNodes`) and used to partition at `pkg/controller/workflow_controller.go:538`. So the same tier at the same node count produces wildly different object counts depending only on which entry ran. At 1000 nodes:

| Entry | Jobs | Shards | Size each |
|---|---|---|---|
| `diagnostics/dcgm-level4` (`numNodes: 1` hardcoded at `dcgm-level4.yaml:71`) | 1000 | 1000 | ~1.6 KiB |
| `communication/nccl-all-reduce`, `nodesPerJob` unset | 1 | 6 | 768 KiB |
| `communication/nccl-all-reduce` at `nodesPerJob: 8` | 125 | 125 | well under the bound |
| NVL72 intra-rack P2P | per domain | 1 per domain, 55 domains | well under the bound |
| `compute/cutlass-gemm` ([ADR-091](091-compute-gemm-catalog-entry.md), `numNodes: 1`) | 1000 | 1000 | well under the bound |

The two NCCL rows and the P2P row are sized by their Job count, which is real, and not by a row count, which is not. Today's parser extracts a message size, an algorithm bandwidth and a bus bandwidth per line (`pkg/nccl/parser.go:77-96`); it produces no per-link rows at all. What a per-link NCCL producer would emit is a question for the row schema ADR, so those three rows claim only that their shards sit under the bound, which follows from the Job count alone. An earlier draft put per-shard byte figures in those cells. They were derived by dividing a raw row estimate by one measured ratio, and the ratio does not transfer between corpora, as the next section shows.

1000 shards or 6, from the same fleet. This is why the sealed count plus selector is the right status shape and a reference list is not: at 69 bytes per reference, 1000 references is 67 KiB of Workflow status, and the Certification controller mirrors category references (`pkg/controller/certification_controller.go:375-380`), so eight categories is 539 KiB on one object. That is over half the status budget to describe data that is not even stored there.

### Measured sizing

Rows carry a component reference, a GPU UUID, an AWS private-DNS node name, an index, a sample number, a metric name, a two-decimal float value, a unit, and an outcome, serialized as compact JSON and compressed at `gzip.BestCompression`, which is what `pkg/controller/compress/gzip.go:18` already uses. Compression ratio is a property of the corpus, not of the row schema, so the corpus is stated for every figure. Values are drawn from a narrow band, which is the realistic case for a healthy fleet and the pessimistic one for the order argument.

| Corpus | Rows | Row size | gzip | Ratio | Order gain |
|---|---|---|---|---|---|
| 1 node, 8 GPU, one sample each | 8 | 204 B | 415 B | 3.9x | 1.00x |
| 1 node, 4 GPU, one sample each | 4 | 204 B | 291 B | 2.8x | 1.00x |
| 1 node, 8 GPU, 25 metrics each | 200 | 200 B | 1.6 KiB | 24.3x | 1.08x |
| 1 node, 8 GPU, 1 Hz for 30 min | 14,400 | 205 B | 86.4 KiB | 33.4x | 1.20x |
| 1 node, 8 GPU, 1 Hz for 2 h | 57,600 | 208 B | 343.4 KiB | 34.0x | 1.27x |
| 1000 nodes, 8 GPU, one sample each | 8,000 | 205 B | 231.5 KiB | 6.9x | 1.15x |
| 1000 nodes, 8 GPU, 1 Hz for 1 min | 480,000 | 205 B | 3,144.8 KiB | 30.6x | 5.32x |

Two things fall out, and only one of them was in an earlier draft of this record.

**Row size is stable and the ratio is not.** A row is 200 to 208 B across every shape above, so 204 B is a figure the design can lean on. The ratio spans 2.8x to 34x on the same schema, so it is not. An earlier draft stated a single 18.4x ratio with no corpus attached and derived shard sizes from it; those derived figures are gone from the table above for that reason. Decision 2 never consumes a ratio, because the counting writer measures the bytes gzip actually produced, and the spread here is the empirical case for that choice rather than a row bound.

**Emission order earns its keep at node cardinality, not everywhere.** Sorting by `(node, componentID, index, name)` is worth 5.32x when a shard holds 1000 nodes and 1.00x when it holds one, because on a single-node shard every row shares one node value and the leading key does no work. Two of the rows in the table above are `numNodes: 1` entries, so "every shard" would be false. **(node, componentID, index, name) is still a normative emission order**, on two grounds that hold regardless of node count: it is worth up to 5.32x on the multi-node shards that are the ones at risk of rolling, and it makes shard bytes deterministic for the same input, which the per-file sha256 in the bundle manifest depends on. The writer sorts a Job's rows before emitting.

The generator for this table is committed with the implementation, under `pkg/controller/shard/testdata/`, so the figures can be re-derived rather than taken on trust.

## Implementation

### What this changes in existing code

**The ConfigMap informer is already unscoped, and this work must fix it.** `CacheOptions()` scopes only `corev1.Node` (`pkg/controller/cache.go:45-54`). `recordNodeResults` writes through the cached client with `controllerutil.CreateOrUpdate` (`pkg/controller/node_results.go:145`), whose Get already runs a cluster-wide ConfigMap informer against a 1Gi controller limit. Adding shards without scoping that informer would make an existing problem considerably worse. The fix is a `ByObject` entry with a label selector.

**Shards are deliberately outside that selector.** The selector matches the node-result ConfigMaps the controller reads back, and shards are not among them: nothing in the controller ever reads a shard, and `nvcrectl` resolves them through its own client. Caching them would put every retained shard's `binaryData` in the controller's heap, growing with retention against the same 1Gi limit the scoping exists to protect. Shards are written with a direct client, and the one read the write path needs, the idempotency check on retry, goes through `mgr.GetAPIReader()` rather than the cache.

**Ordering matters here and is a real hazard.** The existing succeeded-nodes and failed-nodes ConfigMaps carry no labels (`pkg/controller/node_results.go:145-158` sets only the owner reference and `BinaryData`). If the selector lands before those objects are labelled, their cached Gets start returning NotFound. The labels go in first.

**Sorting a Job's rows is bounded by one Job's output, not the run's.** Both the normative emission order and the seal written ahead of the first shard need a Job's rows in hand before the first byte is written. That is per Job, so the bound is the largest single Job's output rather than the fleet's: at `numNodes: 1` it is one node's rows, and at `nodesPerJob` unset it is the whole target, which is the case that needs care. The rows are spooled to a temporary file and sorted there, so the resident set stays proportional to the sort buffer rather than the row count, and the seal comes from the spooled count. This is the one part of the write path whose memory profile does not follow from existing code, and the implementation measures it.

**Shard write failures must not be swallowed.** Every existing `record*Nodes` call site logs and continues:

```go
if err := r.recordFailedNodes(ctx, workflow, job.Status.FailedNodes); err != nil {
    log.Error(err, "Failed to record failed nodes to ConfigMap")
}
```

That is `pkg/controller/workflow_controller.go:1615-1617`, and the same shape appears at :1898, :1901, :2150, :2153, :2209, :2297 and :2324. For node lists that is a tolerable degradation. For the product's primary evidence it is not: a partial shard set would be indistinguishable from a complete one, and the bundle's manifest would then certify it as whole. The sealed count is written **before** the first shard is created, and a shard write failure requeues.

**Retry and iteration must be in the shard name.** The retry branch deletes the Job and resets the group, and `getGroupJobName` keys only on workflow, group and iteration, so attempt 2 reuses the Job name with a new UID. Shard names carry the Job UID and the iteration, and the retry path deletes the losing attempt's shards, so superseded rows cannot appear beside the rows that replaced them.

**`WriteJSON` needs fixing before anything is built on it.** Today:

```go
func WriteJSON(path string, reports []*CertReport) error {
	var v any = reports[0]
	if len(reports) > 1 {
		v = reports
	}
	data, err := json.MarshalIndent(v, "", "  ")
	...
	if err := os.WriteFile(path, data, 0644); err != nil {
```

That is `pkg/report/report.go:2040-2052`. It indexes `reports[0]` with no length guard, and `os.WriteFile` is not atomic. The guard goes in, and every bundle file is written to `<path>.tmp`, fsynced, then renamed. There is no `os.Rename` or `.Sync()` anywhere in the tree today, so this is net-new code rather than a tweak.

A single encoder is used for all sizes. An earlier draft dispatched to a streaming encoder above 4 MiB to bound memory; that is dropped, because `json.MarshalIndent` and `json.Encoder` with `SetIndent` are not byte-identical (the Encoder appends a trailing newline), and measuring the size in order to choose requires marshalling anyway, which defeats the memory argument.

**`--results-file` is silently a no-op without `--wait`.** It is documented as requiring it (`pkg/certification/certification.go:889`), but nothing enforces it, and the run returns at `:1277` before `handleReport` is reached. Combined with `--cleanup`, a user gets exit 0 and no file. Both export flags are rejected without `--wait`.

### Bundle integrity

`manifest.json` is written last and carries `formatVersion`, a per-file sha256, and a file count. Three reader rules are normative: a missing manifest means refuse the bundle, with no directory-glob fallback; a manifest that fails to parse, or whose file count does not match what is on disk, is a hard error rather than a degraded read; and the reader hashes every file the manifest lists and refuses the bundle when a digest does not match. The third rule is what the sha256 is for. A file count and a row count both survive a file being edited in place, so a bundle that passes only those checks is certified as whole on evidence that cannot detect the one thing the digest exists to catch.

The shard reader compares the resolved shard count and the summed row count against the sealed values and refuses to write the manifest on mismatch. It specifically does **not** copy the pattern in `FailedNodesFromRef` (`pkg/report/report.go`), which returns nil on a Get error, on a decode error, and on a nil reference alike, so an unreadable ConfigMap is indistinguishable from nothing-to-report. For the primary evidence path that conflation is not acceptable.

Rows lost to log pagination limits (`ErrLogUnpageable`, `pkg/podlogs/pager.go`) are recorded as a partial read in `truncation.json` and are never collapsed to zero rows. A run that could not read all its evidence says so.

## Rationale

Sharding is the only mechanism that addresses the per-object cap without adding a new storage system, and it has direct precedent in ADR-068. Rolling on compressed bytes is not a refinement of a row bound; it is the only bound that matches what the API server actually measures.

The bundle addresses egress because the customer's constraint is that they have no Prometheus and must read a file. A directory with a manifest is readable by anything, pushable anywhere with the customer's own credentials, and requires no new infrastructure on our side.

The sealed-count-plus-selector shape falls out of the per-entry shard arithmetic. A reference list looks natural until DCGM L4 produces 1000 of them and the Certification mirrors the whole set across categories.

## Consequences

- A `diagnostics/dcgm-level4` run at 1000 nodes creates 1000 shard ConfigMaps in one namespace. That is a deliberate consequence of the entry's `numNodes: 1`, documented here so a reviewer seeing it can tell it from a bug.
- The ConfigMap informer gets scoped, which is an improvement this work pays for rather than a cost it introduces.
- `WriteJSON` gains its first test coverage. It has none today on any path.
- Shard write failures now requeue, so a Workflow can be held open by a persistently failing ConfigMap write where previously it would have completed with silently missing data.
- A CRD change is required: a per-attempt sealed shard count and row count on `JobStatus`, the summed totals and a finality marker plus the selector value on `WorkflowStatus`, mirrored minimally onto `CategoryStatus`.
- A row that cannot be compressed under 256 KiB is dropped rather than retried. No producer is anywhere near that today, and the alternative is a Workflow held open by a row that can never be written.

## Alternatives Considered

**Object store sink (S3 or equivalent).** Rejected for now. The strongest case for it was that `--cleanup` deletes the generated namespace, which no owner-reference scheme survives. That case does not hold: `--cleanup` defaults to false; there is no TTL or retention code anywhere in `api/`, `pkg/` or `cmd/`; and the report is written before the cleanup defer fires, since `handleReport` runs at `pkg/certification/certification.go:1329` inside `finishCertificationWait`, while the defer is registered at `:1137` and runs only after that function returns. Results therefore persist by default until the user deletes the Certification.

A sink would buy unattended egress and long-term durability. Both are real, both are separate questions, and neither has a stated customer requirement yet. Nothing here forecloses it: gzipped NDJSON shards are already the exact unit an uploader would push, and a sealed count with a label selector drives an uploader better than a reference list would.

**PVC sink.** Rejected on the same reasoning, with an additional cost: it would make every results-producing run depend on a `StorageClassName` the cluster may not have.

**A row-count bound per shard.** Rejected as incorrect, not merely suboptimal. Validation counts compressed bytes; a raw-row bound has no relationship to the thing being enforced, and the measured ratio spread of 2.8x to 34x is how wrong it would be.

**Coalescing a Workflow's shards into fewer objects.** Rejected. A `dcgm-level4` run at 1000 nodes produces 1000 small ConfigMaps, which reads like something to tidy up, but the objects are small, `nvcrectl` resolves them in one paginated list by selector, and the coalescing pass would itself be a read-modify-write across the whole set. That opens a window where a crash mid-rewrite leaves a partial set that looks complete, which is the exact failure the sealed count exists to make impossible. Paying that risk to reduce an object count that nothing is straining against is a bad trade.

**`[]corev1.TypedLocalObjectReference` on status.** Rejected on the per-entry arithmetic above.

**Size-dispatched streaming encoder.** Rejected: the two encoders do not produce identical bytes, and choosing between them requires the marshal the dispatch was meant to avoid.

## Amendments

These record where the implementation departs from the decisions above, and why. Each was found by building the thing: the figures are measured, not estimated. The encoder is `pkg/shard`, the controller write path is `pkg/controller/component_*.go`, and the egress half, `nvcrectl --results-bundle` with its reader and the three normative bundle rules, is `pkg/bundle`.

### The per-row cap is 128 KiB of raw input, not 256 KiB of compressed output

Decision 2 gives one row "256 KiB of compressed output, a third of the shard bound". Two problems. The arithmetic does not close: 768 KiB of roll plus a 256 KiB row is exactly the 1 MiB object budget, leaving nothing for the plaintext `data` block or for output gzip has buffered but not yet emitted when the roll is checked. And a row's compressed contribution inside a shared stream is not well defined, because it depends on everything written before it; measuring it by compressing each row alone doubles the work for a backstop that only has to be conservative in one direction.

The cap is therefore 128 KiB of **raw** encoded bytes. gzip's worst-case expansion is about 0.03% plus a fixed header, so a row under that raw bound cannot exceed it compressed by more than a rounding error. Decision 2's objection to row bounds — the measured 2.8x to 34x ratio spread — is an argument about the *aggregate* roll bound, where a raw figure really is meaningless, and does not apply to a per-row backstop.

### Rolling needs a periodic flush, not just a counting writer

Decision 2 says "a counting writer wraps the gzip stream and the writer starts a new shard when the output crosses 768 KiB". A counting writer alone does not work: flate buffers, and 387 KB of rows compressing to 8.5 KB were measured emitting **nothing** through the counter until `Close`. The roll never fires and the whole set lands in one object too large to write.

The writer therefore flushes the stream every 64 KiB of input, which makes the count accurate at that granularity. The cost is real and accepted: the block boundaries cost about 11% of compressed size on a 1,920-row fixture. No corpus that exists is near the bound, so the ratio buys nothing back — a two hour per-GPU soak is 343 KiB against a 768 KiB roll.

This is also what the shard-size arithmetic rests on. The roll fires at 768 KiB on an accurate count; at most 64 KiB of buffered input and one further 128 KiB row can still land; so the worst shard is 960 KiB, leaving 64 KiB for the `data` block. `NewBuilder` refuses option sets whose arithmetic does not close, so the backstop assertion in the writer is unreachable by construction.

### Rows are sorted in memory under a 16 MiB budget; there is no spool

The Implementation section says "the rows are spooled to a temporary file and sorted there, so the resident set stays proportional to the sort buffer rather than the row count". That is not implementable. The manager runs with `readOnlyRootFilesystem: true` and no volumes at all (`helm/cluster-readiness-engine/templates/deployment.yaml`), so there is no writable path, including `/tmp`, and `os.CreateTemp` fails with `EROFS`.

Rows are sorted in memory under a budget instead, and a producer that exceeds it stops and records a `RowBudgetExceeded` truncation rather than growing without bound.

**The budget is 16 MiB, and it is sized against the process rather than the Job.** It counts only the encoded lines, while a Builder also retains its rows with their own copies of every string: measured retention is 2.0x what the budget counts. And the budget is per-Builder while the limit is shared — `MaxConcurrentReconciles` defaults to 10, so ten Builders can be live against one 1Gi limit. An earlier 64 MiB figure measured **2,222 MiB of peak heap, 217% of the limit**: the guard that exists to contain a runaway producer was itself the OOM.

**This has a consequence worth stating plainly, because it is a limit on this record's own sizing table.** The last row of that table, "1000 nodes, 8 GPU, 1 Hz for 1 min" at 480,000 rows, is the corpus whose 5.32x order gain justifies the normative emission order. It does not fit. A single Builder stores roughly 65,000 of those rows and records the rest as truncated. The producer that would emit it — a per-link NCCL producer — does not exist; today's parser extracts a message size and two bandwidths per line and produces no per-link rows at all, as the table's own surrounding text says. If such a producer is written, this budget is the first thing it meets, and either the spool arrives with a writable volume or the Job is partitioned so no single Builder sees the whole fleet.

### The emission order is the normative key plus a byte comparison

The normative order is `(node, componentID, index, name)`, justified on compression and on making shard bytes deterministic. Determinism is the load-bearing half, because the bundle manifest's per-file sha256 and the rewrite-on-retry path both rest on it, and those four fields do not deliver it. A 30 minute soak at 1 Hz produces 1,800 rows sharing all four, and the sort is not stable.

The implemented order appends the sample ordinal, then breaks any remaining tie with a byte comparison of the two rows' encoded lines. Comparing the stored bytes is exact, allocation-free, total by construction, and stays correct if a field is ever added to the row. Two field-by-field attempts at the same thing both leaked: a numeric value comparison ties `-0.0` with `+0.0` while the encoder renders `-0.00` and `0.00`, and formatting the value to compare it allocated two strings across roughly 920,000 comparisons.

### A superseded attempt's seal is retired with its shards

Decision 3 seals per Job and per attempt but does not say what happens to a superseded attempt's seal. Leaving every attempt's seal on the group makes the list grow with `retryFailedGroups`, which has no maximum, and the Workflow's total then falls at an iteration boundary — a reader cannot distinguish a total that fell from one that was never complete.

A superseded attempt's seal is therefore dropped where `retirePreviousAttempts` deletes that attempt's Job and its shards, so at most two are ever held. This is the only bound the existing control flow supports: retirement already enumerates earlier attempts from durable state and already blocks the next launch until every one is confirmed gone, so retries cannot advance past an unretired seal. Bounding `retryFailedGroups` instead would bound the wrong axis and impose a user-visible spec restriction to fix an internal accounting choice.

This makes retirement load bearing on an invariant the type system does not enforce: `retireComponentSeals` runs where `retirePreviousAttempts` already deletes an earlier attempt's Job. If a superseded seal is ever left behind, the bound is `MaxItems=4` and the Workflow stalls on a refused status write rather than silently miscounting.

Corrected while building it, on two points. The shards are addressed in the **Workflow's own namespace**, not `status.namespace`: nothing in the controller writes that field on a Workflow, so reading it sent every delete to namespace `""` and the shards survived.

And the delete is **not** best effort, though it is also not unconditional. The seal is kept when its delete fails, which is the opposite of how `retirePreviousMeasurements` treats a leaked measurement, because the two leaks are not comparable. A leaked measurement is inert. A leaked shard is still resolved: the reader walks namespace and label selector alone, with no Job-name filter, so a shard that outlives its seal is still counted, the resolved count exceeds what the total admits, and the category is refused permanently for a run whose evidence is intact. Holding the seal keeps the total honest about what is still on the cluster and lets the next pass retry. It does not hold up the next launch, which gates on the superseded Job being gone rather than on its seal.

Retention is capped at two, and the cap is the whole reason this is safe. Holding a failed seal indefinitely reintroduces the unbounded growth the paragraph above exists to prevent, by a route that is worse than the original: the ceiling is `MaxItems=4` on a served field, and overrunning it is a 422 on the status write that no retry can clear, so the Workflow stops making progress at all. Weighed against each other, a category whose bundle is refused is a degraded export and a Workflow that cannot write its status is a dead run, so past the cap the seal is dropped anyway and the orphaned shard stem is logged at error level. By that point the delete has failed repeatedly and the shard is far more likely to be genuinely unreachable than transiently so.

### A group's seal carries a truncated-row count, not the per-reason detail

Decision 3 does not distinguish the two places a seal lives, and the distinction turns out to decide whether the status is writable at fleet scale. A Job's seal sits on its own object, one per Job, so eight truncations with explanatory messages cost nothing. A group's seal sits on the Workflow, where a thousand-node fleet puts a thousand of them on **one** object.

Measured on a 1000-group Workflow status, serialised:

| Status | Bytes | Share of a 1 MiB object |
|---|---|---|
| Today, no seals | 240,637 | 22.9% |
| One seal per group | 357,637 | 34.1% |
| Two seals per group, mid-retry | 455,637 | 43.5% |
| Two seals plus eight truncations each | 505,637 | 48.2% |

The last row was **25,929,637 bytes, 2473% of the budget**, while the group seal carried a `Truncations` list. One reason per group across a fleet-wide condition added roughly 300 KB, and about three made the status unwritable — a cluster-wide log problem together with a budget overrun and a seal timeout is three, so this was reachable rather than theoretical.

The group seal therefore carries only `truncatedRowCount`. The reasons and their messages live on each Job's own seal and in the truncation ConfigMap, which has no per-object constraint.

For contrast with this record's own objection to a reference list: the Certification mirror carries the per-category totals and not the seals, and eight categories measure **1,846 bytes**, against the 539 KiB that a mirrored reference list was rejected for.

### The truncation ledger is keyed on the component, not on arrival

Decision 2 requires a dropped row to be "counted in `truncation.json` with its component reference". Keeping the first N records in arrival order does not deliver that. The reason that fires at scale is recorded as rows arrive, so the kept records are contiguous in the producer's emission order and all come from the few components straddling the budget boundary. Measured on the 480,000-row corpus: 416 nodes lost rows and an arrival-ordered ledger named **3** of them, 17 components out of 3,328.

The ledger holds one record per component with a count, bounded at 1,000 components, alongside an exact total. Every affected component is named in the same object budget.

### The ConfigMap informer scoping is already done

The Implementation section calls for scoping a cluster-wide ConfigMap informer with a `ByObject` label selector, and warns that the existing node-result ConfigMaps must be labelled first or their cached reads start returning `NotFound`. Both are obsolete. ADR-092 disabled ConfigMap caching outright — `ClientOptions()` sets `DisableFor: [ConfigMap, PersistentVolumeClaim]` — so there is no informer to scope and no ordering hazard. Labels are still added, for `nvcrectl`'s selector walk rather than the controller's.

### An oversized object is rejected as 422, not 413

`ValidateConfigMap` returns a `field.TooLong` error, which the API server surfaces as `StatusReasonInvalid` — HTTP 422. A handler keyed on `IsRequestEntityTooLargeError` would never fire.

### Nothing owed is recorded by an absent field, not by a phase

Decision 3 seals per Job, and the phase enum includes `None` for a Job that declares no component results. Writing that phase is the obvious reading and it is wrong at this repository's scale: every Job in every certification would take an extra status write, and store a field, to record that a feature none of them uses owes them nothing. It also changed 22 integration goldens to say so.

A Job that declares nothing therefore writes nothing. The absent field is the answer, because a Job that owed rows always has a seal. `None` stays in the enum so that a seal found carrying it reads as terminal rather than as an unfinished write, which is what keeps the group wait from hanging on one; the controller never writes it.

The same rule applies one tier up. The Workflow publishes its totals when there is something to say — any shard, any row, or any row dropped — and leaves the field absent otherwise. Keying that on the run being finished instead, which the first implementation did, put a zero-valued block and a live label selector on every Workflow that reached a terminal condition. A reader cannot tell that block from a run that genuinely finished with an empty result, so it is worse than absent: it invites a bundle for a run that never had a producer.

Truncation is in that test deliberately. A run that dropped every row it produced has no shards and no rows, and suppressing it on counts alone would hide exactly the evidence this mechanism exists to surface.

Counts are not sufficient for that test, though, which the first implementation got wrong. The two reasons that matter most here carry a count of **zero**, because they are the ones a count cannot describe: a log page that could not be fetched gives no way to know how many rows were on it, and a producer that never came up produced nothing to count. Gating on counts alone therefore suppressed exactly the cases the exemption exists for, the category resolved with no `componentResults` block at all, and a run that failed to deliver its evidence went missing from the bundle instead of reporting that it failed. A seal that is `Partial` is now something to say in its own right, whatever its numbers.

### Finality comes from the phase being applied, not from the object

Decision 3's `final` marker is the reader's only way to tell "complete so far" from "missing", and the first implementation could never set it.

The totals are recomputed inside a status-mutation closure, which is correct: it is what makes the value survive a 409 refetch. But every such closure runs **before** `applyExclusiveConditions` writes the new phase, so a closure that derives finality by reading the conditions off the object sees the phase being replaced. On the single write that makes a Workflow terminal it read `InProgress` and returned false. Every run published a non-final total, every `manifest.json` recorded `"final": false`, and the bundle writer warned that complete bundles were incomplete.

Finality is therefore passed down from the condition being applied, in the Workflow's `setExclusiveCondition`, and the helper that read it from status is deleted rather than left available. Deriving it from the phase also keeps a future terminal call site correct by construction, since all three phases go through that one function.

### A loss the controller finds reaches truncation.json, not only the status

"Bundle integrity" requires that rows lost to log pagination are recorded as a partial read. The encoder's own drops satisfied this; the controller's did not. A `LogUnpageable` or `ProducerUnavailable` record was added to the Job's status seal and never to the truncation shard, so `truncation.json` was written as an empty list for a run that had explicitly failed to read its evidence. The shard builder's doc comment already described a caller appending its own records to that set, and no caller did.

Both sets now come from one function, so the status and the bundle cannot disagree, and `shard.Dropped` carries a message because the reason alone does not tell a bundle reader why a read came back short. The branch where nothing could be read at all writes a truncation shard with no row shards beside it, which is the case where this matters most: there is nothing else in the bundle for that category, so without the record it resolves as a run that produced nothing rather than one whose evidence could not be read.

That branch seals `Writing` before the shard write and the terminal phase after, exactly as the main path does, and the ordering is load bearing rather than cosmetic. `Partial` is terminal, and the pass short-circuits on a terminal seal, so sealing it first means the `RequeueAfter` the branch returns on a failed write has nothing left to do when it fires. The first implementation did that, and it discarded the truncation record permanently on the first transient failure to write it: the one artifact that said the evidence could not be read was itself lost to a retry that could not run.

### The group waits for the Job's seal, and waits ahead of the workload delete

Decision 3's finality marker assumes the Workflow's total can be summed once its groups are done. The two controllers are independent, so a group can be finalized on the same status write that made its Job terminal, before the Job tier has produced a row. The group is never revisited, so the total is permanently short of shards that exist and `nvcrectl` refuses the bundle. On the retry path it is worse than short: the attempt's shards are written and labelled, but no seal names them, so retirement cannot delete them and the selector resolves more shards than the total admits.

`completeTerminalGroup` therefore holds the group while a declared seal is non-terminal, bounded by a two-minute grace period measured from the same persisted anchor the pod-drain barrier uses.

**The placement matters more than the wait.** The hold is ahead of `deleteWorkloadForJob`, not after it. That call cascade-deletes the pods whose logs are the only source these rows have, so freeing the GPUs before the Job tier has read them does not make the rows late, it makes them unrecoverable. `captureTimeoutLog` guards the same hazard one tier up for the same reason. A Job that declares no component results is already terminal by this measure and never waits, which is every Job in the repository today.

### Rows come from worker pods through the cache, or from a named replicatedJob live

This record does not say where a producer's output is read from, and ADR-102 makes the choice consequential. The manager's Pod cache holds only pods carrying the job label. Worker pods carry it; MPI launchers do not, and a cached read for a launcher returns no pods **and no error** — the rows would come back empty from a Job that produced them perfectly well.

`Job.spec.componentResults.replicatedJobName` therefore selects the read. Empty means worker pods and the cached read, which is correct and costs no API call for the single-worker-per-node shape ADR-091's GEMM entry uses. Set means `WorkloadPods`, a live read on the JobSet labels every pod in the workload carries. This is the same split the bandwidth controller already makes, because NCCL's output is on the launcher rather than the workers.

### A row is declared by the marker plus a leading brace, and a broken one is counted

The declaration vocabulary is deferred by this record, so the controller needs a minimum: a harness opts a line in by printing `NVCRE_ROW ` followed by a JSON object. Two details are load bearing.

The marker is matched anywhere in the line, because MPI tags every line it relays with a rank prefix and anchoring at column zero would drop every row from a launcher. That means prose can contain the marker too — `could not emit NVCRE_ROW for device 3` is a plausible line from a harness's own error path — so the leading brace is what separates a declared row from a mention of one. Without it, a log message fails the Job's entire result set.

And a line that does open a JSON object and will not parse is **counted, not fatal**. The first implementation returned an error and discarded every row from that pod, which inverts this record's own rule that rows lost to pagination are never collapsed to zero: the decisive case is mundane, since a pod killed mid-write leaves a truncated final line, and that would throw away a thirty-minute soak because its last line was cut short. The rows that parsed are kept, the rejects are counted under `RowUnencodable`, and the first offending line goes into the seal's message so the harness bug is findable.

### A component file name carries the run it came from

Decision 4 gives the bundle `components/*.ndjson` without saying what names those files take. The obvious choice, one file per category, breaks on a command this repository already has: `nvcrectl certification report a b` accepts several certifications and writes one bundle, and two certifications that ran the same category both claim `components/compute-cutlass-gemm.ndjson`. One silently replaces the other and the manifest certifies a bundle missing half its evidence.

The name is therefore `<run>-<domain>-<variant>.ndjson`, and the manifest records the run alongside the domain and variant. A WorkloadRun has no catalog category, so its file is `<run>.ndjson` rather than one carrying two empty segments. `ComponentFileNamesUnique` still checks for a collision before anything is written, because a silent overwrite is the one outcome that must not be possible here.

### The shard namespace is resolved per category, not per Certification

`WorkflowStatus.Namespace` is the resolved namespace where a Workflow's Jobs and their shards live, and the controller auto-generates it when the spec leaves it unset. Two categories of one Certification can therefore sit in two namespaces, and a bundle that listed one namespace for all of them would find no shards for the rest.

That failure would not be silent, which is what makes it dangerous: the walk would report a sealed-count mismatch, which is the correct diagnosis for a genuinely missing shard and a misleading one for a bundle looking in the wrong place. So each category's namespace is read from its own Workflow. A Workflow that cannot be read is an error rather than a fallback to the Certification's namespace, for the same reason.

### The bundle carries a row schema, and the writer verifies its own output

Decision 4 lists `schema/` without saying what is in it. It holds a JSON Schema for one NDJSON line. The bundle is meant to be self-describing: a consumer handed a directory can validate its rows without knowing which NVCRE version wrote them, and an archived bundle stays readable after this repository has moved on.

The writer then runs the three reader rules against what it just wrote, before reporting success. Not belt and braces: the rules are normative, so a writer that cannot produce a bundle its own reader accepts has produced something no consumer should trust, and the alternative is that a digest or file-count disagreement surfaces weeks later when whatever caused it is no longer reproducible.

### A manifest path is checked before it is read

The three reader rules say to hash every file the manifest lists. A bundle travels, so that manifest is attacker-controlled input: an entry of `../../etc/passwd` would send the digest read outside the directory the user named, and the comparison would answer whether that file matches a hash the bundle's author chose.

Rule 2 happens to reject such an entry today, because a traversing path cannot appear in a walk of the bundle and the paths-versus-disk comparison runs first. That is an accident of rule ordering rather than a guarantee, so the path is checked where it is first trusted: absolute paths, volume names, unnormalised paths and any `..` segment are refused by name.

`truncation.json` is also always written, including when nothing was lost, and carries its own `formatVersion`. An absent file and an empty one are different claims, and a consumer asking whether evidence is complete must be able to tell "nothing was lost" from "this bundle predates truncation reporting".

### The sizing generator is not committed

The Measured sizing section says "the generator for this table is committed with the implementation, under `pkg/controller/shard/testdata/`". The package is `pkg/shard`, chosen so the encoder carries no controller dependency and `nvcrectl` can read what the controller writes, and no generator is committed. The figures above that were re-measured during implementation are cited where they are used; the table's own figures are not re-derivable from this repository.

## Notes

The customer's ask that results be readable "without knowing how NVCRE's objects relate" cannot be satisfied as literally one Kubernetes object: one NVL72 rack's P2P pairs alone exceed the per-object cap. It is satisfied instead as one command and one typed document, with `nvcrectl` performing the walk.

## References

- [ADR-068: Offloading Inline Node Lists from the Workflow CR via Compressed ConfigMaps](068-group-nodes-compressed-configmap.md)
- [ADR-072: Freeze GoodputMeasurement Status at Job Terminal State](072-goodput-terminal-freeze.md)
- `k8s.io/kubernetes@v1.34.1 pkg/apis/core/validation/validation.go`, `ValidateConfigMap`
- `k8s.io/api@v0.36.5 core/v1/types.go:7970`, `MaxSecretSize`
