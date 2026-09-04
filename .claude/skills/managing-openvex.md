---
name: managing-openvex
description: Use when adding, updating, or removing CVE/GHSA suppressions in `.openvex.json`, or when acting on a finding reported by the weekly image vulnerability scan. Triggers on "VEX", "OpenVEX", ".openvex.json", "suppress CVE", "ignore CVE", "vulnerability suppression", or any request to act on the Slack alert from `Vulnerability Scan (images)`.
---

# Managing `.openvex.json`

`.openvex.json` carries per-CVE reachability evidence used to suppress findings in
the `ghcr.io/nvidia/cluster-readiness-engine/manager` image.

## How it is wired

`.github/workflows/vuln-scan-images.yml` passes `vex: .openvex.json` to
`anchore/scan-action`, which forwards it to grype as `--vex`. Grype resolves that
relative path from `GITHUB_WORKSPACE`, which is why the scan job checks the
repository out even though it scans a registry digest.

Three things hold that wiring, because each failure mode is silent:

| Guard | What it prevents |
|---|---|
| `TestVulnScanPassesTheVexDocument` | dropping the `vex:` input — grype stops reading the file and every statement stops applying at once |
| `TestVulnScanChecksOutBeforeScanning` | removing the checkout as "we scan by digest" — same outcome |
| `TestGrypeConfigCarriesNoSuppressions` | a second suppression home appearing in `.grype.yaml` |

`TestVulnScanPassesTheVexDocument` also asserts `config:` stays **unset** on the
scan step: setting it disables grype's auto-detection of `.grype.yaml`.

Two further guards close the gap those cannot reach. `TestScanWaitsForSuppressionValidation`
holds the ordering edge, which produces no output reference and so is invisible to
the workflow-graph checks. And because a YAML assertion can only prove *this*
repository's side of the contract — an unrecognised `with:` key is a GitHub
annotation, never a failure, and dependabot moves the action pin weekly — the
`report` job additionally requires every declared `(advisory, package)` pair — one
per statement per subcomponent — to appear as an applied `vex`-namespace ignore
rule on at least one target. Pairs rather than advisory IDs, because two
statements can scope the same advisory to different packages and an ID-level
comparison would let either one satisfy both. That fails closed if the input stops
being forwarded, if the product or subcomponent PURL is wrong, or if a statement
has rotted.

The `validate-suppressions` job runs the policy tests **before** the scan fans
out, so a statement that has gone stale fails the run rather than being applied
by it.

Not wired, and out of scope here: there is no OpenVEX *attestation* in the release
path. ADR-074's artifact contract has no OpenVEX row and `attest.yml` does not
produce one. This document is consumed at scan time only; it is not published or
signed. Adding that is a separate change to the release path.

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

## One place, and why `.grype.yaml` is not the other one

Grype will happily apply `.grype.yaml` ignore rules and `.openvex.json` statements
in the same run. This repo deliberately uses only the second.

Two suppression homes means the impact analysis for a CVE can be in either file,
so answering "why is this not reported?" means checking both. Worse, they enforce
different things — a `.grype.yaml` rule gets no product-PURL check, no
justification enum and no impact statement — so a suppression would silently get
weaker discipline by being written in the easier place. `.grype.yaml` is three
lines of YAML; a VEX statement makes you show your work. That asymmetry decides
which one people reach for, so the weaker option is removed rather than
discouraged: `TestGrypeConfigCarriesNoSuppressions` fails the build if `ignore:`
is non-empty.

Consolidating gave up two properties the `.grype.yaml` rules had, and both are
restored rather than accepted. **Expiry** became the re-affirmation rule
(invariant 5), since OpenVEX has no expiry field. **Per-package scope** became the
subcomponent requirement (invariant 1) — a VEX product with no subcomponents
matches every package in the image, so without it a statement analysing one
package silences the advisory in all of them.

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

One product entry is enough — the scan only ever reads registry digests, so there
is no second local-build PURL to cover. But the entry must also name the affected
package under `subcomponents`:

```json
"products": [
  {
    "@id": "pkg:oci/manager",
    "identifiers": { "purl": "pkg:oci/manager" },
    "subcomponents": [
      { "@id": "pkg:golang/google.golang.org/grpc@v1.83.0",
        "identifiers": { "purl": "pkg:golang/google.golang.org/grpc@v1.83.0" } }
    ]
  }
]
```

**Without `subcomponents` the statement suppresses the advisory across the whole
image.** go-vex's `Product.Matches` returns true for any component when the list
is empty, so a single advisory that hits two packages — a Go stdlib or
`golang.org/x/*` issue reaching both the vendored copy in the manager binary and
one carried by the base layer — is silenced in both, while the impact statement
analysed one. `checkOpenVEX` rejects a statement that omits it.

If you ever scan a locally built image, derive its PURL by repeating the probe in
"Local reproduction" — do not guess from labels.

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
       | "\(.artifact.name) \(.vulnerability.id) (\(.relatedVulnerabilities|map(.id)|join(",")))"' /tmp/scan-*.json
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

This is the whole record of why a finding is silenced, and it is what anyone
auditing our triage reads. It is public in this repository, though not currently
attested or published to a registry. Cite at least one of:

- A specific `grep` against this repo's source that returns zero hits, pattern shown.
- A specific import path or package that proves the vulnerable API is unused —
  remember the manager is a Kubernetes controller, so a great deal of a transitive
  dependency's surface is genuinely unreachable.
- A Dockerfile or Helm clause establishing a hardening claim (non-root user,
  dropped capabilities, read-only filesystem).
- Upstream advisory text limiting the trigger to a configuration this project does
  not use.

Boilerplate ("not exploitable", "low risk") will be rejected in review.
`TestOpenVEXStatementsAreTriageable` enforces a minimum length, which makes the
thin version fail rather than merely be frowned upon — but length is a proxy, and
a long statement with no evidence in it is still a bad statement.

### 5. Every statement must be re-affirmed within 180 days

OpenVEX has no expiry field. A statement is true when written and then stays in
the file forever — including after the dependency is upgraded past the fix, the
advisory is withdrawn, or the package leaves the image. At that point it either
suppresses nothing and nobody notices, or it keeps suppressing something whose
reachability has changed.

So each statement carries a `timestamp`, and re-affirming one means refreshing
`last_updated` (which wins when both are present) to **the day you re-verified
it**. Past 180 days the build goes red until a human looks at it:

```json
"timestamp": "2026-03-01T00:00:00Z",
"last_updated": "2026-09-04T00:00:00Z"
```

A date in the **future** is rejected outright. `now.Sub(when)` is negative for a
forward-dated stamp, so the age check could never fire on it and the statement
would be exempt permanently — which is why the check is bounded on both sides.
The likely way to hit this is not evasion but an off-by-one-year typo while
refreshing.

Refreshing the date without re-verifying the claim defeats the entire mechanism.
Re-affirm by re-running the reproduction below and confirming the finding is still
present and still unreachable — if it is simply gone, delete the statement.

## Local reproduction (canonical)

The only way to be sure a statement applies is to run what CI runs and watch the
finding move from `.matches[]` to `.ignoredMatches[]`.

Unlike a repo that builds images to scan, this workflow scans **already published
digests**, so there is no build step — scan the exact bytes CI scans.

All **four** scanned targets must be checked, not just the release. The enforced
PURL carries no version or digest, so a statement written against the release
applies to `main-<sha>` as well — and `main` is where the code has moved, so that
is precisely where reachability may differ.

```bash
# 1. Resolve the same digests the workflow resolves -- both targets.
IMAGE=ghcr.io/nvidia/cluster-readiness-engine/manager
TAG=$(gh release list --repo NVIDIA/cluster-readiness-engine \
        --exclude-drafts --exclude-pre-releases --limit 1 --json tagName --jq '.[0].tagName')

# The newest published main image, found the way the resolve job finds it:
# walk back until a main-<sha> tag exists, because the tip often has no image yet.
for sha in $(gh api "repos/NVIDIA/cluster-readiness-engine/commits?sha=main&per_page=15" --jq '.[].sha[0:7]'); do
  crane digest "${IMAGE}:main-${sha}" >/dev/null 2>&1 && MAIN_TAG="main-${sha}" && break
done

# 2. Use the grype the workflow uses. The version lives in GrypeVersion.js at the
#    scan-action SHA pinned in vuln-scan-images.yml -- re-read it, do not trust
#    this line after the pin moves.
#    At pin 27805bf3b4e84b4a5c980df22ed233c00390a439 that is v0.118.0.

# 3. Run from the repo root so .grype.yaml is auto-detected, as it is in CI.
for tag in "${TAG}" "${MAIN_TAG}"; do
  index=$(crane digest "${IMAGE}:${tag}")
  for arch in amd64 arm64; do
    digest=$(crane digest --platform "linux/${arch}" "${IMAGE}@${index}")
    echo "### ${tag} ${arch}"
    grype "${IMAGE}@${digest}" --only-fixed --vex .openvex.json -o json \
      --file "/tmp/scan-${tag}-${arch}.json"

    # 4. Must be empty for the ID you targeted.
    jq '[.matches[] | select(.vulnerability.severity == "High" or .vulnerability.severity == "Critical")
         | {id: .vulnerability.id, pkg: .artifact.name}]' "/tmp/scan-${tag}-${arch}.json"

    # 5. Confirm it landed via the vex namespace, not some other rule -- and on
    #    the package the statement scoped itself to. `pkg` is what makes this
    #    comparable to a declared subcomponent; without it the check is by
    #    advisory alone, which any statement for that advisory would satisfy.
    jq '[.ignoredMatches[]? | select((.appliedIgnoreRules//[]) | any(.namespace=="vex"))
         | {id: .vulnerability.id, pkg: .artifact.purl, rules: .appliedIgnoreRules}]' \
      "/tmp/scan-${tag}-${arch}.json"
  done
done
```

A statement is correct **only** when step 4 returns `[]` for its advisory and step
5 lists the `(advisory, package)` pair it declared — the advisory ID together with
the subcomponent PURL — with `namespace = "vex"`, on every target it applies to.

