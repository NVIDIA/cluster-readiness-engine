# ADR-102: Cache Only NVCRE's Own Pods

> **Status:** Proposed

## Context

GitHub issue #504 reports `nvcre-manager` v0.7.0 OOMKilled on a cluster with 2,678 nodes and about 80,000 pods. With the limit raised to 8Gi, the manager peaked at 3.83 GiB during its initial cache sync and stayed at 3.1–3.8 GiB while idle, with no certification running. Most of that is the Pod cache:

| Kind | How the cache holds it | Count | JSON size |
|---|---|---|---|
| Pods, all namespaces | unscoped | 80,664 | 779 MB |
| Nodes with `nvidia.com/gpu.present` | label-scoped | 2,500 | 96 MB |
| ResourceSlices, PersistentVolumes | unscoped | ~2,500 | ~2 MB |

The Pod informer runs whether or not NVCRE is doing anything. `RegisterFieldIndexes` registers two Pod indexes (`pkg/controller/indexes.go:42`, `:53`), and controller-runtime's `IndexField` starts the indexed type's informer when the manager starts (`pkg/cache/informer_cache.go:202-207` in v0.24.1). So the manager lists and watches every Pod in the cluster from startup, on a cluster with no GPU node and no NVCRE workload as much as on a busy one. Most of those pods are DaemonSet pods, so the cost grows with nodes times DaemonSets, not with NVCRE's work. Measured on kind with never-scheduled pods cloned from real system pods (2.9 KB of JSON each), the manager's working set was 133 MiB with 10,000 of them and 222 MiB with 20,000, about 10–11 KiB per pod, while a build with the Pod cache limited to the job label stayed at 23 MiB with 20,000.

`CacheOptions()` leaves Pods unscoped on purpose (`pkg/controller/cache.go:38-44`). The obvious selector, `nvcre.nvidia.com/job`, is injected only into the worker replicatedJob's pod template, so MPI launcher pods do not carry it, and the comment names the launcher as the pod NCCL bandwidth measurement and timeout log capture read from. That decision lives only in the comment. Two things have changed since: bandwidth measurement now reads pods through the uncached `APIReader` (`pkg/controller/bandwidthmeasurement_controller.go:112-117`), and timeout log capture selects on the job label (`pkg/controller/workflow_controller.go:3644`), so it never sees a launcher either way.

Every cached Pod read in the controller today:

| Read | Selects on | Needs unlabelled pods |
|---|---|---|
| `activeWorkloadPods` (`pod_drain.go`), `captureFailureLog`, `checkSchedulingBlocked` (`job_controller.go`), `discoverNodes` (`pkg/nodemonitor/nodes.go`) | job index, or job label as fallback | No |
| `captureTimeoutLog` (`workflow_controller.go`) | job label | Wants the launcher; cannot get it today |
| `nodeToJobRequests` (`job_controller.go:2244`) | `spec.nodeName` index, then drops pods without the job label in Go | No |
| Goodput worker discovery (`goodputmeasurement_controller.go:243`, `:492`, `:922`) | `jobset-name` | Only for an MPI WorkloadRun, whose rank-0 output is on the launcher |
| TCPXO installer detection (`tcpxo_plugin_detect.go`, called at `certification_controller.go:568`) | namespace `kube-system` | Yes; retried live through `APIReader` when the cached read finds nothing |

The hot reads, on every running-path Job reconcile (ADR-083) and on Node health events, already select on the job label. Only goodput and the TCPXO check reach outside it.

## Decision

1. **Cache only Pods that carry `nvcre.nvidia.com/job`.** `CacheOptions()` gets a `ByObject` entry for `corev1.Pod` with a label selector requiring the key to exist. The selector is applied by the API server to the informer's list and watch, so pods without the label never reach the controller.

