# ADR-090: Sharded Component Results and the Report Bundle

> **Status:** Proposed

## Context

A customer runs custom workloads on NVCRE and reports that the workloads themselves work well, but that they cannot get detailed enough results out of them. Their asks, in their words, are to report results per GPU and per link rather than only per node or per job; to read those results without knowing how NVCRE's objects relate or when they are cleaned up; to make it explicit when a result is final, when a test failed, and when a result is missing; and to declare in the spec what results a workload produces instead of regex over a fixed set of fields.

That feature is larger than one record. This ADR settles only the two questions that every other part of it depends on: **where per-component result rows are stored**, and **how they leave the cluster**. A later ADR covers the declaration vocabulary, the ingestion controller, and the verdict rules.

The case this design was proved against is a per-GPU GEMM burn-in: one achieved-TFLOPS number per GPU is the simplest result that cannot be expressed per node, since a node average passes while one thermally throttled GPU drags it. [ADR-091](091-compute-gemm-catalog-entry.md) proposes that entry and is the first producer of the rows described here. The two records are deliberately separate: this one is the storage and egress mechanism, which outlives any single workload, and that one is a catalog entry, which the repository records one per ADR (ADR-016, ADR-018, ADR-057).

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

## Notes

The customer's ask that results be readable "without knowing how NVCRE's objects relate" cannot be satisfied as literally one Kubernetes object: one NVL72 rack's P2P pairs alone exceed the per-object cap. It is satisfied instead as one command and one typed document, with `nvcrectl` performing the walk.

## References

- [ADR-068: Offloading Inline Node Lists from the Workflow CR via Compressed ConfigMaps](068-group-nodes-compressed-configmap.md)
- [ADR-072: Freeze GoodputMeasurement Status at Job Terminal State](072-goodput-terminal-freeze.md)
- `k8s.io/kubernetes@v1.34.1 pkg/apis/core/validation/validation.go`, `ValidateConfigMap`
- `k8s.io/api@v0.36.5 core/v1/types.go:7970`, `MaxSecretSize`
