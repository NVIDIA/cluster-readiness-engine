// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	githubRepo        = "NVIDIA/cluster-readiness-engine"
	githubAPIBase     = "https://api.github.com/repos/" + githubRepo
	githubReleaseBase = "https://github.com/" + githubRepo + "/releases/download"
	binaryName        = "ncrectl"
	kubectlPluginName = "kubectl-ncrectl"
)

// semanticVersion represents a parsed semantic version.
type semanticVersion struct {
	Major      int
	Minor      int
	Patch      int
	Original   string
	HasVPrefix bool
}

func (v semanticVersion) String() string {
	if v.HasVPrefix {
		return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// isNewer returns true if v is a newer version than other.
func (v semanticVersion) isNewer(other semanticVersion) bool {
	if v.Major != other.Major {
		return v.Major > other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor > other.Minor
	}
	return v.Patch > other.Patch
}

// httpClient is the HTTP client used for API calls. Replaceable for testing.
var httpClient = http.DefaultClient

// goos mirrors runtime.GOOS. Replaceable for testing ensureKubectlPlugin's
// Windows skip without needing a build-tag-specific test variant.
var goos = runtime.GOOS

// upgradeCheckClient has a short timeout so the forced upgrade check
// doesn't block on slow or unreachable networks.
var upgradeCheckClient = &http.Client{Timeout: 5 * time.Second}

// IsReleaseBuild returns true if the version string is a clean semver
// from the release pipeline (e.g., "1.20.0"), not a dev build
// (e.g., "1.20.0-4-gHASH-dirty" or "dev").
func IsReleaseBuild(v string) bool {
	sv, err := parseSemanticVersion(v)
	if err != nil {
		return false
	}
	return sv.Original == sv.String()
}

// EnforceUpgrade checks if a newer version exists and returns an error
// (causing exit code 1) if the current release build is outdated.
// On network failure, it prints a warning and returns nil (proceeds).
func EnforceUpgrade(currentVersion string, out io.Writer) error {
	current, err := parseSemanticVersion(currentVersion)
	if err != nil {
		return nil // not a release build, skip
	}

	// Use the short-timeout client for the check.
	saved := httpClient
	httpClient = upgradeCheckClient
	latestTag, fetchErr := fetchLatestVersion()
	httpClient = saved

	if fetchErr != nil {
		_, _ = fmt.Fprintf(out,
			"Warning: unable to check for updates (%v). Proceeding.\n", fetchErr)
		return nil
	}

	latest, err := parseSemanticVersion(latestTag)
	if err != nil {
		_, _ = fmt.Fprintf(out,
			"Warning: unable to parse latest version %q. Proceeding.\n", latestTag)
		return nil
	}

	if !latest.isNewer(current) {
		return nil // up to date
	}

	_, _ = fmt.Fprintf(out,
		"\nYour ncrectl version %s is outdated. Latest: %s\n"+
			"Run 'ncrectl upgrade' to update.\n\n",
		current.String(), latest.String())
	return fmt.Errorf("ncrectl %s is outdated (latest: %s)",
		current.String(), latest.String())
}

// NewCommand returns the "upgrade" cobra command. version is the running
// binary version and is threaded through to runUpgrade.
func NewCommand(version string) *cobra.Command {
	var checkOnly bool

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade ncrectl to the latest version",
		Long: `Checks for a newer version of ncrectl and installs it in-place.

Compares the running version against the latest release on GitHub,
shows release notes, and prompts before upgrading.

Use --check to only check for updates without installing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgrade(version, checkOnly, os.Stdin, os.Stderr)
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false,
		"Only check for updates, do not install")

	return cmd
}

// runUpgrade checks for and optionally installs a newer version.
func runUpgrade(version string, checkOnly bool, in io.Reader, out io.Writer) error {
	currentVersion, err := parseSemanticVersion(version)
	if err != nil {
		return fmt.Errorf("cannot parse current version %q: %w", version, err)
	}

	_, _ = fmt.Fprintf(out, "Current version: %s\n", version)
	_, _ = fmt.Fprintln(out, "Checking for updates...")

	// Fetch latest version from GitHub.
	latestTag, err := fetchLatestVersion()
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}

	latestVersion, err := parseSemanticVersion(latestTag)
	if err != nil {
		return fmt.Errorf("parse latest version %q: %w", latestTag, err)
	}

	if !latestVersion.isNewer(currentVersion) {
		_, _ = fmt.Fprintln(out, "Already up to date.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "\nNew release available: %s\n", latestTag)

	// Show release notes if available.
	if notes, notesErr := fetchReleaseNotes(latestTag); notesErr == nil && notes != "" {
		_, _ = fmt.Fprintf(out, "\n")
		renderMarkdown(out, notes)
	}

	if checkOnly {
		_, _ = fmt.Fprintln(out, "\nUpdate available.")
		return nil
	}

	// Prompt for confirmation.
	_, _ = fmt.Fprintf(out, "\nWould you like to install the update? [y/N]: ")
	if !promptYesNo(in) {
		_, _ = fmt.Fprintln(out, "Upgrade cancelled.")
		return nil
	}

	// Download and install.
	_, _ = fmt.Fprintf(out, "\nDownloading ncrectl %s...\n", latestTag)

	tmpDir, err := os.MkdirTemp("", "ncrectl-upgrade-*")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	binName := generateBinaryName()
	tmpPath := filepath.Join(tmpDir, binName)

	if err := downloadBinary(latestTag, binName, tmpPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	if err := installBinary(tmpPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	_, _ = fmt.Fprintf(out, "ncrectl upgraded successfully to %s.\n", latestTag)
	return nil
}

// promptYesNo reads a single line and returns true if it starts with 'y' or 'Y'.
func promptYesNo(in io.Reader) bool {
	buf := make([]byte, 256)
	n, err := in.Read(buf)
	if err != nil || n == 0 {
		return false
	}
	answer := strings.TrimSpace(string(buf[:n]))
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
}

// fetchLatestVersion queries the GitHub Releases API and returns the latest release tag.
func fetchLatestVersion() (string, error) {
	url := githubAPIBase + "/releases/latest"

	resp, err := httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("decode release response: %w", err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("no release found")
	}

	return release.TagName, nil
}

// fetchReleaseNotes returns the release body for a given tag from GitHub Releases.
func fetchReleaseNotes(tag string) (string, error) {
	url := githubAPIBase + "/releases/tags/" + tag

	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return release.Body, nil
}

// downloadBinary downloads a ncrectl binary from GitHub Releases.
func downloadBinary(tag, binName, destPath string) error {
	url := fmt.Sprintf("%s/%s/%s", githubReleaseBase, tag, binName)

	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write binary: %w", err)
	}

	return nil
}

// generateBinaryName returns the platform-specific binary name.
// Matches the CI naming convention: ncrectl-{os}-{arch}
func generateBinaryName() string {
	return fmt.Sprintf("%s-%s-%s", binaryName, runtime.GOOS, runtime.GOARCH)
}

// installBinary moves the downloaded binary to the same directory as the running binary.
// Falls back to sudo if a direct move fails with a permission error.
func installBinary(srcPath string) error {
	// Find where the current binary lives.
	currentPath, err := exec.LookPath(binaryName)
	if err != nil {
		return fmt.Errorf(
			"%s not found in PATH: cannot determine install location", binaryName)
	}
	installDir := filepath.Dir(currentPath)
	destPath := filepath.Join(installDir, binaryName)

	// Try direct rename first (fast path, works if writable).
	if err := os.Rename(srcPath, destPath); err == nil {
		ensureKubectlPlugin(installDir)
		return nil
	}

	// Fallback: sudo mv (for /usr/local/bin etc.)
	cmd := exec.Command("sudo", "mv", srcPath, destPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo mv failed: %w", err)
	}

	ensureKubectlPlugin(installDir)
	return nil
}

// ensureKubectlPlugin (re)creates the kubectl-ncrectl plugin symlink next to
// the installed ncrectl binary so "kubectl ncrectl ..." keeps working after
// an upgrade (see ADR-067). The symlink targets the relative filename
// "ncrectl", not an absolute path or inode, so it keeps resolving correctly
// even after this same function replaces the binary content at that path on
// a future upgrade.
//
// This mirrors installBinary's own sudo-fallback pattern (live stdout/stderr
// passthrough so a password prompt or error is visible immediately), but
// never fails the upgrade: the plugin symlink is a convenience, not a
// required part of installing ncrectl. If it cannot be created even with
// sudo, a warning is printed and the function returns normally.
func ensureKubectlPlugin(installDir string) {
	if goos == "windows" {
		return // symlinks require elevated privileges on Windows; out of scope
	}

	linkPath := filepath.Join(installDir, kubectlPluginName)
	_ = os.Remove(linkPath) // best-effort; clears any stale file/symlink

	if err := os.Symlink(binaryName, linkPath); err == nil {
		return
	}

	cmd := exec.Command("sudo", "ln", "-sf", binaryName, linkPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr,
			"Warning: could not set up 'kubectl ncrectl' plugin symlink: %v\n", err)
	}
}

// parseSemanticVersion parses a version string like "v1.2.3" or "1.2.3".
// Pre-release suffixes (e.g., "-dirty", "-4-gabcdef") are stripped.
func parseSemanticVersion(v string) (semanticVersion, error) {
	sv := semanticVersion{Original: v}

	s := v
	if strings.HasPrefix(s, "v") {
		sv.HasVPrefix = true
		s = s[1:]
	}

	// Strip pre-release suffix (everything after first hyphen in patch).
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return sv, fmt.Errorf("expected major.minor.patch, got %q", v)
	}

	// Patch may have pre-release suffix: "3-dirty" → "3"
	patchStr := parts[2]
	if idx := strings.Index(patchStr, "-"); idx != -1 {
		patchStr = patchStr[:idx]
	}

	var err error
	sv.Major, err = strconv.Atoi(parts[0])
	if err != nil {
		return sv, fmt.Errorf("invalid major version %q: %w", parts[0], err)
	}
	sv.Minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return sv, fmt.Errorf("invalid minor version %q: %w", parts[1], err)
	}
	sv.Patch, err = strconv.Atoi(patchStr)
	if err != nil {
		return sv, fmt.Errorf("invalid patch version %q: %w", patchStr, err)
	}

	return sv, nil
}

// emojiShortcodes maps GitHub/GitLab emoji shortcodes to Unicode characters.
// Covers the conventional-changelog preset used in release notes.
var emojiShortcodes = map[string]string{
	":sparkles:":                  "\u2728",
	":bug:":                       "\U0001F41B",
	":repeat:":                    "\U0001F501",
	":boom:":                      "\U0001F4A5",
	":memo:":                      "\U0001F4DD",
	":rocket:":                    "\U0001F680",
	":recycle:":                   "\u267B\uFE0F",
	":wrench:":                    "\U0001F527",
	":lock:":                      "\U0001F512",
	":construction:":              "\U0001F6A7",
	":white_check_mark:":          "\u2705",
	":fire:":                      "\U0001F525",
	":zap:":                       "\u26A1",
	":heavy_check_mark:":          "\u2714\uFE0F",
	":warning:":                   "\u26A0\uFE0F",
	":package:":                   "\U0001F4E6",
	":arrow_up:":                  "\u2B06\uFE0F",
	":arrow_down:":                "\u2B07\uFE0F",
	":tada:":                      "\U0001F389",
	":art:":                       "\U0001F3A8",
	":ambulance:":                 "\U0001F691",
	":pencil2:":                   "\u270F\uFE0F",
	":hammer:":                    "\U0001F528",
	":wastebasket:":               "\U0001F5D1\uFE0F",
	":twisted_rightwards_arrows:": "\U0001F500",
	":rewind:":                    "\u23EA",
	":pushpin:":                   "\U0001F4CC",
	":bookmark:":                  "\U0001F516",
	":label:":                     "\U0001F3F7\uFE0F",
	":green_heart:":               "\U0001F49A",
	":construction_worker:":       "\U0001F477",
	":chart_with_upwards_trend:":  "\U0001F4C8",
}

// replaceEmojiShortcodes replaces :shortcode: patterns with Unicode emojis.
func replaceEmojiShortcodes(s string) string {
	for code, emoji := range emojiShortcodes {
		s = strings.ReplaceAll(s, code, emoji)
	}
	return s
}

// renderMarkdown writes a simplified terminal rendering of markdown text.
// Handles headers (##), bullet lists (- / *), bold (**), code (`), and
// GitHub/GitLab emoji shortcodes (:sparkles:, :bug:, etc.).
// ANSI escape codes are used for emphasis when the output is a terminal.
func renderMarkdown(out io.Writer, md string) {
	const (
		bold  = "\033[1m"
		dim   = "\033[2m"
		reset = "\033[0m"
	)

	md = replaceEmojiShortcodes(md)

	for line := range strings.SplitSeq(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			_, _ = fmt.Fprintln(out)
			continue
		}

		// Headers: ## Foo → bold "  Foo"
		if strings.HasPrefix(trimmed, "#") {
			text := strings.TrimLeft(trimmed, "# ")
			_, _ = fmt.Fprintf(out, "  %s%s%s\n", bold, text, reset)
			continue
		}

		// Bullet lists: preserve indent, replace - / * with bullet
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			text := renderInline(trimmed[2:], bold, dim, reset)
			_, _ = fmt.Fprintf(out, "  %s%s %s\n", strings.Repeat(" ", indent), dim+"•"+reset, text)
			continue
		}

		// Regular text
		text := renderInline(trimmed, bold, dim, reset)
		_, _ = fmt.Fprintf(out, "  %s\n", text)
	}
}

// renderInline applies inline markdown formatting: **bold** and `code`.
func renderInline(s, bold, dim, reset string) string {
	// Bold: **text** → BOLD text RESET
	for strings.Contains(s, "**") {
		start := strings.Index(s, "**")
		end := strings.Index(s[start+2:], "**")
		if end == -1 {
			break
		}
		end += start + 2
		s = s[:start] + bold + s[start+2:end] + reset + s[end+2:]
	}
	// Inline code: `text` → DIM text RESET
	for strings.Contains(s, "`") {
		start := strings.Index(s, "`")
		end := strings.Index(s[start+1:], "`")
		if end == -1 {
			break
		}
		end += start + 1
		s = s[:start] + dim + s[start+1:end] + reset + s[end+1:]
	}
	return s
}
