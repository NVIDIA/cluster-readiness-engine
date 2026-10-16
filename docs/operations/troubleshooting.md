---
title: Troubleshooting
description: Diagnose and resolve common issues — stuck jobs, hardware detection failures, and stalls.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This page is for operators who need to diagnose problems with the NVIDIA Cluster Readiness Engine (NVCRE). Each section follows a problem-solution format: symptoms, diagnostic commands, and fixes.

## Read lifecycle Events

`kubectl describe` shows lifecycle Events for Certifications, Workflows, Jobs,
and WorkloadRuns. An `InProgress` or `Succeeded` transition is `Normal`; a
`Failed` transition is `Warning`. The Event reason and message match the
condition that was persisted on the object.

```bash
kubectl describe certifications.nvcre.nvidia.com <name> -n <namespace>
kubectl describe workflows.nvcre.nvidia.com <name> -n <namespace>
kubectl describe jobs.nvcre.nvidia.com <name> -n <namespace>
kubectl describe workloadruns.nvcre.nvidia.com <name> -n <namespace>
```

Job Events also report hardware and validation verdicts. A completed workload
emits `WorkloadCompleted`. If the Job configures thresholds, it subsequently
emits `ThresholdsMet` or the validation failure reason. Without thresholds,
there is no validation verdict Event. A hardware verdict leaves the Job phase
unchanged, but its parent Workflow treats the verdict as terminal for that
group attempt.

Event notes are limited to 1,024 bytes. Longer notes end with `... [truncated]`;
for phase and verdict Events, inspect the object's status conditions for the
full message.

GoodputMeasurement and BandwidthMeasurement do not emit phase-transition
Events. Inspect the owning Job for threshold verdicts; measurement-specific
action failures still appear on the measurement object.

Events are best-effort diagnostics, not the source of truth. Confirm the
current state in `.status.conditions`. Kubernetes retains Events according to
the API server's `--event-ttl` setting, which defaults to one hour, so older
transitions may no longer be present.

## Job not progressing

**Symptoms:** Job stays in `InProgress` for longer than expected. No `Succeeded` or `Failed` condition appears.

**Diagnosis:**

```bash
# Check the Job conditions
kubectl describe jobs.nvcre.nvidia.com <name> -n <namespace>

# Check the underlying workload (use the kind from status.workloadRef)
kubectl get trainjob -n <namespace>
kubectl get pods -l nvcre.nvidia.com/job=<name> -n <namespace> -o wide

# Check controller logs for this job
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep <name>
```

**Solutions:**

- The `InProgress` condition has reason `WorkloadPending` — the workload is queued, not stuck. On Kueue-managed clusters the TrainJob is created with `spec.suspend: true` and held until quota is admitted. NVCRE waits without counting queued time against `timeoutPerJob` or stall detection; check the queue (`kubectl get workloads -A` for Kueue) to see why admission is not happening.
- The workload resource exists but is not completing — check Kubeflow Trainer logs and pod events.
- Pods are `Pending` — verify GPU resources are available on target nodes (`kubectl describe node <node>`).
- The `InProgress` condition has reason `WorkloadSchedulingBlocked` — the workload is admitted but its pods cannot be placed (e.g. GPUs held by another tenant). The condition message relays the scheduler's own diagnosis (also shown on the Workflow as reason `JobSchedulingBlocked` and as a `Blocked:` line in `nvcrectl certification report`); blocked time does not count against `timeoutPerJob` or stall detection. Tune with `spec.schedulingStallGraceSeconds` (default 5m).
- The `InProgress` condition has reason `WorkloadCreationRejected`: the API server is refusing the workload create, usually an admission webhook, a ValidatingAdmissionPolicy or Kyverno/Gatekeeper rule, or an exhausted ResourceQuota. The condition message carries the API server's own error, and the Workflow relays it under `JobSchedulingBlocked` as the `Blocked:` line in `nvcrectl certification report`. Check `kubectl get events -n <namespace> --field-selector reason=WorkloadCreationRejected`. NVCRE keeps retrying, so a rejection that clears (a TrainingRuntime created late, quota freed) recovers on its own. One that lasts past the controller's `--workload-creation-deadline` (Helm `manager.workloadCreationDeadline`, default 10m, independent of `timeoutPerJob`) fails the Job with reason `WorkloadCreationError` and leaves its `status.failedNodes` empty, since nothing ran on them. The deadline is per Job attempt: with `spec.orchestration.execution.retryFailedGroups` set, each retry gets a fresh Job and a fresh clock, so a permanent denial costs (retries + 1) times the deadline before the group gives up. A missing RoleBinding is not a rejection: the controller confirms a `Forbidden` create with a SelfSubjectAccessReview, and an RBAC denial keeps retrying without this deadline.
- Kubeflow Trainer is not running — confirm its pods are healthy (`kubectl get pods -n kubeflow-system`).

