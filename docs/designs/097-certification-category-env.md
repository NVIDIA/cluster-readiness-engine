# ADR-097: Environment Variables on CategoryOptions

> **Status:** Proposed

## Context

A Certification cannot set an environment variable on the workload it runs. `CategoryOptions` (`api/v1alpha1/certification_types.go:139-466`) carries thirty-one fields covering sizing, scheduling, storage, thresholds and image, and not one of them lets an operator say "set this variable on this category". A WorkloadRun can: `spec.env` (`api/v1alpha1/workloadrun_types.go:269`) takes a `[]corev1.EnvVar` that is merged over the auto-injected NCCL defaults, and `spec.framework.mpi.mpiArgs` (`:88`) passes arbitrary flags to `mpirun`. The same cluster is therefore configurable through one tier and not the other.

The asymmetry is not a design decision anyone recorded. It is what happens when one tier grows from a hand-authored spec and the other from a curated catalog: the WorkloadRun path needed a general escape hatch to be usable at all, and the Certification path never got one because the catalog was expected to know every value in advance.

A site running on-prem HGX B300 made that expectation concrete. Their NVSwitch multicast state is such that NCCL's NVLink SHARP path fails at `cuMulticastBindMem` with `CUDA_ERROR_ILLEGAL_STATE`, and on NCCL 2.29 that aborts the job rather than falling back (the fallback branch present through 2.28 was removed upstream). The run completes with `NCCL_NVLS_ENABLE=0` and does not complete without it. On a WorkloadRun they set it in `mpiArgs` and got results; on a Certification there was nothing to set, so the certification path was unavailable to them until the host-side fault is repaired. They ran WorkloadRuns instead and lost the certification verdict, which is the artifact they actually wanted. Issue #499 covers one NVCRE-side contributor to that failure and is independent of this design; even with it fixed, a site whose fabric genuinely needs NVLS off has no way to express that to a Certification.

The shape recurs beyond this one variable. An HCA pin, a RoCE GID index, a debug level raised for one category: all are per-site facts a catalog release cannot anticipate, and all are currently answered by "wait for a release" or "drop a tier". The alternative the repository has used so far is a typed field per variable. `enableMNNVL` is that: one NCCL variable promoted to a CRD field, resolved through `DefaultEnableMNNVL` (`pkg/controller/helpers.go:254`), threaded through roughly twenty call sites across four packages, tokenised in `categoryOptionsSuffix`, and interpolated as `{{ if .EnableMNNVL }}1{{ else }}0{{ end }}` into seventeen catalog fragments. That is the per-variable cost, and it does not amortise: the next variable pays it again.

Two facts about the catalog shape this design more than the API question does.

**The entries do not agree on where environment lives.** The three `communication/nccl-*` MPI entries put NCCL variables on the `mpirun` command line as `-x NAME=VALUE` pairs inside `trainer.args`; their `jobTemplate` trainer block has no `env` key at all. The `training/*` entries, both loopback variants and `diagnostics/dcgm-level4` run under `mlPolicy.torch` and carry their variables as container environment on the dependency TrainingRuntime. A single write target does not exist.

**On an MPI entry, container environment does not reach the ranks.** Kubeflow propagates `TrainJob.spec.trainer.env` onto the container named `node` in the replicated job labelled `trainer.kubeflow.org/trainjob-ancestor-step: trainer` (`pkg/runtime/framework/plugins/jobset/builder.go:142-166`, upserting by name via `pkg/apply/apply.go:79-87`, in `kubeflow/trainer/v2@v2.2.1`). The MPI entries put that label on the **launcher** replicated job (`pkg/catalog/entries/communication/nccl-all-reduce.yaml:97-104`), so a variable written there reaches `mpirun`'s own container. The ranks run on the worker pods, started by `sshd` with a fresh session environment, and never see it. The catalog already relies on this distinction knowingly: the OCI GB200 and GB300 arms set the same variables twice, once in `trainer.env` for the launcher's own UCX initialisation and once as `-x` pairs for the ranks, with a comment saying exactly that.

