#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# fern-freeze-versions.sh materialises the frozen documentation content that
# every registered Fern version points at.
#
# fern/docs.yml registers past releases, and each fern/versions/vX.Y.Z.yml
# navigates to vX.Y.Z-content/<page>.md. That content is a snapshot of docs/ at
# the release tag, so it is derived, not authored: it is rebuilt here rather
# than committed, and fern/.gitignore keeps it out of the tree.
#
# All three consumers run this -- the docs publish workflow, the PR-time docs
# checks, and the PR preview build -- which is the point of it being a script
# rather than three copies. When only the publish workflow built the content,
# `fern docs md check` on a pull request followed docs.yml into a directory that
# does not exist in a plain checkout and failed on every PR that touched docs/
# or fern/, while the publish path, which built the content first, was fine. A
# check that cannot see what it validates is not a check.
#
# Run it locally with `make fern-freeze-versions` before `fern check`.
#
# Requires: git with tags fetched (actions/checkout needs fetch-depth: 0).

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

shopt -s nullglob
version_files=(fern/versions/v*.yml)
shopt -u nullglob

if [ ${#version_files[@]} -eq 0 ]; then
  echo "no registered versions under fern/versions; nothing to freeze"
  exit 0
fi

for version_file in "${version_files[@]}"; do
  version="$(basename "${version_file}" .yml)"
  content="fern/versions/${version}-content"

  if ! git show-ref --verify --quiet "refs/tags/${version}"; then
    echo "::error::tag ${version} not found -- cannot pin frozen docs content." \
      "If this is running in CI, the checkout needs fetch-depth: 0 so tags are present."
    exit 1
  fi

  rm -rf "${content}"
  mkdir -p "${content}"
  git archive "refs/tags/${version}" -- docs/ | tar -x --strip-components=1 -C "${content}"

  # Escape {, } and < for MDX -- but only outside fenced code blocks and inline
  # code spans, where they are legitimate literal characters.
  find "${content}" -name '*.md' -print0 | while IFS= read -r -d '' f; do
    awk '
      /^````*/ || /^~~~~*/ { fence = !fence; print; next }
      fence { print; next }
      {
        n = split($0, p, "`")
        out = ""
        for (i = 1; i <= n; i++) {
          if (i % 2 == 1) {
            gsub(/{/, "\\{", p[i])
            gsub(/}/, "\\}", p[i])
            gsub(/</, "\\&lt;", p[i])
          }
          out = out p[i]
          if (i < n) out = out "`"
        }
        print out
      }
    ' "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"
  done

  echo "froze ${version} docs into ${content}"
done
