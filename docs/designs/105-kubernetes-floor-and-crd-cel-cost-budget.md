# ADR-105: Every CRD Installs on Every Supported Kubernetes Minor, and No CEL Reads a Quantity

> **Status:** Accepted

## Context

The installation guide says NVCRE needs Kubernetes 1.29 or later. No release has met that. Every API server from 1.29 through 1.33 refuses to create the Certification CRD:

```
CustomResourceDefinition.apiextensions.k8s.io "certifications.nvcre.nvidia.com" is invalid:
  ...properties[resources].x-kubernetes-validations[0].rule: Forbidden:
  estimated rule cost exceeds budget by factor of 8.1x
```

`nvcrectl setup init` stops at `[helm] apply CRD certifications.nvcre.nvidia.com`, and a plain `helm install` fails the same way. A cluster on 1.32.10 hit it first. The other six CRDs install. Every release from v0.1.0 up to the one containing this fix carries the cause, which arrived in #232 (commit 7c5a67b). #232 added `CategoryOptions.resources` with four CEL rules:

- one `quantity(string(self)).compareTo(quantity("0")) >= 0` on each of `cpu` and `memory`, on both `requests` and `limits`;
- two rules on `CategoryResources` comparing each request to its limit.

`CategoryOptions` appears twice in the schema, once inline in the spec and once under each `categories[]` item. The four rules therefore appear twice as well.

The API server estimates the worst-case cost of each rule when a CRD is created. It rejects any rule above 10M cost units and any schema whose rules total above 100M. A rule under a list item is charged once for every item the list may hold. For the per-category rules that is `categories.maxItems`, which is 64.

`resource.Quantity` is `x-kubernetes-int-or-string`, which means it holds either a number or a string. Through 1.33, `k8s.io/apiserver/pkg/cel/common/schemas.go` sizes such a value at the maximum request size minus two bytes, about 3 MiB, whatever `maxLength` says:

```go
// handle x-kubernetes-int-or-string by returning the max length/min serialized size of the largest possible string
return maxRequestSizeBytes - 2
```

1.34 added a branch that honours `maxLength`, so 1.34 and later accept the CRD. The `MaxLength=32` that #232 placed on each quantity, and the comment saying it keeps the rules within budget, only take effect on 1.34+. Setting it to 4 makes no difference on 1.29–1.33.

These are the measured per-evaluation costs on envtest API servers. Each was derived from the reported overrun factor at two `maxItems` values.

| Rule | 1.29 | 1.30–1.33 |
|---|---|---|
| `quantity(string(self)).compareTo(quantity("0")) >= 0` | ~315k | ~630k |
| request ≤ limit (two `quantity()` calls) | ~630k | ~1.26M |
| `type(self) == int` | ~315k | ~315k |
| `type(self) == int ? self >= 0 : !self.startsWith('-')` | ~315k | ~315k |
| `self >= 0` | cheap | cheap |

The table carries two lessons.

First, it is not `quantity()` alone. On these versions almost any operation on an int-or-string value is costed as a 3 MiB string: `type()`, `string()`, `startsWith()`. The forms that come out cheap are wrong for one of the two value types. `self >= 0` errors on every string, and `int(self) >= 0` errors on `"1500m"`.

Second, 1.29 and 1.30 price the same rule differently. A variant that passes on 1.29 can fail on 1.30–1.33. Testing only the oldest supported version would not have been enough.

CI never caught any of this. envtest runs one version, derived from `k8s.io/api` in `go.mod`, which is 1.36 today and was never older than 1.34 while these rules existed.

## Decision

1. **The minimum supported Kubernetes version stays 1.29, and it becomes a tested claim.** Every NVCRE CRD must install on every minor from `MIN_K8S_VERSION` (1.29) up to `ENVTEST_K8S_VERSION`. `make test-crd-compat` checks this, and CI runs it. The CRDs are generated into the main chart's `crds` subchart (`helm/cluster-readiness-engine/charts/crds/crds/`), and the `cluster-readiness-engine-crds` chart ships byte-identical copies ([ADR-107](107-crds-chart.md)), so one check covers both charts. The minimum in `docs/getting-started/install.md` must equal `MIN_K8S_VERSION`, and a test fails if the two drift apart.

