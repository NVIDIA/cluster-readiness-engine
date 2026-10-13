// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"io"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/rbac"
)

// RunBindingOptions says how a run namespace came to be and what the user
// allowed, which together decide whether EnsureRunBinding may create a
// binding there.
type RunBindingOptions struct {
	// NamespaceCreated is true when this nvcrectl invocation created the
	// namespace. Deleting the namespace then removes the binding with it.
	NamespaceCreated bool
	// Grant is --grant-run-access: create the binding in a namespace that
	// already existed, which leaves a standing grant there.
	Grant bool
}

// EnsureRunBinding makes sure the controller can write in namespace before a
// run is submitted there (ADR-092), and reports whether it created a
// RoleBinding that the caller now owns.
//
// It does nothing when no per-namespace binding is needed: the chart was
// installed with rbac.scope: cluster, or predates the role split. Otherwise it
// adopts an existing nvcre-manager-run binding, or creates one, but only in a
// namespace this run created or with --grant-run-access. In a namespace that
// already existed it refuses, so a shared namespace such as default never
// becomes a standing grant without the user asking, and the error carries the
// manifest.
//
// The create runs with the user's credentials. Kubernetes allows it only if
// the user holds every permission in nvcre-run-role within the namespace, or
// has bind on the role.
func EnsureRunBinding(ctx context.Context, c client.Client, namespace string, opts RunBindingOptions, out io.Writer) (bool, error) {
	need, err := runBindingNeeded(ctx, c)
	if err != nil {
		return false, err
	}
	if need == bindingNotNeeded {
		return false, nil
	}
	// The user cannot read cluster-scoped RBAC, so whether the install is
	// rbac.scope: cluster is unknown. Refusing would break submissions that
	// worked before ADR-092 on the default scope, so the binding is created
	// where it would be anyway, and otherwise the run is submitted with a
	// warning: if a binding is missing, the controller's wait says so.
	unknown := need == bindingNeedUnknown

	sa := controllerServiceAccount(ctx, c)
	key := client.ObjectKey{Namespace: namespace, Name: rbac.RunBindingName}

	existing := &rbacv1.RoleBinding{}
	switch err := c.Get(ctx, key, existing); {
	case err == nil:
		return false, adoptRunBinding(existing, sa, out)
	case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
		// Forbidden: the create below either succeeds or explains itself.
	default:
		return false, fmt.Errorf("check RoleBinding %s/%s: %w", namespace, rbac.RunBindingName, err)
	}

	manifest, err := rbac.RunBindingYAML(namespace, sa)
	if err != nil {
		return false, err
	}
	if !opts.NamespaceCreated && !opts.Grant {
		if unknown {
			warnScopeUnknown(namespace, out)
			return false, nil
		}
		return false, fmt.Errorf("the controller has no write access in namespace %q, which already existed.\n"+
			"nvcrectl grants access only in namespaces it creates, so an existing namespace does not become a "+
			"standing grant by accident. Either:\n"+
			"  - run in a namespace that does not exist yet: nvcrectl creates it and grants access there\n"+
			"  - pass --grant-run-access to create the RoleBinding below in %q\n"+
			"  - apply it yourself, then run again:\n\n%s",
			namespace, namespace, manifest)
	}

	binding := rbac.RunBinding(namespace, sa)
	binding.Labels = map[string]string{rbac.ManagedByLabel: managedByNvcrectl}
	if err := c.Create(ctx, binding); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Created concurrently, by another run or by the user.
			if err := c.Get(ctx, key, existing); err != nil {
				return false, fmt.Errorf("read RoleBinding %s/%s: %w", namespace, rbac.RunBindingName, err)
			}
			return false, adoptRunBinding(existing, sa, out)
		}
		if apierrors.IsForbidden(err) && unknown {
			warnScopeUnknown(namespace, out)
			return false, nil
		}
		if apierrors.IsForbidden(err) {
			return false, fmt.Errorf("create RoleBinding %s/%s: %w\n\n"+
				"Binding ClusterRole %s needs every permission it holds within namespace %q, or the bind verb "+
				"on clusterroles/%s. Ask a cluster admin to grant one of those, or to apply:\n\n%s",
				namespace, rbac.RunBindingName, err, rbac.RunRoleName, namespace, rbac.RunRoleName, manifest)
		}
		return false, fmt.Errorf("create RoleBinding %s/%s: %w", namespace, rbac.RunBindingName, err)
	}

	_, _ = fmt.Fprintf(out, "[rbac] Created RoleBinding %s/%s granting ServiceAccount %s write access in namespace %s.\n",
		namespace, rbac.RunBindingName, sa, namespace)
	if !opts.NamespaceCreated {
		_, _ = fmt.Fprintf(out, "[rbac] The binding stays until the run object is gone: its finalizers need it. "+
			"--cleanup deletes it after that unless another run in the namespace still uses it; otherwise delete it "+
			"yourself once the run is deleted.\n")
	}
	return true, nil
}