## NCCL aborts on an NVLS multicast bind failure

**Symptoms:** A communication category fails early, before any bandwidth number is produced. The launcher log ends with this, repeated once per rank:

```
transport/nvls.cc:284 NCCL WARN Failed to bind NVLink SHARP (NVLS) Multicast memory of size 2097152 :
CUDA error 401 'the operation cannot be performed in the present state'.
This is usually caused by a system or configuration error in the Fabric Manager or NVSwitches.
Disable NVLS (NCCL_NVLS_ENABLE=0) if you wish to avoid this error in the future.
```

Every rank reports it at the same instant, and the Job fails with the generic reason `WorkloadFailed`. The text above is also captured to `Job.status.failureLog`.

**What it means:** NVLS is NVLink SHARP, the NVSwitch's in-network reduction. NCCL binds GPU memory to a multicast object the switch can operate on; this is that bind being refused. Four things about it are easy to misread:

- **The failing call is intra-node.** The multicast team is built over the local ranks only, and on a chassis reporting `MNNVL 0` the handle never leaves the host. A failure here is not a fabric, network, or multi-node problem, even when the job spans nodes. The absence of `via NET/IB` channel lines in the log is a consequence of the abort happening during init, not independent evidence of a network fault.
- **The `NVLS Head` table is not a cross-chassis group.** On a multi-node job NCCL prints `NVLS Head 0: 0 4`, `NVLS Head 1: 1 5`, and so on, which reads as if ranks on different nodes were joined into one multicast team. The row lists one rank *per node*: these are the per-node heads of an NVLS tree, not members of a multicast group. The block is printed only by the first node's ranks, so its absence elsewhere is not an asymmetry.
- **The capability probe passing does not mean multicast works.** `NVLS multicast support is available on dev N` is the driver answering `CU_DEVICE_ATTRIBUTE_MULTICAST_SUPPORTED`. Seeing that line followed by a failed bind is the signature of a host whose switch-side multicast state is wrong, not of hardware that lacks the feature.
- **NCCL 2.29 made this fatal.** Through NCCL 2.28 a failed bind at the default setting logged `Proceeding without NVLS support.` and the job continued. From 2.29 the fallback is gone and the failure aborts the job. The same cluster state that was survivable on an older image fails on a newer one with no change on the NVCRE side. The last line distinguishes the two: `Disable NVLS (NCCL_NVLS_ENABLE=0)` is 2.29 or later, `Do not force-enable NVLS (NCCL_NVLS_ENABLE=1)` is 2.28 or earlier.

**Diagnosis:**

```bash
# Confirm the signature and which NCCL version produced it
kubectl logs <launcher-pod> -n <namespace> | grep -E "nvls\.cc|NCCL version|multicast support"

# The captured tail is on the Job as well
kubectl get jobs.nvcre.nvidia.com <name> -n <namespace> -o jsonpath='{.status.failureLog}'
```

On each affected node, which is where the answer usually is:

```bash
systemctl status nvidia-fabricmanager
nvidia-smi -q | grep -i -A4 fabric
grep -i error /var/log/fabricmanager.log
```

