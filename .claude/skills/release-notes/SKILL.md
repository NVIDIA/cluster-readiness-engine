---
name: release-notes
description: |
  Use when drafting the human-readable highlights summary for an NVCRE
  release. Triggers on "release notes", "draft release notes",
  "/release-notes", or any request to summarize what shipped since the last
  tag into a release announcement.
  Resolves the tag range from the GitHub releases API, groups changes into
  thematic highlights, and writes a Markdown draft to a temp file for
  hand-editing before it is pasted into the release body.
---

# NVCRE Release Notes Draft

Drafts the **Highlights** prose block — the one part of an NVCRE release body
that no machine writes. Everything else in the body is generated:
`.github/workflows/release.yml` supplies the install, chart, image, and
verification sections from a template, and `generate_release_notes: true`
appends the `## What's Changed` list grouped by the label categories in
`.github/release.yml`.

The output is a draft. The author hand-edits it, then pastes it into the
release body between the intro paragraph (ending "…full feature list.") and
`## Install nvcrectl`.

## When to Use

- A tag is about to be cut and the maintainer needs the highlights block
- A release draft exists and needs its highlights section filled in
- User asks to draft release notes, a release summary, or an announcement
- User invokes `/release-notes`

Do NOT use this skill to cut a tag, create or publish a release, or edit any
file in the repository. It writes one Markdown draft to a temp file.

**Never publish a draft release.** A draft is a release the `Verify release`
job determined it could not verify. Editing a draft's *body* is fine and is
what this skill's output is for; flipping it public from the UI bypasses the
gate entirely. See [RELEASE.md](../../../RELEASE.md).

## Inputs

`gh api .../releases/generate-notes` is the single source of truth for:

- The set of changes in the range.
- The pull request number and link for each one.
- The author handle for each one, rendered as `by @handle`.
- The label grouping the release body will actually use.

It returns exactly what GitHub will render below the summary, so a highlight
that has no line in it does not belong in the summary. Fetch it once into
`$NOTES` and read every later fact out of that. Do NOT re-derive any of it
with `git log`, `gh pr list`, or a call to a different endpoint.

### Resolving the range

Default: the latest stable release through `main`. The tag being cut does not
exist yet, so name it and pass `target_commitish`.

Both paths bind the same three variables, and later steps depend on all
three. `TAG` is empty on the default path — there is no tag yet — and later
steps read it as `${TAG:-HEAD}`.

```bash
REPO=NVIDIA/cluster-readiness-engine
TAG=
PREV=$(gh release list --repo "$REPO" --exclude-drafts --exclude-pre-releases \
  --limit 1 --json tagName --jq '.[0].tagName')

[ -n "$PREV" ] || { echo "no stable release to compare against"; exit 1; }
```

If the user names a tag that already exists (backfilling highlights onto a
release that shipped), use it as `tag_name` and the stable release
immediately *before that tag* as `previous_tag_name`. Find it by position in
the list, not by index:

```bash
TAG=v0.2.0   # the tag the user named
PREV=$(gh release list --repo "$REPO" --exclude-drafts --exclude-pre-releases \
  --limit 100 --json tagName --jq '.[].tagName' \
  | awk -v tag="$TAG" 'found { print; exit } $0 == tag { found = 1 }')
```

Taking the second entry of the list (`--limit 2 --jq '.[1].tagName'`) is
wrong for every tag except the current latest: asked to backfill `v0.2.0`
when `v0.3.0` has shipped, it returns `v0.2.0` itself and compares the tag
against itself.

**Both paths must check `$PREV` before using it.** An empty `$PREV` means the
named tag is the oldest stable release, is not a stable release at all, or
none exists yet. Passing it on sends `previous_tag_name=""` to the API and
`gh release view ""` in Step 1, neither of which fails in a way you will
notice. Stop and ask the user for the range instead.

Once the range is bound, fetch the notes **once** and reuse them. Both paths
converge here, and every later step reads `$NOTES` rather than calling the
endpoint again:

```bash
if [ -n "$TAG" ]; then
  NOTES=$(gh api "repos/$REPO/releases/generate-notes" \
    -f tag_name="$TAG" -f previous_tag_name="$PREV" --jq '.body')
else
  NOTES=$(gh api "repos/$REPO/releases/generate-notes" \
    -f tag_name=v0.0.0-draft -f target_commitish=main \
    -f previous_tag_name="$PREV" --jq '.body')
fi
```

Never guess a range. Test emptiness on the change lines themselves:

```bash
printf '%s\n' "$NOTES" | grep -cE '^\* .* by @'
```

Zero means the range is empty — stop and ask the user which range to
summarize rather than inventing one. Do not test with `grep '^\*'`: an empty
range still returns a `**Full Changelog**` footer, which starts with `*` and
is always present, so the naive test reports one line and you proceed on
nothing.

The body never names the new tag; GitHub renders it in the release header.

## Procedure

### Step 1 — Gather raw material

`$NOTES` is already populated by the range resolution above. Read every line
of it. Then read the previous stable release's body for the house voice:

