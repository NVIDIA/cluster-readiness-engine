---
name: managing-openvex
description: Use when adding, updating, or removing CVE/GHSA suppressions in `.openvex.json`, or when acting on a finding reported by the weekly image vulnerability scan. Triggers on "VEX", "OpenVEX", ".openvex.json", "suppress CVE", "ignore CVE", "vulnerability suppression", or any request to act on the Slack alert from `Vulnerability Scan (images)`.
---

# Managing `.openvex.json`

`.openvex.json` carries per-CVE reachability evidence used to suppress findings in
the `ghcr.io/nvidia/cluster-readiness-engine/manager` image.

## Status: the file and the wiring do not exist yet

Read this before following any recipe below.

- `.openvex.json` is **not in the repo**.
- `.github/workflows/vuln-scan-images.yml` does **not** pass a `vex:` input, so no
  VEX document is consulted today.
- There is no OpenVEX attestation in the release path. ADR-074's artifact contract
  has no OpenVEX row, and `attest.yml` does not produce one.
- Epic #262 lists OpenVEX triage as a **non-goal for now**, and `.grype.yaml` says
  "Deliberately not OpenVEX yet" — the stated gate is that the `.grype.yaml`
  ruleset gets exercised for a few cycles first.

Adopting VEX therefore means updating those texts in the same change, or the repo
contradicts itself.

The mechanism is available whenever you want it: `anchore/scan-action` at the SHA
this repo pins (`27805bf3b4e84b4a5c980df22ed233c00390a439`, v7.4.2) supports a
`vex:` input and forwards it to grype as `--vex`. Wiring it is two lines in the
scan step:

```yaml
        with:
          image: ${{ env.IMAGE }}@${{ matrix.target.digest }}
          vex: .openvex.json        # <- add
          severity-cutoff: high
          only-fixed: true
```

The checkout step in that job is already load-bearing for `.grype.yaml`
(`TestVulnScanChecksOutBeforeScanning` holds it); `.openvex.json` would ride the
same checkout. Note the action's `config:` input **disables** `.grype.yaml`
auto-detection — do not set it.

## Remediate before you suppress

VEX is for findings that **cannot** be fixed by upgrading. In this repo most
findings are Go module dependencies, so the fix is usually a bump plus a release,
not a statement.

Worked example, the first finding this scan ever produced:

```
[High] GHSA-vp52-pcj8-j9qc  google.golang.org/grpc@v1.83.0  fix: 1.83.1  aka CVE-2026-84304
```

`v0.1.0` shipped grpc 1.83.0; `main` was already on 1.83.1. The correct action was
a patch release, **not** a VEX statement — the fixed version was reachable. Writing
a statement there would have hidden a trivially fixable exposure.

Only reach for `.openvex.json` when the upgrade path is genuinely blocked.

## Relationship to `.grype.yaml` — read this before choosing a mechanism

The two are not interchangeable, and the difference is the reason to be careful.

| | `.grype.yaml` ignore | OpenVEX statement |
|---|---|---|
| Expiry | **Required**, max 180 days, enforced | No concept of expiry |
| Enforcement | `TestGrypeIgnoreRulesAreTriageable`, run in CI **and** by the weekly scan before it scans | None |
| Rots silently | No — a lapsed rule fails the build | **Yes** |
| Published | No | Intended for attestation, read by downstream consumers |

`.grype.yaml` suppressions are time-boxed and come back for re-triage on their own.
VEX statements do not. Moving a suppression from `.grype.yaml` to `.openvex.json`
**loses** that guarantee, which is why the stale audit below is mandatory rather
than advisory — it is the compensating control for the missing expiry.

Prefer `.grype.yaml` for anything temporary ("waiting on a base image bump").
Prefer `.openvex.json` for durable reachability claims you are willing to publish.

## Non-negotiable invariants

Violating any of these causes a **silent** no-op: no warning, no failure, no log
line. The only signal is that the finding keeps appearing.

### 1. `products[].purl` must be `pkg:oci/manager`

Grype derives the OCI product PURL from the registry repository **basename**, not
from the full path and not from `org.opencontainers.image.title`.

Verified against the real image and today's real finding, with grype v0.118.0 —
the exact version this repo's pinned scan-action installs:

| Product PURL | Result |
|---|---|
| `pkg:oci/manager` | suppression applies |
| `pkg:oci/cluster-readiness-engine/manager` | no match |
| `pkg:oci/nvidia/cluster-readiness-engine/manager` | no match |