2. **Controller code reads Pods only through `pkg/podread`, which picks the source.** Callers name what they want, not a selector, and each method decides between the cache and the API server:

   | Method | Reads | Source |
   |---|---|---|
   | `JobPods(namespace, job)` | a Job's workload pods | cache, job field index (job label as a fallback) |
   | `ActiveJobPods(namespace, job)` | a Job's workload pods that have not finished | cache; the API server whenever no active pod remains in the cache |
   | `JobPodsOnNode(node)` | NVCRE workload pods bound to a node | cache, `spec.nodeName` index and the job label |
   | `WorkloadPods(namespace, workload, replicatedJob)` | a workload's pods, launchers included | API server, JobSet labels |
   | `NamespacePods(namespace)` | every pod in a namespace | API server |

   A read whose pods all come from NVCRE's worker template goes to the cache, under a selector the method builds itself, so a cached read cannot be unscoped. A read that may need pods without the job label, such as MPI launchers or another component's pods, goes to the API server. A new kind of read is a new method, which is where its source is decided and reviewed.

   `podread` separates no pods from a failed read. No pods is an empty slice and a nil error. A failed read is an error naming the method, logged at error level and counted, so it is visible even when a caller only requeues. `ActiveJobPods` serves a caller that acts on "none active": the drain barrier, which lets teardown proceed when no pods remain. The cache can hold a Job's finished pods while missing a newer one that is still running, so `ActiveJobPods` checks the API server whenever filtering leaves no active pod, not only when the cached list is empty.

3. **The cache refuses Pod reads it cannot answer completely.** `NewCache` wraps the manager's cache, which the manager's client also reads through. A table lists each kind the cache holds under a selector and the condition a read of it must meet; `CacheOptions()` builds its entries from the same table. For Pods, a `List` must require the job label, with `=`, `==`, `in` or exists, or use the job field index. Anything else is refused: no selector, a namespace alone, other labels alone, another index alone, or a negated requirement on the label, which also matches pods without it. A Pod `Get` and a Pod informer are always refused, so a future `Watches(&corev1.Pod{})` never receives a partial stream: controller-runtime retries the informer every 10 seconds, each refusal is logged and counted, and the controller fails once the cache-sync timeout passes. The check is by GroupVersionKind, so metadata-only reads of Pods are covered, and so are unstructured ones that reach the cache through `mgr.GetCache()`; the manager's client sends unstructured reads to the API server. The guard models label scope only, so the Pod cache must not also be narrowed by namespace or field, here or through the cache-wide defaults; a test pins that, and adding such a scope means changing the guard first. A refusal is an error, logged at error level with the calling code's location and counted. This is the backstop for reads that bypass `podread`.

   Nodes are already cached under `nvidia.com/gpu.present=true`, and their reads predate this record. They are listed in the table as exempt: their reads are not checked. Adding them is a table entry and a change to their three reads.

4. **A policy test keeps Pod reads in `podread`.** Every Pod list needs a `corev1.PodList`. The test fails when any non-test Go file in a package the manager binary links, other than `pkg/podread`, names that type.

## Implementation

### `pkg/podread`