## Decision

1. **Add `Env []corev1.EnvVar` to `CategoryOptions`.** Because `CertificationSpec` inlines `CategoryOptions`, it is readable both globally at `spec.env` and per category at `spec.categories[].options.env`. The field mirrors `WorkloadRun.spec.env` in name, type and intent, so an operator moving between tiers meets one concept rather than two.

   ```yaml
   spec:
     env:
       - name: NCCL_NVLS_ENABLE
         value: "0"
     categories:
       - domain: communication
         variant: nccl-all-reduce
         options:
           env:
             - name: NCCL_IB_HCA
               value: mlx5_0,mlx5_1
   ```

2. **Resolve global and per-category values by name, not wholesale.** `resolveCategoryOptions` (`pkg/controller/certification_controller.go:788-880`) replaces maps, slices and pointers outright when the category sets them; `workloadMetadata` is the single exception, merging per key with a rationale at `:881-893` ("a queue label and a cost-attribution label are independent intents"). Environment variables are independent in the same way, and the failure mode under wholesale replacement is worse than for labels: a category that adds one variable silently drops every global one, so a site-wide `NCCL_NVLS_ENABLE=0` disappears from the one category that also wanted an HCA pin, and the run fails or regresses with nothing in the spec that looks wrong. Per-name merging carries the same documented cost `workloadMetadata` accepts: a category can change or add a name but cannot remove a global one.

3. **Write two surfaces, chosen by `mlPolicy` on the referenced TrainingRuntime.** A new `ApplyEnvToJobTemplate` in `pkg/platform/` resolves `trainJob.runtimeRef.name` against `spec.Dependencies`, the same walk `ApplyImageToDependencies` (`pkg/platform/image.go:38-60`) already performs, and branches on whether that runtime declares `mlPolicy.mpi`:

   - **MPI:** write `-x NAME=VALUE` pairs into `trainer.args`. Where a pair for that name already exists, replace its operand in place. Where it does not, insert the pair immediately after the last existing `-x` pair. Where the entry has no `-x` pair at all, return an error naming the domain and variant rather than guessing.
   - **Everything else:** upsert into `trainer.env` by name. Kubeflow then upserts that over the dependency runtime's container environment, so the dependency YAML needs no rewrite.

   Discriminating on `mlPolicy.mpi` rather than on "does `trainer.args` contain a `-x`" matters: the second conflates "is MPI" with "has an anchor", and a future MPI arm with no `-x` pairs would be silently misclassified as torch and written to an environment the ranks never read, which is precisely the invisible no-op this field exists to remove.

   `diagnostics/dcgm-level4` takes the torch branch and receives the variable, which has no effect because `dcgmi` reads no NCCL settings. That is accepted rather than special-cased: a container environment variable that nothing reads is inert everywhere in Kubernetes, and a rule excluding one entry would be a surprise of its own.

4. **Insert MPI pairs before the test binary, not at the end of the list.** `trainer.args` has the shape `[mpirun flags] [-x pairs] [binary] [binary flags]`. Appending puts the pair after the binary path, where `all_reduce_perf_mpi` receives it as its own argument: no error, no effect on the rank environment. Across every platform and GPU architecture combination rendered for the three MPI entries, the `-x` pairs form one contiguous run and the element two positions past the last `-x` is always the test binary. Insertion at `lastXIndex + 2` is therefore well defined, and it places the new pair last among the `-x` flags, which is also the right precedence if a duplicate ever survives. Two anchors that look reasonable are rejected: the first absolute-path token breaks on AWS GB300, whose `--prefix /opt/amazon/openmpi` precedes any `-x`; and a fixed offset past the leading MCA block breaks because that block varies in length and is not present in every arm.

   Replace-in-place rather than append is load-bearing beyond tidiness. `isMNNVLEnabledInJobTemplate` (`pkg/controller/workflow_controller.go:3738`) reads `NCCL_MNNVL_ENABLE` back out of the rendered template to decide whether the diagnose no-NVL screening stage runs, scanning for the first matching prefix. With one occurrence it reads the operator's value, which is correct. With two, `mpirun` takes the last and the scan takes the first, and the controller's belief about the run diverges from what the ranks received.