**Solutions:**

- Fabric Manager was restarted without a subsequent GPU reset — multicast stays non-functional while capability detection still reports success. Rebooting the node is the simplest fix. To reset in place instead, follow the order the [Fabric Manager User Guide](https://docs.nvidia.com/hgx-platforms/fabric-manager-user-guide/index.html) gives: drain the GPU workloads, stop Fabric Manager, run `nvidia-smi -r`, start Fabric Manager, then resume the workloads. Note that `nvidia-smi -r` with no `-i` resets every GPU on the host, and on an HGX system that can reset the NVSwitches with them.
- Fabric Manager is not running, or its version does not match the driver — start it and align the versions. On a fourth-generation NVSwitch system (HGX B100, B200 and B300 8-GPU, and DGX B200/B300) Fabric Manager also needs NVLSM, the NVLink Subnet Manager, so confirm that service is installed and running too. Third-generation systems such as H100 and H200 have no such dependency.
- The run must proceed before the host can be fixed — a WorkloadRun can disable NVLS with `NCCL_NVLS_ENABLE=0` in `spec.env`, or `-x NCCL_NVLS_ENABLE=0` in `spec.framework.mpi.mpiArgs`. A Certification has no equivalent field today ([#501](https://github.com/NVIDIA/cluster-readiness-engine/issues/501)). Treat this as a way to keep working, not as a fix: a certification that passes with NVLS disabled has not exercised NVLink SHARP, and the underlying fault is still there.

NVCRE does not set `NCCL_NVLS_ENABLE` for you. NCCL's own default decides from the device attribute whether to use NVLS; a value in the rendered output comes from a catalog fragment for your platform and GPU architecture.

## MPI workers refuse SSH sessions

**Symptoms:** On a release that predates the fix for [#461](https://github.com/NVIDIA/cluster-readiness-engine/issues/461), the `node` pods of a multi-node NCCL category or a WorkloadRun with `framework.mpi` reach `1/1 Running`, the launcher starts, and `mpirun` fails at once with the generic ORTE banner ("ORTE was unable to reliably start one or more daemons"). On a current release the same cluster shows the workers stuck at `0/1` with no launcher pod, and the Job stays `InProgress` until `timeoutPerJob`. In both cases the worker log shows `Server listening on 0.0.0.0 port 22` followed, for every inbound connection, by `fatal: chroot("/run/sshd"): Operation not permitted [preauth]`.

This is the container runtime's default capability set. `sshd` runs every session through privilege separation, which calls `chroot()` before authenticating; that needs `CAP_SYS_CHROOT`. Docker and containerd grant it by default. CRI-O does not, so on OpenShift the listener comes up and every session dies. The MPI worker container now requests `SYS_CHROOT` explicitly, and its readiness probe opens a real loopback SSH session instead of a TCP connect, so a worker in this state stays `NotReady` and the launcher's `dependsOn` gate never releases.

**Diagnosis:**

```bash
# Workers never become Ready: the probe output names the failure
kubectl describe pod <node-pod> -n <namespace> | grep -A3 "Readiness probe failed"

# Confirm the chroot failure in the worker log
kubectl logs <node-pod> -n <namespace> | grep -E "chroot|preauth"

# Confirm the capability was admitted into the pod
kubectl get pod <node-pod> -n <namespace> \
  -o jsonpath='{.spec.containers[0].securityContext.capabilities.add}'
```

**Solutions:**

- The capability list does not include `SYS_CHROOT`: the release predates the fix. Upgrade; the fix is in the catalog NCCL entries, the WorkloadRun MPI builder, and the AWS platform overrides.
- The pod was rejected at admission with a message naming `SYS_CHROOT`: the SecurityContextConstraints granted to the workload's service account (OpenShift), or the namespace's Pod Security level, does not allow it. Grant an SCC whose `allowedCapabilities` lists both `IPC_LOCK` and `SYS_CHROOT`. Under Pod Security admission only the `privileged` level admits these workers: `baseline` allows `SYS_CHROOT` but not the `IPC_LOCK` they already needed, and `restricted` (like the stock `restricted-v2` SCC) allows neither.
- The workers are `Ready` but `mpirun` still prints the ORTE banner on a current release: the worker image ships `sshd` without an `ssh` client, so the probe fell back to the listening check and could not see the refused sessions. Prebake `openssh-client` alongside `openssh-server`; the recipe in the [air-gapped FAQ](faq.md#can-mpi-workloads-run-in-air-gapped-or-restricted-egress-clusters) does both, and on Debian and Ubuntu the server package pulls the client in on its own.

## Workflow stuck without a Job

**Symptoms:** Workflow condition is `InProgress`, but no child Job appears.

**Diagnosis:**

```bash
# Check Workflow status and conditions
kubectl get workflows.nvcre.nvidia.com <name> -o yaml

# Look for the child Job
kubectl get jobs.nvcre.nvidia.com -l nvcre.nvidia.com/workflow=<name>

# Check if dependencies were created
kubectl get workflows.nvcre.nvidia.com <name> \
  -o jsonpath='{.status.dependencyRefs}' | jq .
```

**Solutions:**

- The InProgress reason is `NamespacePermissionsMissing` or `NamespaceAccessCheckFailed` — the controller has no write access in the namespace yet. See [Workflow waits with NamespacePermissionsMissing](#workflow-waits-with-namespacepermissionsmissing).
- The Workflow failed with `DependencyCreationError` and a `forbidden` message — an admission policy, webhook or ResourceQuota rejected the dependency. The controller checked that its own RBAC allows the write before failing, so the rejection's message names the policy that blocked it.
- Target nodes do not match — verify the Workflow's `nodeSelector` or `nodeNames` matches existing nodes (`kubectl get nodes -l <selector>`).
- The Workflow spec is invalid — look for validation errors in the controller logs.

## Workflow waits with NamespacePermissionsMissing

**Symptoms:** a Workflow, or the Certification category above it, stays `InProgress` with reason `NamespacePermissionsMissing` or `NamespaceAccessCheckFailed`. Nothing is created in the namespace. `kubectl describe` shows a `NamespaceAccessDenied` or `NamespaceAccessReviewError` Warning, and `certification run --wait` shows the same message on the category's `Blocked:` line. After `manager.namespaceAccessTimeout` (default 10 minutes) the Workflow fails with the same reason.

**Cause:** the chart is installed with `rbac.scope: namespaced`, and the namespace has no RoleBinding granting `nvcre-run-role` to the controller ([ADR-092](../designs/092-per-namespace-controller-rbac.md)). Before it writes anything, the controller checks its own access with SelfSubjectAccessReviews. A denied review means the binding is missing (`NamespacePermissionsMissing`). A review that could not be created at all means the check could not run (`NamespaceAccessCheckFailed`); an unanswered check is never treated as permission.

**Diagnosis:**

```bash
# The message names the ServiceAccount and every permission it lacks
kubectl get workflows.nvcre.nvidia.com <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="NamespaceAccess")]}' | jq .

# Is there a binding, and does it name the right ServiceAccount?
kubectl get rolebinding nvcre-manager-run -n <namespace> -o yaml

# Ask the API server what the controller may do there
kubectl auth can-i create configmaps -n <namespace> \
  --as=system:serviceaccount:nvcre:nvcre-manager

# Which scope is the chart installed with?
helm get values nvcre -n nvcre -a | grep -A2 '^rbac:'
```

**Solutions:**

- Apply the binding. The Workflow resumes on its own within a minute, and its `NamespaceAccess` condition turns `True`:

  ```bash
  nvcrectl rbac print --namespace <namespace> | kubectl apply -f -
  ```

- The binding exists but names another ServiceAccount: the release was installed under another name or namespace. Compare the subject with `kubectl get clusterrolebinding nvcre-manager-rolebinding -o yaml` and print the binding with `--service-account` and `--controller-namespace`.
- `NamespaceAccessCheckFailed` for any other reason: the reviews themselves fail, usually because the API server or an admission webhook is struggling. The Workflow keeps rechecking, backing off to once every two minutes, and resumes on its own once reviews succeed again.
- `NamespaceAccessCheckFailed` with `selfsubjectaccessreviews ... is forbidden`: Kubernetes normally grants these reviews to authenticated identities, including ServiceAccounts, through `system:basic-user`. This error means that default grant was removed or narrowed and the controller has no other grant. Check the `system:basic-user` role and binding, and `nvcre-manager-role`. Upgrading the NVCRE release adds an explicit `create` grant on `selfsubjectaccessreviews` to `nvcre-manager-role`; an older chart alone does not cause this error.
- For a long-lived namespace, list it in `rbac.runNamespaces` so the chart keeps the binding.
- The Workflow already failed after the timeout: apply the binding and run again. A failed Workflow does not retry.

## Workflow cleanup or deletion pauses on terminating pods

**Symptoms:** After a Job fails, times out, or the Workflow is deleted, the group stays `Running` (or the Workflow stays terminating) for a while, and dependency resources such as the ComputeDomain are not removed immediately.

This is expected: the controllers wait for the workload's pods to finish terminating before deleting the dependency resources that provide their DRA allocations. Deleting a ComputeDomain while pods are still terminating would revoke NVLink/NVSwitch channel allocations under running CUDA kernels and kill every pod process with CUDA error 719. The wait is bounded to 5 minutes per Job; after that, cleanup proceeds anyway and the controller logs a warning.

**Diagnosis:**

```bash
# See which pods the controller is waiting on
kubectl get pods -l nvcre.nvidia.com/job=<name> -n <namespace>

# Confirm the wait in the controller logs
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep -E "still terminating|drain"
```

**Solutions:**

- Pods are terminating normally — no action needed; cleanup resumes as soon as they are gone.
- A pod is stuck `Terminating` (for example, its node is unreachable) — cleanup proceeds automatically after the 5-minute grace period. To unblock sooner, force-delete the pod: `kubectl delete pod <pod> --grace-period=0 --force`. **Only do this when the node is confirmed unreachable or the pod's processes are known to be dead**: force deletion removes the Pod object without waiting for its processes to stop, so the controller may then delete the ComputeDomain while CUDA kernels are still running on the node — the exact CUDA error 719 failure the drain wait exists to prevent.

## Failed with a name-collision reason

**Symptoms:** A Certification fails with reason `WorkflowNameCollision`, or a Workflow fails with reason `JobNameCollision` or `DependencyNameCollision`.

**What it means:** The name NVCRE generated for a child resource (a Workflow, a Job, or a dependency such as a PVC, ConfigMap, or TrainingRuntime) is already taken by an object NVCRE did not create. NVCRE never adopts such an object and never deletes it during cleanup — it fails the run instead, and the pre-existing object is left untouched.

**Diagnosis:**

```bash
# The condition message names the colliding object
kubectl get certifications.nvcre.nvidia.com <name> -n <namespace> -o yaml
kubectl get workflows.nvcre.nvidia.com <name> -n <namespace> -o yaml

# Inspect the colliding object's owners and labels
kubectl get <kind> <colliding-name> -n <namespace> -o yaml
```

**Solutions:**

- The colliding object belongs to someone else — run the certification in a dedicated namespace, or rename your Certification/Workflow so the generated child names no longer collide.
- The colliding object is a leftover from an earlier run that you no longer need — delete it manually, then re-create the Certification or Workflow.

If the colliding object is already being deleted (it has a `deletionTimestamp`), NVCRE does not fail: it retries with backoff until the name is released. This covers deleting and re-creating a same-named Certification or Workflow while the previous run's children are still terminating.

## Hardware failures not detected

**Symptoms:** A node has a known hardware problem, but the Job does not report the `HardwareFailed` condition.

**Diagnosis:**

```bash
# Verify pods are running on the expected nodes
kubectl get pods -l nvcre.nvidia.com/job=<name> -o wide

# Check that the pod label was injected
kubectl get pods -l nvcre.nvidia.com/job=<name> --show-labels

# Inspect the node for the expected conditions/taints
kubectl get node <node> -o yaml

# Check controller logs for CEL evaluation errors
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep "CEL"
```

**Solutions:**

- The CEL expression has a syntax error — look for parse errors in controller logs. Test your expression against a node object.
- Pods are not scheduled on target nodes — the controller only evaluates nodes where Job pods are running. Verify pod placement.
- The pod label is missing — the controller injects `nvcre.nvidia.com/job` automatically. If pods were created before the controller started, they may lack the label.
- Node conditions or taints do not exist yet — NVCRE only reads node state; it relies on your cluster's health monitoring stack to set the conditions or taints your CEL expression checks. Verify with `kubectl describe node`.

## High reconciliation latency

**Symptoms:** Status updates are slow. The `nvcre_reconcile_duration_seconds` P95 is above 5 seconds.

**Diagnosis:**

```bash
# Check controller resource usage
kubectl top pod -n nvcre

# Check for API server throttling (429 responses)
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep "throttling"
```

**Solutions:**

- Controller is resource-constrained — increase CPU and memory limits. See the [Deployment](./deployment.md) page for sizing guidance.
- Too many nodes per health check — large clusters increase CEL evaluation time. Consider splitting workloads across fewer nodes.
- API server is under load — check API server metrics and reduce concurrent reconciles if needed.

## Certification not progressing

**Symptoms:** Certification stays `InProgress` after Workflows have finished.

**Diagnosis:**

```bash
# Check category statuses
kubectl get certifications.nvcre.nvidia.com <name> \
  -o jsonpath='{.status.categoryStatuses}' | jq .

# List child Workflows
kubectl get workflows.nvcre.nvidia.com -l nvcre.nvidia.com/certification=<name>

# Check for CategoryNotFound errors
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep "CategoryNotFound"
```

**Solutions:**

- A category is not registered in the catalog — the controller sets the Certification to `Failed` with reason `CategoryNotFound`. Verify your domain and variant values against `nvcrectl certification list-categories`.
- A child Workflow is still running — the Certification waits for all Workflows to complete before transitioning.

## Stall detection not triggering

**Symptoms:** A Job appears stuck but is not marked `Failed` with reason `WorkloadStalled`.

**Diagnosis:**

```bash
# Verify stallMultiplier is set on the Job
kubectl get jobs.nvcre.nvidia.com <name> -o jsonpath='{.spec.stallMultiplier}'

# Check if a GoodputMeasurement exists and has step data
kubectl get goodputmeasurement -l nvcre.nvidia.com/job=<name> -o yaml

# Look for avgStepTimeSec and lastStepTimestamp
kubectl get goodputmeasurement -l nvcre.nvidia.com/job=<name> \
  -o jsonpath='{.items[0].status.avgStepTimeSec}'
```

**Solutions:**

- `stallMultiplier` is not set — stall detection is opt-in. Add `stallMultiplier` to the Job spec (e.g., `10` means stalled if no step for 10x average step time).
- No GoodputMeasurement exists — stall detection requires a GoodputMeasurement to provide `avgStepTimeSec` and `lastStepTimestamp`. Configure `goodputMeasurement` on the Job.
- Not enough training steps — the controller needs at least two non-warmup steps to compute `avgStepTimeSec`. Wait for the workload to produce more log output.

## BandwidthMeasurement not reporting results

**Symptoms:** A BandwidthMeasurement exists but `status.results` is empty.

**Diagnosis:**

```bash
# Check the BandwidthMeasurement status
kubectl get bandwidthmeasurement <name> -o yaml

# Verify the LogProfile has a bandwidthResult pattern
kubectl get logprofile <profile-name> -o jsonpath='{.spec.patterns.bandwidthResult}'

# Check pod logs for NCCL output
kubectl logs <pod-name> | head -50
```

**Solutions:**

- The LogProfile is missing the `bandwidthResult` pattern — BandwidthMeasurement requires a LogProfile with a `bandwidthResult` regex. Add it to the LogProfile spec.
- The regex does not match the NCCL output format — verify the regex against actual log output. NCCL test output format varies between versions.
- The workload has not produced output yet — bandwidth results appear only after the NCCL test completes its message-size sweep.
- The `replicatedJobName` is wrong — for MPI workloads, set `workerStrategy.replicatedJobName: launcher` in the LogProfile since NCCL output goes to the launcher pod.
- The `logProfileRef` does not name an existing LogProfile — check `kubectl get logprofile <name>`; LogProfiles are cluster-scoped.
- The LogProfile's `containerName` names no container in the launcher pod — the log read fails with an error from the API server. Leave it empty to read the pod's only container.

## Bandwidth threshold fails with MeasurementTimeout

**Symptoms:** A Job succeeded, but `ValidationFailed` is `True` with reason `MeasurementTimeout` for `busBandwidthGBps` or `algBandwidthGBps`, and its BandwidthMeasurement is `Complete` with reason `LogsUnavailable` or `NoDataCollected`.

Bandwidth thresholds are evaluated only against final results, read from the launcher's full log after the Job succeeds. The measurement could not produce them, so the value stays unmeasured and validation fails once `measurementTimeout` expires.

**Diagnosis:**

```bash
# The Complete condition's message names the cause
kubectl get bandwidthmeasurement <name> -o jsonpath='{.status.conditions[?(@.type=="Complete")]}'

# Retries of the final read are recorded as Warning events
kubectl get events --field-selector involvedObject.name=<name>
```

**Solutions:**

- `LogsUnavailable` with an error reading the pod log — the launcher pod or its log was gone or unreachable for the whole retry period, or the LogProfile's `containerName` names no container in the pod. Check that nothing deletes the TrainJob or its pods before thresholds are evaluated, and that the controller can reach the kubelet for `pods/log`. If the log is only slow to become readable, raise `measurementTimeout`; the read is retried for as long as the Job waits (see [BandwidthMeasurement](../api-reference/bandwidth-measurement.md#how-it-works) for where to set it).
- `LogsUnavailable` with an error naming the LogProfile (`getting LogProfile ...`, a pattern that does not compile) or a missing pod (`no pods found for ... replicatedJob ...`) — the measurement never found the log to read. Check `logProfileRef` and `workerStrategy.replicatedJobName` as described in [BandwidthMeasurement not reporting results](#bandwidthmeasurement-not-reporting-results).
- `LogsUnavailable` because the log cannot be read in full — for example a line longer than 1 MiB. Retrying cannot help; reduce the workload's log output.
- `LogsUnavailable` because the referenced Job no longer exists — it was deleted, or replaced when its group was retried. The replacement Job gets its own BandwidthMeasurement.
- `NoDataCollected` — the whole log was read but no line matched the `bandwidthResult` pattern. See [BandwidthMeasurement not reporting results](#bandwidthmeasurement-not-reporting-results).

## Enable debug logging

For deeper investigation, enable debug-level logging by adding the flag to the manager container args:

```yaml
# In the Deployment spec
args:
  - --zap-log-level=1
```

This surfaces detailed reconciliation traces and CEL evaluation results.

## Next steps

- [Monitoring](./monitoring.md) — Set up alerts so you catch issues before they need manual diagnosis.
- [Deployment](./deployment.md) — Review resource sizing and RBAC.
- [Health Monitoring & Failed Node Attribution](../concepts/health-monitoring-remediation.md) — Understand how CEL-based hardware detection works.
- [Certify a Cluster](../how-to-guides/certify-a-cluster.md) — End-to-end certification walkthrough.
