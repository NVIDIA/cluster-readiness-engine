# Contributing to NVCRE

Thank you for your interest in contributing! We welcome contributions from the community.

## Getting Started

Before contributing:

1. Read the [README.md](README.md) to understand the project
2. Check existing [issues](../../issues) to avoid duplicates
3. Review the [security policy](SECURITY.md) for security-related contributions
4. Review the [Code of Conduct](CODE_OF_CONDUCT.md)

## How to Contribute

Ways to contribute:

- Report bugs via issues
- Suggest features through feature requests
- Improve documentation
- Add tests to increase coverage
- Fix issues with code contributions
- Add new workload adapters or catalog entries

## Issue-First Workflow

Open an issue before you write code:

1. File an issue that describes the bug or the feature.
2. Wait for a maintainer to acknowledge it and agree on the approach.
3. Comment on the issue that you are working on it.
4. Reference the issue from your pull request (`Closes #NNN`).

This protects your time: it prevents duplicate work and PRs that conflict with planned changes. **Pull requests without a linked, acknowledged issue may be closed unreviewed.** Trivial fixes (typos, broken links) are exempt.

## Reporting Issues

When reporting issues:

1. Use the issue templates — they ask for what we need
2. Provide clear reproduction steps
3. Include environment details (NVCRE version, Kubernetes version, GPU architecture, platform)
4. Add relevant logs or error messages, with secrets removed
5. Search existing issues first to avoid duplicates

## Submitting Pull Requests

1. Fork the repository and create a feature branch
2. Follow the coding standards and existing patterns
3. Write or update tests for your changes
4. Update documentation if needed
5. Sign your commits (see the DCO section below)
6. Fill in the pull request template, including the test plan and risk assessment

**Pull Request Guidelines**:
- Keep PRs focused on a single issue or feature
- Include tests for new functionality
- Ensure all CI checks pass
- Be responsive to feedback and code review

**CI on fork pull requests**: CI does not run directly on fork branches. After a maintainer vets your PR, the copy-pr-bot service copies it to a `pull-request/<number>` branch in this repository, and the required checks run there (a trustee triggers this by commenting `/ok to test` on the PR). If your PR shows no checks yet, nothing is wrong; wait for a maintainer to trigger them.

## Commit Message Format

We use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/). Format the subject line as `type: short imperative summary`:

```
feat: add bandwidth threshold option to nccl catalog entries
fix: requeue workflow when dependency resources are not ready
docs: document checkpoint restart behavior
test: add integration case for job iteration limits
chore: bump controller-runtime to v0.22
```

Allowed types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `chore`, `ci`, `build`. Use the body to explain *why* the change is needed. Every commit also carries a DCO sign-off line (`git commit -s`).

## Code Review Process

- A project maintainer reviews every pull request. CODEOWNERS assigns reviewers automatically once your PR is open.
- Expect a first response within **5 business days**. Reviews of large PRs take longer — splitting work into small PRs gets you faster feedback.
- Address review comments with new commits (do not force-push during review; we squash on merge).
- If a PR sits without response for more than a week, add a comment to ping the reviewers. Escalate by mentioning the maintainers listed in MAINTAINERS.md.
- Merging requires: green CI, an approving maintainer review, and the DCO check passing.

## Development Setup

**Prerequisites**:
- Go 1.27+
- Docker (for container builds)
- Make (for build targets)

**Quick Setup**:

1. Clone the repository:
   ```bash
   git clone https://github.com/NVIDIA/cluster-readiness-engine.git
   cd cluster-readiness-engine
   ```

2. Generate code and manifests (required after modifying `*_types.go`):
   ```bash
   make manifests generate
   ```

3. Run the tests:
   ```bash
   make test
   ```
   You do not need a Kubernetes cluster. The `make test` target installs envtest, which has a lightweight etcd and kube-apiserver for the integration tests.

4. Run linting:
   ```bash
   make lint
   ```

5. Build the binaries:
   ```bash
   make build          # controller manager -> bin/manager
   make build-nvcrectl  # CLI -> bin/nvcrectl
   ```

**Running a single test**:
```bash
go test ./pkg/workload/ -run TestAdapterForSpec -v
```