5. **Run the transform in the existing post-resolve stage.** `ApplyResolvedWorkflowTransforms` (`pkg/platform/transform.go:76-96`) exists to run every post-resolve rewrite in one pinned order after catalog and platform overrides have resolved, so the owner's options win over both; ADR-079 fixed that order. The environment write becomes a step in it, positioned after the image replacement and before the workload-metadata write. It touches `trainer.args` and `trainer.env`, disjoint from every other step, so only its position relative to the closing `ValidateResolvedJobTemplate` matters, and that already runs last.

   The resulting precedence, stated in full:

   1. the catalog entry's own values;
   2. platform and GPU-architecture overrides, in the order the entry lists them;
   3. `spec.env`, replacing by name;
   4. `spec.categories[].options.env`, replacing that by name.

   `enableMNNVL` is not a layer in this list. Every fragment that reads it interpolates `{{ if .EnableMNNVL }}1{{ else }}0{{ end }}` into an ordinary value during catalog render, which is step 1 or 2, so an `NCCL_MNNVL_ENABLE` set in `env` wins over it by construction rather than by rule. The one thing below step 4 that still rewrites an environment value is the diagnose no-NVL screening stage, which sets `NCCL_MNNVL_ENABLE=0` at Job creation for the stages whose purpose is to test without NVLink; fault isolation needs NVL off to isolate the fault, so it wins for those stages only.

6. **Reject `valueFrom`, and reject names the launch mechanism owns.** A variable reaches MPI ranks by value on a command line, which cannot carry a reference to a Secret or ConfigMap; accepting `valueFrom` would mean a field that works on four entries and silently does not on three. A secret-sourced value is a WorkloadRun's job. Rejecting now is reversible and accepting is not.

   The denied names are those where a user value breaks the launch rather than merely overriding a default: the `OMPI_MCA_*` variables Kubeflow's MPI plugin writes on the launcher, which carry the hostfile and rsh arguments, and the `PET_*` variables its torch plugin already rejects at TrainJob admission. Turning the latter into an immediate field error is better than an opaque webhook failure three tiers down. Deliberately not denied are platform defaults such as `NCCL_SOCKET_IFNAME`, `NCCL_IB_GID_INDEX` and `NCCL_TOPO_FILE`: overriding those is the purpose of the field.

7. **Bound the list, and bound it for a reason.** `MaxItems=32`. `CategoryOptions` appears both inlined at the spec root and nested under `categories[]`, which is itself `MaxItems=64` with a comment recording that the bound exists to keep nested CEL validation inside the API server's cost budget. A CEL rule over an unbounded list in that position is costed against the same budget. The bound also keeps the validation expressible as one rule rather than several, since each is costed separately and multiplied by the parent cardinality.

8. **Record the resolved variable names on the report.** `CategoryReport` (`pkg/report/report.go:79-122`) carries one configuration field, `MNNVL`, so a certification artifact does not currently say what the run was configured with. An operator can already change the verdict through `image`, `thresholds`, `resources` and `maxBytes` with no trace in the report; `env` would be the next instance, and the most pointed one, since the motivating variable disables a hardware feature the certification exists to exercise. The resolved names, not their values, are added to the category's report entry and printed in its scope box. Names only: a value may embed a path or a hostname, and the report is a document people forward.

   The verdict itself is not downgraded. An operator who sets `NCCL_SOCKET_IFNAME` so the test runs at all on an unusual NIC layout has not narrowed the scope of anything, and a warning that fires on legitimate use stops being read.

9. **Add a token to `categoryOptionsSuffix`.** Two categories of one domain and variant differing only in `env` must not share a Workflow name; without a token the second adopts the first's Workflow and reports a result for a configuration that never ran. Issue #430 is that defect in general, across the twenty-three options the suffix omits today, and its resolution decides the derivation. This design follows whatever #430 lands rather than inventing a parallel scheme: if the suffix comes to be derived from the resolved options wholesale, `env` is covered without further work, which is the outcome this ADR prefers and has said so on that issue.

