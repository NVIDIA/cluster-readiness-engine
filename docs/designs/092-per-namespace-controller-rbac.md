# ADR-092: Per-Namespace Write Access for the Controller

> **Status:** Proposed

## Context

The controller runs under one ClusterRole, `nvcre-manager-role`, bound cluster-wide by `nvcre-manager-rolebinding`. Every namespaced resource it writes is therefore writable in every namespace of the cluster. GitHub issue #143 reported this for ConfigMaps. The same is true of every other namespaced type in the role:

| Resource | Verbs today | What a cluster-wide grant permits beyond NVCRE's own runs |
|---|---|---|
| `configmaps` | full lifecycle | Overwrite any ConfigMap, including `kube-system/aws-auth` on EKS (IAM-to-RBAC mapping) and `kube-system/coredns`. Read every ConfigMap in the cluster. |
| `persistentvolumeclaims` | create, delete, get, list | Delete any PVC. With a `Delete` reclaim policy that destroys the data. |
| `trainer.kubeflow.org` TrainJobs | full lifecycle | Create a TrainJob in any namespace, and the Trainer controller then creates pods there. |
| `trainer.kubeflow.org` TrainingRuntimes | create, delete, get, list, patch, update | Change the pod template other teams' TrainJobs resolve against. |
| `resource.k8s.io` ResourceClaimTemplates | create, delete, get, list, patch, update | Delete or alter another workload's device claims. |
| `resource.nvidia.com` ComputeDomains | create, delete, get, list, patch, update | Delete or alter another workload's MNNVL domain. |
| `pods/log` | get | Read the logs of any pod in the cluster. |

None of this breadth is used. Every namespaced object the controller creates lands in the namespace of the Workflow that owns it: dependencies are stamped with `workflow.Namespace` (`pkg/controller/workflow_controller.go:3356`), node-results ConfigMaps likewise (`pkg/controller/node_results.go:141-144`), and the TrainJob is created in the Job's namespace.

The obvious narrowing does not work. Issue #143 suggested a namespace-scoped Role in the install namespace, but runs do not happen there. `nvcrectl certification run` and `nvcrectl workloadrun run` create a fresh `nvcrectl-<timestamp>` namespace per run unless one is given (`pkg/certification/certification.go:1204`, `pkg/workloadrun/workloadrun.go:893`, both through `setup.EnsureNamespace` at `pkg/setup/setup.go:1546`). A Role installed by Helm cannot cover a namespace that does not exist yet. The narrowed role would pass envtest, which runs without RBAC enforcement, and fail with Forbidden on the first real run.

Two properties of the current code also constrain any redesign:

1. **The cache needs cluster-wide list/watch.** controller-runtime informers list and watch across all namespaces. `recordNodeResults` writes its ConfigMap with `controllerutil.CreateOrUpdate` through the cached client (`pkg/controller/node_results.go:145`), which silently starts a cluster-wide ConfigMap informer. That is why the role needs `list` and `watch` on every ConfigMap, and why the controller holds every ConfigMap in the cluster in memory. Issue #424 showed what happens when RBAC is narrower than the cache: a cached PVC read started an informer the role could not watch, and the manager looped on `Failed to watch persistentvolumeclaims: forbidden`. The fix read the PVC through the uncached `APIReader` instead (`pkg/controller/workflow_controller.go:3526-3529`). `Owns(&TrainJob{})` (`pkg/controller/job_controller.go:1896`) needs cluster-wide `list` and `watch` on TrainJobs for the same reason.

2. **Teardown depends on the controller's own permissions.** Certification, Workflow, Job, GoodputMeasurement and BandwidthMeasurement all carry finalizers, and each `handleDeletion` deletes child resources explicitly. If the controller loses write access to a namespace while those finalizers are still pending, deletion returns Forbidden, the finalizer is never removed, and the namespace stays `Terminating`.

The RBAC rules are maintained by hand in `helm/cluster-readiness-engine/templates/manager-role.yaml`; `make manifests` runs controller-gen with `output:rbac:none` (`Makefile:64-68`). Nothing tests the rendered role.

## Decision

1. **Split the controller's permissions into two ClusterRoles.**
   - `nvcre-manager-role`, bound cluster-wide as today, keeps reads, cluster-scoped writes, writes to NVCRE's own CRDs, and event creation.
   - `nvcre-run-role` holds every write to a namespaced resource outside NVCRE's API group. The chart installs it but does not bind it cluster-wide. A ClusterRole with no binding grants nothing.

