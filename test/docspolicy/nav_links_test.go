// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package docspolicy

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	docsDir  = "../../docs"
	repoRoot = "../.."
	navFile  = "index.yml"

	// repoURL is the prefix of links that leave the site for a file in this
	// repository. Pages that are not published, such as the ADRs, are linked
	// this way.
	repoURL = "https://github.com/NVIDIA/cluster-readiness-engine/"
)

// inlineLink matches the target of an inline link or image: `](target)`.
var inlineLink = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// refDefinition matches a reference-style link definition: `[label]: target`.
var refDefinition = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:\s+(\S+)`)

// repoLink matches a link to a file on main in this repository and captures
// the repository-relative path.
var repoLink = regexp.MustCompile(`^` + regexp.QuoteMeta(repoURL) + `(?:blob|tree)/main/([^#?]+)`)

// TestPublishedPagesLinkOnlyToPublishedPages fails when a page in the Fern
// nav links to something the site does not serve.
//
// Fern resolves a relative `.md` link only when the target is itself in the
// nav. Anything else, most often an ADR under docs/designs/, renders as a link
// to a "Page Not Found" page that answers HTTP 200, so neither `fern check`
// nor a link checker that looks at status codes notices. v0.7.0 shipped three
// of these and v0.8.0 eighteen. A page that is deliberately left out of the
// nav is linked by its GitHub URL instead, and those URLs are checked here
// against the tree so a typo in one does not trade a soft 404 for a hard one.
func TestPublishedPagesLinkOnlyToPublishedPages(t *testing.T) {
	nav := navPages(t)
	exists := func(p string) bool {
		_, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(p)))
		return err == nil
	}

	for _, page := range sortedKeys(nav) {
		raw, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(page)))
		if err != nil {
			t.Fatalf("read %s, listed in docs/%s: %v", page, navFile, err)
		}
		for _, problem := range brokenLinks(page, string(raw), nav, exists) {
			t.Errorf("docs/%s: %s", page, problem)
		}
	}
}

// TestBrokenLinksCatchesWhatFernMisses pins the checker itself, so a parsing
// change that stops it seeing links shows up here rather than as a quiet pass
// over the real pages.
func TestBrokenLinksCatchesWhatFernMisses(t *testing.T) {
	nav := map[string]bool{
		"cli-reference/rbac.md":       true,
		"operations/deployment.md":    true,
		"getting-started/install.md":  true,
		"concepts/platform-detect.md": true,
	}
	exists := func(p string) bool { return p == "docs/designs/092-rbac.md" || p == "config/samples/policy" }

	cases := []struct {
		name   string
		body   string
		broken int
	}{
		{name: "relative link to an ADR", body: "See [ADR-092](../designs/092-rbac.md).", broken: 1},
		{name: "relative link to an ADR in a reference definition", body: "[adr]: ../designs/092-rbac.md", broken: 1},
		{name: "relative link to a page not in the nav", body: "[x](./not-in-nav.md)", broken: 1},
		{name: "relative link that leaves docs/", body: "[readme](../../README.md)", broken: 1},
		{name: "relative link to a non-page file", body: "![diagram](./diagram.png)", broken: 1},
		{
			name:   "GitHub link to a file that does not exist",
			body:   "[ADR](" + repoURL + "blob/main/docs/designs/999-nope.md)",
			broken: 1,
		},
		{name: "two broken links on one line", body: "[a](../designs/1.md) and [b](../designs/2.md)", broken: 2},
		{name: "link after a fence closed by a longer run", body: "```\ncode\n````\n[a](../designs/x.md)", broken: 1},
		{name: "link after a backtick a blank line leaves unclosed", body: "a ` b\n\n[a](../designs/x.md) `", broken: 1},
		{name: "link after a fence indented four spaces", body: "    ~~~\n[a](../designs/x.md)", broken: 1},
		{name: "link between escaped backticks", body: "\\` [a](../designs/x.md) \\`", broken: 1},

		{name: "GitHub link to an ADR", body: "[ADR](" + repoURL + "blob/main/docs/designs/092-rbac.md#context)"},
		{name: "GitHub link to a directory", body: "[samples](" + repoURL + "tree/main/config/samples/policy)"},
		{name: "relative link to a nav page", body: "[deploy](../operations/deployment.md)"},
		{name: "relative link to a nav page with an anchor", body: "[deploy](../operations/deployment.md#rbac-requirements)"},
		{name: "relative link to the same directory", body: "[rbac](./rbac.md)"},
		{name: "relative link with a title", body: `[deploy](../operations/deployment.md "Deployment")`},
		{name: "anchor on the same page", body: "[below](#flags)"},
		{name: "external link", body: "[k8s](https://kubernetes.io/docs/)"},
		{name: "link inside inline code", body: "pattern `[a](../designs/x.md)` is literal"},
		{name: "link inside a double-backtick span", body: "``[a](../designs/`x`.md)`` is literal"},
		{name: "link inside a fenced block", body: "```markdown\n[a](../designs/x.md)\n```\n"},
		{name: "link inside a tilde fence", body: "~~~\n[a](../designs/x.md)\n~~~\n"},
		{name: "link inside a longer fence", body: "~~~~\n~~~\n[a](../designs/x.md)\n~~~~\n"},
		{name: "fence line with an info string does not close", body: "```\n```go\n[a](../designs/x.md)\n```\n"},
		{name: "link inside a span after an escaped backslash", body: "\\\\`[a](../designs/x.md)`"},
		{name: "fence line indented four spaces does not close", body: "```\n    ```\n[a](../designs/x.md)\n```\n"},
		{name: "link inside a code span that crosses a line", body: "see `[a](../designs/x.md)\nb` here"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := brokenLinks("cli-reference/rbac.md", c.body, nav, exists)
			if len(got) != c.broken {
				t.Errorf("got %d broken links, want %d: %q", len(got), c.broken, got)
			}
		})
	}
}