So a statement targets:

```json
"products": [
  { "@id": "pkg:oci/manager", "identifiers": { "purl": "pkg:oci/manager" } }
]
```

One entry is enough. The scan only ever reads registry digests, so there is no
second local-build PURL to cover. If you ever scan a locally built image, derive
its PURL by repeating the probe in "Local reproduction" — do not guess from labels.

### 2. `vulnerability.name` must be grype's primary ID

Grype emits one primary ID per match. For ecosystem advisories carrying both a
GHSA and a CVE, the primary is usually the **GHSA**; the CVE appears only as an
alias. OpenVEX matches by exact name, so a CVE will not match a GHSA primary even
though they describe the same advisory.

The Slack message prints both, primary first:

```
[High] GHSA-vp52-pcj8-j9qc  google.golang.org/grpc@v1.83.0  fix: 1.83.1  aka CVE-2026-84304
        ^^^^^^^^^^^^^^^^^^ use this                                          ^^^^^^^^^^^^^^ not this
```

Or extract it directly:

```bash
jq -r '.matches[] | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
       | "\(.artifact.name) \(.vulnerability.id) (\(.relatedVulnerabilities|map(.id)|join(",")))"' /tmp/scan.json
```

### 3. Justifications must use the OpenVEX v0.2.0 enum

Allowed values for `not_affected`:

- `component_not_present` — the package is not in the image at all.
- `vulnerable_code_not_present` — the package is present but the vulnerable
  symbol/build is absent.
- `vulnerable_code_not_in_execute_path` — the code exists but the controller never
  invokes it. Most common choice here.
- `vulnerable_code_cannot_be_controlled_by_adversary` — reachable, but inputs are
  not attacker-influenced.
- `inline_mitigations_already_exist` — runtime hardening blocks the trigger.

### 4. `impact_statement` must cite concrete evidence

These are published to a public registry and read by auditors and customers. Cite
at least one of:

- A specific `grep` against this repo's source that returns zero hits, pattern shown.
- A specific import path or package that proves the vulnerable API is unused —
  remember the manager is a Kubernetes controller, so a great deal of a transitive
  dependency's surface is genuinely unreachable.
- A Dockerfile or Helm clause establishing a hardening claim (non-root user,
  dropped capabilities, read-only filesystem).
- Upstream advisory text limiting the trigger to a configuration this project does
  not use.

Boilerplate ("not exploitable", "low risk") will be rejected in review.

## Local reproduction (canonical)

The only way to be sure a statement applies is to run what CI runs and watch the
finding move from `.matches[]` to `.ignoredMatches[]`.

Unlike a repo that builds images to scan, this workflow scans **already published
digests**, so there is no build step — scan the exact bytes CI scans.

```bash
# 1. Resolve the same digests the workflow resolves.
IMAGE=ghcr.io/nvidia/cluster-readiness-engine/manager
TAG=$(gh release list --repo NVIDIA/cluster-readiness-engine \
        --exclude-drafts --exclude-pre-releases --limit 1 --json tagName --jq '.[0].tagName')
DIGEST=$(crane digest --platform linux/amd64 "${IMAGE}@$(crane digest "${IMAGE}:${TAG}")")

# 2. Use the grype the workflow uses. The version lives in GrypeVersion.js at the
#    scan-action SHA pinned in vuln-scan-images.yml -- re-read it, do not trust
#    this line after the pin moves.
#    At pin 27805bf3b4e84b4a5c980df22ed233c00390a439 that is v0.118.0.

# 3. Run from the repo root so .grype.yaml is auto-detected, as it is in CI.
grype "${IMAGE}@${DIGEST}" --only-fixed --vex .openvex.json -o json --file /tmp/scan.json

# 4. Must be empty for the ID you targeted.
jq '[.matches[] | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
     | {id: .vulnerability.id, pkg: .artifact.name}]' /tmp/scan.json

# 5. Confirm it landed via the vex namespace, not some other rule.
jq '[.ignoredMatches[]? | select((.appliedIgnoreRules//[]) | any(.namespace=="vex"))
     | {id: .vulnerability.id, rules: .appliedIgnoreRules}]' /tmp/scan.json
```

A statement is correct **only** when step 4 returns `[]` for its ID and step 5
lists it with `namespace = "vex"`.

Repeat for `linux/arm64` — the workflow scans both platforms, and a statement that
applies to one is not evidence about the other.

Caveat: a local grype DB fresher than the last CI run can surface advisories CI has
not seen. Treat those as incoming findings, not discrepancies.

