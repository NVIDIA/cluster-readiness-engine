// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package rbac names the controller's per-namespace write grant (ADR-092) and
// builds the RoleBinding that carries it.
//
// The controller, nvcrectl and the chart all refer to the same role, binding
// and ServiceAccount. Keeping the names and the manifest here means the
// RoleBinding nvcrectl creates, the one `nvcrectl rbac print` writes, and the
// one the controller quotes when it is waiting for access cannot drift apart.
package rbac

import (
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	// RunRoleName is the ClusterRole holding every write the controller makes
	// to a namespaced resource outside NVCRE's API group. The chart installs it
	// but binds it cluster-wide only under rbac.scope: cluster.
	RunRoleName = "nvcre-run-role"

	// RunBindingName is the RoleBinding that grants RunRoleName to the
	// controller in one run namespace.
	RunBindingName = "nvcre-manager-run"

	// ClusterModeRunBindingName is the ClusterRoleBinding the chart renders
	// under rbac.scope: cluster. Its presence means every namespace is already
	// granted, so nvcrectl creates no per-namespace binding.
	ClusterModeRunBindingName = "nvcre-run-rolebinding"

	// ManagerRoleBindingName is the chart's cluster-wide binding of
	// nvcre-manager-role. Its subject is the controller ServiceAccount, which
	// is how nvcrectl learns the ServiceAccount of a release it did not name.
	ManagerRoleBindingName = "nvcre-manager-rolebinding"

	// DefaultControllerNamespace and DefaultServiceAccountName identify the
	// controller as `nvcrectl setup init` installs it: release "nvcre" in
	// namespace "nvcre" with the chart's nameOverride.
	DefaultControllerNamespace = "nvcre"
	DefaultServiceAccountName  = "nvcre-manager"

	// ManagedByLabel marks objects by the tool that created them.
	ManagedByLabel = "app.kubernetes.io/managed-by"
)

// ServiceAccount identifies the controller's ServiceAccount.
type ServiceAccount struct {
	Namespace string
	Name      string
}

// DefaultServiceAccount returns the ServiceAccount `nvcrectl setup init` installs.
func DefaultServiceAccount() ServiceAccount {
	return ServiceAccount{Namespace: DefaultControllerNamespace, Name: DefaultServiceAccountName}
}

// String formats the ServiceAccount as namespace/name.
func (s ServiceAccount) String() string {
	return s.Namespace + "/" + s.Name
}

// RunBinding returns the RoleBinding that grants RunRoleName to sa in
// runNamespace.
func RunBinding(runNamespace string, sa ServiceAccount) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		APIVersion: rbacv1.SchemeGroupVersion.String(),
		Kind:       "RoleBinding",
		Name:       RunBindingName,
		Namespace:  runNamespace,
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     RunRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      sa.Name,
			Namespace: sa.Namespace,
		}},
	}
}

// RunBindingYAML renders RunBinding as a manifest a user can apply.
func RunBindingYAML(runNamespace string, sa ServiceAccount) (string, error) {
	out, err := yaml.Marshal(RunBinding(runNamespace, sa))
	if err != nil {
		return "", fmt.Errorf("render RoleBinding %s/%s: %w", runNamespace, RunBindingName, err)
	}
	return string(out), nil
}

// GrantsRunRole reports whether binding grants RunRoleName to sa. A binding
// with the right name but another role or subject is not a grant, and nvcrectl
// refuses to adopt it.
func GrantsRunRole(binding *rbacv1.RoleBinding, sa ServiceAccount) bool {
	if binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != "ClusterRole" ||
		binding.RoleRef.Name != RunRoleName {
		return false
	}
	for _, s := range binding.Subjects {
		if s.Kind == rbacv1.ServiceAccountKind && s.Name == sa.Name && s.Namespace == sa.Namespace {
			return true
		}
	}
	return false
}
