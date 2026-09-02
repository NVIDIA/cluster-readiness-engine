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

   Sign the tag (`-s`). Pushing the tag is the release trigger; there is no button to
   press afterwards.
4. Watch the `Release` workflow. If it fails, the release is incomplete — see
   Troubleshooting.
5. Check the published release, then announce it.

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
| Create GitHub Release | the GitHub Release, its notes, and the assets below |

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

The release workflow verifies its own output: the Build CLI Binaries job stamps the
binaries with the tag explicitly and fails if `nvcrectl --version` does not report the
tag exactly, and the Create GitHub Release job re-downloads the published `installer`,
`checksums.txt`, and `nvcrectl-linux-amd64` assets, checks the installer is a runnable
shell script (not an error page), verifies checksums, and re-checks the binary's
self-reported version.

To verify manually:

```bash
VERSION=v0.1.0
curl -fsSLO "https://github.com/NVIDIA/cluster-readiness-engine/releases/download/${VERSION}/checksums.txt"
curl -fsSLO "https://github.com/NVIDIA/cluster-readiness-engine/releases/download/${VERSION}/nvcrectl-linux-amd64"
sha256sum --check --ignore-missing checksums.txt
```

Releases up to and including `v0.1.0-rc.7` predate the checksum step and carry no
`checksums.txt`.

## Troubleshooting

**The workflow failed partway.** Jobs publish independently, so a release can be
half-published: the chart pushed but no GitHub Release, or the reverse. Read the run,
fix the cause, and re-run the failed jobs from the Actions tab. Do not delete and re-push
a tag that has already published artifacts — consumers may have it. Cut the next patch
version instead.

**The Helm push failed on `check-clean-version`.** The tag was not clean. Delete the tag
if nothing published, commit your work, and tag again.

**`releases/latest` returns 404.** No stable release exists yet. Use an explicit version
in the download URL.

## See also

- [CONTRIBUTING.md](CONTRIBUTING.md) — how to get a change into `main` before it ships
- [MAINTAINERS.md](MAINTAINERS.md) — who the maintainers are
