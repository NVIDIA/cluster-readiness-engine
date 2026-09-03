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

From `v0.2.0-rc.1` onward the installer checks the binary it downloads against that
release's bundle and refuses to install if it cannot. `--skip-verify` overrides that and
is never inferred from a missing tool.

## Prerequisites

| Tool | Used for | Version this page was checked with |
|---|---|---|
| [`cosign`](https://docs.sigstore.dev/cosign/installation/) | every verification below | `v3.1.3` |
| [`crane`](https://github.com/google/go-containerregistry/tree/main/cmd/crane) | resolving per-platform image digests | `v0.21.1` |
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
git ls-remote https://github.com/NVIDIA/cluster-readiness-engine "refs/tags/${TAG}"
```

`head -1` above is deliberate. A retried signing attempt can leave more than one valid
attestation on a digest, which is expected and is not tampering — but it means `.payload`
can return several lines, and feeding all of them to `base64 -d` produces nonsense. Take
one, or loop and require every statement to agree.

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

Pulling by digest is what closes the loop. Content addressing means you receive those
exact bytes or an error, so there is no window between verifying and installing.

## Verifying a downloaded binary and its SBOM

Each `nvcrectl-*` binary ships **two** bundles whose names differ by one segment, and
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
```

### Reading the SBOM

The SBOM is published as a plain asset, so no extraction is needed for binaries:

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
  | jq -r '.payload' | head -1 | base64 -d | jq '.predicate' > "${tmp}" \
  && mv "${tmp}" sbom-linux-amd64.json
jq -r '.components | length' sbom-linux-amd64.json
```

Do not write straight to the final path. `>` truncates the target before the pipeline
runs and `jq` exits 0 on empty input, so a failed verification leaves a zero-length file
and a zero exit status — a broken result that reads as success. `set -o pipefail` plus a
deferred `mv` is what makes the failure visible.

## Air-gapped verification

`cosign verify` needs the Sigstore trust root and, by default, fetches it. On a machine
with no egress, fetch it once somewhere with network:

```bash
cosign initialize
tar -czf sigstore-root.tgz -C "${HOME}" .sigstore
```

Move `sigstore-root.tgz` and the artifacts across, then unpack it into `$HOME` on the
target. Verification is otherwise unchanged and needs no network, because the bundle
carries the signature, the certificate and the inclusion proof.

Blob bundles are the easy case: an asset and its `.sigstore.json` are two files, and
`cosign verify-blob-attestation` reads both from disk. Registry attestations need the
registry, so mirror the image and chart with `crane copy` before disconnecting.

## Troubleshooting

**`no matching CertificateIdentity found`** — the identity did not match. The message
prints the SAN it expected and the SAN it found; compare them. The usual cause is a `TAG`
that does not match the artifact, since the identity embeds the tag.

**`no matching attestations`** — right identity, wrong subject or predicate type. Check
you are using the index digest for provenance and a per-platform digest for an SBOM, and
that `--type` matches (`slsaprovenance1` for provenance, `cyclonedx` for an SBOM).

**`Error: fetching signatures: reading layout ...`** on a release before `v0.2.0-rc.1` —
those releases carry no bundles. Nothing was signed; there is nothing to verify.

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