## Triage a finding from the Slack alert

The weekly scan (Mondays 08:00 UTC) posts fixable High and Critical findings. The
summary lists every target, including clean ones, so you can see coverage rather
than infer it.

For each ID:

1. **Check upstream first.** If a fixed version is reachable, bump the dependency
   in `go.mod`, not `.openvex.json`. For a finding present in a release but already
   fixed on `main`, the action is a patch release. See "Remediate before you
   suppress".
2. **If the upgrade is blocked**, prove non-reachability. Identify the vulnerable
   function upstream, then show this repo does not reach it — transitively, not
   just directly.
3. **Author the statement** using invariants 1–4.
4. **Reproduce locally** on both platforms.
5. **Run the stale audit** below. Every edit includes it.
6. **Dispatch and confirm CI agrees:**
   ```bash
   gh workflow run vuln-scan-images.yml --repo NVIDIA/cluster-readiness-engine \
     --ref main -f notify_slack=true
   gh run watch <id> --repo NVIDIA/cluster-readiness-engine --exit-status
   ```
   Note `notify_slack=true` still only posts when findings remain — a run that your
   statement made completely clean will post nothing. That is expected, and the
   step summary is the confirmation.

## Stale audit (mandatory on every edit)

Statements rot: dependencies get upgraded past fixes, advisories get withdrawn,
packages leave the image. A stale statement applies to nothing, silently. Because
VEX has no expiry to catch this, the audit is the only control.

```bash
# Applied: IDs actually suppressed via the vex namespace
jq -r '[.ignoredMatches[]? | select((.appliedIgnoreRules//[]) | any(.namespace=="vex"))
        | .vulnerability.id] | unique[]' /tmp/scan.json | sort > /tmp/applied.txt

# Declared: every statement in the document
jq -r '.statements[].vulnerability.name' .openvex.json | sort > /tmp/declared.txt

comm -23 /tmp/declared.txt /tmp/applied.txt   # stale candidates
```

Classify each candidate before deleting:

1. **Gone entirely** (`grep <id> /tmp/scan.json` → no hits, aliases included): the
   finding no longer exists. **Delete.**
2. **Present but ignored as `wont-fix`** (in `.ignoredMatches[]` with an empty
   namespace): `--only-fixed` already hides it, so the statement never applies.
   **Delete.** Do not keep it "just in case" — if a fix ships later, the finding
   must surface so it can be bumped rather than pre-suppressed.
3. **Present under a different primary ID** (a CVE statement while grype emits the
   GHSA): **not stale** — a name mismatch. Fix per invariant 2.

Deleting must be count-neutral: re-scan both platforms afterwards and confirm the
remaining suppressions still apply.

Bump the document `version` and refresh `timestamp` in the same edit.

## Anti-patterns

- **Suppressing something an upgrade would fix.** The most likely mistake in this
  repo, because most findings are Go module dependencies with an available fix.
- **Using the CVE when grype emits the GHSA as primary.** Not interchangeable.
- **Using the full image path in the PURL.** It is the basename, `pkg:oci/manager`.
  Verified above; the other two forms silently match nothing.
- **Moving a temporary suppression out of `.grype.yaml` into VEX.** You lose the
  enforced expiry and it never comes back for re-triage.
- **Adding a statement without local reproduction.** A statement that fails to
  apply is invisible. There is no warning — the only signal is the CVE reappearing.
- **Boilerplate impact statements.** They are published.
- **Forgetting `version` / `timestamp`.** Downstream consumers use them to detect drift.

## Quick reference

- Workflow: `.github/workflows/vuln-scan-images.yml`
- Grype config (time-boxed ignores, enforced): `.grype.yaml`
- Suppression policy tests: `test/releasepolicy/grype_suppressions_test.go`
- Resolve-step tests: `test/releasepolicy/vuln_scan_resolve_test.go`
- Image: `ghcr.io/nvidia/cluster-readiness-engine/manager`
- Product PURL: `pkg:oci/manager`
- Targets: newest stable release and newest published `main-<sha>`, each × amd64/arm64
- Grype version: `GrypeVersion.js` at the scan-action SHA pinned in the workflow
- Scan flags in CI: `--only-fixed`, `--fail-on high` (exit code only, not a filter);
  the workflow filters the report to Critical/High when staging it
- Slack line format: `[Severity] <primary-id>  <pkg>@<ver>  fix: <ver>  aka <aliases>`
- Security policy and triage obligations: `SECURITY.md`