// navPages returns every page path in docs/index.yml, relative to docs/.
func navPages(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(docsDir, navFile))
	if err != nil {
		t.Fatalf("read docs/%s: %v", navFile, err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse docs/%s: %v", navFile, err)
	}

	pages := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if p, ok := v["path"].(string); ok && strings.HasSuffix(p, ".md") {
				pages[path.Clean(p)] = true
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)

	if len(pages) == 0 {
		t.Fatalf("docs/%s lists no pages; if the nav format changed, update navPages", navFile)
	}
	return pages
}

// brokenLinks returns a description of each link on page that the published
// site cannot follow. page is relative to docs/; nav holds the published
// pages in the same form; exists reports whether a repository-relative path
// is in the tree.
func brokenLinks(page, body string, nav map[string]bool, exists func(string) bool) []string {
	prose := stripCode(body)

	inline := inlineLink.FindAllStringSubmatch(prose, -1)
	refs := refDefinition.FindAllStringSubmatch(prose, -1)
	targets := make([]string, 0, len(inline)+len(refs))
	for _, m := range inline {
		targets = append(targets, m[1])
	}
	for _, m := range refs {
		targets = append(targets, m[1])
	}

	var out []string
	for _, target := range targets {
		target = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")

		if m := repoLink.FindStringSubmatch(target); m != nil {
			if !exists(strings.TrimSuffix(m[1], "/")) {
				out = append(out, target+" names a file that is not on main")
			}
			continue
		}
		if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") ||
			strings.HasPrefix(target, "#") || strings.HasPrefix(target, "/") {
			continue
		}

		file, _, _ := strings.Cut(target, "#")
		resolved := path.Clean(path.Join(path.Dir(page), file))
		if !nav[resolved] {
			out = append(out, target+" is not a page in docs/"+navFile+
				"; Fern renders it as Page Not Found. Link a page that is not published, "+
				"such as an ADR, by its URL: "+repoURL+"blob/main/docs/"+resolved)
		}
	}
	return out
}

// stripCode blanks fenced blocks and inline code spans, where link syntax is
// literal text rather than a link.
//
// A fence closes only on a run of its own marker at least as long as the one
// that opened it, with nothing after the run but whitespace. Either fence may
// be indented at most three spaces; four makes it indented code. A code span may
// cross a line ending but not a blank line, so spans are stripped a paragraph
// at a time.
func stripCode(body string) string {
	var b, para strings.Builder
	flush := func() {
		b.WriteString(stripCodeSpans(para.String()))
		para.Reset()
	}

	var marker byte
	fenceLen := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		fenceIndent := len(line)-len(trimmed) <= 3
		if fenceLen > 0 {
			if n := runLength(trimmed, 0, marker); fenceIndent && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				fenceLen = 0
			}
			b.WriteString("\n")
			continue
		}
		if fenceIndent && trimmed != "" && (trimmed[0] == '`' || trimmed[0] == '~') {
			if n := runLength(trimmed, 0, trimmed[0]); n >= 3 {
				flush()
				marker, fenceLen = trimmed[0], n
				b.WriteString("\n")
				continue
			}
		}
		para.WriteString(line)
		if strings.TrimSpace(line) == "" {
			flush()
		}
	}
	flush()
	return b.String()
}

// stripCodeSpans removes inline code from a paragraph. A span opened by a run
// of N backticks closes at the next run of exactly N, as CommonMark has it. A
// backslash-escaped backtick is literal and opens nothing.
// Line endings inside a removed span are kept, so line-anchored patterns still
// see the same lines.
func stripCodeSpans(para string) string {
	var b strings.Builder
	for i := 0; i < len(para); {
		if para[i] != '`' || escaped(para, i) {
			b.WriteByte(para[i])
			i++
			continue
		}
		n := runLength(para, i, '`')
		closeAt := -1
		for j := i + n; j < len(para); {
			if para[j] != '`' {
				j++
				continue
			}
			m := runLength(para, j, '`')
			if m == n {
				closeAt = j + m
				break
			}
			j += m
		}
		if closeAt < 0 {
			// An unclosed run is literal backticks.
			b.WriteString(para[i : i+n])
			i += n
			continue
		}
		b.WriteString(strings.Repeat("\n", strings.Count(para[i:closeAt], "\n")))
		i = closeAt
	}
	return b.String()
}

// escaped reports whether s[i] is preceded by an odd number of backslashes.
func escaped(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// runLength counts the run of c starting at s[i].
func runLength(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