2. **Grant `nvcre-run-role` one namespace at a time with a RoleBinding** named `nvcre-manager-run`, whose subject is the controller ServiceAccount. Three paths create it:
   - **`nvcrectl`** creates it in the run namespace, with the user's credentials, in the same step that ensures the namespace exists. This covers generated namespaces and namespaces passed with `-n` or `metadata.namespace`.
   - **Helm values** list long-lived run namespaces (`rbac.runNamespaces`), and the chart renders one RoleBinding per entry. This serves GitOps and Run:ai project namespaces.
   - **Users** apply the RoleBinding themselves, next to their Certification. The docs give the manifest.

3. **Keep the current behaviour available behind `rbac.scope`.** `rbac.scope: cluster` binds `nvcre-run-role` cluster-wide, which is equivalent to today's grant. `rbac.scope: namespaced` does not. The release that ships this change defaults to `cluster`, so upgrades change nothing. The default flips to `namespaced` one minor release later, announced in `CHANGELOG.md` ahead of time.

4. **Wait, do not fail, when a namespace has no binding.** Before creating dependencies, the Workflow controller checks its permissions in the target namespace with SelfSubjectAccessReviews. If any is denied it stays `InProgress` with reason `NamespacePermissionsMissing`, emits a Warning event naming the RoleBinding to create, and requeues. If a review cannot be created at all, it stays `InProgress` with reason `NamespaceAccessCheckFailed` and requeues; an unanswered check is never treated as permission. The Workflow's existing timeout bounds either wait. A GitOps sync that applies the Certification a few seconds before the RoleBinding must not fail the run.

5. **Never let a missing binding wedge a finalizer.** When a child deletion in `handleDeletion` returns Forbidden and the object's namespace is `Terminating`, the controller treats the children as the namespace controller's to delete and removes its finalizer. `nvcrectl` cleanup deletes the RoleBinding only after the Certification or WorkloadRun is gone.

6. **Stop caching ConfigMaps and PVCs.** List both in the manager client's `Cache.DisableFor`, so every read of those types goes to the API server. This removes the need for cluster-wide `list` and `watch` on ConfigMaps, and replaces the per-call-site `APIReader` workaround for PVCs with one rule that also covers future call sites.

## Implementation

### Role split

| Rule | `nvcre-manager-role` (cluster-wide) | `nvcre-run-role` (per namespace) |
|---|---|---|
| `nvcre.nvidia.com` CRDs, `/status`, `/finalizers` | unchanged | — |
| LogProfiles, nodes, pods | get, list, watch | — |
| ResourceSlices | get, list, watch | — |
| PersistentVolumes | get, list, watch, patch | — |
| Namespaces | get | — |
| `authorization.k8s.io` SelfSubjectAccessReviews | create | — |
| Events | create, patch | — |
| TrainJobs | get, list, watch; `trainjobs/status` get | create, delete, patch, update |
| ConfigMaps | — | get, create, update, patch, delete |
| PVCs | — | get, create, delete |
| TrainingRuntimes, ResourceClaimTemplates, ComputeDomains | — | get, create, update, patch, delete |
| `pods/log` | — | get |

Notes on the placement:

- **PersistentVolumes** are cluster-scoped, so no RoleBinding can grant them. `patch` stays cluster-wide (see Consequences).
- **Namespaces** are new to the role. `handleDeletion` reads the run namespace to tell whether it is terminating (decision 5), and Namespaces are cluster-scoped, so the run-namespace binding cannot grant that read. Only `get` is added: the read goes through the uncached `APIReader`, because a cached read would start a cluster-wide Namespace informer and need `list` and `watch` as well, the failure mode of issue #424.
- **Events** stay cluster-wide because the controller must be able to report a missing binding in the namespace that lacks it.
- **TrainJob reads** stay cluster-wide because `Owns()` needs a cluster-wide watch. Reading TrainJobs everywhere exposes little.
- **NVCRE CRD writes** stay cluster-wide. A controller that writes its own API types in any namespace is the conventional shape, and the risk is confined to NVCRE objects.
- **`list` is dropped** on PVCs, TrainingRuntimes, ResourceClaimTemplates and ComputeDomains. The controller never lists any of the four: dependencies are read one object at a time by name, and the only list-shaped need, finding a PV by `claimRef`, already uses the PV field index (`pkg/controller/indexes.go:67`). These are the only intentional removals from today's rule set.
- **Workflow dependencies** are read and written as `unstructured.Unstructured` (`pkg/controller/workflow_deps.go:335-339`). controller-runtime does not cache unstructured objects by default, so those reads are already live and need only `get` in the namespace.

