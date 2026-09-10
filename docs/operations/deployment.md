---
title: Deployment
description: Resource sizing, high availability, RBAC, install paths, and cleanup for running the controller in production.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This page covers deploying the NVIDIA Cluster Readiness Engine (NVCRE) controller in production: install paths, resource sizing, high availability, RBAC, network policies, health probes, and the cleanup steps that are easy to miss.

## Installation methods

NVCRE supports two install paths. Both pull the same artifacts from the GitHub Container Registry (GHCR).

| Method | Audience | Installs Kubeflow Trainer |
|--------|----------|---------------------------|
| `nvcrectl setup init` | Operators, quick setup | Yes (skip with `--skip-phases=deps`) |
| Helm chart (`oci://ghcr.io/nvidia/cluster-readiness-engine`) | GitOps / platform teams | No (install separately) |

### nvcrectl setup init

Install the CLI first with the installer script (see [Install](../getting-started/install.md) for the full walkthrough):

```bash
curl -fsSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer | bash
```

Then set up the cluster:

```bash
nvcrectl setup init
```

`setup init` runs two phases:

1. **deps** — Kubeflow Trainer (required for `TrainJob` workloads)
2. **helm** — the NVCRE Helm chart: CRDs, controller Deployment, RBAC, metrics Service/ServiceMonitor, and built-in LogProfiles. The CRDs are server-side-applied from the chart before the Helm release is installed or upgraded, on every run — Helm alone would only install them once and never update them.

By default the Helm chart is pulled from GHCR at the CLI's own version, so a tagged release needs no version flag. **Dev builds (built from `main`) require `--version`** to name the chart version explicitly:

```bash
nvcrectl setup init --version <chart-version>
```

