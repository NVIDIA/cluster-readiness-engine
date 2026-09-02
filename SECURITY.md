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

- CLI binaries include SHA256 checksums in each release.

## Product Security Resources

For all security-related concerns: https://www.nvidia.com/en-us/security