### Chart

- `templates/manager-role.yaml` loses the moved rules. A new `templates/run-role.yaml` defines `nvcre-run-role`.
- `templates/run-rolebinding.yaml` renders:
  - one ClusterRoleBinding to `nvcre-run-role` when `rbac.scope: cluster`;
  - one RoleBinding per `rbac.runNamespaces` entry when `rbac.scope: namespaced`. The chart does not create the namespaces. A listed namespace that does not exist fails the install with the API server's own NotFound error, and the values comment says so. The chart does not use `lookup`, because it returns nothing under `helm template` and would break offline rendering.
- `values.yaml` gains `rbac.scope` and `rbac.runNamespaces: []`.

### nvcrectl

- `pkg/setup` gains `EnsureRunBinding(ctx, c, namespace, controllerNamespace)`. It creates or adopts the `nvcre-manager-run` RoleBinding, labelled `app.kubernetes.io/managed-by: nvcrectl`, and reports whether it created it. Kubernetes allows the create only if the caller holds every permission in `nvcre-run-role` within that namespace, or has `bind` on it. On Forbidden, `nvcrectl` stops before submitting anything and says which permission is missing.
- `certification run` and `workloadrun run` call it right after `EnsureNamespace`.
- Cleanup: if `nvcrectl` created the namespace, deleting the namespace removes the binding, and decision 5 covers the ordering. If the namespace already existed and `nvcrectl` created the binding, cleanup waits for the Certification or WorkloadRun to be fully deleted and then deletes the binding.
- `nvcrectl rbac print --namespace <ns>` writes the RoleBinding manifest to stdout, for users who apply manifests themselves. The manifest comes from the same code `EnsureRunBinding` uses, so the role and ServiceAccount names cannot drift between the two.

### Controller

- `cmd/manager/main.go`: `Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.PersistentVolumeClaim{}}}}`. The `jobReader()` call in `cleanupPVForPVC` becomes an ordinary `r.Get`, and its #424 comment moves to the `DisableFor` line.
- Permission check (`pkg/controller/namespace_access.go`): one SelfSubjectAccessReview per (namespace, resource, verb) that the Workflow will need, with results cached for 30 seconds per namespace so a waiting Workflow does not hammer the API server. `nvcre-manager-role` grants `create` on SelfSubjectAccessReviews explicitly. The default `system:basic-user` binding allows it for every authenticated identity, but an operator can remove that binding, and the role should list everything the controller needs. A review only answers questions about the caller's own permissions, so the grant adds no reach. The check separates a denial from a failure. A review that comes back denied means the binding is missing, so the Workflow waits as decision 4 describes. A review that cannot be created at all (a timeout, an admission webhook error, or Forbidden if the grant above is missing) means the check could not run, and the Workflow does not proceed on it: it stays `InProgress` with reason `NamespaceAccessCheckFailed`, emits a Warning event carrying the error, and requeues with backoff. Proceeding would be worse than waiting. Today a forbidden workflow-scoped dependency goes through `failWorkflowForDependencyError` and fails the Workflow terminally (`pkg/controller/workflow_controller.go:338`), and a forbidden job-scoped dependency returns an error from `createJobDependencies` (`pkg/controller/workflow_controller.go:1178-1190`) without updating status. Either way a missing binding would surface as a dependency failure, not as the wait decision 4 promises. A Forbidden returned by a dependency or Job write after the check has passed is classified before it is acted on, because HTTP 403 does not mean only RBAC: a ValidatingAdmissionPolicy, an admission webhook (Kyverno, Gatekeeper) and an exhausted ResourceQuota all reject with Forbidden too. The controller re-runs the SelfSubjectAccessReview for that resource and verb. If the review is now denied, the binding was removed mid-run, and the Workflow is requeued with `NamespacePermissionsMissing`. If the review is allowed, the rejection came from admission or quota, and the error takes the existing path through `failWorkflowForDependencyError`, so the policy's message reaches the Workflow status at once instead of after a timeout. If the review itself cannot be created, the Workflow waits with `NamespaceAccessCheckFailed` as above. Classifying by re-asking the API server avoids matching on error message text, which differs between admission plugins and Kubernetes versions. The check runs in the Workflow controller, before dependency creation, because every write starts there whether the Workflow came from a Certification, a WorkloadRun, or `kubectl apply`. Certification and WorkloadRun surface the Workflow's condition as they already do.
- New reason constants `reasonJobNamespacePermissionsMissing` (a review was denied, or a write was forbidden) and `reasonJobNamespaceAccessCheckFailed` (a review could not be created) on the Workflow tier, following the tier-prefix convention. Both are non-terminal.
- `handleDeletion` in the Workflow and Job controllers: on Forbidden from a child delete, read the namespace through `APIReader` (a live `get`, see the Namespaces note under Role split); if its `deletionTimestamp` is set, log at V(1), remove the finalizer, and return. If the namespace read itself fails, keep the finalizer and requeue rather than guess.