2. **No CEL rule may read an int-or-string value while any supported minor is 1.33 or older.** On those versions such a rule costs about 315k per evaluation even when it does nothing useful. That allows about 31 evaluations per rule, far below the 64 categories one Certification may hold. Validation for these fields uses OpenAPI keywords, which have no CEL cost, or moves to the controller.

3. **`cpu` and `memory` in `CategoryResourceList` are validated at admission by pattern.** A `+kubebuilder:validation:Pattern` restricts each to a non-negative quantity that `resource.ParseQuantity` decodes, and decodes quickly. It is controller-gen's Quantity pattern without a leading `-`, with the exponent limited to a whole number of at most two digits.
   - **Why the pattern must be stricter than controller-gen's.** Every typed client decodes these fields with `ParseQuantity`. One Certification that fails to decode breaks every typed List of Certifications in its namespace, including the controller's informer and nvcrectl's run-namespace RoleBinding cleanup. controller-gen's pattern is looser than `ParseQuantity`: it allows a fractional exponent, and `"1e1.5"` matches it but does not parse. The `quantity()` rule used to reject that; with the rule gone, the pattern has to.
   - **Why the exponent is capped at two digits.** A whole-number exponent is not enough on its own, because `maxLength: 32` leaves room for long ones:
     - past int64, `ParseQuantity` refuses it;
     - past int32, it wraps silently to a different value (`1e4294967297` decodes as 10);
     - a large negative one decodes slowly (`1e-9999999` took 1.1 s, and `1e-99999999` ran past a 15 s limit).

     Two digits allow `1e99` and `1e-99`, far beyond any CPU or memory value. Of about a million generated strings that the pattern accepts, every one decodes, in 2.7 s in total. The cap buys a decode that always succeeds, quickly, with no int64 refusal and no int32 wrap. It does not make decoding exact. `ParseQuantity` still rounds a value below `1n` up to `1n` (`1e-99` decodes as `1e-9`), and a plain number past int64 loses digits. Both are general `Quantity` behaviour, which the controller-gen pattern on `main` shares.
   - **How the schema is generated.** controller-gen emits a signed pattern for every `resource.Quantity`, and it cannot merge two patterns. Left alone, it would put both under an `allOf`. The API server then reports every bad value twice, once as the pattern failure and once as an `allOf` failure with no field path. Instead, `+kubebuilder:validation:Schemaless` drops the generated Quantity schema, and `XIntOrString`, `MaxLength` and `Pattern` rebuild it with the one pattern.
   - **Type checking still holds.** `x-kubernetes-int-or-string` alone still limits the value to an integer or a string. Measured on 1.29 and 1.36, floats, booleans and objects are rejected.
   - **Result.** Negative and malformed strings are still rejected at admission on every supported version, with one error each.

4. **The request ≤ limit comparison moves to the Certification controller, which also catches negative integers.** `ValidateCertificationResources` checks the spec-level `resources` and each category's own `resources`. It rejects a negative quantity and a request above its matching limit. The controller runs it next to the existing unknown-category check, before it creates any Workflow, and marks the Certification `Failed` with reason `InvalidResources`. `nvcrectl` runs the same function when it reads a certification file, so `certification render` and `run --cert-file` refuse the file before it reaches a cluster. This is the same pattern as `validateCategoryEnv` (ADR-097).

5. **`categories.maxItems` stays at 64.** With no CEL on the quantity fields, the remaining per-category rules still fit at `maxItems: 1024` on 1.29 and on 1.33. Those are the env, `nicResourceName`, `networkAttachments` and workload label rules. The `networkAttachments` rules are the first to exceed the budget, at 2048, because they sit under a second list.

