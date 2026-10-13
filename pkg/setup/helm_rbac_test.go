// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// --set arguments that pin rbac.scope.
const (
	scopeCluster    = "rbac.scope=cluster"
	scopeNamespaced = "rbac.scope=namespaced"
)

// TestReleaseRBACSetArgs covers what `setup init` passes to helm upgrade for
// rbac.*: the installed release's values carried forward, --rbac-scope over
// them, and nothing at all on a first install so the chart defaults apply.
// Without it, init's --set for the image makes Helm drop every earlier value
// and reset rbac.scope (ADR-092).
func TestReleaseRBACSetArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		userValues string // helm get values -o json
		scopeFlag  string
		want       []string
	}{
		{
			name:       "first install keeps chart defaults",
			userValues: "null",
		},
		{
			name:       "release without rbac values",
			userValues: `{"manager":{"image":{"tag":"v0.8.0"}}}`,
		},
		{
			name:       "namespaced scope and run namespaces are kept",
			userValues: `{"rbac":{"scope":"namespaced","runNamespaces":["team-a","team-b"]}}`,
			want: []string{
				helmFlagSet, scopeNamespaced,
				helmFlagSetJSON, `rbac.runNamespaces=["team-a","team-b"]`,
			},
		},
		{
			name:       "explicit cluster pin is kept once the default flips",
			userValues: `{"rbac":{"scope":"cluster"}}`,
			want:       []string{helmFlagSet, scopeCluster},
		},
		{
			name:       "an explicitly empty run namespace list is kept",
			userValues: `{"rbac":{"scope":"namespaced","runNamespaces":[]}}`,
			want: []string{
				helmFlagSet, scopeNamespaced,
				helmFlagSetJSON, "rbac.runNamespaces=[]",
			},
		},
		{
			name:       "flag overrides the installed scope and keeps run namespaces",
			userValues: `{"rbac":{"scope":"namespaced","runNamespaces":["team-a"]}}`,
			scopeFlag:  rbacScopeCluster,
			want: []string{
				helmFlagSet, scopeCluster,
				helmFlagSetJSON, `rbac.runNamespaces=["team-a"]`,
			},
		},
		{
			name:       "flag on a first install",
			userValues: "",
			scopeFlag:  rbacScopeNamespaced,
			want:       []string{helmFlagSet, scopeNamespaced},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installed, err := parseReleaseRBACValues([]byte(tc.userValues))
			require.NoError(t, err)
			got, err := resolveRBACValues(installed, tc.scopeFlag).setArgs()
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestValidateRBACScope(t *testing.T) {
	for scope, ok := range map[string]bool{
		"":              true,
		"cluster":       true,
		"namespaced":    true,
		"per-namespace": false,
		"Cluster":       false,
	} {
		err := validateRBACScope(scope)
		require.Equal(t, ok, err == nil, "scope %q: %v", scope, err)
	}
}

// TestReadReleaseRBACValues runs the read against a fake helm. Only a release
// Helm has no record of counts as a first install; any other failure stops
// init, because upgrading without the installed values would reset rbac.scope.
func TestReadReleaseRBACValues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		want    rbacValues
		wantErr string
	}{
		{
			name:   "installed values",
			script: "printf '%s' '{\"rbac\":{\"scope\":\"namespaced\",\"runNamespaces\":[\"team-a\"]}}'",
			want:   rbacValues{Scope: "namespaced", RunNamespaces: []string{runNS}, runNamespacesSet: true},
		},
		{
			name:   "no release yet",
			script: "printf 'Error: release: not found\\n' >&2; exit 1",
		},
		{
			name:    "cluster unreachable",
			script:  "printf 'Error: Kubernetes cluster unreachable\\n' >&2; exit 1",
			wantErr: "Kubernetes cluster unreachable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helm := filepath.Join(t.TempDir(), "helm")
			require.NoError(t, os.WriteFile(helm, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755))
			got, err := readReleaseRBACValues(helm, "", "")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestReleaseRBACArgsReportsWhatItKeeps pins init's output, so an operator sees
// that a re-run kept their scope rather than wondering whether it reset it.
func TestReleaseRBACArgsReportsWhatItKeeps(t *testing.T) {
	helm := filepath.Join(t.TempDir(), "helm")
	require.NoError(t, os.WriteFile(helm, []byte(
		"#!/bin/sh\nprintf '%s' '{\"rbac\":{\"scope\":\"namespaced\",\"runNamespaces\":[\"team-a\"]}}'\n"), 0o755))

	var out bytes.Buffer
	args, err := releaseRBACArgs(helm, "", "", rbacScopeCluster, &out)
	require.NoError(t, err)
	require.Equal(t, []string{helmFlagSet, scopeCluster, helmFlagSetJSON, `rbac.runNamespaces=["team-a"]`}, args)
	require.Equal(t,
		"[helm] Keeping rbac.scope=namespaced rbac.runNamespaces=[team-a] from the installed release.\n"+
			"[helm] Setting rbac.scope=cluster (--rbac-scope).\n",
		out.String())
}