### Documentation

- `docs/operations/deployment.md`, "RBAC requirements": the two-role table, `rbac.scope`, `rbac.runNamespaces`, and a section titled "Running without nvcrectl" containing the RoleBinding manifest:

  ```yaml
  apiVersion: rbac.authorization.k8s.io/v1
  kind: RoleBinding
  metadata:
    name: nvcre-manager-run
    namespace: <run-namespace>
  roleRef:
    apiGroup: rbac.authorization.k8s.io
    kind: ClusterRole
    name: nvcre-run-role
  subjects:
  - kind: ServiceAccount
    name: nvcre-manager
    namespace: nvcre
  ```

- `docs/operations/troubleshooting.md`: an entry for `NamespacePermissionsMissing`, replacing the generic advice at line 92 to check the logs for RBAC errors.
- `docs/cli-reference/`: `nvcrectl rbac print`.
- `docs/designs/000-adr.md:398`: the "Least-privilege RBAC" paragraph is corrected. It currently says the controller has no ConfigMap permissions and patches nodes, both false since ADR-061.
- `CHANGELOG.md`: an entry under `[Unreleased]`.

### Testing plan

- `test/helm/`: golden renders for `rbac.scope: cluster`, `rbac.scope: namespaced` with two `runNamespaces`, and `namespaced` with none. A table test asserts that, in cluster mode, the union of the two roles' rules equals the rule set shipped before this change, minus the `list` removals and plus the Namespace `get` and SelfSubjectAccessReview `create` additions listed under Role split. The expected differences are spelled out in the test table, so the default mode cannot gain or lose a permission that this record does not name.
- `cmd/integration/testdata/reconcile/`: envtest does not enforce RBAC, so the permission check is exercised through an injected `AccessReviewer` interface. Cases: binding present, binding absent (Workflow stays InProgress with `NamespacePermissionsMissing` and a Warning event), binding appearing on a later reconcile (Workflow proceeds), and the reviewer returning an error (Workflow stays InProgress with `NamespaceAccessCheckFailed` and creates no dependencies). Two further cases inject Forbidden from a dependency create after the check passes. When the re-run review is denied, the Workflow is requeued with `NamespacePermissionsMissing`, not failed. When it is allowed, as for an admission-policy rejection, the Workflow fails with `DependencyCreationError` and the rejection message, as it does today.
- `pkg/controller`: a unit test that `handleDeletion` removes the finalizer on Forbidden when the namespace is `Terminating`, and retries when it is not.
- UAT (`test/uat/`): the Kind cluster does enforce RBAC. One run with `rbac.scope: namespaced` through `nvcrectl certification run` proves the binding path end to end, and one `kubectl apply` of a Certification with no binding proves the wait and the event.

## Rationale

- **The user who creates the namespace grants the access.** `nvcrectl` already creates the run namespace with the user's own credentials, so it can create the binding in the same step without the controller holding any RBAC-writing permission. The controller's reach then follows from what a human chose to start, rather than being a standing grant over the whole cluster.
- **Waiting matches how manifests are applied.** GitOps tools and `kubectl apply -f dir/` do not promise that the RoleBinding lands before the Certification. A terminal failure would turn an ordering detail into a failed run. A non-terminal condition with an event explains itself and recovers on its own.
- **Disabling the cache is the fix #424 already made, applied by type.** Reading ConfigMaps and PVCs live costs one GET per access, and those accesses happen a handful of times per Workflow. In return the controller stops watching every ConfigMap in the cluster, which is the read exposure issue #143 raises, and stops holding them all in memory.
- **A mode switch makes the change safe to ship.** Existing installs, including any that create Certifications outside `nvcrectl`, keep working in `cluster` mode while the namespaced path proves itself.

## Consequences

