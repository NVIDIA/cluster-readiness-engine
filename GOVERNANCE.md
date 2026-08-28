# Governance

This document describes how the nvcre (NVCRE) project makes decisions and how contributors gain responsibility.

## Scope and Charter

NVCRE is a Kubernetes controller and CLI for GPU cluster burn-in certification: it runs cataloged training and communication workloads against a cluster, measures goodput and bandwidth, and remediates failing nodes. Changes that serve this charter are in scope. Features unrelated to cluster certification are out of scope and are declined, with reasons, in the issue discussion.

## Roles

### Contributor

Anyone who files an issue, improves documentation, or submits a pull request. No formal membership is required. Contributors follow the [contribution workflow](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).

### Reviewer

A contributor with a track record of quality contributions who reviews pull requests in one or more areas.

- **How to become one**: sustained contributions over roughly 3 months — several merged PRs and helpful reviews — then nomination by a maintainer.
- **Responsibilities**: review PRs for correctness and fit with project patterns; a reviewer approval is input to, not a substitute for, the maintainer approval that merges a PR.

### Maintainer

A reviewer trusted with merge rights and project direction. Maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md).

- **How to become one**: act as a reviewer for roughly 3 months, then be nominated by an existing maintainer and approved by a majority vote of the current maintainers. The path is open to external (non-NVIDIA) contributors.
- **Responsibilities**: review and merge PRs, triage issues, cut releases, maintain CI health, uphold the Code of Conduct, and mentor contributors.

## Decision Making

- **Default: lazy consensus.** Most decisions happen in issues and pull requests. A change is accepted when a maintainer approves it and no maintainer objects within 3 business days of the approval. Routine changes merge on approval.
- **Significant changes** (CRD schema changes, new subsystems, dependency policy, governance itself) require an issue describing the design, and approval from a majority of maintainers. Design records live in `docs/designs/`.
- **Voting.** When consensus fails, any maintainer may call a vote in the issue. Each maintainer has one vote; a majority of all current maintainers decides. Votes stay open for 5 business days or until the outcome cannot change.
- **Tie-breaking.** If a vote ties, the maintainers discuss it synchronously and re-vote once. If it ties again, the change is rejected in its current form — the status quo wins, and the proposal returns to design discussion.

## Adding and Removing Maintainers

- **Adding**: nomination by an existing maintainer, then a majority vote of current maintainers. The nominee's contribution and review history is the evidence.
- **Stepping down**: maintainers may step down at any time by opening a PR that moves them to the emeritus section of MAINTAINERS.md.
- **Removal for inactivity**: a maintainer with no project activity (reviews, merges, issue triage) for 6 consecutive months moves to emeritus after an attempt to reach them.
- **Removal for cause**: Code of Conduct violations or repeated abuse of maintainer rights lead to removal by majority vote of the other maintainers.
- **Emeritus**: emeritus maintainers are listed in MAINTAINERS.md with our thanks. They may return to active status by a majority vote of current maintainers.

## Amending This Document

Changes to governance follow the significant-change process above: an issue, discussion, and a majority vote of maintainers. Amendments merge as ordinary pull requests to this file.