// DeleteRunBinding deletes a RoleBinding EnsureRunBinding created. Call it
// only once the run object is fully deleted: its finalizers write in the
// namespace until then.
func DeleteRunBinding(ctx context.Context, c client.Client, namespace string, out io.Writer) error {
	binding := &rbacv1.RoleBinding{}
	binding.Name, binding.Namespace = rbac.RunBindingName, namespace
	_, _ = fmt.Fprintf(out, "[cleanup] Deleting RoleBinding %s/%s...\n", namespace, rbac.RunBindingName)
	if err := c.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete RoleBinding %s/%s: %w", namespace, rbac.RunBindingName, err)
	}
	return nil
}

// ReleaseRunBinding deletes, during cleanup, a RoleBinding EnsureRunBinding
// created in a namespace the run did not create, and reports whether it
// printed a warning. runGone says whether the run object (runKind) is fully
// deleted: until it is, its finalizers write in the namespace and need the
// binding, so it is kept with a warning instead. It is also kept while any
// other run is still in the namespace: a later run adopts the binding rather
// than creating its own, so deleting it would take that run's access away,
// mid-run or in its finalizers. If the other runs cannot be listed, it is kept
// too.
func ReleaseRunBinding(ctx context.Context, c client.Client, namespace string, runGone bool, runKind string, out io.Writer) bool {
	if !runGone {
		_, _ = fmt.Fprintf(out, "[cleanup] Warning: keeping RoleBinding %s/%s because the %s is not gone yet; "+
			"the controller still needs it there. Delete the binding once the %s is deleted.\n",
			namespace, rbac.RunBindingName, runKind, runKind)
		return true
	}
	others, err := runsInNamespace(ctx, c, namespace)
	if err != nil {
		_, _ = fmt.Fprintf(out, "[cleanup] Warning: keeping RoleBinding %s/%s because other runs in the namespace "+
			"could not be listed: %v. Delete it once no Certification, WorkloadRun or Workflow is left there.\n",
			namespace, rbac.RunBindingName, err)
		return true
	}
	if len(others) > 0 {
		_, _ = fmt.Fprintf(out, "[cleanup] Keeping RoleBinding %s/%s, still used by %s. Delete it once those are gone.\n",
			namespace, rbac.RunBindingName, strings.Join(others, ", "))
		return false
	}
	if err := DeleteRunBinding(ctx, c, namespace, out); err != nil {
		_, _ = fmt.Fprintf(out, "[cleanup] Warning: %v\n", err)
		return true
	}
	return false
}

// runsInNamespace lists the NVCRE objects in namespace that may still need the
// controller's access there: Certifications, WorkloadRuns, and the Workflows
// either creates, including any applied directly.
func runsInNamespace(ctx context.Context, c client.Client, namespace string) ([]string, error) {
	var names []string
	certs := &nvcrev1alpha1.CertificationList{}
	if err := c.List(ctx, certs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Certifications: %w", err)
	}
	for _, o := range certs.Items {
		names = append(names, "Certification/"+o.Name)
	}
	runs := &nvcrev1alpha1.WorkloadRunList{}
	if err := c.List(ctx, runs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list WorkloadRuns: %w", err)
	}
	for _, o := range runs.Items {
		names = append(names, "WorkloadRun/"+o.Name)
	}
	workflows := &nvcrev1alpha1.WorkflowList{}
	if err := c.List(ctx, workflows, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Workflows: %w", err)
	}
	for _, o := range workflows.Items {
		names = append(names, "Workflow/"+o.Name)
	}
	return names, nil
}

