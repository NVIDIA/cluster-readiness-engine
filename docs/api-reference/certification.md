---
title: Certification
description: CRD reference for the Certification resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`Certification` is the top-level resource that defines a suite of certification categories to run against a GPU node pool.

## Example

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-cluster-cert
  namespace: nvcre
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  enableMNNVL: false
  gangScheduler:
    schedulerName: kai-scheduler
    queue: high-priority
    # On a Run:ai cluster instead:
    #   schedulerName: runai-scheduler
    #   queueLabelKey: runai/queue
    #   queue: team-a   # must name an existing Run:ai queue
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron5-8b
      options:
        maxSteps: 50
        nodesPerJob: 8
        # Optional: override the DGX-class CPU/memory defaults
        # (limits: cpu "128" / memory 800Gi; requests: cpu "64" / memory 500Gi)
        # so training pods can schedule on smaller GPU nodes.
        resources:
          limits:
            cpu: "6"
            memory: 48Gi
          requests:
            cpu: "4"
            memory: 32Gi
        # Optional: when the category falls back to cloning its source
        # checkout (nothing pre-seeded in the workspace and nothing shipped
        # in the image at /opt/megatron-lm), clone from an internal mirror
        # instead of the default upstream. For the nemotron5 entries the
        # source is Megatron-LM; for air-gapped clusters, prefer baking the
        # source into the workload image so no clone runs at all.
        # sourceRepo: https://git.example.com/mirrors/Megatron-LM.git
```

## Spec fields

_Fields documented so far:_

| Field | Type | Description |
|-------|------|-------------|
| `gangScheduler` | GangSchedulerSpec | Optional. Opts every category's workload pods into a gang-aware scheduler such as KAI Scheduler. When set, the scheduler name is injected as `schedulerName` into every pod template of every category's resolved `TrainingRuntime` dependency (for MPI-based categories, both the launcher and the worker pods) and the queue is applied as a label (`gangScheduler.queueLabelKey`, `kai.scheduler/queue` when unset) on both each replicated job's template metadata and its pod template metadata, so the scheduler holds all pods until the entire gang can be placed. Applied after the catalog and platform overrides resolve, so it also replaces a scheduler name a catalog entry hardcodes. On a Run:ai cluster set `schedulerName: runai-scheduler` and `queueLabelKey: runai/queue`, and make `queue` name an existing Run:ai queue |
| `gangScheduler.schedulerName` | string | Required; minimum length 1. Name of the gang-aware scheduler to use (e.g., `kai-scheduler`, or `runai-scheduler` on a Run:ai cluster). Injected as `schedulerName` in each workload pod spec |
| `gangScheduler.queue` | string | Optional. Scheduler queue to submit the workloads to; defaults to `default-queue` when unset. On a Run:ai cluster it must name an existing Run:ai queue. When non-empty, must be a valid Kubernetes label value: at most 63 characters, beginning and ending with an alphanumeric character, and containing only alphanumerics, hyphens, underscores, or dots (pattern `^$\|^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`) |
| `gangScheduler.queueLabelKey` | string | Optional. Label key the queue value is written under; defaults to `kai.scheduler/queue` when unset. Set it to `runai/queue` on a Run:ai cluster, which reads that label and ignores `kai.scheduler/queue`. When non-empty, must be a valid Kubernetes label key (qualified name): an optional DNS-subdomain prefix of at most 253 characters followed by `/`, then a name of at most 63 characters; at most 317 characters in total |
| `sourceRepo` | string | Optional. Git repository a category clones its source checkout from when the clone actually runs. Categories with a source checkout resolve it at pod start in three steps: an existing `/mnt/workspace/megatron-lm` workspace is used unchanged, then source shipped in the workload image at `/opt/megatron-lm` is copied in, and only otherwise does the init container clone. For air-gapped or restricted-egress clusters the primary recommendation is therefore baking the source into the workload image; `sourceRepo` is for sites that run an internal Git mirror. Each catalog entry defines what its source is and its default upstream; the `training/nemotron5-8b` and `training/nemotron5-56b` entries are the current consumers, and their entry-defined default is Megatron-LM (`https://github.com/NVIDIA/Megatron-LM.git`, branch `core_v0.15.2`). The mirror must serve the entry's pinned branch; entries that clone no source ignore the field. The URL must use an authenticated remote scheme (`https://` or `ssh://`); `http://` and `git://` URLs, scp-style `git@host:path` syntax, `file://` URLs, and URLs containing whitespace or shell metacharacters are rejected (max length 2048, pattern `^(https\|ssh)://[A-Za-z0-9._~:/@%+-]+$`). `http://` and `git://` are rejected deliberately: the cloned source is executed by the workload, and those transports are unauthenticated, so an on-path attacker could substitute the code. `file://` is rejected deliberately as well: the contract is a remote git mirror. Non-TLS mirrors and local source belong in the image or on the pre-seeded checkpoint PVC instead. Settable globally on `spec` or per category under `categories[].options`; the per-category value wins. See [Training categories: Megatron-LM source](../operations/deployment.md#training-categories-megatron-lm-source) |
| `nicResourceName` | string | Optional; also settable per category via `categories[].options.nicResourceName` (per-category wins). Kubernetes extended resource name of the RDMA NIC devices to request on workload containers for on-prem GB200/GB300 targets, for example `rdma/ib` or `nvidia.com/mlnxnics`. Must be a fully qualified extended resource name (domain, slash, and a name segment of at most 63 characters); the reserved `kubernetes.io` and `k8s.io` domains are rejected. When unset, the controller detects the name automatically: if exactly one candidate resource (`rdma/*` or `nvidia.com/mlnxnics`) is allocatable at the resolved `mlnxPerNode` count on every target node, it is requested; otherwise nothing is requested and a Normal `NICResourceDetection` event on the Certification explains what was found: no candidate on any node, candidates below the requested count (naming the count), or multiple qualifying candidates. Set the field to override detection or when detection is ambiguous; requesting a resource the nodes do not advertise at the requested count leaves pods permanently Pending. Offline `nvcrectl certification render` (without `--dry-run`) has no cluster to inspect and requires the field. The per-container count always comes from `mlnxPerNode` (GB200/GB300 default to 8; sites running a shared-device plugin should set `mlnxPerNode: 1`, or detection will refuse a pooled resource advertised as 1) |
| `mlnxPerNode` | int32 | Optional; also settable per category via `categories[].options.mlnxPerNode`. Overrides the auto-detected Mellanox NIC count per node used by InfiniBand/RoCE platforms and as the `nicResourceName` request count. When unset, derived from GPU architecture and platform via the catalog's `gpu-defaults.yaml` |

