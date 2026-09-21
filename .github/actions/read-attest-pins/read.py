#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Emit the signer tool pins from attest.yml.

Verifier jobs (release.yml verify-release, docs-verify.yml) must consume these
values rather than carrying their own copies. Dependabot cannot raise
workflow_call input defaults; reading the file is how the copies stay one pin.
"""

from __future__ import annotations

import os
import re
import sys
from pathlib import Path

VERSION_RE = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


def attest_path() -> Path:
    here = Path(__file__).resolve()
    # .github/actions/read-attest-pins/read.py -> .github/workflows/attest.yml
    bundled = here.parents[2] / "workflows" / "attest.yml"
    candidates = [bundled]
    workspace = os.environ.get("GITHUB_WORKSPACE")
    if workspace:
        candidates.append(Path(workspace) / ".github/workflows/attest.yml")
    candidates.append(Path(".github/workflows/attest.yml"))
    for path in candidates:
        if path.is_file():
            return path
    raise SystemExit(
        "could not find .github/workflows/attest.yml; looked in: "
        + ", ".join(str(p) for p in candidates)
    )


def input_default(text: str, name: str) -> str:
    # workflow_call inputs are indented six spaces; their fields, eight.
    # The job-output assignment `cosign_version: ${{ ... }}` has no `default:`.
    match = re.search(
        rf"(?m)^      {re.escape(name)}:\n(?:        .*\n)*?        default: ['\"]([^'\"]+)['\"]",
        text,
    )
    if not match:
        raise SystemExit(f"could not find workflow_call default for {name}")
    return match.group(1)


def crane_sha256(text: str) -> str:
    match = re.search(r"(?m)^          CRANE_SHA256: ([0-9a-f]{64})$", text)
    if not match:
        raise SystemExit("could not find CRANE_SHA256 in attest.yml")
    return match.group(1)


def emit(key: str, value: str) -> None:
    line = f"{key}={value}"
    print(line)
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as handle:
            handle.write(line + "\n")


def main() -> int:
    path = attest_path()
    text = path.read_text(encoding="utf-8")
    cosign = input_default(text, "cosign_version")
    crane = input_default(text, "crane_version")
    digest = crane_sha256(text)
    if not VERSION_RE.match(cosign):
        raise SystemExit(f"cosign_version {cosign!r} is not vMAJOR.MINOR.PATCH")
    if not VERSION_RE.match(crane):
        raise SystemExit(f"crane_version {crane!r} is not vMAJOR.MINOR.PATCH")
    if not SHA256_RE.match(digest):
        raise SystemExit("CRANE_SHA256 is not a 64-char lowercase hex digest")
    emit("cosign_version", cosign)
    emit("crane_version", crane)
    emit("crane_sha256", digest)
    return 0


if __name__ == "__main__":
    sys.exit(main())
