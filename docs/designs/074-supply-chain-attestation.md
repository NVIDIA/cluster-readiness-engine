# ADR-074: Supply Chain Artifact and Verification Contract

> **Status:** Proposed

## Context

NVCRE is installed with cluster-admin-adjacent RBAC on GPU fleets. The Helm chart carries the CRDs and the manager ClusterRole; the controller image runs in that role; `installer` is fetched with `curl … | bash` by the instructions in our own README. A consumer who wants to know *what they are about to run* has almost nothing to check.

One artifact is partially covered. [`publish.yml:90`](../../.github/workflows/publish.yml#L90) signs the controller image with keyless cosign, and [`publish.yml:102-105`](../../.github/workflows/publish.yml#L102-L105) attaches a CycloneDX SBOM. Everything else — the Helm chart pushed at [`release.yml:66`](../../.github/workflows/release.yml#L66), the four `nvcrectl` binaries, and `installer` — ships with nothing but a SHA-256 line in `checksums.txt` ([`release.yml:166`](../../.github/workflows/release.yml#L166)). A checksum served from the same origin as the artifact it describes proves nothing against an adversary who can write to that origin, and `checksums.txt` is itself unsigned.

The image's own coverage has four defects that only become visible once you ask what each artifact actually asserts.

**The SBOM describes the wrong thing.** [`Makefile:256`](../../Makefile#L256) sets `PLATFORMS ?= linux/arm64,linux/amd64`, so every release publishes a multi-platform index. [`publish.yml:96`](../../.github/workflows/publish.yml#L96) points `anchore/sbom-action` at that index digest and produces exactly one document. An SBOM enumerates the contents of one root filesystem; it has no way to describe two. Syft resolves the index to a single platform and emits that platform's packages. The result is published as though it described the image, so an operator scanning the `linux/arm64` image is reading `linux/amd64` package data.

**Nothing asserts origin.** There is no SLSA statement anywhere in the release. A cosign signature proves that a holder of a Fulcio certificate signed these bytes. It does not say which commit they were built from, which workflow built them, or which run. "Signed by someone in this repository" and "built from tag `vX.Y.Z`" are different claims, and only the second one is useful when deciding whether to deploy.

**The verification instructions we publish do not verify what they claim.** [`SECURITY.md:68`](../../SECURITY.md#L68) tells users to pass `--certificate-identity-regexp='https://github.com/NVIDIA/cluster-readiness-engine'`. That pattern is unanchored and names no workflow and no ref. [`publish.yml:8-10`](../../.github/workflows/publish.yml#L8-L10) triggers on pushes to `main` as well as on tags, so every merge to `main` publishes a `main-<sha>` image signed under `…/publish.yml@refs/heads/main` — an identity that regexp accepts. A user following our documentation exactly will accept an unreleased development build as a release. This is worse than publishing no instructions, because it converts an unverified install into a falsely verified one.

**The release asserts a fact it never checked.** [`release.yml:226`](../../.github/workflows/release.yml#L226) states in the release notes that the controller image for the release is `…/manager:<tag>`. `release.yml` and `publish.yml` are independent workflows on the same trigger. The release job never confirms that image exists, never learns its digest, and cannot verify a signature for a digest it does not have. The post-publish gate at [`release.yml:274`](../../.github/workflows/release.yml#L274) checks SHA-256 sums and the binary's self-reported version, and never runs `cosign verify` at all — so a release in which every signing step silently no-ops still publishes green, and the first party to discover it is a user.

Underneath all of this, the signing toolchain is not pinned. [`publish.yml:94`](../../.github/workflows/publish.yml#L94) references `anchore/sbom-action@v0.24.2`, a mutable tag, while every other action in the repository is SHA-pinned. [`publish.yml:86`](../../.github/workflows/publish.yml#L86) pins the `cosign-installer` action but not `cosign-release`, so the cosign version — and with it the attestation bundle format and which Rekor backend entries land in — is whatever default that action currently carries. Both are inputs to claims we ask users to trust, and both can change without a commit to this repository.

This ADR fixes the contract. The workflow changes that implement it are tracked as separate issues under the supply chain epic.

## Decision

### 1. Artifact contract

Every artifact a user can pull or download from a tagged release carries a signature, a provenance statement, and — where the concept applies — an SBOM.

| Artifact | Signature | SBOM | Provenance | Attestation subject |
|---|---|---|---|---|
| `manager` image, per platform | cosign | CycloneDX-JSON, one per platform | — | per-platform manifest digest |
| `manager` image, index | cosign | — | SLSA Build Provenance v1 | index digest |
| Helm chart (OCI) | cosign | — | SLSA Build Provenance v1 | chart OCI digest |
| `nvcrectl-{linux,darwin}-{amd64,arm64}` | cosign `attest-blob` bundle | CycloneDX-JSON, one per binary | SLSA Build Provenance v1 | file digest |
| `installer` | cosign `attest-blob` bundle | — | SLSA Build Provenance v1 | file digest |
| every `*.cyclonedx.json` release asset | cosign `attest-blob` bundle | — | — | file digest |

The last row is not redundancy. The SBOM is the document a security team feeds to a scanner to decide whether they are exposed, which makes it worth tampering with on its own. An unsigned SBOM beside a signed binary is the one unsigned link in the chain.

### 2. Subject policy

**An SBOM is attested to a per-platform manifest digest. Provenance is attested to the index digest.**

The asymmetry is not stylistic. An SBOM makes a claim about the contents of one root filesystem, and a multi-platform index has more than one; attaching two SBOMs to a shared index digest makes them indistinguishable without parsing the predicate body, and attaching one makes it silently wrong for the other platform. Provenance makes a claim about the build that produced the release image as a whole, which is exactly what the index identifies.

Digests are resolved from a source the caller did not supply. For a registry subject that means resolving the tag the caller just pushed, not the digest it asserts: resolving `name@<digest>` would query the registry by the value under test, and content addressing guarantees that returns the same digest or fails, so the comparison could never catch a caller naming a real-but-wrong digest. Per-platform digests are resolved with `crane digest --platform`, and the resolution fails closed in two cases: a platform digest equal to the index digest means the reference is a plain manifest rather than an index (so the release is not multi-platform and something upstream is wrong), and two equal platform digests mean the release lost an architecture. Both are release-blocking, because both would otherwise ship a mislabeled SBOM.

### 3. Identity contract

All release attestations are produced by one reusable workflow, `.github/workflows/attest.yml`. Consumers pin exactly one identity:

```
--certificate-oidc-issuer https://token.actions.githubusercontent.com
--certificate-identity    https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/<TAG>
```

`--certificate-identity`, never `--certificate-identity-regexp`. The exact form names the workflow and the tag, so it rejects a `main` build, rejects a signature from a workflow that is not the attestor, and rejects release `vX`'s signature presented as release `vY`. Every regexp we could publish instead answers a weaker question, and the weakening is invisible to whoever copies the command.

This contract is published to users and enforced by us. Both the release-time gate and the daily re-verification job use the same exact identity, so the command in our documentation is the command our own CI runs.

### 4. Attestation runs from a reusable workflow

`attest.yml` is `workflow_call`-only and becomes the sole place in the repository that invokes `cosign sign`, `cosign attest`, or `cosign attest-blob`. That invariant is established as callers migrate — the image, chart and binary paths move across in their own changes, and a workflow policy test then keeps it true.

The reason is decision 3. cosign uses the OIDC `job_workflow_ref` as the certificate SAN, so signing from one reusable workflow collapses every artifact onto one identity path where only the ref varies. Signing inline in each caller would give the chart, the image, and the binaries three different identities, and a `main` build a fourth that looks just as legitimate. It also isolates the signing step: caller-defined build steps run in a different job from the one holding the signing token.

**This design targets SLSA Build L2, not L3.** The distinction matters and is easy to overclaim. L3 requires the *build* to be isolated from user-defined steps, and GitHub's mechanism for that is moving the build itself into the reusable workflow. Here the builds stay in the callers — `docker buildx` in `publish.yml`, the Go cross-compile and `helm package` in `release.yml` — and `attest.yml` receives a digest and signs it. A caller that produced the wrong artifact would get a faithful signature over the wrong digest. Isolating the signer is worth having, but it is not the isolation L3 asks for.

Two consequences follow. First, no artifact or document may claim L3 — not the ADR, not the release notes, not `SECURITY.md`. Second, the provenance predicate's `runDetails.builder.id` must name the workflow that actually performed the build, not `attest.yml`. Naming the attestor as the builder would make the predicate false on its face, which is worse than claiming the wrong level.

Reaching L3 later means moving image, chart, and binary generation into the protected reusable workflow. That is a larger restructure than this record covers — it rewrites the build path rather than adding to it — and it should be its own decision once this contract is in place and stable. Recorded as deferred, not rejected.

`attest.yml` validates every input before use: digests must match `^sha256:[0-9a-f]{64}$`, no input may contain a newline or carriage return, and the caller's authoritative `expected_digest` is compared against an independently resolved digest with a mismatch failing the job. It refuses to run on a non-tag ref unless an explicit `allow_untagged` input is set, so a test run cannot quietly produce something that looks like a release attestation.

### 5. SBOM format stays CycloneDX-JSON

Image SBOMs keep the CycloneDX-JSON format already emitted at [`publish.yml:97`](../../.github/workflows/publish.yml#L97), and binary SBOMs use the same format. The predicate type stays `cyclonedx`.

The alternative formats are interchangeable on every axis a consumer cares about — cosign treats them as equal predicate types, both are formally standardized, and Grype, Trivy, and Syft ingest either. That leaves switching cost and forward fit as the deciding factors, and both favor staying.

CycloneDX carries vulnerability data natively: a `vulnerabilities` array, and VEX statements that can be embedded in or linked from the same document. Weekly vulnerability scanning is part of this epic and structured suppression is the intended follow-on (see Notes), so a format that can eventually carry the SBOM and its VEX in one signed predicate is the better foundation than one that forces a second artifact.

Changing format would also break anyone parsing the predicate body today, in exchange for no capability the consumer tooling does not already have.

Format is one flag on `cosign attest`. Nothing else in this contract depends on it — the subject policy, the identity contract, the reusable workflow, and both verification gates are format-independent.

### 6. Verify before publishing, and keep verifying

Producing attestations without verifying them means the first party to discover they are broken is a user. Two gates close that.

A **release-time gate** runs after publication and before the release leaves draft, verifying every row of the artifact contract over the same channel a user takes — unauthenticated download for public assets, with the authenticated fallback already implemented at [`release.yml:280-294`](../../.github/workflows/release.yml#L280-L294) while the repository is private. It checks presence against a **static expected-asset list** held in the workflow, not against the release's own inventory: expectations derived from the artifact under test would make deleting one asset produce a clean run over the survivors.

For image and chart subjects the gate reads the **registry** copy of the attestation, not the GitHub Attestations API copy, and includes a round-trip check that fails when an API attestation exists but its registry referrer does not. See Transport for why the two can diverge.

Verifying the signature is not enough on its own. A valid signature under the pinned identity proves who signed and on which ref, but says nothing about what the predicate *contains* — so the gate must also read the provenance body and assert that `externalParameters.repository` is this repository, that `externalParameters.ref` is `refs/tags/<TAG>`, and that `resolvedDependencies[].digest.gitCommit` is the commit the released tag actually points at. Without that last check, an artifact built from a different commit under a tag that was later moved still verifies: the certificate names the right tag, and nothing compares the tag's current target against what the predicate recorded. Identity answers "who signed this"; the predicate body answers "what was built", and the release must confirm both agree.

A **daily re-verification job** re-runs the same suite against the latest release, because the release-time gate proves correctness at publication and says nothing about ten minutes later. Assets can be deleted or replaced, and registry tags can be moved, without any commit to this repository. That job must distinguish tampering from a Sigstore outage: it probes both the TUF CDN and Fulcio for liveness before classifying anything, and classification is **demote-only**, so an ambiguous case is reported as operational rather than suppressed — the failure mode is a real finding filed under the wrong severity, never a real finding that goes unfiled.

## Implementation

### Workflow topology

```
publish.yml (tag)  ──┐
                     ├──> attest.yml (workflow_call, isolated signer)
release.yml (tag)  ──┘         │
                               ├─ image index      -> provenance
                               ├─ image amd64/arm64 -> CycloneDX SBOM (each)
                               ├─ chart OCI digest  -> signature + provenance
                               └─ binaries, installer, *.cyclonedx.json -> attest-blob bundles
```

`release.yml` becomes authoritative for digests. It either consumes the image publish as a dependency or resolves and asserts the published digest before creating the release, and it carries the index digest, both platform digests, and the chart digest into the release notes so the notes reference immutable artifacts.

### Transport

Registry-attached attestations for the image and chart; detached `.sigstore.json` bundles as loose release assets for the binaries, the installer, and the SBOM documents.

The split is deliberate. Registry attestations are the natural home for something already addressed by digest, and `cosign attest` writes them to the registry alongside the subject. A loose sidecar is the right shape for a downloaded file: a user can `curl` the binary and its bundle and verify with cosign alone — no GitHub API, no registry credentials, no network beyond the two downloads and the trust root. That is what makes air-gapped verification work.

**Provenance is produced by a predicate this repository writes, signed with `cosign attest` / `cosign attest-blob` — not by `actions/attest-build-provenance`.** That action sets `runDetails.builder.id` to the workflow generating the attestation, which here is `attest.yml`, the attestor rather than the builder. Since decision 4 requires `builder.id` to name the workflow that actually built the artifact, and since detached blob bundles need a hand-written predicate regardless, using the action for images and a hand-written predicate for blobs would make `builder.id` mean different things depending on artifact type. One mechanism for all three subject kinds is worth more than the action's convenience. The consequence is that no copy is written to the GitHub Attestations API — which the next paragraph explains we did not want to be authoritative anyway.

**Registry subjects are verified against the registry copy.** There is only one copy to verify: `cosign attest` writes to the registry and nowhere else, so `cosign` — which is registry-native — is the whole verification story, and `gh attestation verify` is not part of the contract at all.

This is the second reason to prefer it over `actions/attest-build-provenance`. That action's `push-to-registry: true` writes *two* copies, one to GitHub's attestation store and one to the registry, and they can diverge: a registry write can fail, and a GHCR referrer can be deleted afterwards, neither of which touches the API copy. `gh attestation verify` reads the API copy by default, so a gate using its bare form would pass on an image whose registry attestation is gone while a consumer running `cosign verify-attestation` against that same image gets nothing. Having one copy removes the divergence rather than documenting how to work around it. This is the same "verify over the channel the user takes" rule decision 6 applies to downloaded assets; the registry is that channel for images and charts.

Note that ghcr.io does not implement the OCI 1.1 `/v2/<name>/referrers/<digest>` endpoint. Registry attestations land through the specification's referrers **tag fallback** (`sha256-<hex>`). This is transparent to cosign and oras clients but is why published retrieval commands use cosign rather than a raw referrers API call.

### Toolchain pinning

Every tool that contributes to a signature is pinned: `anchore/sbom-action` by commit SHA, `cosign-release` to an explicit version, and `govulncheck` to a version rather than `@latest`.

Bundle format is pinned by the cosign version, for both attestation shapes. An earlier draft of this record specified passing `--new-bundle-format=true` explicitly on `cosign sign` and `cosign attest` so the layout could not move with a cosign default. That was wrong on the facts: the flag already defaults to `true` in both cosign v3.0.6 (the `cosign-installer` v4.1.2 default) and the pinned v3.1.3, and v3.1.3 marks it deprecated — "this will be the only supported format in future versions". Setting it changes no behaviour, emits a deprecation warning on every release run, and would fail the release once cosign removes the flag. `cosign attest-blob` never had the flag at all.

So the version pin carries the guarantee for registry and detached bundles alike, which is a further reason `cosign-release` must be explicit rather than inherited.

An earlier version of this section claimed verification closes that loop — that "the release gate and the daily job reject a legacy-format bundle rather than falling back to reading it." Neither half was true. No rejection logic exists anywhere in the release path, and the daily job is still unbuilt (#273). The pin is therefore the only thing holding the format today: a cosign change that altered the emitted shape would publish a format our documented commands do not describe, and nothing would fail. Rejecting a legacy bundle at verification time remains open on #270.

Every `cosign` invocation is wrapped in a bounded retry (three attempts, 5s and 10s backoff, no sleep after the final failure) with a per-call timeout. Rekor is a real network dependency with real transient failures, and the cosign CLI exposes no native retry flag.

### Regression protection

The contract lives in YAML, and the failure mode is silent: a signing step deleted in a refactor does not break the build, it just stops signing. Go tests parse the release-path workflows and assert the invariants directly — that `attest.yml` is the only signer, that no action resolves by mutable tag, that no `cosign verify*` in the release path uses `--certificate-identity-regexp`, and that the expected-asset list matches what the release actually uploads. A PR that removes a signing step fails `make test` rather than surfacing at release time.

## Rationale

- **Exact identity over regexp** is the single highest-value decision here. Every other gap is a missing artifact, which is visibly missing. A too-permissive verification command is an artifact that appears present and correct while asserting less than the reader believes.
- **Reusable workflow** improves the security property (signing isolated from caller-defined build steps) and simplifies the consumer contract at the same time. Those usually trade against each other. It does not by itself reach Build L3 — see decision 4.
- **Per-platform SBOM subjects** follow from what an SBOM is. Getting this wrong is not a policy choice, it is a category error, and it is already shipping.
- **Verify what we produce** costs one job and converts a class of silent failure into a red release. Attestations nobody checks are decoration.
- **Signing the SBOMs** closes the gap that remains after everything else is signed, at the cost of a few more bundles.

## Consequences

### Positive

- Every released artifact answers "who built this, from what source, containing what," with one command and one pinned identity.
- The multi-platform SBOM defect is fixed, and the fail-closed digest checks prevent it from recurring silently.
- Provenance is SLSA Build L2 with a single pinnable builder identity, which is a real improvement over no provenance at all. L3 remains available as a follow-on and is not foreclosed by anything here.
- Admission controllers can enforce the same contract the documentation publishes, so install-time and runtime checks cannot drift.
- Post-publication tampering has a bounded detection window instead of depending on a user noticing.

### Negative

- The release path gains a hard dependency on Sigstore availability. Retries and timeouts bound the damage, but a sustained Fulcio or Rekor outage will block a release. This is accepted: a release that cannot be attested should not ship.
- More moving parts in CI, and a reusable-workflow indirection that is one more hop to read when debugging a release.
- The daily re-verification job will occasionally file operational issues during upstream incidents. That is the deliberate direction of the demote-only rule.
- Pinned tool versions need deliberate bumps; Renovate/Dependabot coverage must include them or they rot.

### Neutral

- `checksums.txt` stays. It is cheap, `installer` already consumes it, and it works with no tooling installed.
- `installer` **fails closed** when it cannot verify a signature. An earlier draft of this record had it fall back to checksum-only with a printed notice, which contradicts this record's own Context: `checksums.txt` is served from the same origin as the artifact and is itself unsigned, so an adversary who can replace the binary can replace its checksum line in the same write. A silent downgrade to that check is not a weaker guarantee, it is no guarantee, and the printed notice puts the decision in front of a user who is watching a pipe scroll past.

  The bare-machine path is preserved two ways. `installer` bootstraps `cosign` when it is absent and the platform allows it, verifying the downloaded cosign against a pinned digest embedded in the installer. Where that is not possible, installation stops with instructions, and an explicit `--skip-verify` flag — never a default, never inferred from a missing binary — lets an operator proceed knowingly. Verification is skipped only when someone typed the words asking to skip it.

- **`installer`'s own bytes cannot be authenticated by `installer`.** The `curl … | bash` path executes the script before anything has checked it, so an adversary who can replace that asset can equally delete the verification logic inside it, pinned digest and all. The bullet above hardens what the installer *installs*; it does not and cannot harden the installer. Treating it as if it did would be the same category of error as the `checksums.txt` fallback it replaced.

  The trust anchor therefore has to sit outside the script. `installer` is in the artifact contract and ships with its own `.sigstore.json`, so the **documented primary path is: download, verify the bundle with a cosign the operator already trusts, then execute.** `curl … | bash` remains available as a convenience and the docs state plainly what it does and does not get you — integrity against a network attacker via TLS, nothing against a compromised release asset. Presenting an unauthenticated pipe as a verified install would recreate, at the outermost layer, precisely the false-assurance problem this record exists to remove.

## Alternatives Considered

1. **Migrate the release to GoReleaser.** GoReleaser has first-class SBOM, signing, and archive support, and would replace hand-rolled cross-compilation with configuration. Rejected for now: [`release.yml`](../../.github/workflows/release.yml) carries specific guards earned from issues #194 and #195 — explicit tag passing instead of `git describe`, a self-reported version check against the tag, and an unauthenticated post-publish download gate. A GoReleaser migration rewrites all of that at the same time as introducing attestation, and a failure would be hard to attribute to one or the other. The two changes are separable and should stay separate. Revisit once the contract in this ADR is stable.

2. **Keep signing inline in each workflow.** Simpler to read, no `workflow_call` indirection. Rejected: it produces a different certificate identity per calling workflow and per ref — the root of the `main`-build-looks-like-a-release problem in `SECURITY.md`. Consumers would have to pin several identities or fall back to a regexp, which is the failure this ADR exists to remove. It also leaves the signing token in the same job as caller-defined build steps.

3. **Private Sigstore or KMS-backed signing.** Full control over the trust root, no dependency on public-good Sigstore availability, and works in environments that cannot reach `fulcio.sigstore.dev`. Rejected: it requires key custody, rotation, and a distribution story for the trust root before a single artifact is signed, and it makes verification harder for the public consumers who are the primary audience. Keyless public-good Sigstore has no key material to protect and needs no infrastructure from us. Revisit if a consumer requires an air-gapped trust root that a cached public root cannot satisfy.

4. **An `nvcrectl verify` subcommand.** A single command wrapping the whole contract would be a better user experience than several cosign invocations. Rejected: it is verification surface we would have to keep correct, and a bug in it fails open by definition — a verifier that wrongly reports success is worse than no verifier. `cosign` and `gh` are maintained by the projects that define these formats. Reconsider only if the documented commands prove too error-prone in practice, and then as a thin wrapper that shells out rather than a reimplementation.

5. **Switch the SBOM format to SPDX-JSON**, matching the other NVIDIA release pipeline in this org so verification instructions and reviewer familiarity are shared. Rejected: the consistency is worth one flag value on `cosign attest` and nothing else, against a breaking change for consumers parsing the predicate today and the loss of native VEX carriage. Arguments that it is the more standard or better-supported choice do not hold — `cyclonedx` and `spdxjson` are both first-class cosign predicate types, and both formats are formally standardized. Revisit only if a consumer requires SPDX by name, such as a procurement process citing ISO/IEC 5962, in which case emitting both formats costs less than switching.

6. **Diff-based vulnerability and verification reporting** (report only what changed since the last run). Less noise per run. Rejected for the assurance jobs: a finding that stops being reported because it is no longer new is a finding nobody fixed. Full-set reporting every run, with explicit expiry-dated suppressions as the only way to silence something.

## Notes

- `CLAUDE.md` and `AGENTS.md` describe the design record range as ADR-000 through ADR-069. The actual range is ADR-000 through ADR-073, which is why this record is 074 rather than the 070 named in the epic. Both files should be corrected in a separate change.
- ADRs are indexed in [`docs/designs/README.md`](README.md), not in the Fern navigation — `docs/index.yml` has no designs section. New records go in that README table.
- Vulnerability triage is deliberately out of scope for this record. Structured suppression is the right long-term answer for findings that do not apply, but it needs a triage process to exist first. Expiry-dated `.grype.yaml` ignore rules start that process. Once the ruleset has been exercised, there are two ways to publish it, and decision 5 keeps both open: a separate signed OpenVEX attestation, or CycloneDX's own VEX carriage inside the SBOM predicate we already sign. Prefer the latter if it holds up — it is one fewer artifact to attest, distribute, and keep in sync with the SBOM it qualifies.

  **Superseded 2026-09-04.** The expiry-dated `.grype.yaml` ignore rules described above no longer exist. Suppressions moved to `.openvex.json`, consumed by the weekly scan as grype's `--vex`, with `.grype.yaml` restricted to scanner configuration and enforced empty of suppressions. OpenVEX carries no expiry field, so the deadline became a re-affirmation rule — a statement stale beyond 180 days fails the build, and the weekly scan runs that check before it scans. The choice in decision 5 is still open: this consumes VEX at scan time and publishes nothing, so the artifact contract above still has no OpenVEX row. See [`.claude/skills/managing-openvex.md`](../../.claude/skills/managing-openvex.md).

## References

- Epic and subtask breakdown: issue #262
- [SLSA v1.0 Build Levels](https://slsa.dev/spec/v1.0/levels)
- [Using artifact attestations and reusable workflows to achieve SLSA v1 Build Level 3](https://docs.github.com/actions/security-guides/using-artifact-attestations-and-reusable-workflows-to-achieve-slsa-v1-build-level-3)
- [Sigstore cosign](https://docs.sigstore.dev/cosign/overview/)
- [CycloneDX ECMA-424](https://ecma-international.org/publications-and-standards/standards/ecma-424/)
- [CycloneDX VEX](https://cyclonedx.org/capabilities/vex/)
- [OCI Distribution Specification — Referrers API and tag fallback](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers)
- ADR-064: Helm Chart Distribution — the chart publishing path this record adds signing to
