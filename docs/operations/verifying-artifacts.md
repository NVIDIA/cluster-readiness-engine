<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Verifying release artifacts

Every artifact a NVCRE release publishes is signed, and each one carries a statement
saying which commit, workflow and tag produced it. This page is how you check that
yourself.

You do not have to trust this page, the release notes, or the installer. Everything below
can be run from a machine with only `cosign` on it, against artifacts fetched over their
public URLs.

## Install the CLI, verified

Start here. The README shows `curl … | bash` because it is short, and it is honest about
what it gives you: TLS integrity in transit, and nothing at all about whether the script
on the other end is the one we published. The script runs before anything has checked it.

Set `TAG` to the release you actually have. These commands verify the artifact `TAG`
names and nothing else — pointed at a different release they will happily report success
while telling you nothing about the file on your disk. `releases/latest` resolves to the
newest *stable* release, so it is not `v0.2.0-rc.1`.

To know what you are about to run, check it first:

```bash
TAG=v0.2.0-rc.1
BASE="https://github.com/NVIDIA/cluster-readiness-engine/releases/download/${TAG}"
ID="https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/${TAG}"
ISSUER='https://token.actions.githubusercontent.com'

curl -fsSLO "${BASE}/installer"
curl -fsSLO "${BASE}/installer.sigstore.json"

cosign verify-blob-attestation \
  --bundle installer.sigstore.json \
  --type https://slsa.dev/provenance/v1 \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  installer

bash installer -v "${TAG}"
```

The trust anchor is the `cosign` you already have, not anything inside the script. That
is the point: `installer` verifies what it *installs*, but it cannot verify itself, and
neither can any other single-file installer.

Run the two commands back to back. Verifying a file and then executing it leaves a window
in which something with write access could swap it — small, and it requires a compromise
that is already worse than this, but it is a window and not a proof.

The installer also verifies the binary *it* downloads, refusing to install one whose
bundle it cannot check, with `--skip-verify` as the only override. That landed after
`v0.2.0-rc.1` was cut, so the installer published with the release pinned above does not
yet do it — it checks only `checksums.txt`. Verify `installer` yourself, as above, until
a release carries the newer one.

## Prerequisites