The image and chart are public on GHCR, so no token is needed. For a private fork on GHCR, `--image-pull-secret <github-token>` creates the `nvcrectl-pull-secret` image pull secret in the `nvcre` namespace and authenticates chart pulls from GHCR. For a chart hosted on a private non-GHCR mirror, run `helm registry login <mirror>` before `setup init` instead. Use `--skip-phases=deps` when Kubeflow Trainer is already installed, and `--auto-approve` to skip the confirmation prompt in CI. On clusters that cannot reach GHCR at all, `--chart-ref` and `--trainer-chart-ref` point both chart pulls at a mirror registry; see [Restricted egress and air-gapped installs](#restricted-egress-and-air-gapped-installs).

Check the installation at any time:

```bash
nvcrectl setup status
```

### Helm chart

For GitOps workflows, install the chart directly. The chart is public on GHCR, so no registry login is needed. Inspect and install (the snippet resolves the newest stable release without authentication; set `NVCRE_VERSION` to an explicit tag for reproducible installs):

```bash
NVCRE_VERSION=$(curl -fsSL https://api.github.com/repos/NVIDIA/cluster-readiness-engine/releases/latest | jq -re .tag_name)
: "${NVCRE_VERSION:?no stable release found}"

helm show chart oci://ghcr.io/nvidia/cluster-readiness-engine --version "$NVCRE_VERSION"

helm install nvcre \
  oci://ghcr.io/nvidia/cluster-readiness-engine \
  --version "$NVCRE_VERSION" \
  --namespace nvcre \
  --create-namespace
```

The controller image is `ghcr.io/nvidia/cluster-readiness-engine/manager`, tagged with the same release version. Chart and image versions move together — name the same tag everywhere. The Helm path does not install Kubeflow Trainer; install it separately before running `TrainJob` workloads.

Key chart values:

| Value | Default | Purpose |
|-------|---------|---------|
| `manager.replicas` | `1` | Controller Deployment replicas |
| `manager.image.repository` | `ghcr.io/nvidia/cluster-readiness-engine/manager` | Controller image |
| `manager.image.tag` | `""` (uses chart `appVersion`) | Controller image tag |
| `manager.image.digest` | `""` | Pin the controller image by digest. Wins over `tag`; the only form that names the exact bytes you verified |
| `manager.imagePullSecrets` | `[]` | Pull secrets for the controller image |
| `manager.resources` | `10m/500m` CPU, `1Gi/1Gi` memory | Controller resource requests/limits |
| `manager.affinity` | `{}` | Replace the complete controller affinity; when empty, the chart prefers spreading replicas across nodes |
| `pdb.enabled` | `false` | Create a controller PodDisruptionBudget |
| `pdb.minAvailable` | `1` | Minimum ready controller pods during voluntary eviction; integer or percentage |
| `metrics.port` | `8443` | Controller metrics port |
| `metrics.serviceMonitor.enabled` | `true` | Install a `ServiceMonitor` (requires the Prometheus Operator CRDs; set to `false` on clusters without them) |

### Restricted egress and air-gapped installs

By default the install path reaches GHCR for three artifacts: the NVCRE Helm chart (`oci://ghcr.io/nvidia/cluster-readiness-engine`), the Kubeflow Trainer Helm chart (`oci://ghcr.io/kubeflow/charts/kubeflow-trainer`), and the controller image (`ghcr.io/nvidia/cluster-readiness-engine/manager`). On clusters that cannot reach GHCR, either point `setup init` at a mirror or bypass it entirely.

**Mirror the artifacts.** Copy both charts and the image set to a registry the cluster can reach: the controller image, the images referenced by the Kubeflow Trainer chart, and the workload images used by the certification categories you plan to run. The published Kubeflow Trainer chart package vendors its JobSet chart dependency inside the archive (`charts/jobset/` in the `.tgz`), so mirroring the chart artifact is sufficient; repackaging the chart from source without its vendored dependencies would reintroduce a registry fetch at install time. Then override every reference on `setup init`:

```bash
nvcrectl setup init \
  --chart-ref oci://registry.example.com/mirror/cluster-readiness-engine \
  --trainer-chart-ref oci://registry.example.com/mirror/kubeflow-trainer \
  --image registry.example.com/mirror/manager:<version>
```

`--chart-ref` is used both for the release install and for the CRD extraction (`helm show crds`), so the `helm` phase needs no GHCR access. When the chart ref points at a non-GHCR registry, `setup init` does not attempt a GHCR registry login at all, even with `--image-pull-secret` set, so the install cannot fail on unreachable GHCR. If only one of `--chart-ref` and `--trainer-chart-ref` points at a mirror while the other still resolves to `ghcr.io` (and the GHCR-bound pull is not skipped, e.g. via `--skip-phases=deps`), `setup init` prints a non-fatal warning naming the chart that would still be pulled from GHCR. The chart versions do not change: the mirror must host the NVCRE chart at the CLI version (or `--version`) and the Kubeflow Trainer chart at the pinned version (`2.2.1` for this release). If the mirror requires authentication for the chart pulls, run `helm registry login <mirror>` before `setup init`; Helm then uses its stored credentials for the pulls. `--image-pull-secret` authenticates against `ghcr.io` only; it still creates the `nvcrectl-pull-secret` Kubernetes secret (scoped to `ghcr.io`) regardless of the chart location.

For a controller image mirrored off GHCR, omit `--image-pull-secret`: both its token and the secret it creates are scoped to `ghcr.io`, so they cannot authenticate pulls from the mirror. Authenticate the mirror pull one of two ways instead:

- **Node-level registry credentials.** Configure the mirror credentials in the nodes' container runtime (containerd registry configuration or a kubelet credential provider). Nothing is bound to the controller pod, so `setup init` works exactly as shown above.
- **A pull secret bound through the chart.** Create the secret in the `nvcre` namespace and bind it through the chart's `manager.imagePullSecrets` value. `setup init` has no flag to bind a custom-named pull secret today (the only secret it wires into `manager.imagePullSecrets` is the `ghcr.io`-scoped `nvcrectl-pull-secret`), so this path means installing the chart directly with Helm, from the mirrored chart ref or from the in-repo chart described below:

  ```bash
  kubectl create namespace nvcre
  kubectl create secret docker-registry mirror-pull-secret --namespace nvcre \
    --docker-server registry.example.com \
    --docker-username <user> --docker-password <password>

  helm upgrade --install nvcre oci://registry.example.com/mirror/cluster-readiness-engine \
    --version <version> --namespace nvcre \
    --set manager.image.repository=registry.example.com/mirror/manager \
    --set manager.image.tag=<version> \
    --set 'manager.imagePullSecrets[0].name=mirror-pull-secret'
  ```

  Installing the chart with Helm directly carries the same caveats as bypassing `setup init` below: install Kubeflow Trainer yourself, and re-apply the CRDs on upgrades.

**Bypass `setup init`.** Install the in-repo chart (`helm/cluster-readiness-engine` in the source tree) directly with `helm install`, setting `manager.image.repository` and `manager.image.tag` to your mirrored image (and `manager.imagePullSecrets` when the mirror needs credentials), and install Kubeflow Trainer manually. Nothing is pulled from a chart registry, but you take on installing the Kubeflow Trainer version this release supports and re-applying the CRDs on upgrades yourself.

## Resource requirements

The controller runs as a single Deployment. The shipped defaults are:

```yaml
resources:
  requests:
    cpu: 10m
    memory: 1Gi
  limits:
    cpu: 500m
    memory: 1Gi
```

Size it according to the number of nodes and concurrent workloads in the cluster:

| Cluster size | CPU request / limit | Memory request / limit | Notes |
|--------------|---------------------|------------------------|-------|
| Up to ~100 nodes | 10m / 500m | 1Gi / 1Gi | Shipped chart defaults |
| ~100 – 500 nodes | 200m / 500m | 1Gi / 1Gi | Increase CPU if reconcile latency rises |
| ~500 – 1000 nodes | 500m / 1000m | 1Gi / 2Gi | Raise both CPU and memory for heavier concurrency |

## High availability

Leader election is enabled by default (the manager runs with `--leader-elect`). To run the controller in HA mode, scale the Deployment to two or more replicas:

```yaml
manager:
  replicas: 2
```

Only one replica holds the leader lease at a time. Standby replicas take over automatically if the leader fails, with a temporary reconciliation gap while leadership is acquired. Existing workloads continue independently if their nodes remain healthy.

### Node maintenance and disruption protection

To retain a ready controller replica during voluntary evictions such as `kubectl drain`, enable the optional PodDisruptionBudget (PDB). `nvcrectl setup init` currently has no PDB configuration flags, so manage these values through a direct Helm install or upgrade, or through your GitOps values.

```yaml
manager:
  replicas: 2
pdb:
  enabled: true
  minAvailable: 1
```

By default, the chart gives controller replicas a preferred pod anti-affinity rule for `kubernetes.io/hostname`. The scheduler spreads replicas across nodes when possible but may co-locate them when necessary. A non-empty `manager.affinity` replaces that complete default; it is not merged with the preferred rule.

**Upgrade note:** Earlier chart versions rendered no affinity when `manager.affinity` was empty. Upgrading from those versions with empty `manager.affinity` adds preferred hostname anti-affinity and triggers a controller Deployment rollout, even if `pdb.enabled` is false. With multiple replicas, the scheduler prefers placing them on different nodes, but still permits co-location and scheduling on a single-node cluster.

For a hard HA guarantee, include both infrastructure-node placement and required pod anti-affinity in the override. The example below assumes infrastructure nodes carry the `node-role.kubernetes.io/infra` label; replace that key with the label used by your cluster. Set `app.kubernetes.io/instance` to your Helm release name and `app.kubernetes.io/name` to the chart's rendered name label (`nvcre` by default; adjust it if you change `nameOverride`). Both should match the controller Deployment's selector:

```yaml
manager:
  replicas: 2
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: node-role.kubernetes.io/infra
                operator: Exists
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              app.kubernetes.io/name: nvcre
              app.kubernetes.io/instance: nvcre
              control-plane: manager
          topologyKey: kubernetes.io/hostname
```

The default preferred rule does not guarantee separation, so both replicas may still share a node. During a drain, one pod can be evicted, but the PDB then waits for the Deployment to schedule and ready a replacement elsewhere before allowing the other eviction. If no replacement can become Ready, the drain remains blocked. Co-location also leaves both replicas exposed to an involuntary failure of that node, which a PDB cannot prevent.

This placement requires at least two eligible infrastructure nodes. For rolling upgrades with two replicas, provide a third eligible node with capacity for a controller pod. The chart does not set a Deployment strategy, so the Kubernetes rolling-update defaults allow one surge pod and zero unavailable pods at this replica count. On exactly two nodes, the required anti-affinity also matches the old replicas, leaving the surge pod Pending and the rollout stalled. If only two nodes are available, retain preferred anti-affinity, or customize the Deployment's `spec.strategy.rollingUpdate.maxUnavailable` to `1` through your deployment tooling. The chart does not expose this strategy setting as a Helm value; allowing one unavailable replica also temporarily reduces redundancy during upgrades.

The PDB can still allow eviction of the leader; it preserves a ready replica, not uninterrupted reconciliation or process memory. It does not protect against node failure, direct pod deletion, or Deployment rolling updates.

With **one replica and `minAvailable: 1`, the PDB blocks node drains even when no Certification is running**. Before maintenance, either scale to two and wait for a ready replica on another node, or arrange a maintenance window, temporarily disable the PDB (or set `minAvailable: 0`), and restore protection after the replacement is ready. Completing a Certification does not automatically relax the budget. The PDB is disabled by default to preserve existing maintenance behavior.

More generally, voluntary eviction of a healthy controller pod is blocked while the current ready replica count is at or below the required minimum. For example, two replicas with `minAvailable: 2` or `minAvailable: "100%"` leave no room for voluntary eviction. Percentage minimums are calculated from the desired replica count and rounded up to a whole number of pods.

## RBAC requirements

The controller's ClusterRole (`nvcre-manager-role`) is scoped to the resource types NVCRE actually manages — there are no wildcard rules. Review these before deploying to locked-down clusters:

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `nvcre.nvidia.com/*` (Certifications, Workflows, Jobs, GoodputMeasurements, BandwidthMeasurements, WorkloadRuns) + `/status`, `/finalizers` | full lifecycle | Reconcile the CRD hierarchy |
| `nvcre.nvidia.com` LogProfiles | get, list, watch | Read log-parsing profiles |
| `nodes`, `pods` | get, list, watch | Discover nodes for scheduling and health checks; track workload pod placement |
| `pods/log` | get | Read training logs for goodput and bandwidth measurement |
| `configmaps` | full lifecycle | Workflow dependencies and failed-node result records |
| `persistentvolumeclaims` | create, delete, get, list | Checkpoint storage dependencies |
| `persistentvolumes` | get, list, patch, watch | Checkpoint storage handling |
| `events` | create, patch | Emit Kubernetes events |
| `resource.k8s.io` ResourceClaimTemplates | create, delete, get, list, patch, update | RoCE/DRA network resources |
| `resource.nvidia.com` ComputeDomains | create, delete, get, list, patch, update | Multi-Node NVLink (MNNVL) domains |
| `trainer.kubeflow.org` TrainingRuntimes, TrainJobs | create, delete, get, list, patch, update (TrainJobs also watch; `trainjobs/status` get) | Training workloads via Kubeflow Trainer |

The Workflow dependency system creates supporting resources (ConfigMaps, PVCs, ComputeDomains, TrainingRuntimes) before workloads start, so the role includes create/delete on exactly those types. Inspect the full ClusterRole:

```bash
kubectl get clusterrole nvcre-manager-role -o yaml
```

## Cluster scope and tenancy model

The controller operates as a cluster-scoped infrastructure service, the same model used by the GPU Operator, Node Problem Detector, and other Kubernetes-native controllers. Burn-in certification is an infrastructure concern that reads cluster-scoped Node objects to evaluate GPU health.

NVCRE never modifies nodes — it does not taint, cordon, or patch them. It **records** failed nodes with a reason (`HardwareFailureDetected`, `ThresholdViolation`, or `WorkloadFailed`) in the Certification status, and leaves quarantine and repair to your platform's own tooling.

For teams that need per-team access control, the chart ships `admin`, `editor`, and `viewer` ClusterRoles for the Certification, Workflow, Job, and GoodputMeasurement CRDs. Bind these to team-specific groups or service accounts using standard Kubernetes RoleBindings scoped to each team's namespace.

## Network policies

The chart does not ship a NetworkPolicy. If your cluster enforces network policies, allow ingress to the metrics port (`8443`) from your monitoring namespace:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-metrics-traffic
  namespace: nvcre
spec:
  podSelector:
    matchLabels:
      control-plane: manager
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              metrics: enabled
      ports:
        - port: 8443   # metrics
```

If you want to restrict egress to the Kubernetes API server or gate health probes separately, layer additional NetworkPolicies on top of this example.

## Scaling characteristics

The controller uses controller-runtime's work queue model — each reconciler processes events concurrently. A single replica comfortably handles clusters up to 1,000 nodes and hundreds of concurrent burn-in Jobs. Leader election ensures only one replica reconciles at a time, while standby replicas provide automatic failover.

The controller has no external database, no admission webhook, and no sidecar injection. It depends on Kubeflow Trainer for `TrainJob` workloads, and `nvcrectl setup init` installs Kubeflow Trainer by default unless you skip the `deps` phase. CRD validation is handled through CEL-based validation rules embedded in the CRD schema, which the API server evaluates natively. Operationally, that reduces the stack to the controller Deployment plus the Kubernetes APIs and Trainer CRDs it uses.

### Tuning for large clusters (200+ nodes)

| Parameter | Default | Large cluster | Notes |
|-----------|---------|---------------|-------|
| Controller CPU limit | 500m | 1000m | CEL evaluation scales with node count |
| Controller memory limit | 1Gi | 2Gi | Status objects grow with group count |
| GoodputMeasurement `sampleInterval` | 60s | 120s | Reduces API server log fetch load |
| Certification category concurrency | Unlimited | Partition by node groups | Use multiple Certifications for >500 nodes |

CEL evaluation is lightweight per node, pod-to-node lookups use field indexes, and node watches filter for health-relevant changes only (taints, conditions, schedulability), so unrelated node updates do not trigger reconciliation.

## Air-gapped and disconnected environments

The controller runs without external network access at runtime. All catalog entries are embedded in the binary at compile time, and the controller makes no outbound API calls — it communicates only with the Kubernetes API server. For air-gapped deployment:

1. Mirror the controller image (`ghcr.io/nvidia/cluster-readiness-engine/manager`) and the Kubeflow Trainer images to your internal registry
2. Install the Helm chart with `manager.image.repository` pointing at your internal registry
3. Pre-load any workload images referenced by catalog entries (NeMo, NCCL tests)

No internet access, external telemetry endpoints, or license servers are required at runtime.

### Training categories: Megatron-LM source

The `training/nemotron5-8b` and `training/nemotron5-56b` categories run a `megatron-clone` init container that resolves the entry's source checkout at pod start, in this order: if the workspace already holds the source at `/mnt/workspace/megatron-lm`, it is used unchanged; otherwise, if the workload image ships the source at `/opt/megatron-lm`, it is copied into the workspace; otherwise the init container clones over the network. Both entries define Megatron-LM as their source, with `https://github.com/NVIDIA/Megatron-LM.git` (branch `core_v0.15.2`) as their entry-defined default upstream. The clone step is workload-pod egress, so image mirroring alone does not cover it unless the image itself carries the source. Three ways to run these categories without GitHub access, in order of preference:

1. **Bake the source into the workload image.** `/opt/megatron-lm` is the documented in-image location the entries look for. Extend the training image with the pinned checkout:

   ```dockerfile
   RUN git clone --depth 1 -b core_v0.15.2 https://github.com/NVIDIA/Megatron-LM.git /opt/megatron-lm
   ```

   That exact line keeps the branch pin identical to the one the init container would clone and leaves `.git` present for anything that expects a git checkout. The init container copies the tree into the workspace on every fresh pod start, so the source travels with the image and is covered by the ordinary workload-image pre-loading described at the top of this section; no git egress happens at runtime. The entries have no image knob, so serve the extended image under the same `nvcr.io/nvidia/pytorch:25.08-py3` reference they use: pre-load it on the nodes or publish it through your registry mirror under that name.

2. **Pre-seed the workspace PVC.** Set `enableCheckpoint: true` (plus `storageClassName` if the cluster has no default StorageClass). The category then mounts a PersistentVolumeClaim named `<variant>-pvc` (for example `nemotron5-8b-pvc`) at `/mnt/workspace` instead of a memory-backed `emptyDir`. Pre-populate that volume with the Megatron-LM source at `megatron-lm/` before creating the Certification: the init container uses the workspace unchanged whenever `/mnt/workspace/megatron-lm` exists (a plain source export works; `.git` is not required). Without `enableCheckpoint` the workspace is an `emptyDir`, so this option does not apply and the source is resolved from the image or the network on every pod start.

3. **Point the clone at an internal Git mirror.** For sites that run one, set `sourceRepo` on the Certification, either globally in `spec` or per category under `categories[].options`, to a Git mirror of the entry's source. Each catalog entry defines what its source is and its default upstream; for these two entries the source is Megatron-LM. The branch pin is unchanged, so the mirror must serve the `core_v0.15.2` branch. The URL must use an authenticated remote scheme (`https://` or `ssh://`); `http://` and `git://` URLs (unauthenticated transports the workload would execute code from), scp-style `git@host:path` syntax, and `file://` URLs are rejected by CRD validation (non-TLS mirrors and local source belong in the image or on the pre-seeded PVC above). The clone only runs when neither the workspace nor the image provides the source.

   ```yaml
   spec:
     sourceRepo: https://git.example.com/mirrors/Megatron-LM.git
   ```

The rest of the training path makes no other network calls: the training script builds the local checkout with `pip install -e . --no-deps --no-build-isolation` rather than installing from PyPI, and trains on mock data with a null tokenizer, so no dataset or tokenizer downloads occur.

## Health checks

The controller exposes two probe endpoints on port `8081`, and the chart configures both probes on the Deployment:

| Endpoint | Probe type | Purpose |
|----------|-----------|---------|
| `/healthz` | Liveness | Restart the pod if the process is deadlocked |
| `/readyz` | Readiness | Remove the pod from service until it is ready to reconcile |

Shipped probe configuration:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8081
  initialDelaySeconds: 15
  periodSeconds: 20

readinessProbe:
  httpGet:
    path: /readyz
    port: 8081
  initialDelaySeconds: 5
  periodSeconds: 10
```

## Upgrades

1. Review the release notes for breaking changes.
2. Apply the new chart version's CRDs. Helm never updates CRDs on `helm upgrade` — it applies a chart's `crds/` directory only on the first install — so skipping this step leaves the installed CRDs at the old schema:
   ```bash
   helm show crds oci://ghcr.io/nvidia/cluster-readiness-engine --version <new-version> \
     | kubectl apply --server-side --force-conflicts -f -
   ```
3. Upgrade the Helm release:
   ```bash
   helm upgrade nvcre \
     oci://ghcr.io/nvidia/cluster-readiness-engine \
     --version <new-version> \
     --namespace nvcre
   ```
4. Verify the rollout:
   ```bash
   kubectl rollout status -n nvcre deploy/nvcre-manager
   ```

To roll back:

```bash
helm rollback nvcre <revision> --namespace nvcre
```

If you installed with `nvcrectl setup init`, upgrade by installing the new CLI version and re-running `nvcrectl setup init` — it reconciles the CRDs from the chart on every run before upgrading the Helm release, so no manual CRD step is needed.

## Uninstall and cleanup

### nvcrectl setup reset

```bash
nvcrectl setup reset
```

`setup reset` runs three phases: **cr** (deletes all NVCRE custom resource instances while the controller can still process finalizers), **helm** (removes the NVCRE Helm release and then explicitly deletes the NVCRE CRDs), and **deps** (removes Kubeflow Trainer and its CRDs). Use `--skip-phases=deps` to keep Kubeflow Trainer.

**What `setup reset` retains** — clean these up yourself if you want a pristine cluster:

- The `nvcre` and `kubeflow-system` namespaces are not deleted.
- The `nvcrectl-pull-secret` image pull secret created by `setup init --image-pull-secret` remains in the `nvcre` namespace.

```bash
# Removes both retained namespaces (and the pull secret inside them)
kubectl delete namespace nvcre kubeflow-system
```

### helm uninstall leaves the CRDs behind

Helm intentionally never deletes CRDs that live in a chart's `crds/` directory (to avoid accidental data loss), so a manual `helm uninstall nvcre` leaves all seven `nvcre.nvidia.com` CRDs — and every remaining custom resource instance — in the cluster. Delete them explicitly:

```bash
kubectl delete crd \
  bandwidthmeasurements.nvcre.nvidia.com \
  certifications.nvcre.nvidia.com \
  goodputmeasurements.nvcre.nvidia.com \
  jobs.nvcre.nvidia.com \
  logprofiles.nvcre.nvidia.com \
  workflows.nvcre.nvidia.com \
  workloadruns.nvcre.nvidia.com
```

**Warning:** deleting a CRD deletes all instances of that resource cluster-wide, including any Certification results you have not exported. Save reports first with `nvcrectl certification report <name> --results-file <path>`.

## Production security checklist

Use this checklist before going live. Each item addresses a specific risk surface.

| Item | Status | What to verify |
|------|--------|----------------|
| **Network policy** | Required | Restrict egress to the Kubernetes API server and DNS only. No NetworkPolicy ships with the chart — add one for your environment. |
| **RBAC audit** | Required | Run `kubectl get clusterrole nvcre-manager-role -o yaml` and verify the permissions match your security requirements. |
| **TLS for metrics** | Recommended | The default ServiceMonitor uses `insecureSkipVerify: true`. Configure cert-manager to issue a serving certificate for the controller's metrics endpoint. |
| **Controller node affinity** | Recommended | Schedule the controller on infrastructure nodes, not GPU nodes, using `manager.affinity`. This value replaces the chart's complete default affinity, so include pod anti-affinity in the override when running multiple replicas. |
| **Image provenance** | Recommended | Verify the image signature and its SLSA provenance against the exact signing identity before deploying, then pin what you verified with `--set manager.image.digest=sha256:...` rather than deploying by tag — a tag can be repointed after you check it. The provenance names the commit, ref and workflow that built it. See [Verifying release artifacts](./verifying-artifacts.md). Scan images with your vulnerability tooling before deployment. |
| **Pod Security Standards** | Verify | The controller runs as non-root with `seccompProfile: RuntimeDefault`, a read-only root filesystem, and all capabilities dropped. Verify with `kubectl get pod -n nvcre -o yaml`. |
| **CRD backup** | Recommended | Include the NVCRE CRDs in your cluster backup strategy. Certification resources contain node health state that may be needed for audit. |

## Next steps

- [Monitoring](./monitoring.md) — Set up Prometheus metrics and alerting.
- [Troubleshooting](./troubleshooting.md) — Diagnose common production issues.
- [Install](../getting-started/install.md) — First-time installation instructions.