- New package. It owns the job label (`nvcre.nvidia.com/job`), the job field index (`metadata.labels.nvcre.nvidia.com/job`) and the node-name index (`spec.nodeName`) as constants; `pkg/nodemonitor` and `pkg/controller` refer to them instead of defining their own.
- `podread.New(cached, live client.Reader)`. Reconcilers build it from their client and `APIReader`; `APIReader` is defaulted from the manager in `SetupWithManager`, as the Job, Workflow and Certification reconcilers already do, and Goodput and Bandwidth now do too. A nil `APIReader` is not replaced by the cache: every live read then fails with an explicit error, logged and counted, since a live read the cache answered would be incomplete without any error (and `ActiveJobPods`' check would become a second cached read). A unit test that needs live reads sets `APIReader` to its fake client.
- `JobPods` falls back from the job index to the job label when the index read fails, which the scheduling check and node discovery already do, for fake clients without the index.
- Every read is counted in `nvcre_pod_reads_total{read, source, result}`, with `result` one of `found`, `empty` or `error`.

### Controller cache

- `pkg/controller/cache.go`: a `guardedScopes` table (Pods) and an `exemptScopes` table (Nodes, with the reason). `CacheOptions()` builds its Pod selector from the Pod entry. No `ByObject` entry sets a `Transform`, so `DefaultTransform` still strips `managedFields`.
- `NewCache`, a `cache.NewCacheFunc`, wraps `cache.New`. Its `List`, `Get`, `GetInformer` and `GetInformerForKind` resolve the object's GroupVersionKind and apply decision 3. `IndexField` is not wrapped: controller-runtime's informer cache starts the indexed informer through its own `GetInformer` (`pkg/cache/informer_cache.go:203` in v0.24.1), so the Pod field indexes still register.
- Refusals are counted in `nvcre_unscoped_cache_reads_total{kind, op}`.
- `cmd/manager/main.go` and `cmd/integration/integration_test.go` pass `NewCache: controller.NewCache`. The manager's client reads through the wrapped cache (`pkg/cluster/cluster.go:211-212`); `mgr.GetAPIReader()` is a separate client and is not wrapped.

### Call sites

Every Pod read moves to `podread`; none changes the pods it returns except where noted.

- `JobPods`: `checkSchedulingBlocked`, `captureFailureLog` (`job_controller.go`), `captureTimeoutLog` (`workflow_controller.go`), node discovery (`pkg/nodemonitor/nodes.go`, whose `NewNodeDiscoverer` takes a `*podread.Reader`).
- `ActiveJobPods`: the drain barrier (`pod_drain.go`).
- `JobPodsOnNode`: `nodeToJobRequests`.
- `WorkloadPods`: `pkg/podutil.WorkerDiscoverer`, used by goodput worker discovery, which moves from the cache to the API server, and bandwidth, which already read live.
- `NamespacePods`: TCPXO installer detection, which reads `kube-system` and now reads it live only. Its cached first read and its live confirmation go, since the cached read could only come back empty. A failed live List is retried once after a second: detection runs once per Workflow and its images are fixed when the Workflow is created, so a transient API error would otherwise pin the fallback profile for the whole run. A List that fails twice falls back with the Warning event, as before. The Warning names the API error without `podread`'s wrapper. The CLI's dry-run path calls it with its own uncached client.

### Documentation

- `docs/operations/deployment.md`: Resource requirements and the large-cluster tuning row describe memory as following NVCRE's own workload pods and the cached GPU Nodes, not the cluster's Pod count. Keep the #504 figures as the reason.
- `CHANGELOG.md`: an `[Unreleased]` entry.

### Testing plan

- `pkg/podread`: for each method, the source that answers, using a cached and a live fake client holding different pods; `ActiveJobPods` reading the API server whenever the cache leaves no active pod, including when the cache holds only finished pods while a newer one runs, and not when an active pod is cached; the index-to-label fallback; errors wrapped with the method name and counted.
- `pkg/controller`: TCPXO detection golden cases for a List that fails on both tries, pinning the whole Warning text, and one that fails once and then finds the release. The drain barrier's golden cases gain `liveOnlyPods`, pods the API server has and the cache does not yet, and a case where the cache holds only a finished pod while a newer one runs; the barrier must wait. The cache guard over a recording inner cache. Scoped Pod lists pass; unscoped lists, a Pod `Get`, a Pod informer, and metadata-only and unstructured Pod lists are refused; Node and other kinds pass untouched. A test that every `ByObject` entry with a selector is in `guardedScopes` or `exemptScopes`, so a kind scoped later cannot skip the table.
- `test/readpolicy`: the policy test of decision 4, with `go list -deps ./cmd/manager` deciding which packages are in the manager.
- `cmd/integration/testdata/reconcile/`: the harness runs the real `CacheOptions()` and `NewCache`, and after every case, once its manager has stopped and its objects are deleted, asserts that no cache read was refused, so a refused read fails its case even when the calling code swallows the error, including reads made while the case winds down.
  - The bandwidth launcher pods (`bandwidth-measurement*`, `bandwidth-final-read-*`) drop `nvcre.nvidia.com/job`, as real launchers have no such label. They must pass unchanged, since bandwidth reads live.
  - A new goodput case, `goodput-measurement-unlabelled-pod`, whose workload's only pod has no job label, like an MPI launcher. Goodput must find it through the API server, and the golden is `goodput-measurement`'s, unchanged.
  - The TCPXO cases (`certification-gcp-h100-*tcpxo*`) keep their unlabelled `kube-system` installer pods and must still detect the release.
  - `workflow-unpinned-deleted-job-attributed` and `workflow-unpinned-retry-reattributes` remove the job label from pods mid-test, which with a scoped cache makes the informer drop them. Their goldens are expected to hold, since every read that sees those pods already selects on the label; any change is diagnosed before a golden is touched.
- On kind, the manager's working set from a build of `main` and a build of this change, with many pods that carry no NVCRE label, and no refused read in the manager's log.

## Rationale

- **The hot reads already live inside the selector.** Scoping the cache changes what is cached, not what any current read returns, except the reads that move to the API server. The paths that run most often, Job reconciles and Node events, keep their cached, indexed reads.
- **The decision belongs where the read is named, not where it is checked.** A guard can only reject a read after it is written. `podread` makes the source part of the method a caller picks, and a reviewer sees a new read as a new method with its source next to it.
- **A refusal has to be loud on its own.** Several callers treat any read error as "not found yet": they log at `V(1)` and requeue or skip. A refused read reaching such a caller would behave like an empty one. The guard logs and counts each refusal itself, and the harness fails a case that causes one.
- **Guarding the cache covers what guarding the client does not.** Reads through `mgr.GetCache()`, metadata-only reads, and Pod watches all reach the cache without passing through the client.
- **Reads that need unlabelled pods are rare and cheap live.** Goodput reads one namespaced, label-selected pod list per sample interval per measurement, as bandwidth already does. The TCPXO check runs only on GCP H100 while a Certification resolves its categories, and it already fell back to a live read whenever the cached one found nothing.

## Consequences

- The manager's memory follows the cached GPU Nodes and NVCRE's job-labelled pods, from runs in progress and from finished runs: a terminal Job keeps its workload and pods for log inspection until its Certification is deleted (`reconcileWorkload`), so its pods stay cached. The startup list no longer carries every Pod in the cluster.
- A few more API calls: goodput's live pod list per sample interval, the TCPXO check's live list, and the drain barrier's live check whenever its cached read leaves no active pod.
- A Pod read outside `podread` fails the policy test. A read that bypasses `podread` and reaches the cache unscoped is refused, logged and counted, and fails the integration case that runs it.
- Read failures that callers used to log at `V(1)` are now also logged at error level by `podread`.
- Unit tests that build a fake client directly do not go through the cache guard, though their reads still go through `podread`'s scoped selectors.
- The guard checks a read's scope, not its intent. A cached read on `jobset-name` together with the job label is allowed, and still cannot return an MPI launcher; `podread` keeps such reads on the API server.
- `podread` methods own the cache-or-live decision; reviewers check it there, since a scoped read with the wrong intent returns empty without an error.
- Node reads stay unchecked until Nodes move from `exemptScopes` to `guardedScopes`.
- Sizing guidance in the docs changes from the cluster's Pod count to the GPU nodes and NVCRE's job-labelled pods, including those of the Certifications an operator keeps.

## Alternatives Considered

### Read Pods live, as ADR-092 did for ConfigMaps

`Cache.DisableFor` removed the ConfigMap informer because its cluster-wide `list` and `watch` conflicted with per-namespace access, exposed every ConfigMap, and served only a handful of reads per Workflow. None of that holds for Pods. Pod reads stay cluster-wide in `nvcre-manager-role`, and they are the controller's hottest reads: every running-path Job reconcile and every Node health event. Reading them live would trade memory for steady API server load in large runs.

### Label the MPI launchers too

Injecting the job label into the launcher's pod template would make launchers cacheable. It would also put them in every read that selects on the label: node discovery, the drain barrier, failure log capture. The launcher holds no GPU and no ComputeDomain claim, and some of those paths would then treat its node as part of the run. That is a change to node attribution, not to caching.

### Strip `status.images` from cached Nodes

Nothing in NVCRE reads it, and it is a large part of a GPU node's object. But the node health monitor's CEL expression is evaluated against the whole Node (`pkg/nodemonitor/cel/detector.go`), so dropping a field would change what user expressions see without an error. Pods are 89% of the cached JSON on the #504 cluster; Nodes can wait for a measurement.

### Guard the manager's client instead of its cache

A wrapper from `NewClient` sees reads through the client only. Reads through `mgr.GetCache()`, metadata-only Pod lists, which the client serves from the cache, and Pod watches all bypass it, and its errors reach callers that may swallow them.

### A source scan that decides whether each read is cached or live

Both readers are a `client.Reader`, so a scan cannot tell them apart without a hand-kept list of call sites. The policy test of decision 4 asks a simpler question, whether a Pod list is written outside `podread`, which needs no such list.

### Raise the default memory limit

It moves the threshold without changing the slope: the manager's memory would still follow the cluster's Pod count. ADR-098 already lets an operator raise the limit and keeps it across runs.

## Notes

`captureTimeoutLog` prefers the MPI launcher but reads only job-labelled pods, so it never finds one. Moving it to `WorkloadPods` would fix that, and would change the failure log of a timed-out MPI run; it is left for its own change.

`nvcre.nvidia.com/job` is injected by `TrainJobAdapter.InjectPodLabel`. A new workload adapter that does not inject it would leave its pods outside the cache, and outside node monitoring, drain and failure log capture. That is already true today.

The CEL node health detector keeps an unstructured copy of every Node it evaluates and never evicts it. That is a second, separate memory cost in a large run, not addressed here.

## Amendment: a watch-only cache for MPI launchers (#529)

PR #529 (issue #498) reads a failed MPI launcher's log while the workload still reports running, because JobSet deletes the launcher pod about a second after it fails. That needs a watch on launcher pods, which decision 3 refuses on the manager's cache, and the launchers are not in that cache anyway.

1. **Launchers get a label of their own.** The Job controller sets `nvcre.nvidia.com/launcher: <job name>` on the launcher's pod template, through the controller-owned `launcher` runtimePatch entry that `SetNodeAffinity` and `SetTolerations` patch (`workload.InjectLauncherPodLabel`). It does so only when `HasLauncherTarget` is true, since Trainer's webhook rejects a patch for a replicated job the runtime lacks. `nvcre.nvidia.com/job` stays on the workers only, so node discovery, the drain barrier and failure attribution do not change (see "Label the MPI launchers too" above).
2. **A second cache holds only those pods and backs only the watch.** `controller.NewLauncherPodCache` builds a cache whose only scope is Pods with the launcher label, adds it to the manager's cache group, and `cmd/manager` passes it to the Job controller (`SetupWithManager` builds it when unset). The controller watches it with `WatchesRawSource(source.Kind(...))`, and the map function takes the Job name from the label. This cache is not wrapped by `NewCache`: a watch on it sees every pod it is meant to see, which is what decision 3 protects. Nothing reads through it.
3. **The launcher is still read live.** `launcherPods` reads through `podread.WorkloadPods`, and `test/readpolicy` still holds.

Memory follows the launchers of runs in progress and of kept Certifications, at most one or two pods per MPI Job. Launchers of workloads created before this change carry no label and are not watched; their failures are still found live by `captureFailureLog` once the workload reports the failure, if the pod remains.

## Amendment: the timeout log reads the launcher (#546)

`captureTimeoutLog` now adds the workload's launchers from `launcherPods` to the job-labelled pods and prefers a running one, which settles the first paragraph of Notes. A timed-out MPI run records the launcher's log, with the NCCL output, instead of the first running worker's sshd output. The launchers are read live, as for `captureFailureLog`, so `test/readpolicy` still holds. A workload without a launcher still records its first running node pod, and a failed launcher lookup falls back to that too, since the capture is best-effort.

## References

- Issue [#504](https://github.com/NVIDIA/cluster-readiness-engine/issues/504)
- [ADR-083](083-scheduling-stall-visibility.md): one cached pod list per running-path Job reconcile
- [ADR-090](090-component-result-sharding.md): label-scoping an informer with `ByObject`
- [ADR-092](092-per-namespace-controller-rbac.md): `Cache.DisableFor` for ConfigMaps and PersistentVolumeClaims
- [ADR-098](098-setup-init-chart-values.md): `setup init` chart values, including the manager's memory limit
- `pkg/controller/cache.go`: `CacheOptions()`, `ClientOptions()`, `NewCache`
- `pkg/podread`: the Pod reads
- `pkg/controller/indexes.go:42`, `:53`: the Pod field indexes that start the informer
- controller-runtime v0.24.1 `pkg/cache/informer_cache.go:202-207` (`IndexField` starts the informer), `pkg/cache/cache.go:497`, `:517-520` (`ByObject.Label` applied to list and watch), `pkg/cluster/cluster.go:189` (`NewCache`), `:211-212` (the client reads through that cache), `:221` (the API reader is a separate client)