## Implementation

- `api/v1alpha1/certification_types.go`: the `Env` field on `CategoryOptions`, with the `MaxItems` bound and the CEL rule rejecting `valueFrom` and the reserved names. Then `make manifests generate`; the CRD lands in `helm/cluster-readiness-engine/crds/nvcre.nvidia.com_certifications.yaml`. No `+kubebuilder:rbac` marker changes, so `manager-role.yaml` is untouched.
- `pkg/controller/certification_controller.go`: a `resolveEnv` helper beside `resolveWorkloadMetadata`, called before the `override == nil` early return for the same aliasing reason; the `Env` field on the `ResolvedWorkflowTransforms` literal; and the `categoryOptionsSuffix` token, shaped by #430.
- `pkg/certification/certification.go`: the same transform field on the CLI render path, and the reserved-name and `valueFrom` checks in `readCertification`, since the kubebuilder markers bind only server-side and `nvcrectl certification render` never reaches an API server.
- `pkg/platform/transform.go`: the `Env` field on `ResolvedWorkflowTransforms`, the new step in the pinned order, and the doc comment extended to match.
- `pkg/platform/env.go` (new): `ApplyEnvToJobTemplate`, with the structural contract `ApplyImageToJobTemplate` (`pkg/platform/image.go:126-134`) established — a no-op on empty input, never creating structure. The `-x` scan reuses `mpiEnvArgName` and `mpiEnvArg` (`pkg/platform/nccl.go:86-100`) for parsing and rendering, but not `MPIEnvArgs` itself: that function skips names already forwarded in `mpiArgs`, because on a WorkloadRun those are the user's and must win. Here the existing pairs are the catalog's and must lose. The merge direction is inverted and the polarity has to be written for this caller.
- `pkg/report/report.go`: the resolved names on `CategoryReport` and one line in the printed scope box.
- `docs/api-reference/certification.md`: a field row and a short precedence section, modelled on the existing one. The page is already in the published nav, so `docs/index.yml` does not change.
- `config/samples/`: one example carrying a global and a per-category value.

Tests follow `testutil.TestCaseParser` with testdata golden files, as the affected packages require. The cases that have to exist: MPI replace-in-place where the name is already an `-x` pair; MPI insertion where it is not; an MPI entry with no `-x` pair failing closed; torch upsert over a dependency runtime value; `dcgm-level4` rendering the variable inertly; the OCI arms that write both surfaces; per-name merge of global and per-category; a reserved name rejected on both the controller and CLI paths; and an empty `env` leaving every existing render byte-identical. That last one is the regression guard for the whole design: a Certification that sets nothing must produce exactly what it produces today.

## Rationale

**One field rather than a field per variable.** The alternative with precedent is `enableMNNVL`: promote the variable, add a default resolver, thread it through the render path, tokenise it for naming, interpolate it into every fragment that cares. That cost is justified when the setting is architecture-determined and needs a computed default, which MNNVL is. It is not justified for a per-site fact, and repeating it for NVLS, then for an HCA pin, then for a GID index, grows the API one knob at a time with no end state. A single `env` covers the whole class, including variables nobody has thought of yet.

**Per-name merging over wholesale replacement.** Argued at Decision 2. The deciding consideration is which failure is louder: under per-name merging, an operator who expected to replace the global set instead adds to it, and sees a variable they did not want in a render they can inspect. Under wholesale replacement, an operator loses a variable they did set, in a category they were not editing, and finds out from a failed run.

**Two surfaces rather than one.** Writing only `trainer.env` would be simpler and would be a silent no-op on the three communication entries, which are exactly the entries whose settings this field exists to carry. Writing only `-x` would leave the torch entries untouched. The split is in the catalog, not in this design, and a transform that ignores it would be wrong in one direction or the other.

