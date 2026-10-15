// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Values for the chart's rbac.scope (ADR-092).
const (
	rbacScopeCluster    = "cluster"
	rbacScopeNamespaced = "namespaced"
)

// rbacValues are the chart's rbac.* values that `setup init` carries from the
// installed release into the upgrade.
//
// init upgrades with --set for the controller image, and Helm reuses a
// release's earlier values only when an upgrade sets none, so without this
// every re-run would reset rbac.* to the chart defaults. An operator who chose
// rbac.scope: namespaced would silently get the cluster-wide grant back; once
// the default becomes namespaced, an explicit rbac.scope: cluster pin would be
// dropped instead, and every run outside a bound namespace would stall.
type rbacValues struct {
	Scope            string   `json:"scope,omitempty"`
	RunNamespaces    []string `json:"runNamespaces,omitempty"`
	runNamespacesSet bool
}

// validateRBACScope checks a --rbac-scope value. Empty means keep the
// installed release's value, or the chart default on a first install.
func validateRBACScope(scope string) error {
	switch scope {
	case "", rbacScopeCluster, rbacScopeNamespaced:
		return nil
	}
	return fmt.Errorf("--rbac-scope must be %q or %q, got %q", rbacScopeCluster, rbacScopeNamespaced, scope)
}

// parseReleaseRBACValues extracts rbac.* from the JSON `helm get values -o
// json` prints: the values the release was installed or upgraded with, not
// the chart defaults. A release that set no rbac values yields the zero value.
func parseReleaseRBACValues(userValues []byte) (rbacValues, error) {
	var values struct {
		RBAC *struct {
			Scope         string    `json:"scope"`
			RunNamespaces *[]string `json:"runNamespaces"`
		} `json:"rbac"`
	}
	trimmed := bytes.TrimSpace(userValues)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return rbacValues{}, nil
	}
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return rbacValues{}, fmt.Errorf("parse the installed release's values: %w", err)
	}
	if values.RBAC == nil {
		return rbacValues{}, nil
	}
	out := rbacValues{Scope: values.RBAC.Scope}
	if values.RBAC.RunNamespaces != nil {
		out.RunNamespaces, out.runNamespacesSet = *values.RBAC.RunNamespaces, true
	}
	return out, nil
}

// resolveRBACValues applies --rbac-scope over the installed release's values.
func resolveRBACValues(installed rbacValues, scopeFlag string) rbacValues {
	if scopeFlag != "" {
		installed.Scope = scopeFlag
	}
	return installed
}

// setArgs renders the values as helm upgrade flags. Only values that are set
// are passed, so a first install keeps the chart defaults. runNamespaces goes
// through --set-json so a namespace name is never parsed as Helm --set syntax.
func (v rbacValues) setArgs() ([]string, error) {
	var args []string
	if v.Scope != "" {
		args = append(args, helmFlagSet, "rbac.scope="+v.Scope)
	}
	if v.runNamespacesSet {
		namespaces := v.RunNamespaces
		if namespaces == nil {
			namespaces = []string{}
		}
		b, err := json.Marshal(namespaces)
		if err != nil {
			return nil, err
		}
		args = append(args, helmFlagSetJSON, "rbac.runNamespaces="+string(b))
	}
	return args, nil
}

// describe says what init is carrying forward, for its output.
func (v rbacValues) describe() string {
	var parts []string
	if v.Scope != "" {
		parts = append(parts, "rbac.scope="+v.Scope)
	}
	if v.runNamespacesSet {
		parts = append(parts, "rbac.runNamespaces=["+strings.Join(v.RunNamespaces, ",")+"]")
	}
	return strings.Join(parts, " ")
}

// readReleaseRBACValues reads rbac.* from the installed NVCRE release. A
// release Helm has no record of is a first install and yields the zero value.
// Any other failure is returned rather than guessed at: upgrading without the
// installed values would silently reset rbac.scope, which is the change this
// read exists to prevent.
func readReleaseRBACValues(helmPath, kubeconfig, kubeContext string) (rbacValues, error) {
	args := []string{helmSubcommandGet, "values", helmReleaseName, helmFlagNamespace, nvcreNamespace, helmFlagOutput, outputJSON}
	args = appendKubeconfigArgs(args, kubeconfig, kubeContext)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(helmPath, args...) // #nosec G204 -- helmPath and args come from this CLI, not from untrusted input
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "release: not found") {
			return rbacValues{}, nil
		}
		return rbacValues{}, fmt.Errorf("read the installed %s release's values to keep its rbac settings: %w: %s",
			helmReleaseName, err, strings.TrimSpace(stderr.String()))
	}
	return parseReleaseRBACValues(stdout.Bytes())
}

// releaseRBACArgs resolves the rbac.* flags for the NVCRE upgrade and reports
// what it carries forward.
func releaseRBACArgs(helmPath, kubeconfig, kubeContext, scopeFlag string, out io.Writer) ([]string, error) {
	installed, err := readReleaseRBACValues(helmPath, kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	if kept := installed.describe(); kept != "" {
		_, _ = fmt.Fprintf(out, "[helm] Keeping %s from the installed release.\n", kept)
	}
	resolved := resolveRBACValues(installed, scopeFlag)
	if scopeFlag != "" && scopeFlag != installed.Scope {
		_, _ = fmt.Fprintf(out, "[helm] Setting rbac.scope=%s (--rbac-scope).\n", scopeFlag)
	}
	return resolved.setArgs()
}
