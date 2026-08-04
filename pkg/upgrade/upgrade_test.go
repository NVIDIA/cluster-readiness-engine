// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSemanticVersion(t *testing.T) {
	tests := []struct {
		input   string
		want    semanticVersion
		wantErr bool
	}{
		{
			input: "v1.2.3",
			want:  semanticVersion{Major: 1, Minor: 2, Patch: 3, Original: "v1.2.3", HasVPrefix: true},
		},
		{
			input: "1.2.3",
			want:  semanticVersion{Major: 1, Minor: 2, Patch: 3, Original: "1.2.3", HasVPrefix: false},
		},
		{
			input: "v0.18.3-5-g20a7053-dirty",
			want:  semanticVersion{Major: 0, Minor: 18, Patch: 3, Original: "v0.18.3-5-g20a7053-dirty", HasVPrefix: true},
		},
		{
			input: "v1.0.0-rc1",
			want:  semanticVersion{Major: 1, Minor: 0, Patch: 0, Original: "v1.0.0-rc1", HasVPrefix: true},
		},
		{
			input:   "dev",
			wantErr: true,
		},
		{
			input:   "v1.2",
			wantErr: true,
		},
		{
			input:   "not-a-version",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseSemanticVersion(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want.Major, got.Major)
			assert.Equal(t, tt.want.Minor, got.Minor)
			assert.Equal(t, tt.want.Patch, got.Patch)
			assert.Equal(t, tt.want.HasVPrefix, got.HasVPrefix)
			assert.Equal(t, tt.want.Original, got.Original)
		})
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		name   string
		a, b   string
		expect bool
	}{
		{"major bump", "v2.0.0", "v1.9.9", true},
		{"minor bump", "v1.3.0", "v1.2.9", true},
		{"patch bump", "v1.2.4", "v1.2.3", true},
		{"equal", "v1.2.3", "v1.2.3", false},
		{"older major", "v1.0.0", "v2.0.0", false},
		{"older minor", "v1.1.0", "v1.2.0", false},
		{"older patch", "v1.2.2", "v1.2.3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := parseSemanticVersion(tt.a)
			require.NoError(t, err)
			b, err := parseSemanticVersion(tt.b)
			require.NoError(t, err)
			assert.Equal(t, tt.expect, a.isNewer(b))
		})
	}
}

func TestGenerateBinaryName(t *testing.T) {
	name := generateBinaryName()
	// Should match ncrectl-{os}-{arch} pattern
	assert.True(t, strings.HasPrefix(name, "ncrectl-"),
		"expected prefix ncrectl-, got %s", name)
	parts := strings.Split(name, "-")
	assert.Len(t, parts, 3, "expected ncrectl-os-arch, got %s", name)
}

func TestSemanticVersionString(t *testing.T) {
	v := semanticVersion{Major: 1, Minor: 2, Patch: 3, HasVPrefix: true}
	assert.Equal(t, "v1.2.3", v.String())

	v2 := semanticVersion{Major: 0, Minor: 1, Patch: 0, HasVPrefix: false}
	assert.Equal(t, "0.1.0", v2.String())
}

func TestPromptYesNo(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{name: "y", input: "y\n", expected: true},
		{name: "Y", input: "Y\n", expected: true},
		{name: "yes", input: "yes\n", expected: true},
		{name: "YES", input: "YES\n", expected: true},
		{name: "n", input: "n\n", expected: false},
		{name: "no", input: "no\n", expected: false},
		{name: "empty", input: "\n", expected: false},
		{name: "eof", input: "", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := promptYesNo(strings.NewReader(tt.input))
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ---------------------------------------------------------------------------
// Tests for the kubectl plugin symlink (ADR-067)
// ---------------------------------------------------------------------------

func TestEnsureKubectlPlugin(t *testing.T) {
	linkPath := func(dir string) string { return filepath.Join(dir, kubectlPluginName) }

	t.Run("creates symlink when absent", func(t *testing.T) {
		dir := t.TempDir()

		ensureKubectlPlugin(dir)

		target, err := os.Readlink(linkPath(dir))
		require.NoError(t, err)
		assert.Equal(t, binaryName, target)
	})

	t.Run("refreshes a stale symlink pointing elsewhere", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Symlink("something-else", linkPath(dir)))

		ensureKubectlPlugin(dir)

		target, err := os.Readlink(linkPath(dir))
		require.NoError(t, err)
		assert.Equal(t, binaryName, target)
	})

	t.Run("replaces a stale regular file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(linkPath(dir), []byte("not a symlink"), 0o755))

		ensureKubectlPlugin(dir)

		target, err := os.Readlink(linkPath(dir))
		require.NoError(t, err)
		assert.Equal(t, binaryName, target)
	})

	t.Run("skipped on windows", func(t *testing.T) {
		dir := t.TempDir()
		saved := goos
		goos = "windows"
		defer func() { goos = saved }()

		ensureKubectlPlugin(dir)

		_, err := os.Lstat(linkPath(dir))
		assert.True(t, os.IsNotExist(err), "expected no symlink to be created on windows")
	})
}

