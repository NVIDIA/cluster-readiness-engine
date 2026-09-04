# Security

NVIDIA is dedicated to the security and trust of our software products and services, including all source code repositories.

**Please do not report security vulnerabilities through GitHub.**

## Reporting Security Vulnerabilities

To report a potential security vulnerability in any NVIDIA product:

- **Web**: [Security Vulnerability Submission Form](https://www.nvidia.com/object/submit-security-vulnerability.html)
- **Email**: psirt@nvidia.com
  - Use [NVIDIA PGP Key](https://www.nvidia.com/en-us/security/pgp-key) for secure communication

**Include in your report**:
- Product/Driver name and version
- Type of vulnerability (code execution, denial of service, buffer overflow, etc.)
- Steps to reproduce
- Proof-of-concept or exploit code
- Potential impact and exploitation method

NVIDIA offers acknowledgement for externally reported security issues under our coordinated vulnerability disclosure policy. Visit [PSIRT Policies](https://www.nvidia.com/en-us/security/psirt-policies/) for details.

## Response Expectations

- Reports submitted through the channels above are **acknowledged within 5 business days**.
- NVIDIA PSIRT coordinates triage, remediation, and disclosure with the reporter under the [coordinated vulnerability disclosure policy](https://www.nvidia.com/en-us/security/psirt-policies/).

## Supported Versions

Security fixes land on `main` and are released in the latest minor release line.

| Version | Supported |
| --- | --- |
| Latest `v0.x` minor release | ✅ |
| Older releases | ❌ — upgrade to the latest release |

While NVCRE is pre-1.0, we do not backport fixes to older minor versions.

## Vulnerability Fix Timelines

Once a vulnerability in NVCRE is confirmed:

- **Critical / High severity**: a fix or a documented mitigation ships within **30 days** of confirmation.
- **Medium / Low severity**: a fix ships in the next scheduled release.

CVEs affecting NVCRE are published through the NVIDIA PSIRT process (NVIDIA is a CVE Numbering Authority).

## Scope

In scope: vulnerabilities in NVCRE itself — the controller, the `nvcrectl` CLI, the Helm chart, and the container images this repository publishes.

Out of scope:

- Vulnerabilities requiring physical access to cluster nodes
- Social engineering of maintainers or users
- Denial of service that requires cluster-admin or the ability to schedule arbitrary workloads
- Theoretical issues without a proof of concept or demonstrated impact
- Vulnerabilities in third-party dependencies without a demonstrated impact on NVCRE (report those upstream; we still welcome a heads-up)

## Reporter Credit

We credit reporters of confirmed vulnerabilities in the release notes of the fixed version and in the NVIDIA security bulletin, unless the reporter asks not to be named.

## Supply Chain

- NVCRE is licensed under Apache-2.0. Dependencies are reviewed for license compatibility with Apache-2.0; attributions are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
- Container images are signed with Sigstore cosign (keyless OIDC). A released image carries a signature and a SLSA Build Provenance v1 statement on its multi-platform **index** digest, and a signature plus a CycloneDX SBOM on each **per-platform** manifest digest. An SBOM describes one root filesystem, so it is bound to the platform it actually describes rather than to the index.

  All release attestations are produced by one reusable workflow, so verification pins one exact identity:

  ```bash
  TAG=v1.2.3   # the release you are verifying
  IMAGE=ghcr.io/nvidia/cluster-readiness-engine/manager

  cosign verify \
    --certificate-identity "https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/${TAG}" \
    --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
    "${IMAGE}:${TAG}"
  ```

  Use `--certificate-identity`, not `--certificate-identity-regexp`. An identity that names no workflow and no ref also accepts images built from branches, which are not releases and are labelled non-production when they are signed.

  Retrieve the provenance with `cosign verify-attestation --type slsaprovenance1` against the index digest, and a platform's SBOM with `--type cyclonedx` against that platform's manifest digest (`crane digest --platform linux/amd64 "${IMAGE}:${TAG}"`).

- CLI binaries, the installer and the SBOMs are each signed, and every release asset ships with a detached Sigstore bundle (`<asset>.sigstore.json`) verified under the same identity as the image:

  ```bash
  TAG=v1.2.3
  BASE="https://github.com/NVIDIA/cluster-readiness-engine/releases/download/${TAG}"
  curl -fsSLO "${BASE}/nvcrectl-linux-amd64"
  curl -fsSLO "${BASE}/nvcrectl-linux-amd64.sigstore.json"

  cosign verify-blob-attestation \
    --bundle nvcrectl-linux-amd64.sigstore.json \
    --type https://slsa.dev/provenance/v1 \
    --certificate-identity "https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/${TAG}" \
    --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
    nvcrectl-linux-amd64
  ```

  Each binary additionally carries `<binary>.cyclonedx.sigstore.json`, which binds its SBOM to that binary — verify it with `--type cyclonedx` against the **binary**, not against the SBOM file.

  `checksums.txt` is also published, but it is served from the same origin as the assets and is itself unsigned, so it detects corruption rather than tampering. Verify the bundle, not the checksum.

- **`installer` verifies what it installs, and cannot verify itself.** From the first release cut after this behaviour landed — later than `v0.2.0-rc.1`, whose installer checks only `checksums.txt` — the installer checks the binary it downloads against that release's `.sigstore.json` under the identity above, using `cosign` if present and otherwise fetching a copy pinned by digest inside the script. If it cannot verify, it stops. `--skip-verify` overrides that, and is never inferred from a missing tool or a failed download — verification is skipped only when someone asks for it by name.

  That hardens what the installer *installs*. It does nothing for the installer itself: under `curl … | bash` the script executes before anything has checked it, and whoever could replace that asset could delete the verification logic and its pinned digest in the same write. The pipe buys TLS integrity in transit and nothing against a compromised release asset.

  The verified path puts the trust anchor outside the script:

  ```bash
  TAG=v1.2.3
  BASE="https://github.com/NVIDIA/cluster-readiness-engine/releases/download/${TAG}"
  curl -fsSLO "${BASE}/installer"
  curl -fsSLO "${BASE}/installer.sigstore.json"

  cosign verify-blob-attestation \
    --bundle installer.sigstore.json \
    --type https://slsa.dev/provenance/v1 \
    --certificate-identity "https://github.com/NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/${TAG}" \
    --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
    installer

  bash installer -v "${TAG}"
  ```

- **The published images are re-scanned weekly, after release.** Signing and SBOMs answer "was this what we built"; they say nothing about a CVE published after we shipped. `.github/workflows/vuln-scan-images.yml` runs every Monday at 08:00 UTC against the newest **stable** release and the newest published `main` image, both per platform, by digest. Fixable High and Critical findings are posted to Slack; a run that does not complete is posted too, so a broken scan cannot look like a clean week.

  The scan reports, it does not gate — an unfixable upstream base-image CVE must not turn releases red. Findings feed the timelines in [Vulnerability Fix Timelines](#vulnerability-fix-timelines) above.

  Suppressions and the impact analysis behind them live in one place: [`.openvex.json`](.openvex.json), an [OpenVEX](https://openvex.dev) v0.2.0 document the scan passes to grype as `--vex`. `.grype.yaml` carries scanner configuration only, and a test fails the build if an ignore rule is added there instead.

  Every statement must name the advisory exactly as grype reports it, target the image, name the affected package as a subcomponent so it cannot silence the advisory in a package nobody analysed, choose a justification from the OpenVEX v0.2.0 enum, and carry a substantive impact statement. The document is public in this repository and is what anyone auditing our triage reads; it is not currently attested or published to a registry. OpenVEX has no notion of expiry, so a statement must additionally be re-affirmed within 180 days — and may not be dated in the future — or the build goes red. The weekly scan runs that check *before* it scans, so a statement cannot lapse during a quiet week and go on suppressing a finding, and it separately fails the run if a declared statement matched nothing. Re-affirm only after re-confirming the claim still holds; do not simply refresh the date.

  VEX is for findings that cannot be remediated by upgrading. If a fixed version is reachable, the answer is a dependency bump and a release, not a statement. See [`.claude/skills/managing-openvex.md`](.claude/skills/managing-openvex.md) for the full triage procedure.

## Product Security Resources

For all security-related concerns: https://www.nvidia.com/en-us/security