```bash
gh release view "$PREV" --repo "$REPO" --json body --jq '.body'
```

Read past the generated template — the voice to match is the intro paragraph
and the prose in the verification sections: declarative, specific, no
marketing register.

### Step 2 — Classify into themes

Group by **what a user can now do, not by what the repository now does**.
Certifying a cluster is the product; the machinery that builds and ships it
is not. Two changes of equal engineering weight do not get equal billing.

**Capability themes.** These lead:

1. **Certification coverage** — new catalog categories (`domain`/`variant`),
   GPU architectures, CSP platforms, orchestration or scheduling targets.
   The headline for most releases. Use a bulleted sub-list at 3+.
2. **Controller behavior** — changes across the Certification → Workflow →
   Job tiers: failure detection, node health, checkpoint restart, how failed
   nodes are reported.
3. **Measurement** — goodput, bandwidth and NCCL parsing, `LogProfile`
   patterns, thresholds.

**Supporting themes.** Real, but subordinate:

4. **Supply chain and release integrity** — signing, SBOMs, provenance,
   installer verification, vulnerability scanning.
5. **Docs and DX** — `nvcrectl` ergonomics, the Fern docs site, an ADR that
   changes how someone should use the project.
6. **Other improvements** — leftover user-visible wins.

**Ordering is a rule, not a preference.** If anything in 1–3 shipped, it
opens the summary and it is what the opening sentence names. Never open with
tooling. Collapse supply-chain and release work into **one** block however
many commits it took — twelve pipeline commits are one theme, not three.

If that leaves the tooling block looking small next to the effort it
represents, that is the intended outcome. Effort is not the unit; what the
reader can now do is. A release whose engineering was mostly internal is a
release with short highlights, and writing it that way is honest.

**A deliberate focus is context, not a shortfall.** When a release
concentrated on a supporting theme on purpose — a cycle spent closing supply
chain gaps, say — name that intent in the block's opening sentence. Do not
apologize for the shape of the release, and do not raise it as a question for
the author: they chose it.

**Do not delete tooling that changed a user-facing guarantee.** A change that
lets someone verify something they could not verify before belongs in the
supporting block even though it only touched `.github/workflows/`. A change
that reshuffles a job without changing what anyone downstream can observe
does not belong at all.

Exclude from the narrative — they still appear in `## What's Changed` below
the summary:

- `build(deps):` bumps (already their own **Dependency Updates** section)
- CI changes with no observable effect outside the repository
- Pure refactors, unless the cumulative effect is a public surface change
  worth flagging
- Test-only changes, unless the test *is* the deliverable (a gate that now
  provably rejects something it previously accepted)
- Doc-style fixups and golden-file regeneration

### Step 3 — Check the three unstable surfaces

`RELEASE.md` puts the project at `v0.x` and names three surfaces that can
still change in a minor release: **CRD schemas**, the **`nvcrectl` command
line**, and **Helm values**. It also commits to calling breaking changes out
in the release notes.

Check them directly:

**The CLI is not under `cmd/nvcrectl`.** That directory holds only `main.go`,
which wires subcommands together and defines no flags. Every command and flag
lives in a `pkg/` package — `pkg/certification`, `pkg/cluster`, `pkg/render`,
`pkg/setup`, `pkg/workloadrun` today. Derive the list rather than pasting it,
so a command package added later cannot fall outside the check:

```bash
SURFACES=$(grep -rl 'cobra.Command{' --include='*.go' cmd/nvcrectl pkg)
SURFACES="api/v1alpha1 helm/cluster-readiness-engine/values.yaml $SURFACES"

git diff --stat "$PREV..${TAG:-HEAD}" -- $SURFACES
```

Filtering on `cmd/nvcrectl` alone reports no hits when a release removes a
flag, because the file that defined it is somewhere else. The check would
pass and the required section would never be written.

`${TAG:-HEAD}` matters on the backfill path: diffing to `HEAD` would answer
for today's `main` rather than for the release being summarized, which is a
different and possibly much larger range.

Expect this `git diff` to fail with `bad revision` more often than not, on
either path. It needs both endpoints as local refs, and `$PREV` is the newest
stable release — routinely cut after your last fetch, so it is missing
locally even on the default path. Do not `git fetch` to fix it. Ask GitHub
for the same comparison, which needs no local ref:

```bash
gh api "repos/$REPO/compare/$PREV...${TAG:-main}" --jq '.files[].filename' \
  | grep -Ff <(printf '%s\n' $SURFACES)
```

Then read the patch for each hit with
`--jq '.files[] | select(.filename=="<path>") | .patch'`.

`$SURFACES` is derived from the working tree, not from either tag. That is
the intent — it answers "where does the CLI live now" — but it means a
command package deleted during the range will not appear in it. A deletion is
a breaking change you should already be describing.

A non-empty diff is a prompt to look, not proof of a break — most changes to
these paths are additive. Read the diff and decide.