// ---------------------------------------------------------------------------
// Tests for forced upgrade check
// ---------------------------------------------------------------------------

func TestIsReleaseBuild(t *testing.T) {
	tests := []struct {
		version  string
		expected bool
	}{
		{"1.20.0", true},
		{"v1.20.0", true},
		{"0.0.1", true},
		{"1.20.0-4-gHASH", false},
		{"1.20.0-dirty", false},
		{"1.20.0-4-gHASH-dirty", false},
		{"v1.0.0-rc1", false},
		{"dev", false},
		{"", false},
		{"not-a-version", false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsReleaseBuild(tt.version),
				"IsReleaseBuild(%q)", tt.version)
		})
	}
}

func TestEnforceUpgrade(t *testing.T) {
	// Helper: create a test GitHub Releases API server returning a specific tag.
	newTagServer := func(tag string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := struct {
				TagName string `json:"tag_name"`
			}{TagName: tag}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
	}

	// Helper: swap the GitHub API base URL to point at a test server.
	// Returns a cleanup function.
	// We can't change the const, but we can swap httpClient to a client
	// that intercepts requests.
	swapClient := func(server *httptest.Server) func() {
		saved := httpClient
		savedCheck := upgradeCheckClient
		// Create a transport that redirects GitHub API calls to our test server.
		httpClient = server.Client()
		upgradeCheckClient = server.Client()
		// Monkey-patch: the fetchLatestVersion function uses githubAPIBase const,
		// so we need to intercept at the transport level.
		transport := &testTransport{
			server:  server,
			wrapped: http.DefaultTransport,
		}
		httpClient = &http.Client{Transport: transport}
		upgradeCheckClient = &http.Client{Transport: transport}
		return func() {
			httpClient = saved
			upgradeCheckClient = savedCheck
			server.Close()
		}
	}

	t.Run("up to date returns nil", func(t *testing.T) {
		server := newTagServer("1.20.0")
		cleanup := swapClient(server)
		defer cleanup()

		var buf bytes.Buffer
		err := EnforceUpgrade("1.20.0", &buf)
		assert.NoError(t, err)
		assert.Empty(t, buf.String())
	})

	t.Run("newer on server returns nil", func(t *testing.T) {
		// Current version is newer than server — no error.
		server := newTagServer("1.19.0")
		cleanup := swapClient(server)
		defer cleanup()

		var buf bytes.Buffer
		err := EnforceUpgrade("1.20.0", &buf)
		assert.NoError(t, err)
	})

	t.Run("outdated returns error", func(t *testing.T) {
		server := newTagServer("2.0.0")
		cleanup := swapClient(server)
		defer cleanup()

		var buf bytes.Buffer
		err := EnforceUpgrade("1.20.0", &buf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outdated")
		assert.Contains(t, buf.String(), "1.20.0")
		assert.Contains(t, buf.String(), "2.0.0")
		assert.Contains(t, buf.String(), "ncrectl upgrade")
	})

	t.Run("network error warns and proceeds", func(t *testing.T) {
		// Point to a closed server.
		server := newTagServer("2.0.0")
		cleanup := swapClient(server)
		server.Close() // force connection refused
		defer cleanup()

		var buf bytes.Buffer
		err := EnforceUpgrade("1.20.0", &buf)
		assert.NoError(t, err, "should proceed on network error")
		assert.Contains(t, buf.String(), "Warning")
	})

	t.Run("dev version returns nil", func(t *testing.T) {
		var buf bytes.Buffer
		err := EnforceUpgrade("dev", &buf)
		assert.NoError(t, err)
		assert.Empty(t, buf.String())
	})
}

// testTransport redirects all requests to the test server regardless of host.
type testTransport struct {
	server  *httptest.Server
	wrapped http.RoundTripper
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Redirect to test server.
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	// Strip the GitHub API prefix, keep the path.
	req.URL.Path = strings.TrimPrefix(req.URL.Path,
		fmt.Sprintf("/repos/%s", githubRepo))
	return t.wrapped.RoundTrip(req)
}