## Implementation

- `api/v1alpha1/certification_types.go`:
  - drop the four CEL rules on `CategoryResourceList` and `CategoryResources`;
  - give `cpu` and `memory` `Schemaless`, `XIntOrString`, `MaxLength` and the non-negative `Pattern`;
  - rewrite the comments that claimed `MaxLength` and `MaxItems` kept the rules within budget;
  - regenerate with `make manifests generate`. Only the Certification CRD changes, in both the `crds` subchart and the CRDs chart's `files/` (ADR-107).
- `pkg/controller/resources_validation.go`: `ValidateCertificationResources`. Errors name the field path and both quantities, for example `spec.categories[1].options.resources: requests.cpu (256) must not exceed limits.cpu (128)`. It reports at most ten violations and counts the rest. The message becomes the Failed condition's message, which is capped at 32768 bytes, and 64 categories with six violations each would overrun that cap and leave the status unwritable.
- `pkg/controller/certification_controller.go`: call it in `initializeCategoryStatuses`. Add `ReasonInvalidResources` to `helpers.go`.
- `pkg/certification/certification.go`: call it from `readCertification`.
- `test/crdcompat/`:
  - `TestCRDsInstall` creates every CRD in the `crds` subchart in envtest. test/helm's `TestCRDsChartMatchesSubchart` keeps the CRDs chart's `files/` identical to it, so the check also covers the CRDs chart. When `NVCRE_CRD_COMPAT_K8S_VERSION` is set, it also asserts that the server really is that minor, so a misconfigured asset path cannot quietly test the default version eight times.
  - `TestDocumentedFloorIsTested` ties the install guide to `MIN_K8S_VERSION`.
- `Makefile`: `MIN_K8S_VERSION` and `test-crd-compat`, which loops over every minor with `setup-envtest use 1.N`. It fails if either bound is unreadable or the range is empty, so a misconfigured bound cannot pass by testing nothing.
- `.github/workflows/ci.yml`: a `crd-compat` job that runs `make test-crd-compat`.
- Tests:
  - the admission cases in `cmd/integration/testdata/validation/` now pin what admission accepts;
  - a reconcile case pins the controller rejection;
  - render-error cases pin the nvcrectl rejection.

## Rationale

The constraint comes from the API servers people actually run, not from NVCRE. Managed services still offer 1.30–1.33, and certification targets are often the clusters operators are least able to upgrade. Raising the minimum to 1.34 would turn away exactly the clusters that had already reported the failure.

Within that constraint, admission keeps every check a schema keyword can express. The checks it cannot express move to the earliest point that still runs before work starts.

A request above its limit was never accepted silently before #232 either. Pod admission refuses such a container. Before #232, though, the refusal surfaced deep in a TrainJob as pods that never appeared. The controller check fails the whole Certification on its first reconcile, before any category runs. The message names the exact field and both values.

Testing every minor, not only the minimum, follows from the measurements. The cost estimator changed between 1.29 and 1.30 and again in 1.34. Nothing guarantees that a future rule fitting at the minimum also fits in between.

## Consequences