If anything in the range removes or changes the meaning of a field, a flag,
or a value, a `### Breaking changes` section is **required**: one bullet per
item giving what changed, what to do instead, and what happens to an existing
manifest that does not change. Omit the heading entirely when nothing broke.
Do not soften a break into an "improvement" bullet.

### Step 4 — Build the contributor list

The thanks line comes entirely from the `by @handle` annotations already in
the `generate-notes` output:

```bash
printf '%s\n' "$NOTES" \
  | grep -oE 'by @[^ ]+' \
  | sed 's/by @//' \
  | sort -uf \
  | grep -viE '\[bot\]$'
```

The `[^ ]+` capture runs to the next space, so `dependabot[bot]` comes out
intact and the final `grep` drops it. Keep both halves — narrowing the
capture to `[A-Za-z0-9-]+` would truncate bot handles to `dependabot` and
leak them into the credits.

Then:

- Sort alphabetically, case-insensitive. No handle gets a reserved position.
- Do NOT link the @-mentions — GitHub auto-links them.

The generated notes also carry a **New Contributors** section. Leave it
alone; the credits line names everyone, including returning contributors,
and the duplication is intentional.

### Step 5 — Write the draft

Write to `$TMPDIR/nvcre-release-notes.md` — fixed filename, no version
suffix, overwrite any prior draft. Do NOT write under the repo tree; this is
a hand-edit draft, not a checked-in artifact.

**Append an "Unresolved questions for hand-edit" section**, separated from
the credits line by a horizontal rule. The author edits the file directly, so
the questions belong in the file, not in chat:

```markdown
---

## Unresolved questions for hand-edit

1. **<topic>** — <one or two sentences explaining the call to make>
2. **<topic>** — <…>
```

Raise only genuine calls: emphasis the rules above do not settle, things to
verify before publishing, and omissions where the call was close. Do not
re-litigate a decision the rules already made — folding supply-chain detail
into one block is the rule, not a question, and listing it as one spends the
author's attention on something already correct.

The section is deleted before the notes are pasted in.

After writing, print to chat:

1. A ready-to-run macOS clipboard command, on its own line in a fenced bash
   block: `pbcopy < <absolute-path>`. That line *is* the path — do not also
   print the bare path.
2. One line naming the themes the draft surfaced, so the user can tell at a
   glance whether something is missing.

Do NOT cat the draft back into chat, and do NOT restate the unresolved
questions — they are already in the file.

## Output Format Reference

```markdown
## Highlights

This release focuses on <theme-1-bolded>, <theme-2-bolded>, and <theme-3-bolded>.

**<Theme 1 Title>** — <1–3 sentences on what shipped and why it matters to
someone certifying a cluster. Commands in backticks: `nvcrectl certification
render`. Pull requests as [#NNN](https://github.com/NVIDIA/cluster-readiness-engine/pull/NNN).>

**<Theme with enumerated items>**

* <Concrete item 1>
* <Concrete item 2>
* <Concrete item 3>

**Other improvements**

* <Leftover user-visible win>

### Breaking changes

* <What changed> — <what to do instead>. <What happens to a manifest that
  does not change.>

***Thanks to*** @alice, @bob, and @carol.
```

Every heading above is conditional. Omit any section with no content —
no breaking changes, no `### Breaking changes`; nothing left over, no
**Other improvements**. An empty heading reads as an oversight, and a
heading kept alive by a padded bullet is worse than either.

Style rules, drawn from the repository's own prose:

- Pull request references use `[#NNN](https://github.com/NVIDIA/cluster-readiness-engine/pull/NNN)`
  — the short label because the reader is already on the repository, the full
  URL so the link survives being quoted somewhere else. Take the number from
  the `generate-notes` line; never write a bare `#NNN`.
- Backtick CLI commands, CRD kinds, field paths, and image or chart names.
- Em dash (` — `, with spaces) for the inline definition pattern.
- No emoji. No `## What's Changed` heading — the workflow appends its own. No
  version-comparison link; GitHub adds it.
- Say what a user can now do, not what a commit did. "The installer verifies
  what it installs" over "added verification to the installer".
- Keep the summary to roughly 250–400 words, excluding the generated sections.

## Failure Modes

- **`generate-notes` returns a body with no change lines** — the range is
  empty, usually because the tag already shipped. Ask the user which range to
  summarize.
- **`gh release list` returns nothing** — no stable release exists yet. Ask
  the user for the range; do not fall back to the first commit.
- **The previous release has no body worth mirroring** — fall back to the
  README's opening and the prose in `RELEASE.md` for voice.
- **Uncommitted changes in the working tree** — harmless. Nothing here reads
  the working tree except the `git diff` in Step 3, which is tag-to-`HEAD`.

## What This Skill Does NOT Do

- Does not run `git tag` or push a tag
- Does not create, edit, or publish a GitHub release, and never publishes a
  draft
- Does not edit `.github/workflows/release.yml`, `.github/release.yml`, or
  any other file in the repository
- Does not re-derive the change list or author handles outside the
  `generate-notes` output