**Running a single integration test**:
```bash
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./cmd/integration/ -v -timeout 300s -count=1 -run TestIntegration/reconcile/job-checkpoint-restart
```

## Replicate CI Locally

Five checks are required before a pull request can merge: Lint, Build, Test, Verify, and UAT.

One command runs the first four:

```bash
make ci
```

The target chains `make verify` (generated files, `go.mod` tidiness, license headers), `make lint`, `make build`, and `make test`. Run it before every push. It needs no cluster and no container runtime.

UAT is the fifth check and `make ci` does not run it:

```bash
make test-uat
```

UAT renders the catalog against a Kind cluster with KWOK-simulated GPU nodes and compares the result to golden files. It needs Docker, Kind and Tilt, and it takes several minutes, so it is not part of `make ci`. Run it when you change anything under `pkg/catalog/`.

`make test-uat` creates the cluster, deploys, tests, and tears down each time. When you are iterating, keep the cluster and re-run only the tests:

```bash
make setup-test-uat   # once
make tilt-uat         # leave running in another terminal; hot-reloads
make test-uat-run     # re-run as needed
make cleanup-test-uat # when finished
```

By default, Kind uses the Kubernetes node image bundled with the developer's
installed Kind version. To exercise a specific Kubernetes version, select its
node image when creating the cluster (Kind v0.32 or newer is required for the
Kubernetes 1.36 image):

```bash
make setup-test-uat \
  KIND_NODE_IMAGE=kindest/node:v1.34.8
```

or:

```bash
make setup-test-uat \
  KIND_NODE_IMAGE=kindest/node:v1.36.1
```

The image is used only when creating a new cluster. If `nvcre-test-uat` already
exists, delete it with `make cleanup-test-uat` before changing versions.

Notes:

- Run `make ci` on a committed tree. `make verify` regenerates code and fails when the result differs from what is committed.
- The first run downloads the pinned tools into `bin/` (controller-gen, golangci-lint, addlicense, setup-envtest) and the envtest Kubernetes binaries. Later runs reuse them.
- Tool versions are pinned in the Makefile. The Go version and the envtest Kubernetes version derive from `go.mod`. Local runs and CI resolve the same versions.
- CI's Test job runs `make test-ci`, which is the same test suite with JUnit and coverage output for the CI artifacts.

## AI-Assisted Contributions

You may use AI tools (Claude, Copilot, and similar) to help write contributions, under these rules:

- **You are accountable for the change.** Submit only code you have read, understood, and tested yourself.
- AI-assisted PRs get the same review bar as any other PR — no exceptions.
- Do not open PRs that are unreviewed AI output. Low-effort machine-generated PRs are closed under the issue-first rule.
- The DCO sign-off certifies that *you* have the right to submit the contribution. That certification is yours, not the tool's.

## Community Guidelines

- Be respectful and inclusive in all interactions
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md)
- Help maintain a welcoming environment
- Focus on constructive feedback in reviews

### Code of Conduct Enforcement

Report Code of Conduct violations to **GitHub_Conduct@nvidia.com**.

- Reports are **acknowledged within 3 business days**.
- The maintainers review the report, gather context from all parties, and **decide on a resolution within 14 days**. Outcomes follow the enforcement ladder in the [Code of Conduct](CODE_OF_CONDUCT.md): correction, warning, temporary ban, or permanent ban.
- Reporter identity stays confidential.

**Out of scope for CoC enforcement**: technical disagreements argued respectfully, code review feedback about the code (not the person), and conduct on platforms unrelated to this project. Those are handled through normal project discussion, not the CoC process.

## Developer Certificate of Origin

The sign-off is a simple signature at the end of the description for the patch. Your signature certifies that you wrote the patch or otherwise have the right to pass it on as an open-source patch.

The rules are pretty simple, and sign-off means that you certify the below (from [developercertificate.org](http://developercertificate.org/)):

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
1 Letterman Drive
Suite D4700
San Francisco, CA, 94129

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.

Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

To sign off, add the following line to every git commit message:

```
Signed-off-by: Your Name <your.email@example.com>
```

**Automatic sign-off**:
```bash
git config user.name "Your Name"
git config user.email "your.email@example.com"
git commit -s  # Automatically adds sign-off
```