| Tool | Used for | Version this page was checked with |
|---|---|---|
| [`cosign`](https://docs.sigstore.dev/cosign/installation/) | every verification below | `v3.1.3` |
| [`crane`](https://github.com/google/go-containerregistry/tree/main/cmd/crane) | resolving per-platform image digests | `v0.20.6` (the version the release pipeline pins) |
| `jq` | reading provenance and SBOM predicates | any |
| `helm` | pulling the chart by digest | `v3.x` |

`cosign` is the only one that is strictly required. Pin the version you use: the bundle
format is governed by the cosign version that produced it, and the release pipeline pins
`v3.1.3` for exactly that reason.

## The identity contract

Every attestation in a NVCRE release is produced by one reusable workflow, so there is
exactly one identity to pin:

```
--certificate-identity   https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/<TAG>
--certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Use `--certificate-identity`. Do not use `--certificate-identity-regexp`.

This is not style. An identity that names no workflow and no ref also accepts builds from
branches, and this project publishes `main-<sha>` development images. A pattern like
`https://github.com/NVIDIA/cluster-readiness-engine` accepts those too — so a command
built on it reports success for an artifact that was never released. The exact form names
the workflow **and** the tag, so a signature from `v0.1.0` cannot pass as `v0.2.0`, and a
branch build cannot pass as either.

## Verifying the container image

The image is a multi-platform index. Two different things are attested, to two different
subjects, and the distinction matters:

| Subject | Carries |
|---|---|
| the **index** digest | signature + SLSA Build Provenance v1 |
| each **per-platform** manifest digest | signature + a CycloneDX SBOM for that platform |

An SBOM describes the contents of one root filesystem, and an index has more than one. A
single SBOM attached to the index would be silently wrong for whichever platform it did
not describe, so each platform gets its own.

Signature and provenance, against the tag:

```bash
TAG=v0.2.0-rc.1
IMAGE=ghcr.io/nvidia/cluster-readiness-engine/manager
ID="https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/${TAG}"
ISSUER='https://token.actions.githubusercontent.com'

cosign verify \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${IMAGE}:${TAG}"

cosign verify-attestation --type slsaprovenance1 \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${IMAGE}:${TAG}"
```

The SBOM lives on the platform manifest, so resolve that digest first rather than reusing
the tag:

```bash
AMD64="$(crane digest --platform linux/amd64 "${IMAGE}:${TAG}")"
echo "linux/amd64 -> ${AMD64}"

cosign verify-attestation --type cyclonedx \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${IMAGE}@${AMD64}"
```

## What the provenance actually proves

A signature says who signed and under which ref. Only the provenance predicate says what
was built. Read it:

```bash
set -o pipefail
STATEMENT="$(cosign verify-attestation --type slsaprovenance1 \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${IMAGE}:${TAG}" 2>/dev/null | jq -r '.payload' | head -1 | base64 -d)"

# An empty STATEMENT means verification failed, not that there is nothing to say.
# Without this the jq below prints nothing and exits 0.
[[ -n "${STATEMENT}" ]] || { echo "provenance verification failed" >&2; exit 1; }

jq -r '{
  repository: .predicate.buildDefinition.externalParameters.repository,
  ref:        .predicate.buildDefinition.externalParameters.ref,
  commit:     .predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit,
  builder:    .predicate.runDetails.builder.id
}' <<<"${STATEMENT}"
```

| Field | What it proves |
|---|---|
| `externalParameters.repository` | which repository the build ran in |
| `externalParameters.ref` | the exact ref — `refs/tags/<TAG>` for a release |
| `resolvedDependencies[0].digest.gitCommit` | the source commit, which you can compare against the tag |
| `runDetails.builder.id` | the workflow that performed the build |

Confirm the commit is the one the tag points at:

```bash
git ls-remote --exit-code https://github.com/NVIDIA/cluster-readiness-engine "refs/tags/${TAG}^{}"
```

The `^{}` is required and easy to miss. Release tags here are signed, so
`refs/tags/<TAG>` resolves to the *tag object*, not the commit — comparing that against
the provenance would mismatch on every correct release. `^{}` peels it to the commit the
provenance actually names. `--exit-code` makes a tag that does not exist fail rather than
print nothing and succeed.

`head -1` above is deliberate, and it is also why the `git ls-remote` check is **not
optional**. A retried signing attempt can leave more than one valid attestation on a
digest — expected, and not tampering — so `.payload` can return several lines, and feeding
all of them to `base64 -d` produces nonsense. Taking the first is what makes the command
runnable; comparing the commit it reports against the tag is what makes taking the first
safe. Do both, or loop over every line and require each statement to agree before
trusting any of them.

## Verifying the Helm chart before installing it

Verify the digest, then install *that digest* — not the tag you verified a moment ago:

```bash
CHART=ghcr.io/nvidia/cluster-readiness-engine
DIGEST="$(crane digest "${CHART}:${TAG}")"

cosign verify \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${CHART}@${DIGEST}"

cosign verify-attestation --type slsaprovenance1 \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${CHART}@${DIGEST}"

helm pull "oci://${CHART}@${DIGEST}"
```

Pulling by digest is what makes the download match what you verified: content addressing
means you receive those exact bytes or an error.

Then install **the file you pulled**, not the tag:

```bash
IMAGE_DIGEST="$(crane digest "${IMAGE}:${TAG}")"

helm install nvcre ./cluster-readiness-engine*.tgz \
  --namespace nvcre --create-namespace \
  --set manager.image.digest="${IMAGE_DIGEST}"
```

Two things there are doing work.

Installing from the local `.tgz` rather than `oci://…/cluster-readiness-engine --version
<tag>` matters because the second form re-resolves the tag at install time, which throws
away everything you just checked.

`manager.image.digest` matters for the same reason one level down. The chart otherwise
deploys `manager:<tag>`, and a tag can be repointed after you verified it. Setting the
digest pins the image to the bytes whose signature you checked. Leave it unset and you
get the tag, which is fine for a development cluster and not what you did this work for.

## Verifying a downloaded binary and its SBOM

Each `nvcrectl-*` binary ships **three** bundles whose names differ by one segment, and
they prove different things:

| Bundle | Verify against | Proves |
|---|---|---|
| `<binary>.sigstore.json` | the **binary** | the binary came from this release |
| `<binary>.cyclonedx.sigstore.json` | the **binary** | this SBOM describes that binary |
| `<binary>.cyclonedx.json.sigstore.json` | the **SBOM file** | the SBOM document is unaltered |

The middle one is the one people miss. It binds the SBOM to the binary, so a real SBOM
for a *different* build cannot be presented alongside this one.

```bash
ASSET=nvcrectl-linux-amd64
curl -fsSLO "${BASE}/${ASSET}"
curl -fsSLO "${BASE}/${ASSET}.sigstore.json"
curl -fsSLO "${BASE}/${ASSET}.cyclonedx.json"
curl -fsSLO "${BASE}/${ASSET}.cyclonedx.sigstore.json"
curl -fsSLO "${BASE}/${ASSET}.cyclonedx.json.sigstore.json"

# 1. the binary is from this release
cosign verify-blob-attestation \
  --bundle "${ASSET}.sigstore.json" \
  --type https://slsa.dev/provenance/v1 \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${ASSET}"

# 2. the SBOM beside it describes that binary -- note the subject is the BINARY
cosign verify-blob-attestation \
  --bundle "${ASSET}.cyclonedx.sigstore.json" \
  --type cyclonedx \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${ASSET}"

# 3. the SBOM FILE on your disk has not been altered -- subject is the FILE
cosign verify-blob-attestation \
  --bundle "${ASSET}.cyclonedx.json.sigstore.json" \
  --type https://slsa.dev/provenance/v1 \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${ASSET}.cyclonedx.json"
```

Step 3 is the one that is easy to skip and the one that protects the file you are about
to read. Steps 1 and 2 both take the **binary** as their subject, so neither of them looks
at `${ASSET}.cyclonedx.json` at all: replace that file with a doctored SBOM and both still
report `Verified OK`. Only step 3 fails, with `provided artifact digests do not match
digests in statement`.

### Reading the SBOM

Read it only after step 3 above. The SBOM is published as a plain asset, so no extraction
is needed for binaries:

```bash
jq -r '.metadata.component.name, (.components | length)' "${ASSET}.cyclonedx.json"
```

For the **image**, the SBOM is inside an attestation and has to be extracted. Write it
through a temporary file:

```bash
set -o pipefail
tmp="$(mktemp)"
cosign verify-attestation --type cyclonedx \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${IMAGE}@${AMD64}" 2>/dev/null \
  | jq -r '.payload' | head -1 | base64 -d | jq '.predicate' > "${tmp}"
mv "${tmp}" sbom-linux-amd64.json
jq -r '.components | length' sbom-linux-amd64.json
```

Do not write straight to the final path. `>` truncates the target before the pipeline
runs and `jq` exits 0 on empty input, so a failed verification leaves a zero-length file
and a zero exit status — a broken result that reads as success.

The `mv` is a separate statement on purpose. Chaining it with `&&` would put the pipeline
on the left of an `&&`, where `set -e` does not apply, so the failure would not abort
there either. Written as two statements, `set -euo pipefail` aborts on the pipeline
itself.

## Air-gapped verification

`cosign verify` needs the Sigstore trust root. Copying `~/.sigstore` across is **not
enough**: cosign v3.1.3 attempts a TUF refresh on every verification and fails with
`tuf refresh failed: ... connection refused` rather than falling back to that cache. It
fails closed, which is the right direction, but it does not verify.

Export the trust root as a file instead, on a machine with network:

```bash
cosign initialize
cp ~/.sigstore/root/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json .
```

Move `trusted_root.json` across with the artifacts, and pass it explicitly. That path
takes no network at all:

```bash
cosign verify-blob-attestation \
  --trusted-root ./trusted_root.json \
  --bundle "${ASSET}.sigstore.json" \
  --type https://slsa.dev/provenance/v1 \
  --certificate-identity "${ID}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${ASSET}"
```

`--trusted-root` works on every `cosign verify*` command. The bundle already carries the
signature, the certificate and the inclusion proof, so with the trust root on disk there
is nothing left to fetch.

Refresh `trusted_root.json` periodically. It pins the Sigstore keys, and a stale copy
will eventually reject signatures made with newer ones.

Blob bundles are the easy case: an asset and its `.sigstore.json` are two files, and
`cosign verify-blob-attestation` reads both from disk. Registry attestations need the
registry, so mirror the image and chart with `crane copy` before disconnecting.

Mirroring the bytes is half the job. The cluster still has to pull from the mirror, so
point the chart at it — see [Air-gapped and disconnected
environments](./deployment.md#air-gapped-and-disconnected-environments) for the
`manager.image.repository` override. Verifying artifacts and then deploying a reference
the cluster cannot reach leaves you with a correct signature and a pod that never starts.

## Enforcing this automatically

Everything above is something a person runs. Tools that sync from the registry — Flux
`OCIRepository`, Argo CD, Argo CD Image Updater — do not wait for the GitHub Release, so
the release gate does not protect them; they see the image and chart as soon as those are
pushed. They are the consumers who most need the identity contract enforced
declaratively rather than typed.

Flux supports this natively with [`spec.verify`](https://fluxcd.io/flux/components/source/ocirepositories/#verification)
using the `cosign` provider, matching the same OIDC issuer and identity used above.
Whether that path works end to end against a real Flux version is still being confirmed
(issue #267), so this page does not yet publish a manifest for it — an untested
verification config is exactly the kind of false assurance the rest of this page exists to
avoid.

### Admission-policy samples

Samples under [`config/samples/policy/`](https://github.com/NVIDIA/cluster-readiness-engine/tree/main/config/samples/policy)
pin the same issuer and `attest.yml@refs/tags/<TAG>` identity as the commands above, and
they expect the **new-bundle / OCI referrer** layout ADR-074 publishes (not a legacy
`sha256-<digest>.sig` tag).

| Verifier | Reads our format? | Sample |
|---|---|---|
| `cosign verify` / `verify-attestation` (v3.1.3) | Yes | this page |
| Kyverno `ImageValidatingPolicy` (v1.19+) | Yes — referrer / bundle path | `config/samples/policy/kyverno-verify-images.yaml` |
| Kyverno `ClusterPolicy` / `verifyImages` | **No** — looks for legacy `.sig` tags | do not use |
| Sigstore policy-controller with `signatureFormat: bundle` | Schema/format yes; **this sample shape unconfirmed** end-to-end (issue #272 asked for a re-test before ruling it in) | `config/samples/policy/policy-controller-verify-images.yaml` |
| Sigstore policy-controller at chart default (legacy format) | **No** against current release digests | set `signatureFormat: bundle` |
| Flux `OCIRepository` `spec.verify.provider: cosign` | Unconfirmed (issue #267) | — |

Both samples require a signature **and** SLSA Build Provenance v1 on the image **index**
digest, matching the two `cosign` commands in [Verifying the container image](#verifying-the-container-image).
A per-platform child manifest is signed and carries a CycloneDX SBOM but **not**
provenance (`emit_provenance: false` on those legs); pin the index digest in Helm with
`manager.image.digest`, as above.

#### Apply runbook

1. **Edit the pinned tag** in the sample so `subject:` names the release you run. Nothing
   in CI rewrites that string when a new tag ships. Applying a pin that does not match the
   image already in the cluster will deny the next upgrade, `rollout restart`, node drain,
   or crash-loop replacement until you edit the policy.
2. **If you mirrored the controller image** off `ghcr.io` (see
   [deployment.md](./deployment.md#air-gapped-and-disconnected-environments)), rewrite every
   image glob in the sample (`matchImageReferences` for Kyverno, `images[].glob` for
   policy-controller) to your mirror repository path before applying. Kyverno fails *open*
   when no glob matches (`images.*` becomes `[]` and `[].all(...)` is true); policy-controller
   chart-default `no-match-policy: deny` fails *closed* on unmatched images — either way a
   stale `ghcr.io/...` glob does not protect the mirrored manager image you meant to admit.
3. **Label the namespace** that runs the controller (default `nvcre`) so enforcement is
   opt-in, not cluster-wide:

   ```shell
   # Kyverno sample (matchConstraints.namespaceSelector)
   kubectl label namespace nvcre \
     kubernetes.nvcre.nvidia.com/image-admission=enforce

   # policy-controller sample (chart-default opt-in). Labelling puts the whole
   # namespace under deny-by-default for images that match no ClusterImagePolicy
   # unless you set no-match-policy to allow|warn.
   kubectl label namespace nvcre policy.sigstore.dev/include=true
   ```

4. Apply the sample that matches the admission controller you already run:

   ```shell
   kubectl apply -f config/samples/policy/kyverno-verify-images.yaml
   # or
   kubectl apply -f config/samples/policy/policy-controller-verify-images.yaml
   ```

#### Why the samples look the way they do

These constraints are load-bearing; a first attempt that skipped them was parked (issue
#272) because it would break an operator's cluster.

| Concern | What the samples do |
|---|---|
| Namespace scoping | Kyverno: `matchConstraints.namespaceSelector` on an opt-in label. policy-controller: namespace must carry `policy.sigstore.dev/include=true`. On policy-controller that label is deny-by-default for *every* image in the namespace (chart-default `no-match-policy: deny`), not a narrowing to NVCRE images alone. |
| `failurePolicy` / enforce mode | Fail-closed on purpose, but only after the namespace opt-in above. Image globs are evaluated *inside* the controller and cannot narrow which requests hit a Fail webhook. |
| Mutating webhook (Kyverno) | `validationConfigurations` is set explicitly (`mutateDigest` / `verifyDigest` / `required: true`). Applying the Kyverno sample therefore also registers a mutating webhook that rewrites matching tags to digests — CRD defaults, called out so it is a decision rather than a surprise. |
| Provenance | Both require a verified `https://slsa.dev/provenance/v1` attestation, not signature-only. |
| Identity matching | Live field is exact `subject:` for one release tag. A regexp belongs under `subjectRegExp:` — putting `refs/tags/.+` under `subject:` matches no SAN and denies everything. Kyverno globs name `.../manager`, `.../manager:*`, and `.../manager@*`; policy-controller omits the bare form (it matches `ref.Name()`, which always has a tag or digest) and uses `.../manager:*` + `.../manager@sha256:*`. Both deliberately avoid a `manager*` prefix wildcard. Mirror installs must rewrite the `ghcr.io` prefix. |
| Bundle format | policy-controller sets `signatureFormat: bundle` and checks the `cosign/sign/v1` + `slsa.dev/provenance/v1` attestations that referrers expose. |
| Container coverage (Kyverno) | Validations concatenate `images.containers` + `initContainers` + `ephemeralContainers`, and `matchConstraints` includes `pods/ephemeralcontainers` so `kubectl debug` is in scope. `validationConfigurations.required` already denied unchecked init images; the concatenation makes the deny message name the real check. |

#### Kind reproduction (operator / maintainer)

This environment may not have Docker or Kind. On a machine that does, the intended check
is server-side dry-run so the admission webhook actually runs:

```shell
TAG=v0.2.0
IMAGE=ghcr.io/nvidia/cluster-readiness-engine/manager
DIGEST="$(crane digest "${IMAGE}:${TAG}")"
ID="https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/${TAG}"
# Live main-<sha7> tag from GHCR (publish.yml does not publish a floating :main).
# GHCR lists tags oldest-first, so `crane ls | grep '^main-' | head -n1` returns
# an unsigned early build. publish.yml computes the tag as main-${GITHUB_SHA::7}:
#   DEV_TAG="main-$(git rev-parse --short=7 origin/main)"
DEV_TAG=main-c2d4d47

# --- Kyverno path (primary for issue #272) ---
kubectl create namespace nvcre --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace nvcre \
  kubernetes.nvcre.nvidia.com/image-admission=enforce --overwrite

# Edit the sample's subject pin to ${TAG}, then:
kubectl apply -f config/samples/policy/kyverno-verify-images.yaml

# Admitted: signed release index by digest
kubectl run nvcre-admit --image="${IMAGE}@${DIGEST}" -n nvcre \
  --dry-run=server --restart=Never -o name

# Denied: signed under attest.yml@refs/heads/main (development image).
# Uses a real published main-<sha7> tag so admission resolves the manifest and
# exercises the subject: identity pin (a floating :main tag is not published).
kubectl run nvcre-deny-main --image="${IMAGE}:${DEV_TAG}" -n nvcre \
  --dry-run=server --restart=Never -o name

# Denied: published but unsigned. Pre-dates release signing, which starts at
# v0.1.0-rc.9, so admission resolves the manifest and fails on missing
# signature material rather than on the lookup. (These old tags could be GC'd;
# a 404 then is obvious. :not-a-release is a lookup failure, not this criterion.)
kubectl run nvcre-deny-unsigned --image="${IMAGE}:v0.1.0-rc.1" -n nvcre \
  --dry-run=server --restart=Never -o name

# Untouched on Kyverno: image outside matchImageReferences, even in the labeled
# namespace (validations never see it).
kubectl run nvcre-other --image="busybox:1.36" -n nvcre \
  --dry-run=server --restart=Never -o name

# --- policy-controller caveat ---
# If you apply the policy-controller sample instead, labelling with
# policy.sigstore.dev/include=true puts the namespace under chart-default
# no-match-policy=deny. The busybox create above is then DENIED (not untouched)
# because busybox matches no ClusterImagePolicy. Untouched only holds if
# no-match-policy is allow|warn, or the namespace is unlabeled. Prefer a
# separate namespace for that check, and treat the policy-controller sample
# as format guidance until the #272 re-test lands.
```

These repro steps are fenced as `shell` rather than `bash` so Docs Verify (which extracts and runs every `bash` block on this page) does not try them without a cluster. CI does **not** reproduce live admission: it needs a cluster and registry access. Structural
tests under `test/docspolicy` pin the kind, `validationActions: [Deny]`, fail-closed
`failurePolicy`, issuer, identity prefix (and anchored repository path), single
attestor / authority, namespace selector, provenance attestation, narrow globs, and
`signatureFormat: bundle` so the samples cannot silently rot into the parked shape.

## Troubleshooting

**`no matching CertificateIdentity found`** — the identity did not match. The message
prints the SAN it expected and the SAN it found; compare them. The usual cause is a `TAG`
that does not match the artifact, since the identity embeds the tag.

**`none of the attestations matched the predicate type: <type>, found: <types>`** — right
identity and subject, wrong `--type`. Provenance is `slsaprovenance1` on the index digest;
an SBOM is `cyclonedx` on a per-platform digest. The message lists what the subject
actually carries, which tells you which of the two you got wrong.

**`reading <file>: no such file or directory`** — the bundle is not on disk. Usually the
`curl` for it failed: releases before `v0.2.0-rc.1` carry no bundles at all, so
`curl -fsSLO .../installer.sigstore.json` returns `curl: (56) ... error 404` and cosign
never runs. Nothing was signed there; there is nothing to verify.

**`exec: "docker-credential-osxkeychain"`** from `helm pull` — a local Docker credential
helper is declared in `~/.docker/config.json` but not installed. It has nothing to do
with the chart, which is public and needs no credentials. Point helm at an empty registry
config to bypass it:

```bash
echo '{}' > /tmp/helm-reg.json
HELM_REGISTRY_CONFIG=/tmp/helm-reg.json helm pull "oci://${CHART}@${DIGEST}"
```

**Verification fails intermittently** — Rekor and the registry are network services.
`cosign` has no retry flag; run the command again before concluding anything. A transient
failure and a real mismatch produce different messages, and the first one is far more
common.

**A checksum passed but you want more** — `checksums.txt` is served from the same origin
as the assets and is itself unsigned, so whoever can replace an asset can replace its
checksum line in the same write. It detects corruption, not tampering. This is not
theoretical: a release built with a binary deliberately modified after signing produced
`ok checksums.txt` and was caught only by the signature check. Verify the bundle.

## See also

- [SECURITY.md](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/SECURITY.md) — reporting policy and the supply chain summary
- [RELEASE.md](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/RELEASE.md) — what a release publishes and how it verifies itself
- [ADR-074](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/074-supply-chain-attestation.md) — the artifact and verification contract, and why each choice was made
