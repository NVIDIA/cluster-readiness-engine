// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"fmt"
)

// Values for the chart's rbac.scope (ADR-092).
const (
	rbacScopeCluster    = "cluster"
	rbacScopeNamespaced = "namespaced"
)

// validateRBACScope checks a --rbac-scope value. Empty means keep the
// installed release's value, or the chart default on a first install. The
// installed release's rbac.scope and rbac.runNamespaces are kept with the rest
// of its values, and kept even with --reset-values (ADR-098), so a re-run
// never silently widens a namespaced grant.
func validateRBACScope(scope string) error {
	switch scope {
	case "", rbacScopeCluster, rbacScopeNamespaced:
		return nil
	}
	return fmt.Errorf("--rbac-scope must be %q or %q, got %q", rbacScopeCluster, rbacScopeNamespaced, scope)
}