- In `namespaced` mode the controller cannot write ConfigMaps, PVCs, TrainJobs, TrainingRuntimes, ResourceClaimTemplates or ComputeDomains, or read pod logs, in any namespace without a binding. It can no longer read ConfigMaps anywhere it is not bound.
- **The controller gains `get` on Namespaces**, a permission it does not hold today. It exposes namespace metadata (labels, annotations, phase) and nothing inside the namespace.
- **PersistentVolume `patch` stays cluster-wide.** The code patches only PVs that carry the Workflow's UID annotation (`pkg/controller/workflow_controller.go:3489-3512`), but RBAC does not enforce that, so the controller can still switch any PV's reclaim policy to `Delete`. Enforcing it needs a ValidatingAdmissionPolicy and is left out of this record (see Notes).
- **Users who skip `nvcrectl` carry one extra manifest per namespace**, or one Helm value per long-lived namespace. Their runs wait with a named reason until it exists.
- **`nvcrectl` needs permission to create RoleBindings** in the run namespace. A cluster admin has it. A user with less needs `bind` on `nvcre-run-role`, or admin rights in the namespace.
- **Every ConfigMap and PVC read is a live API call.** Expected volume is a few calls per Workflow.
- **Two ClusterRoles to maintain by hand instead of one.** The helm golden and the union test stand in for the generation step the repository does not have.
- The controller's ServiceAccount name becomes part of a contract users write into their own manifests. Renaming it later is a breaking change, which `nvcrectl rbac print` softens but does not remove.

## Alternatives Considered

### Namespace-scoped Role in the install namespace (issue #143 as filed)

Rejected. Runs do not happen in the install namespace, and `nvcrectl` creates a new namespace per run. Every run would fail with Forbidden.

### The controller creates the RoleBinding in each run namespace

Rejected. The controller would need cluster-wide `create` on RoleBindings, plus `bind` or `escalate`. A controller that can bind roles can grant itself whatever those roles hold in any namespace, which is broader than the grant this record removes.

### Pin every run to one namespace

Rejected as the only mechanism. It removes per-run isolation, which `nvcrectl` relies on for cleanup: deleting the run namespace removes everything the run created. It stays possible within this design: list one namespace in `rbac.runNamespaces` and always pass `-n`.

### A RoleBinding with a finalizer the controller removes

Considered for decision 5. The binding would outlive the NVCRE objects in a terminating namespace, because RBAC honours bindings that have a `deletionTimestamp`. Rejected because it needs cluster-wide `patch` on RoleBindings so the controller can remove the finalizer. Treating Forbidden in a terminating namespace as already handled reaches the same result with no new permission.

### Per-namespace informer caches

Rejected. controller-runtime fixes its namespace set when the cache starts. Runs create namespaces at any time, so the set would need restarts or a custom multi-cache.

## Notes

- **Why the default flips in two steps.** Defaulting to `namespaced` straight away would close the issue at once, but it would break every existing `kubectl apply` or GitOps flow until those users add bindings. One release with `cluster` as the default and `namespaced` documented gives them time to add bindings. The issue stays open for anyone who does not opt in during that release.
- **Follow-up: enforce the PV patch with a ValidatingAdmissionPolicy.** A CEL policy matching this ServiceAccount could admit PV updates only when the object carries `annotationWorkflowUID`, or when the update adds it to a PV whose `claimRef` names a PVC the Workflow owns. That is a separate change with its own test surface.
- `CLAUDE.md` lists `templates/role*.yaml` and `templates/*_role*.yaml` as generated files that must not be edited, but `make manifests` writes no RBAC (`output:rbac:none`), and `manager-role.yaml` is maintained by hand. Implementing this record means editing that file, so the `CLAUDE.md` rule should be corrected in the same change.
- envtest does not enforce RBAC, which is why the Forbidden behaviour is tested through an injected reviewer plus one UAT run, not integration goldens alone.

## References

- GitHub issue #143: least-privilege redesign for the manager's cluster-wide ConfigMap grant
- GitHub issue #424: manager loops on a forbidden PVC watch after a cached PVC read
- ADR-000: the original least-privilege RBAC statement, corrected by this record
- ADR-034: inferred dependency lifecycle, which defines where dependencies are created
- ADR-042 and ADR-050: `nvcrectl certification run` and its namespace handling
- ADR-061: removal of node mutation from the controller
- `pkg/controller/node_results.go`, `pkg/controller/workflow_controller.go`, `pkg/controller/cache.go`, `pkg/setup/setup.go`
- `helm/cluster-readiness-engine/templates/manager-role.yaml`, `docs/operations/deployment.md`