The pair, not the advisory alone. This is the same identity the `report` job
enforces, and for the same reason: two statements can scope one advisory to
different packages, so confirming by ID would let either one satisfy both. A
statement that applies on one target is not evidence about the others.

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
3. **Author the statement** using invariants 1–5. Invariant 5 is not optional
   polish: a statement carrying no `timestamp` fails `checkOpenVEX` outright, and
   the stale audit's "refresh `timestamp`" step refreshes the *document*
   timestamp, not the statement's.
4. **Reproduce locally** against every target the scan applies it to — the stable
   release and the newest `main-<sha>`, each on both platforms. The enforced PURL
   carries no version or digest, so a statement written against the release also
   applies to `main`, where reachability may differ because the code moved.
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

This can be run against a CI run rather than a local scan. The `vuln-report-*`
artifacts carry a `<target>.raw` file, which is the unfiltered grype document
including `.ignoredMatches[]` — download them and stage them where the commands
below expect to find them:

```bash
# Clear both first. `gh run download` extracts each matched artifact into its own
# subdirectory, so a flat glob finds nothing -- and if a previous local scan left
# /tmp/scan-*.json behind, the audit below would silently read that instead and
# report on a run you are not looking at.
rm -rf /tmp/vex /tmp/scan-*.json

gh run download <run-id> --repo NVIDIA/cluster-readiness-engine --pattern 'vuln-report-*' --dir /tmp/vex

# Recursive: the files land at /tmp/vex/vuln-report-<target>/<target>.raw
find /tmp/vex -name '*.raw' -exec sh -c 'cp "$1" "/tmp/scan-$(basename "${1%.raw}").json"' _ {} \;
ls /tmp/scan-*.json   # expect one per scanned target
```

Compare `(advisory, package)` pairs, not advisory IDs alone — the same identity the
`report` job uses. Comparing IDs would report a statement as applied whenever
*any* statement for that advisory applied, so a statement scoped to a package the
advisory never matched reads as healthy. That is the failure this audit exists to
find.

```bash
# Applied: (advisory, package) pairs actually suppressed via the vex namespace.
# grype reports the package as .artifact.purl, which is what a subcomponent names.
jq -r '[.ignoredMatches[]? | select((.appliedIgnoreRules//[]) | any(.namespace=="vex"))
        | .vulnerability.id + " " + (.artifact.purl // .artifact.name)] | unique[]' \
  /tmp/scan-*.json | sort -u > /tmp/applied.txt

# Declared: one pair per statement per subcomponent.
jq -r '[.statements[] | .vulnerability.name as $v | .products[]? | .subcomponents[]?
        | $v + " " + (.identifiers.purl // .["@id"])] | unique[]' .openvex.json \
  | sort > /tmp/declared.txt

comm -23 /tmp/declared.txt /tmp/applied.txt   # stale candidates
```

Classify each candidate before deleting:

1. **Gone entirely** (`grep <id> /tmp/scan-*.json` → no hits on any target,
   aliases included): the finding no longer exists. **Delete.**
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
- **Writing an `ignore:` rule in `.grype.yaml` instead of a statement here.** Three
  lines, and it skips every check above — which is exactly why
  `TestGrypeConfigCarriesNoSuppressions` refuses it.
- **Using `status: "fixed"` to silence something.** Grype suppresses it exactly
  like `not_affected`, but OpenVEX forbids a `fixed` statement from carrying a
  justification or impact statement, so it hides a finding with no reviewable
  claim on record. Rejected by `checkOpenVEX`.
- **Dating `last_updated` in the future.** The age check cannot fire on a forward
  dated stamp, so it would exempt the statement permanently. Also rejected.
- **A statement with no subcomponents.** It silences the advisory image-wide while
  the impact statement describes one package.
- **Adding a statement without local reproduction.** A statement that fails to
  apply is invisible. There is no warning — the only signal is the CVE reappearing.
- **Boilerplate impact statements.** They are the entire record of why a finding
  is silenced.
- **Forgetting `version` / `timestamp`.** They are how a reader tells which triage
  round a statement belongs to.

## Quick reference

- Workflow: `.github/workflows/vuln-scan-images.yml`
- Suppressions and impact analysis (the only place): `.openvex.json`
- Grype config, no suppressions: `.grype.yaml`
- Statement policy tests: `test/releasepolicy/openvex_test.go`
- Wiring and one-place guards: `test/releasepolicy/grype_suppressions_test.go`
- Resolve-step tests: `test/releasepolicy/vuln_scan_resolve_test.go`
- Image: `ghcr.io/nvidia/cluster-readiness-engine/manager`
- Product PURL: `pkg:oci/manager`
- Targets: newest stable release and newest published `main-<sha>`, each × amd64/arm64
- Grype version: `GrypeVersion.js` at the scan-action SHA pinned in the workflow
- Scan flags in CI: `--only-fixed`, `--fail-on high` (exit code only, not a filter);
  the workflow filters the report to Critical/High when staging it
- Slack line format: `[Severity] <primary-id>  <pkg>@<ver>  fix: <ver>  aka <aliases>`
- Security policy and triage obligations: `SECURITY.md`
