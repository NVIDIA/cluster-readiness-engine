#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# verify-doc-links.sh checks that every relative markdown link in README.md
# and docs/**/*.md resolves to an existing file or directory in the working
# tree. External links (http, https, mailto, any URI scheme) and pure
# fragment links (#anchor) are ignored. Fragments and query strings are
# stripped before resolution, so [text](path#anchor) checks only that path
# exists. Links inside fenced code blocks and inline code spans are skipped.
#
# Exits non-zero and prints a file:line listing when any link target is
# missing. Requires only POSIX sh, awk, and find.

set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

# Markdown files to scan: README.md plus everything under docs/, excluding
# agent scratch and vendored/generated directories.
files="README.md
$(find docs \
    -type d \( -name '.claude' -o -name 'vendor' -o -name 'node_modules' -o -name 'third_party' -o -name '_generated' \) -prune \
    -o -type f -name '*.md' -print | sort)"

# Emit one "file<TAB>line<TAB>resolved-path<TAB>raw-target" record per
# relative link. Repo markdown filenames contain no whitespace, so the
# unquoted expansion of $files is safe here.
# shellcheck disable=SC2086
awk '
FNR == 1 { infence = 0 }
{
    line = $0
    # Toggle fenced code block state on ``` / ~~~ fences and skip fenced lines.
    if (line ~ /^[ \t]*(```|~~~)/) { infence = !infence; next }
    if (infence) next
    s = line
    # Drop inline code spans so example links are not checked.
    gsub(/`[^`]*`/, "", s)
    while (match(s, /\]\([^)]*\)/)) {
        tgt = substr(s, RSTART + 2, RLENGTH - 3)
        s = substr(s, RSTART + RLENGTH)
        gsub(/^[ \t]+/, "", tgt)
        gsub(/[ \t]+$/, "", tgt)
        # <path with spaces> form, else strip a trailing "title" after the path.
        if (tgt ~ /^</) { sub(/^</, "", tgt); sub(/>.*$/, "", tgt) }
        else            { sub(/[ \t].*$/, "", tgt) }
        raw = tgt
        if (tgt == "") continue
        if (tgt ~ /^#/) continue                       # same-file anchor
        if (tgt ~ /^[A-Za-z][A-Za-z0-9+.-]*:/) continue # URI scheme (https:, mailto:, ...)
        if (tgt ~ /^\/\//) continue                     # protocol-relative URL
        sub(/[#?].*$/, "", tgt)                         # strip fragment / query
        if (tgt == "") continue
        if (tgt ~ /^\//) {
            resolved = "." tgt                          # root-relative
        } else {
            dir = FILENAME
            if (dir ~ /\//) { sub(/\/[^\/]*$/, "", dir) } else { dir = "." }
            resolved = dir "/" tgt
        }
        printf "%s\t%d\t%s\t%s\n", FILENAME, FNR, resolved, raw
    }
}
' $files | {
    checked=0
    broken=0
    while IFS="$(printf '\t')" read -r file lineno resolved raw; do
        checked=$((checked + 1))
        if [ ! -e "$resolved" ]; then
            echo "$file:$lineno: broken link: ($raw) -> $resolved" >&2
            broken=$((broken + 1))
        fi
    done
    if [ "$broken" -ne 0 ]; then
        echo "ERROR: $broken broken relative markdown link(s) out of $checked checked." >&2
        exit 1
    fi
    echo "verify-doc-links: OK ($checked relative links checked)"
}
