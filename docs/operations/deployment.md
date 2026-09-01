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
| Helm chart (`oci://ghcr.io/nvidia/nvcre`) | GitOps / platform teams | No (install separately) |

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

The Helm chart is pulled from GHCR at the CLI's own version, so a tagged release needs no version flag. **Dev builds (built from `main`) require `--version`** to name the chart version explicitly:

```bash
nvcrectl setup init --version <chart-version>
```

The image and chart are public on GHCR, so no token is needed. For clusters that pull from a private mirror or fork, `--image-pull-secret <github-token>` creates the `nvcrectl-pull-secret` image pull secret in the `nvcre` namespace and authenticates the Helm chart pull. Use `--skip-phases=deps` when Kubeflow Trainer is already installed, and `--auto-approve` to skip the confirmation prompt in CI.

Check the installation at any time:

```bash
nvcrectl setup status
```

### Helm chart

For GitOps workflows, install the chart directly. The chart is public on GHCR, so no registry login is needed. Inspect and install (the snippet resolves the newest stable release without authentication; set `NVCRE_VERSION` to an explicit tag for reproducible installs):

```bash
NVCRE_VERSION=$(curl -fsSL https://api.github.com/repos/NVIDIA/cluster-readiness-engine/releases/latest | jq -r .tag_name)

helm show chart oci://ghcr.io/nvidia/nvcre --version "$NVCRE_VERSION"

helm install nvcre \
  oci://ghcr.io/nvidia/nvcre \
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
| `manager.imagePullSecrets` | `[]` | Pull secrets for the controller image |
| `manager.resources` | `10m/500m` CPU, `1Gi/1Gi` memory | Controller resource requests/limits |
| `manager.affinity` | `{}` | Controller pod affinity |
| `metrics.port` | `8443` | Controller metrics port |
| `metrics.serviceMonitor.enabled` | `true` | Install a `ServiceMonitor` (requires the Prometheus Operator CRDs; set to `false` on clusters without them) |

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

Only one replica holds the leader lease at a time. Standby replicas take over automatically if the leader fails. No additional configuration is required.

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
   helm show crds oci://ghcr.io/nvidia/nvcre --version <new-version> \
     | kubectl apply --server-side --force-conflicts -f -
   ```
3. Upgrade the Helm release (log in to `ghcr.io` first, as above):
   ```bash
   helm upgrade nvcre \
     oci://ghcr.io/nvidia/nvcre \
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
| **Controller node affinity** | Recommended | Schedule the controller on infrastructure nodes, not GPU nodes, using the `manager.affinity` chart value to avoid consuming GPU resources. |
| **Image provenance** | Recommended | Pin container images by digest rather than tag. Scan images with your vulnerability tooling before deployment. |
| **Pod Security Standards** | Verify | The controller runs as non-root with `seccompProfile: RuntimeDefault`, a read-only root filesystem, and all capabilities dropped. Verify with `kubectl get pod -n nvcre -o yaml`. |
| **CRD backup** | Recommended | Include the NVCRE CRDs in your cluster backup strategy. Certification resources contain node health state that may be needed for audit. |

## Next steps

- [Monitoring](./monitoring.md) — Set up Prometheus metrics and alerting.
- [Troubleshooting](./troubleshooting.md) — Diagnose common production issues.
- [Install](../getting-started/install.md) — First-time installation instructions.