Like the rest of `spec`, `gangScheduler` is immutable after the Certification is created.

When `gangScheduler` is set, its resolved queue is also placed on each category's generated workload object (the `TrainJob`), under `gangScheduler.queueLabelKey`, in addition to the runtime Job- and pod-template placement described above. See [Workload object labels](#workload-object-labels).

## Workload object labels

`workloadMetadata` sets labels on the workload object each category's Job creates — today a Kubeflow `TrainJob`. That object's own `metadata.labels` is where Kueue selects a local queue and where Kubeflow's KAI Scheduler guide documents the queue label, both read before any workload pod exists.

Because `CertificationSpec` inlines the shared category options, the one field is read at two paths: globally at `spec.workloadMetadata` and per category at `spec.categories[].options.workloadMetadata`.

```yaml
spec:
  workloadMetadata:
    labels:
      environment: burn-in
  categories:
    - domain: communication
      variant: nccl-all-reduce
      options:
        workloadMetadata:
          labels:
            kueue.x-k8s.io/queue-name: nccl-queue
```

That `nccl-all-reduce` category's `TrainJob` receives both `environment: burn-in` and `kueue.x-k8s.io/queue-name: nccl-queue`.

These labels are not pod labels. See [Job workload object labels](job.md#workload-object-labels) for the full contract, reserved keys, limits, and immutability rules.

### Precedence

Unlike the other maps, slices, and pointers in category options — `thresholds`, `resources`, `imagePullSecrets` — workload labels **merge per key** rather than replacing the whole value. The order is:

1. labels already present in the resolved catalog job spec,
2. global `spec.workloadMetadata.labels`,
3. per-category `options.workloadMetadata.labels`, and
4. the resolved `gangScheduler` queue label.

Steps 2 and 3 replace earlier values by key. Step 4 does not overwrite: a missing key is inserted, the same value is accepted, and a different value fails.

The consequence of merging is that a category can **change or add** a key but cannot **remove** a global one — there is no deletion syntax, and an empty string is a real label value. A category needing a wholly different map should not set those keys globally.

### Per-category queue conflicts

`spec.gangScheduler` is global, so every category receives the same resolved queue, including the `default-queue` default. A category that sets that same KAI or Run:ai key to a different value fails, naming the key, both values, and the field path. Per-category Kueue queues remain independent, because `kueue.x-k8s.io/queue-name` is a different key.

<Note>
A Certification that sets `gangScheduler` and no `workloadMetadata` now emits the resolved queue label — the explicit `queue`, or `default-queue` when omitted — on every category's `TrainJob` as well as on the runtime templates. This is an intended behavior change that aligns the submitted object with Kubeflow's documented KAI placement.
</Note>

## Category options

`spec` embeds a set of workload options that apply to every category, and each `spec.categories[]` entry can override them via `options`; the per-category value wins. Options documented so far:

| Field | Type | Description |
|-------|------|-------------|
| `workloadMetadata` | WorkloadMetadata | Optional. Labels applied to the generated workload object (the `TrainJob`). Settable globally on `spec` and per category under `categories[].options`; unlike the other options the two levels **merge per key**, with the per-category value winning. See [Workload object labels](#workload-object-labels) |
| `workloadMetadata.labels` | map[string]string | Optional. At most 32 entries after merging and after any `gangScheduler` queue label is inserted — exceeding the cap fails rather than dropping labels. Keys must be valid Kubernetes label keys and values valid Kubernetes label values; `app.kubernetes.io/managed-by` and any key under `nvcre.nvidia.com/` are rejected |
| `image` | string | Optional. Overrides the workload container image for catalog workloads. Replaces the trainer image in the rendered job template, the primary workload container (`containers[0]`) of every replicated job in the resolved `TrainingRuntime` dependencies, and every init container in those pods whose image exactly equals the primary's pre-override image (the workload-derived inits such as `fix-ssh-permissions` and `megatron-clone`); init containers with a distinct image (such as GCP's `tcpxo-daemon`) and any additional containers keep their catalog images. Applied after the catalog and platform overrides resolve, so it also replaces an image a platform override selects. Settable at the spec level and per category (`categories[].options.image`). **Beware**: on AWS EFA platforms (H100, GB200) the platform overrides land the workers on an `nccl-tests` image that ships the aws-ofi-nccl (EFA) plugin; setting `image` replaces that image, and you then own the EFA OFI plugin being present in the replacement. NVCRE does not restrict which registries or images the field may reference; clusters that require trusted images should enforce that with cluster-wide admission policy (for example Kyverno or the Sigstore policy-controller), which covers this field, `WorkloadRun` `spec.image`, and every other pod alike |

## Spec immutability

<Warning>
The entire `spec` is **immutable** after the Certification is created (a `self == oldSelf` transition rule on the CRD rejects every update with `spec is immutable after creation`). The controller never applies edits to an active run, so mutable fields would either be silently ignored or applied only to later categories. To run with different inputs, delete the Certification and create a new one. The minimum is 1 category.
</Warning>

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | InProgress, Succeeded, Failed (mutually exclusive) |
| `categoryStatuses` | []CertificationCategoryStatus | Per-category status including `domain`, `variant`, `status`, `workflowRef`, `succeededNodesRef`, and `failedNodesRef` |

Each `categoryStatuses` entry includes a `failedNodesRef` — a `TypedLocalObjectReference` pointing to a ConfigMap that stores the failed-node list (name, reason, message) for that category. To read the failed nodes:

```bash
# Get the ConfigMap name from the category status
kubectl get certification <name> -o jsonpath='{.status.categoryStatuses[0].failedNodesRef.name}'
# Read the ConfigMap contents
kubectl get configmap <ref-name> -o yaml
```

## Lifecycle

1. Controller creates one `Workflow` per entry in `spec.categories`. The spec cannot be changed after creation — delete and recreate to modify it.
2. Workflows run **sequentially** — the controller processes one category at a time. `maxConcurrent` controls job/group parallelism *within* a single Workflow (how many node groups run at once), not across categories.
3. When all Workflows complete, Certification is marked `Succeeded` or `Failed`.
4. Failed nodes are recorded in ConfigMaps referenced by `status.categoryStatuses[].failedNodesRef`. NVCRE does not taint or cordon nodes.
