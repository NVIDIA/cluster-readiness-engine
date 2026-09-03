<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Release Process

How a release of the NVIDIA Cluster Readiness Engine is cut, what it publishes, and how to
check that you received what we published.

## Versioning

NVCRE follows [Semantic Versioning](https://semver.org/). Tags are `vMAJOR.MINOR.PATCH`,
optionally with a pre-release suffix, for example `v0.1.0` or `v0.1.0-rc.8`.

The project is at `v0.x`. Under SemVer that means the public surface can still change in
a minor release. Treat CRD schemas, the `nvcrectl` command line, and Helm values as
unstable until `v1.0.0`. Breaking changes are called out in the release notes.

## Cadence

There is no fixed schedule. NVCRE releases when there is something worth releasing.

Do not wait for a date to ship a fix, and do not cut a release to meet one.

## Who can cut a release

The maintainers listed in [MAINTAINERS.md](MAINTAINERS.md). Pushing a tag is what starts
a release, so anyone with write access to the repository can technically start one; by
convention it is a maintainer who does.

Announce your intent on the pull request or issue that motivates the release, so two
people do not tag at the same time.

## Cutting a release

1. Make sure `main` is green. Every required check must pass: Lint, Build, Test, Verify,
   and UAT.
2. Decide the version. See Versioning above.
3. Tag the commit on `main` and push the tag.

   ```bash
   git checkout main && git pull --ff-only
   git tag -s v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

   If you work from a fork, push the tag to the remote that points at
   `NVIDIA/cluster-readiness-engine`, which is usually `upstream`.

   Sign the tag (`-s`). Pushing the tag is the release trigger.
4. Watch the `Release` workflow. The GitHub Release is created as a **draft** and is
   made visible only by the `Verify release` job, after it has verified every published
   artifact. If that job fails, the release stays a draft — see Troubleshooting.
5. Check the published release, then announce it.

Do not publish a draft release by hand. A draft left behind by a failed `Verify release`
is a release the pipeline determined it could not verify; publishing it from the UI is
the one action that bypasses the gate entirely. Re-run the job instead.

Tags must be clean. `make check-clean-version` refuses to publish a Helm chart when the
version contains `-dirty`, a `-N-gSHA` suffix, or is `dev`, which is what you get from
tagging a working tree that is not committed. It also rejects anything that does not
look like a `vX.Y.Z[-prerelease]` tag, such as the bare commit SHA that `git describe
--tags --always` falls back to when no tag is reachable from the checkout.

### Release candidates

Cut `-rc.N` tags to validate a release before it becomes stable. They publish through the
same pipeline and are marked as pre-releases on GitHub, so they do not become the
`latest` release.

When an RC is good, tag the **same commit** with the stable version. Do not rebuild or
re-merge between the RC and the stable tag; that would ship something nobody validated.

Note that `curl .../releases/latest/download/installer` resolves to the newest
**stable** release, never a pre-release. To install an RC, name its tag explicitly
in the `releases/download/<tag>/installer` URL and pass `-v <tag>` to the installer.

## What a release publishes

Pushing a `v*` tag runs `.github/workflows/release.yml`, which owns every release artifact:

| Job | Publishes |
|---|---|
| Build release image | `ghcr.io/nvidia/cluster-readiness-engine/manager:<tag>`, multi-platform |
| Attest image (index, amd64 SBOM, arm64 SBOM) | signature, SLSA provenance, per-platform CycloneDX SBOMs |
| Publish Helm Chart | `oci://ghcr.io/nvidia/cluster-readiness-engine` |
| Attest Helm chart | signature and provenance on the chart digest |
| Build CLI Binaries | cross-compiled `nvcrectl` for linux and macOS, amd64 and arm64 |
| Attest binaries | a Sigstore bundle per binary, per SBOM, and for the installer |
| Create GitHub Release | the GitHub Release as a **draft**, its notes, and the assets below |
| Verify release | verifies every published artifact, then makes the release visible |

The release builds the container image itself. `.github/workflows/publish.yml` no longer
runs on tags — it now builds only `main-<sha>` development images, and is pinned to
`refs/heads/main` so it cannot be dispatched against a release tag. Previously both
workflows fired on a tag push and raced, and the release notes named an image the release
had never resolved.

Each artifact is published only after the one it depends on is signed: the chart is not
pushed until the image it references exists and is attested, and the GitHub Release is not
created until all three families are attested.

**Re-running part of a release.** There is no longer a workflow that rebuilds only the
image for an existing tag. Re-run the whole release with
`gh workflow run Release --ref vX.Y.Z -f tag=vX.Y.Z` — dispatching at the tag is required,
because signatures take their identity from the ref the run was dispatched from. That
re-executes the chart, binary and release steps as well, which is a wider blast radius
than the old image-only rebuild.

Release assets:

- `installer` — the install script the README points at
- `nvcrectl-linux-amd64`, `nvcrectl-linux-arm64`
- `nvcrectl-darwin-amd64`, `nvcrectl-darwin-arm64`
- `checksums.txt` — SHA-256 of every asset above

## Verifying a release

The release workflow verifies its own output before anyone can see it. The Build CLI
Binaries job stamps the binaries with the tag and fails if `nvcrectl --version` does not
report it exactly. The release is then created as a draft, and the `Verify release` job
holds it there until it has checked, against the exact signing identity
`…/attest.yml@refs/tags/<tag>`:

- every release asset against its Sigstore bundle, and each binary against the bundle
  binding its SBOM to it
- `checksums.txt` against the downloaded assets
- the image index signature and provenance, and both per-platform SBOMs
- that `manager:<tag>` still resolves to the index digest that was verified
- the provenance predicate itself — repository, ref and commit — not just the signature
- the chart's signature and provenance

Only then is the release published, after which the assets are re-fetched anonymously
and re-verified over the channel a user actually takes. If anything fails after
publication, the release is returned to draft.

To verify manually, check the signature — not the checksum:

```bash
VERSION=v0.2.0-rc.1
BASE="https://github.com/NVIDIA/cluster-readiness-engine/releases/download/${VERSION}"
curl -fsSLO "${BASE}/nvcrectl-linux-amd64"
curl -fsSLO "${BASE}/nvcrectl-linux-amd64.sigstore.json"

cosign verify-blob-attestation \
  --bundle nvcrectl-linux-amd64.sigstore.json \
  --type https://slsa.dev/provenance/v1 \
  --certificate-identity "https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/${VERSION}" \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  nvcrectl-linux-amd64
```

`checksums.txt` is still published and `installer` still checks it, but do not mistake it
for this. It is served from the same origin as the binary and is itself unsigned, so
whoever can replace one can replace the other in the same write: it detects corruption,
not tampering. See [SECURITY.md](SECURITY.md#supply-chain) for the image, chart and
installer equivalents.

Releases up to and including `v0.1.0-rc.7` predate the checksum step and carry no
`checksums.txt`.

## Troubleshooting

**The workflow failed partway.** Jobs publish independently, so a release can be
half-published: the chart pushed but no GitHub Release, or the reverse. Read the run,
fix the cause, and re-run the failed jobs from the Actions tab. Do not delete and re-push
a tag that has already published artifacts — consumers may have it. Cut the next patch
version instead.

**`Verify release` failed and the release is stuck as a draft.** This is the designed
failure mode, not a broken run: the release is withheld precisely because something did
not verify. Read `release-verification-log` on the run, which records every cosign
invocation, and fix the cause. Then re-run the failed jobs. Do not publish the draft from
the UI.

The draft is also invisible to `installer`, deliberately. It resolves the newest
*published* release on every path, and refuses outright when `-v <tag>` names a draft.
Without that, a maintainer or an in-repo CI job running the installer while the gate was
still working — or after it had refused a build — would install exactly what the draft
exists to withhold. Draft assets are reachable only with push access, so this never
affected anonymous users; it affected the accounts closest to the release.

The installer also refuses when it cannot *tell* — `Could not determine whether release
<tag> is a draft after 3 attempts`. An unreadable state is not a published one, so it
stops rather than guess. That is a GitHub API problem, not a bad release: retry, or check
GitHub status. Anonymous installs never hit it, because no draft is reachable without
push access and so there is nothing to determine. `test/releasepolicy` covers every
branch of that decision, including the two ways it previously got it wrong.

Note what the draft does **not** hold back. The image and the chart are pushed to GHCR
before the gate runs and cannot be unpublished, and GitOps automation watching the
registry — Flux `OCIRepository`, Argo CD Image Updater, Renovate — does not wait for the
GitHub Release. If the gate failed on the image or the chart rather than on a release
asset, treat those registry tags as suspect and say so in the follow-up release, because
`<tag>` remains the newest version in the registry until the next one ships.

**A release already exists for this tag.** The `Require the release to be a draft` step
fails when the tag already has a *published* release. That is deliberate: the action
that creates the release honours `draft:` only on creation, so re-running against a
published tag would upload freshly built, unverified assets into a live release. Cut the
next patch version instead.

**`installer` refused: "Signature verification failed".** It checks the binary against
that release's Sigstore bundle and will not install one it cannot verify. Read the cosign
output printed above the error — it distinguishes a genuine identity or signature mismatch
from verification that could not complete, such as Rekor being unreachable. A mismatch on
a release you cut is worth investigating before anything else.

**`installer` refused: "Could not download &lt;asset&gt;.sigstore.json".** That release
carries no bundles; anything before `v0.2.0-rc.1` predates them. Install a newer release,
or pass `--skip-verify` if you accept installing something nothing has vouched for.

**The Helm push failed on `check-clean-version`.** The tag was not clean. Delete the tag
if nothing published, commit your work, and tag again.

**`releases/latest` returns 404.** No stable release exists yet. Use an explicit version
in the download URL.

## See also

- [CONTRIBUTING.md](CONTRIBUTING.md) — how to get a change into `main` before it ships
- [MAINTAINERS.md](MAINTAINERS.md) — who the maintainers are