**No special case for `dcgm-level4`.** An inert variable is the normal outcome of setting one on a process that does not read it.

**Documenting the precedence as an ordered list.** The interaction between `enableMNNVL` and an `NCCL_MNNVL_ENABLE` in `env` is the kind of thing that is obvious to whoever writes the code and opaque six months later. Stating it as four steps, with the note that `enableMNNVL` is not a step, costs a paragraph and settles the question.

## Consequences

An operator can now set a value on a Certification that changes what the certification exercises, including disabling a hardware feature it is meant to test. That is the point of the field and also its risk, and the report surfacing at Decision 8 is the mitigation: the names appear in the artifact, so a reader of a passing certification can see that it was tuned. The verdict is not qualified, because the field's legitimate uses are more common than its abusive ones.

The API grows one field that is inlined in two positions, each of which carries a CEL rule. The `MaxItems` bound keeps that inside the API server's validation budget, and the bound is checked at `make manifests` time rather than discovered on a cluster.

A category that sets `env` and a sibling that does not now render differently and must be named differently, which depends on #430. Until that lands, this field's token is the only one in `categoryOptionsSuffix` derived from a map-shaped option, and it should be written to be replaced by whatever derivation #430 chooses rather than to persist alongside it.

Existing Certifications are unaffected. An absent `env` leaves every rendered Workflow byte-identical, which the test suite pins.

## Alternatives Considered

**A typed `enableNVLS *bool`, mirroring `enableMNNVL`.** Rejected. It solves one site's problem and leaves the class open, it creates a second mechanism controlling a value the catalog fragments already set with no principled precedence between them, and it invites the same request for the next variable. `enableMNNVL` earned its field by being architecture-determined; NVLS is per-site tuning, and the nine fragments that set it disagree on the value, which is what per-platform tuning looks like.

**`mpiArgs []string` on `CategoryOptions`, mirroring the WorkloadRun field.** Rejected. It would cover the three MPI entries and do nothing for the four others, and it exposes the launch mechanism in the API of a tier whose entire premise is that the catalog owns the launch.

**Nothing on the CRD; add `NCCL_NVLS_ENABLE=0` to the on-prem `b200`/`b300` catalog block instead.** Rejected, and separately from this ADR. ADR-094 Decision 2 deliberately left that block deps-only, and the evidence since is a failure with NVLS enabled, not a success with it disabled. Shipping the value would mask a genuine fabric fault at every on-prem B200 and B300 site and cost in-network reduction bandwidth at the healthy ones. A site-specific fact belongs in a site's spec, which is what this field provides.

**Deny `NCCL_MNNVL_ENABLE` in `env` to avoid overlapping `enableMNNVL`.** Rejected. The overlap is resolved by precedence rather than prohibition, and a carve-out forbidding the one variable the API already models would be hard to explain. The real interaction is `isMNNVLEnabledInJobTemplate` reading the value back, and that is addressed by replace-in-place, not by a deny-list.

**Leave the asymmetry and document the WorkloadRun fallback.** Rejected as the end state, though it is what sites do today. A Certification produces a verdict across a fleet; a WorkloadRun does not. Telling an operator with one unusual fabric setting to give up the verdict is a larger concession than adding a field.

## Notes

The motivating cluster's NVLS failure is a host-side fault, most likely a Fabric Manager restarted without a subsequent GPU reset, and the right fix there is on the node rather than in a spec. This field is what lets that site keep certifying while the fault is repaired, not a substitute for repairing it. Issue #500 adds the troubleshooting section that makes the distinction clear to an operator reading the failure.

## References

- Issue #501, the gap this design closes
- Issue #430, Workflow naming for categories that differ in an unencoded option
- Issue #499, `BaseNCCLEnvVars` force-enabling NVLS
- Issue #500, troubleshooting for NVLS multicast bind failures
- [ADR-079](079-workload-object-labels.md), which fixed the order of the post-resolve transform stage
- [ADR-094](094-hgx-b300-architecture-and-onprem-nic-override.md), the on-prem HGX override that is deliberately deps-only