// runBindingNeed says whether a run namespace needs the RoleBinding.
type runBindingNeed int

const (
	bindingNeeded runBindingNeed = iota
	bindingNotNeeded
	// bindingNeedUnknown: the user cannot read the cluster-scoped RBAC that
	// would tell.
	bindingNeedUnknown
)

// runBindingNeeded reports whether the installed chart grants nvcre-run-role
// per namespace. It is not needed when the chart's cluster-mode
// ClusterRoleBinding exists, nor when nvcre-run-role does not exist at all: a
// chart from before the role split grants every write cluster-wide through
// nvcre-manager-role. It is needed only when the ClusterRoleBinding is known
// to be absent and the role known to exist. Anything the user cannot read
// leaves the answer unknown.
func runBindingNeeded(ctx context.Context, c client.Client) (runBindingNeed, error) {
	crb := &rbacv1.ClusterRoleBinding{}
	crbErr := c.Get(ctx, client.ObjectKey{Name: rbac.ClusterModeRunBindingName}, crb)
	switch {
	case crbErr == nil:
		return bindingNotNeeded, nil
	case apierrors.IsNotFound(crbErr), apierrors.IsForbidden(crbErr):
	default:
		return bindingNeeded, fmt.Errorf("check ClusterRoleBinding %s: %w", rbac.ClusterModeRunBindingName, crbErr)
	}

	role := &rbacv1.ClusterRole{}
	roleErr := c.Get(ctx, client.ObjectKey{Name: rbac.RunRoleName}, role)
	switch {
	case apierrors.IsNotFound(roleErr):
		return bindingNotNeeded, nil
	case roleErr != nil && !apierrors.IsForbidden(roleErr):
		return bindingNeeded, fmt.Errorf("check ClusterRole %s: %w", rbac.RunRoleName, roleErr)
	case roleErr == nil && apierrors.IsNotFound(crbErr):
		return bindingNeeded, nil
	}
	return bindingNeedUnknown, nil
}

// warnScopeUnknown explains a submission made without knowing whether the
// controller has access in the namespace.
func warnScopeUnknown(namespace string, out io.Writer) {
	_, _ = fmt.Fprintf(out, "[rbac] Warning: cannot read ClusterRoleBinding %s or ClusterRole %s, so whether the "+
		"controller can write in namespace %s is unknown; submitting anyway. If the chart uses rbac.scope: "+
		"namespaced and %s has no RoleBinding %s, the run waits with NamespacePermissionsMissing until one is "+
		"applied (nvcrectl rbac print --namespace %s).\n",
		rbac.ClusterModeRunBindingName, rbac.RunRoleName, namespace, namespace, rbac.RunBindingName, namespace)
}

// controllerServiceAccount reads the controller's ServiceAccount from the
// subject of the chart's nvcre-manager-rolebinding, so a release installed
// under another name is still bound correctly. It falls back to the
// ServiceAccount `nvcrectl setup init` installs when the binding cannot be
// read.
func controllerServiceAccount(ctx context.Context, c client.Client) rbac.ServiceAccount {
	crb := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: rbac.ManagerRoleBindingName}, crb); err == nil {
		for _, s := range crb.Subjects {
			if s.Kind == rbacv1.ServiceAccountKind && s.Name != "" && s.Namespace != "" {
				return rbac.ServiceAccount{Namespace: s.Namespace, Name: s.Name}
			}
		}
	}
	return rbac.DefaultServiceAccount()
}

func adoptRunBinding(existing *rbacv1.RoleBinding, sa rbac.ServiceAccount, out io.Writer) error {
	if !rbac.GrantsRunRole(existing, sa) {
		return fmt.Errorf("RoleBinding %s/%s exists but does not bind ClusterRole %s to ServiceAccount %s; "+
			"fix or delete it (nvcrectl rbac print --namespace %s shows the expected binding)",
			existing.Namespace, existing.Name, rbac.RunRoleName, sa, existing.Namespace)
	}
	_, _ = fmt.Fprintf(out, "[rbac] Using existing RoleBinding %s/%s.\n", existing.Namespace, existing.Name)
	return nil
}
