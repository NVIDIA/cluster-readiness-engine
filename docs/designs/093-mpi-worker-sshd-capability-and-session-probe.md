# ADR-093: MPI Workers Request `SYS_CHROOT`, and Readiness Means a Working SSH Session

> **Status:** Proposed

## Context

A customer running the multi-node NCCL categories on OpenShift reported (GitHub issue #461) that every `node` pod reached `1/1 Running`, the launcher started, and `mpirun` failed immediately with the generic ORTE banner. The worker logs told the real story: `Server listening on 0.0.0.0 port 22`, then for every inbound connection `fatal: chroot("/run/sshd"): Operation not permitted [preauth]`.

Two defects produce that outcome, and the second is what turned the first from a crash into a silent failure.

**1. The MPI worker never asks for the capability `sshd` needs.** OpenSSH has run every session through privilege separation since 7.5 (2017), and that path is not configurable: `privsep_preauth_child()` calls `chroot()` on the compiled-in `PRIVSEP_PATH` before authenticating, and `fatal()`s on `EPERM`. That `chroot()` needs `CAP_SYS_CHROOT`. Docker and containerd include it in their default capability set, which is why the worker has worked everywhere NVCRE has been run until now. CRI-O removed it from `default_capabilities`, so on OpenShift the listener comes up and every session dies. The worker requests `IPC_LOCK` explicitly and nothing else.

The defect lives in two code paths that share no code. `BuildMPIRuntime` in `pkg/platform/runtime.go` renders the worker for WorkloadRun; the three communication catalog entries (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`) declare their own worker container in YAML for Certification. Both carry the same `add: [IPC_LOCK]` list.

A fix to the Go builder alone also does not reach two platforms. The AWS GB200 and AWS H100 blocks in `pkg/platform/overrides/workloadrun.yaml` restate the worker's `securityContext.capabilities.add`. The dependency merge in `pkg/controller/workflow_detect.go` (`mergeValue`, `mergeNamedSlices`, `indexByName`) unions slices only when every element is a map with a `name` key; a capability list is `[]string`, so the override replaces the builder's list wholesale. Both blocks would silently drop the new capability on exactly the two AWS architectures the overrides exist to support. The comment on the GB200 block said the block covered the torch path "without duplicating it for MPI", which is how the restatement went unnoticed.

**2. The readiness probe reports a worker that refuses every session as Ready.** The worker's probe is `tcpSocket: 22`. `sshd` binds the port before it can serve anything and keeps the listener up however badly individual sessions fail, so a TCP connect answers "is something listening" and nothing more. The launcher's `dependsOn: [{name: node, status: Ready}]` gate consumes that verdict (`pkg/platform/runtime.go`), so the probe decides when `mpirun` may dial. With the probe satisfied, the gate released `mpirun` against workers that would kill every `orted` session, and the only visible symptom was the ORTE banner on the launcher.

A banner-level check would not have caught it either. OpenSSH writes its version string in `kex_exchange_identification()`, which the connection child runs before `privsep_preauth()` forks and chroots (`sshd.c`; `sshd-session.c` from 9.8). A worker failing exactly this way still greets a prober. The chroot is on the path to authentication, so authentication is the earliest point that can observe it.

Under KAI the repository already knows this. KAI cannot gate the launcher on the workers, so the `isKAIGangScheduler` branch of `BuildMPIRuntime` drops the gate and `launcherWaitScript()` in `pkg/platform/gang_scheduler.go` holds the launcher back by opening a real `ssh ... true` session to every worker. Outside KAI the same question was answered by a TCP connect.

## Decision

1. **Every container that runs `sshd` requests `SYS_CHROOT`**, alongside the `IPC_LOCK` it already had. That is the Go builder's worker, the worker in each of the three communication catalog entries, and both AWS override blocks. The override additions are gated on `FrameworkType == "mpi"`, because the same blocks render for torch, where no `sshd` exists and the capability would be an unexamined broadening across every torch pod on AWS.

2. **The worker's readiness probe opens a real loopback SSH session.** The `tcpSocket` probe becomes an `exec` probe running `ssh -n -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 127.0.0.1 true`, under `timeout 8` when coreutils `timeout` is present, with `timeoutSeconds: 10` and `periodSeconds: 30`. It dials this worker's own `sshd` only; probing a peer would couple worker readiness together and deadlock on whichever starts first. `failureThreshold` keeps its default.

3. **An image missing a helper keeps the old semantics.** The probe script checks `command -v ssh` first and, if absent, reads `/proc/net/tcp` for a listening socket on port 22, which is the same question the old probe asked, answered with nothing but `grep`. It checks `command -v timeout` the same way and runs `ssh` bare when there is none, so neither helper's absence turns a healthy worker into a failing one.

4. **The invariant is tested as a projection golden, not a verbatim one.** `TestSSHDWorkerInvariants` in `pkg/platform/sshd_worker_invariants_test.go` drives `testutil.TestCaseParser` over five cases (builder MPI, builder torch, platform overrides MPI, platform overrides torch, the whole catalog). The catalog case walks every place a worker container can be declared or patched: the base `dependencies`, the base `jobTemplate` (which carries `workload.trainJob.runtimePatches`), and each override's `dependencies`, `jobTemplate`, and `jobTemplatePatch`. Each case records, per container that bears on the invariant, the replicated job it sits under, whether it execs `sshd`, its capability list, and a summary of its probe, and computes a `violations` list from the same facts. A non-empty list fails the case before the golden is compared, so regenerating the goldens cannot record a broken value.

5. **No CRD change.** Nothing here is configurable. An image that cannot chroot cannot run multi-node MPI on any runtime, so there is nothing for a field to express.

## Implementation

- `pkg/platform/runtime.go`: `BuildMPIRuntime`'s worker gains `SYS_CHROOT` and uses `sshdReadinessProbe()`, which emits the `exec` probe built from the `sshdProbeCommand` constant. The constant's comment carries the reasoning so the shape is not relaxed by a later edit that reads only the code.
- `pkg/platform/overrides/workloadrun.yaml`: the AWS GB200 and AWS H100 blocks add `SYS_CHROOT` under `{{- if eq .FrameworkType "mpi" }}`. The GB200 comment says the list is a full restatement that must stay in sync with the builder; the H100 comment points at it.
- `pkg/catalog/entries/communication/nccl-all-reduce.yaml`, `nccl-all-gather.yaml`, `nccl-alltoall.yaml`: the worker gains `SYS_CHROOT` and the same `exec` probe, byte-identical across the three.
- `pkg/catalog/entries/_lib/deps/aws-gb300-roce-runtime-patch-comm.yaml` is not touched. It replaces the worker `args` but declares no `securityContext` or probe, so it merges and inherits both.
- `pkg/platform/sshd_worker_invariants_test.go` and `pkg/platform/testdata/sshd-worker-invariants/`: the test and its five cases.
- `docs/operations/troubleshooting.md`: a new section for the symptom on either side of the fix, with the admission guidance. `docs/operations/faq.md`: the prebake recipe names `openssh-client`.

### Testing plan

- Reverting any single fix site must fail `TestSSHDWorkerInvariants`, and the goldens that record the rendered value must change with it. The builder's capability, probe mechanism, or timeout each fail the builder case; the override blocks fail the overrides case; the catalog entries fail the catalog case.
- The catalog case must walk `spec.overrides` and the `jobTemplate` patch documents as well as `spec.dependencies`, because `Entry.Build` returns override fragments unapplied and the controller merges them at reconcile, and because `runtimePatches` reach the worker container by another route. A fragment or patch that restates the worker's list without `SYS_CHROOT`, or restates its probe with any handler other than `exec`, must be a violation.
- No torch rendering may carry `SYS_CHROOT` or run `sshd`; the torch cases guard that with the same predicates and a non-zero container count.
- Every `/usr/sbin/sshd -De` container in `cmd/integration/testdata` and `test/uat/testdata` must carry both capabilities and the session probe; no torch golden may change.

## Rationale

- **The capability is `sshd`'s, not the workload's.** `SYS_CHROOT` is one of the thirteen capabilities the Pod Security `baseline` profile allows by name. It is requested because a program the worker runs cannot function without it, in the same way `IPC_LOCK` is requested for RDMA pinning. Not requesting it means depending on which container runtime happens to grant it by default, and CRI-O has shown that assumption is false.
- **Readiness must mean what the gate thinks it means.** The `dependsOn` gate is the only thing standing between `mpirun` and a worker that is not ready. A probe that cannot distinguish "listening" from "usable" promotes every `sshd` misconfiguration, not only this one, into a silent failure with a misleading symptom. A real session is the cheapest check that cannot be fooled by the listener.
- **Loopback, not a peer.** The launcher-side wait under KAI already proves that an `ssh ... true` session is the right check. Running it on the worker against itself keeps the probe's verdict local, so a slow peer cannot make a healthy worker NotReady.
- **`timeout 8` inside `timeoutSeconds: 10`, when available.** The inner timeout fires first, so a hung session is reported with `ssh`'s own stderr rather than the kubelet's generic "command timed out", and the probe stays bounded on a kubelet whose `ExecProbeTimeout` gate is off. It is guarded like the `ssh` client is: a probe that failed because a helper was missing would turn a healthy worker into a permanently NotReady one, which is the one outcome worse than the defect.
- **`periodSeconds: 30`.** Readiness keeps probing for the pod's life, nothing consumes the verdict after the gate has fired, and `sshd -e` logs every accepted session into the worker's log. Thirty seconds keeps that to two sessions a minute. The cost is up to 30s of added gate latency on an image that installs `openssh-server` at start; a prebaked image passes the first probe at 5s.
- **Default `failureThreshold`.** Readiness starts out failed and flips on the first success, so `failureThreshold` never delays a worker becoming Ready. It only governs how many consecutive failures an already-Ready worker tolerates before it is marked NotReady again, and nothing consumes that after the gate has fired: JobSet defaults `publishNotReadyAddresses` to `true`, so DNS does not depend on readiness. A non-default value would be a knob with no consumer.
- **The fallback exists for images NVCRE does not control.** Every image NVCRE renders today ships both `sshd` and `ssh`: on Debian and Ubuntu `openssh-server` depends on `openssh-client`, and the AWS `nccl-tests` image installs both by name. A user-supplied WorkloadRun image need not. A probe that fails because the prober is missing would report a healthy worker as broken forever, which is a worse regression than the one being fixed. The `/proc/net/tcp` check is strictly what such a worker had before.
- **Framework-gating the override additions is cheaper than the alternative.** The AWS blocks render for both frameworks. Adding `SYS_CHROOT` unconditionally would churn nine torch goldens and grant a capability to pods that have no use for it; the template already branches on `.FrameworkType` throughout.
- **A projection golden, because the verbatim goldens recorded the bug.** The existing goldens pinned `add: [IPC_LOCK]` and `tcpSocket: 22` faithfully. A golden cannot know that the recorded value is wrong. Recording the facts the invariant depends on, and failing on a violation before the compare, is what lets the golden document the expected shape without being able to ratify a regression.

## Consequences

- **Every MPI worker pod now requests one more capability.** On a cluster whose admission policy allows `IPC_LOCK` but not `SYS_CHROOT`, the pod is rejected at admission with a message naming the capability, where before it was admitted and failed silently. No stock OpenShift SCC has that shape: `restricted-v2` already rejects `IPC_LOCK`, and `privileged` allows everything. Under Pod Security admission only the `privileged` level admits these workers, as it already did: `baseline` allows `SYS_CHROOT` but not `IPC_LOCK`. The residual case is a custom SCC written as exactly `[IPC_LOCK]`; the troubleshooting section covers it.
- **A worker whose `sshd` cannot serve sessions stays NotReady, and the Job waits.** The `dependsOn` gate never releases, so JobSet never creates the launcher and nothing produces the ORTE banner. No NVCRE detector reports this state: `checkSchedulingBlocked` acts only on pods the scheduler could not place, and `checkStallTimeout` returns before looking unless `stallMultiplier` is set, which the communication categories do not do. The Job therefore stays `InProgress` until `timeoutPerJob`, which defaults to 1h for the communication categories and 24h for a WorkloadRun; under KAI, `launcherWaitScript()` fails the launcher after 20 minutes instead. That is later than the terminal failure arrived before, when `mpirun` failed within minutes. The trade is that the diagnosis is now direct: the worker shows `0/1`, no launcher pod exists, and `kubectl describe pod` on the worker carries `Readiness probe failed` with `ssh`'s own message. Relaying a persistent worker readiness failure into the Job's condition, the way `WorkloadSchedulingBlocked` relays the scheduler's message, is the natural follow-up and is tracked in issue #469.
- **The probe costs one SSH session per worker every thirty seconds for the life of the pod.** `sshd` forks per connection and `true` exits immediately; the cost is negligible next to a running NCCL rank. Each session leaves `sshd -e`'s accept and disconnect lines in the worker log.
- **Torch is untouched.** No torch runtime, override, or golden changes.
- **The two AWS override blocks now carry a stated obligation.** Their comments say the list is a restatement that must stay in sync with the builder, and the overrides case fails if a future block restates the list without the capability.
- **Golden churn is confined to the two changes.** 31 integration goldens (28 Certification, 3 WorkloadRun MPI) and 8 UAT goldens change only in the capability list and the probe block; `initialDelaySeconds` and `successThreshold` keep the values the API server defaulted before; `periodSeconds` moves from the defaulted 10 to 30. No unit golden outside the new test changes.

## Alternatives Considered

- **Grant the capability only on OpenShift, by platform detection.** Rejected. The capability is needed wherever CRI-O runs, and CRI-O is not an OpenShift-only runtime. Requesting it everywhere is also what a correct pod spec looks like: it names what its processes need rather than what the runtime happens to default.
- **Keep the `tcpSocket` probe and fix only the capability.** Rejected. The two defects are one defect observed twice. Fixing the capability alone ships a cluster that works, with a gate that still cannot tell a working worker from a listening one, so the next `sshd` misconfiguration produces the same silent failure with the same misleading symptom.
- **A `startupProbe` instead of a readiness probe.** Rejected. A failing startup probe restarts the container, which turns the condition into `CrashLoopBackOff` with the diagnostic in a previous container's log. Readiness keeps the pod and its log in place, and the `dependsOn` gate is defined on readiness.
- **Fail the Job as soon as a worker's probe fails.** Deferred. The probe fails legitimately during the `openssh-server` install on images that do not ship it, so "probe failing" alone is not a terminal signal; a time-bounded relay into the Job condition is the right shape and belongs with the scheduling-stall machinery, not here.
- **Run `launcherWaitScript()` on every path, not only under KAI.** Considered and not taken. It would duplicate the worker-side check and add an init container to every MPI launcher, at the cost of discovering the failure after the gate rather than before it. The worker-side probe catches it at the source.
- **A pure `/proc` or `capsh` capability check as the probe.** Rejected. The capability is known at admission and does not change; a probe is the wrong tool for a static fact. The session check also catches host-key and `authorized_keys` failures, which a capability check cannot.
- **Make the probe or the capability list configurable.** Rejected. There is no configuration under which an MPI worker should not be able to chroot, and no configuration under which readiness should mean less than a working session.

## Notes

- **Check the worker image on its own.** On AWS H100 and GB200 both the launcher and the worker run `public.ecr.aws/hpc-cloud/nccl-tests`, a prebaked image on which the guarded `openssh-server` install never runs, so the Debian dependency argument does not apply to it. Its Dockerfile is the authority: it installs `openssh-client` and `openssh-server` together.
- **Do not use `/dev/tcp` in the fallback.** It is a bash builtin and the probe runs under `sh`, which is dash on Debian and Ubuntu. Reading `/proc/net/tcp` (local address `:0016`, state `0A`) works under any shell with `grep`.
- **The worker has three patch routes, and the test walks all of them.** A platform override's `dependencies`, an override's `jobTemplate` or `jobTemplatePatch`, and the base entry's `workload.trainJob.runtimePatches` can each reach the worker container. Only the first restates the capability list today; the test treats all three as fragments so the next one that does is caught where it is written. Trainer's `ContainerPatch` admits only `name`, `env`, `volumeMounts` and `securityContext`, so the `runtimePatches` route can restate the capability list but cannot carry a probe; the test still reports a probe found there rather than relying on the API server to prune it.
- **The probe's shape is load-bearing.** The test requires a single `exec` handler that invokes an `ssh` client against `127.0.0.1` with `BatchMode=yes` and `timeoutSeconds` of at least 10, and rejects any fragment that restates the probe with another handler. Relaxing it to a banner read in the name of removing the `ssh` dependency reintroduces the defect.

## References

- GitHub issue #461; issue #469 (follow-up: surface a workload whose workers never become Ready)
- `pkg/platform/runtime.go`: `sshdProbeCommand`, `sshdReadinessProbe`, `BuildMPIRuntime`
- `pkg/platform/gang_scheduler.go`: `launcherWaitScript`
- `pkg/controller/workflow_detect.go`: `mergeValue`, `mergeNamedSlices`, `indexByName`
- `pkg/controller/job_controller.go`: `checkSchedulingBlocked`, `checkStallTimeout`
- `pkg/platform/sshd_worker_invariants_test.go`
- OpenSSH 7.5 release notes (privilege separation mandatory); `sshd.c` and, from 9.8, `sshd-session.c` (`kex_exchange_identification` before `privsep_preauth`)
- Kubernetes Pod Security Standards, baseline profile capability allowlist
- CRI-O `default_capabilities`
- JobSet `Network.PublishNotReadyAddresses` default
- aws-samples/awsome-distributed-training, `micro-benchmarks/nccl-tests/nccl-tests.Dockerfile`