- The Certification CRD installs on Kubernetes 1.29 through 1.36. Releases before the one containing this fix still need 1.34+, and the install guide says so. That includes the `cluster-readiness-engine-crds` chart, which v0.9.0 first published.
- This is the first change to the Certification CRD's schema since the `cluster-readiness-engine-crds` chart was published. On Helm 4, a user of that chart whose CRDs `nvcrectl setup init` or a `kubectl apply --server-side` also applied needs `--force-conflicts` on the upgrade that brings it ([ADR-107](107-crds-chart.md), decision 6).
- `kubectl apply` of a Certification whose request exceeds its limit now succeeds. The Certification then goes to `Failed` with reason `InvalidResources` and a Warning event of the same reason, before any Workflow is created. Since the spec is immutable, the fix is to delete and recreate it, as for an unknown category. nvcrectl users still get the error before anything is submitted.
- An unquoted negative integer such as `cpu: -1` passes admission and fails in the controller the same way. A negative quoted string such as `"-1"` is still refused at admission. Only raw YAML applied with kubectl or Helm can send the integer form: a Go client, nvcrectl included, serializes every `resource.Quantity` as a string.
- The admission message for a negative string is now the API server's pattern message, which shows the regular expression, instead of "cpu must be a non-negative quantity". The field description states the rule in words.
- `MaxLength=32` on the quantity fields now only bounds input size.
- The pattern is stricter than `ParseQuantity` in one way: an exponent of three or more digits, such as `1e100`, is rejected at admission. No CPU or memory value needs one.
- The published schema for `cpu` and `memory` no longer lists `anyOf: [integer, string]`, so tools that read the OpenAPI document, such as `kubectl explain`, show no type for them. The API server enforces the same two types as before, through `x-kubernetes-int-or-string`.
- CI downloads one envtest bundle per minor, eight today, and runs one short envtest per minor. The matrix grows by one minor each time `k8s.io/api` is bumped, until `MIN_K8S_VERSION` is raised.
- A CEL rule added to any CRD later has to pass on 1.29–1.33. One that reads an int-or-string field will fail `test-crd-compat` rather than a user's install.

## Alternatives Considered

**Lower `maxLength` on the quantities.** Rejected. 1.29–1.33 ignore `maxLength` for int-or-string values, as measured.

**Lower `categories.maxItems` until the rules fit.** Rejected. On 1.30–1.33 the comparison rule fits only at seven or fewer. The catalog has eight entries, and categories may repeat with different options.

**Rewrite the rules into a form the estimator prices lower.** Rejected after measurement:
- guarding with `type()` costs the full 3 MiB;
- `quantity(string(int(x)))` for the integer form is estimated as unbounded, more than 100x over budget on every version including 1.34+;
- each cheap form errors on one of the two value types.

**Declare the quantities `type: string` so `maxLength` applies.** Rejected. It breaks every manifest that writes `cpu: 6`, an integer the API has always accepted. 1.30+ still estimates the `string()` conversion as unbounded, so it does not even fit.

**Emit `minimum: 0` for the integer form.** It works: measured on 1.29 and 1.35, the API server enforces `minimum` on int-or-string nodes. controller-gen's `Minimum` marker refuses non-numeric schemas, though, so it would take a post-generation patch step in `make manifests`. Rejected for now, because the controller check has to exist for the comparison anyway, and it covers this case with one more line.

**A ValidatingAdmissionPolicy for the comparison.** Rejected. It is beta and off by default in 1.29. It would add a policy, a binding and RBAC to the chart, for one rule.

**A validating webhook.** Rejected. NVCRE serves no webhooks. Adding one means certificate management and a new availability dependency on the API path, for one rule.

**Raise the documented minimum to 1.34 and add a preflight error.** Rejected because a fix exists that keeps 1.29. It remains the fallback if a future change cannot be expressed without CEL on an int-or-string value.

## Notes

The estimator charges about 315k per evaluation on 1.29 and about 630k on 1.30–1.33 for `quantity(string(self))`. A plausible reading is that 1.30 began charging for the `string()` conversion as well as the parse, but that cause was not verified in source. Only the totals were measured.

## References

- #232 (commit 7c5a67b), which added `CategoryOptions.resources` and the four rules
- `k8s.io/apiserver` `pkg/cel/common/schemas.go`, `SchemaDeclType` and `estimateMaxStringLengthPerRequest`, before and after 1.34
- [Kubernetes: CRD validation rules, resource-use estimation](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#resource-use-by-validation-functions)
- [ADR-097](097-certification-category-env.md), the nvcrectl mirror of a server-side rule
- [ADR-107](107-crds-chart.md), the two charts that ship the CRDs this decision covers
