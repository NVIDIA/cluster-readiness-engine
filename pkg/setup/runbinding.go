// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/rbac"
)

// runBindingAdoptedAtAnnotation marks an nvcrectl-created RoleBinding that
// another nvcrectl run adopted, with the time it did.
const runBindingAdoptedAtAnnotation = "nvcre.nvidia.com/run-binding-adopted-at"

// runBindingAdoptionLease is how long after an adoption cleanup keeps the
// binding even though the adopter's run object is not visible yet. Adopting
// is the last step before the create, so the real window is milliseconds.
const runBindingAdoptionLease = 2 * time.Minute

// RunBindingRollbackTimeout bounds releasing a binding after a failed submit.
// The release runs on its own context, because an interrupt that cancelled
// the submit would otherwise cancel the release too.
const RunBindingRollbackTimeout = 30 * time.Second

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

	sa, saResolved := controllerServiceAccount(ctx, c)
	key := client.ObjectKey{Namespace: namespace, Name: rbac.RunBindingName}

	existing := &rbacv1.RoleBinding{}
	switch err := c.Get(ctx, key, existing); {
	case err == nil:
		gone, err := adoptRunBinding(ctx, c, existing, sa, saResolved, out)
		if !gone {
			return false, err
		}
		// Another run's cleanup released it between the read and the
		// adoption: grant access afresh below, where this run may.
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

	// Never create a binding for a guessed ServiceAccount: on a release
	// installed under another name it would grant the wrong subject while
	// reporting success. With the scope unknown the binding may not be needed
	// at all, so submit and let a missing binding show as the controller's
	// wait, which names the right account; otherwise stop and say how.
	if !saResolved {
		if unknown {
			warnScopeUnknown(namespace, out)
			return false, nil
		}
		return false, fmt.Errorf("cannot read ClusterRoleBinding %s to find the controller's ServiceAccount, so "+
			"nvcrectl will not guess the subject of RoleBinding %s/%s. Ask a cluster admin for get on "+
			"clusterrolebindings/%s, or to apply the binding with the controller's ServiceAccount:\n\n"+
			"  nvcrectl rbac print --namespace %s --controller-namespace <namespace> --service-account <name> "+
			"| kubectl apply -f -",
			rbac.ManagerRoleBindingName, namespace, rbac.RunBindingName, rbac.ManagerRoleBindingName, namespace)
	}

	binding := rbac.RunBinding(namespace, sa)
	binding.Labels = map[string]string{rbac.ManagedByLabel: managedByNvcrectl}
	if err := c.Create(ctx, binding); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Created concurrently, by another run or by the user.
			if err := c.Get(ctx, key, existing); err != nil {
				if apierrors.IsForbidden(err) && unknown {
					warnScopeUnknown(namespace, out)
					return false, nil
				}
				return false, fmt.Errorf("read RoleBinding %s/%s: %w", namespace, rbac.RunBindingName, err)
			}
			gone, err := adoptRunBinding(ctx, c, existing, sa, saResolved, out)
			if gone {
				return false, fmt.Errorf("RoleBinding %s/%s was deleted while it was being adopted; run again",
					namespace, rbac.RunBindingName)
			}
			return false, err
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
// created in a namespace the run did not create. It reports whether it
// printed a warning, and whether the run should keep owning the binding
// because the release could not complete, so a later --cleanup tries again.
// runGone says whether the run object (runKind) is fully
// deleted: until it is, its finalizers write in the namespace and need the
// binding, so it is kept with a warning instead. It is also kept while any
// other run is still in the namespace: a later run adopts the binding rather
// than creating its own, so deleting it would take that run's access away,
// mid-run or in its finalizers. If the other runs cannot be listed, it is kept
// too, as is one another run adopted within the adoption lease, with a
// warning: the adopter does not release it, so if its run never comes the
// binding is left for the user to delete. Once another run uses or adopted the
// binding, retrying is pointless and the caller gives up ownership.
func ReleaseRunBinding(
	ctx context.Context, c client.Client, namespace string, runGone bool, runKind string, out io.Writer,
) (warned, retry bool) {
	if !runGone {
		_, _ = fmt.Fprintf(out, "[cleanup] Warning: keeping RoleBinding %s/%s because the %s is not gone yet; "+
			"the controller still needs it there. Delete the binding once the %s is deleted.\n",
			namespace, rbac.RunBindingName, runKind, runKind)
		return true, true
	}
	// Read the binding before listing runs. Its resourceVersion is the
	// precondition for the delete below, so an adoption landing after this
	// read makes the delete conflict instead of revoking the adopter.
	binding := &rbacv1.RoleBinding{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: rbac.RunBindingName}, binding); {
	case apierrors.IsNotFound(err):
		return false, false
	case err != nil:
		_, _ = fmt.Fprintf(out, "[cleanup] Warning: keeping RoleBinding %s/%s because it could not be read: %v\n",
			namespace, rbac.RunBindingName, err)
		return true, true
	}
	// An adoption before this read is not a run object yet: the adopter is
	// between EnsureRunBinding and its create. The mark covers that window.
	if adoptedAt, err := time.Parse(time.RFC3339Nano, binding.Annotations[runBindingAdoptedAtAnnotation]); err == nil &&
		time.Since(adoptedAt) < runBindingAdoptionLease {
		_, _ = fmt.Fprintf(out, "[cleanup] Warning: keeping RoleBinding %s/%s: another nvcrectl run adopted it at %s "+
			"and may still be submitting. Delete it once no Certification, WorkloadRun or Workflow is left in the "+
			"namespace.\n", namespace, rbac.RunBindingName, adoptedAt.Format(time.RFC3339))
		return true, false
	}
	others, err := runsInNamespace(ctx, c, namespace)
	if err != nil {
		_, _ = fmt.Fprintf(out, "[cleanup] Warning: keeping RoleBinding %s/%s because other runs in the namespace "+
			"could not be listed: %v. Delete it once no Certification, WorkloadRun or Workflow is left there.\n",
			namespace, rbac.RunBindingName, err)
		return true, true
	}
	if len(others) > 0 {
		_, _ = fmt.Fprintf(out, "[cleanup] Keeping RoleBinding %s/%s, still used by %s. Delete it once those are gone.\n",
			namespace, rbac.RunBindingName, strings.Join(others, ", "))
		return false, false
	}
	_, _ = fmt.Fprintf(out, "[cleanup] Deleting RoleBinding %s/%s...\n", namespace, rbac.RunBindingName)
	err = c.Delete(ctx, binding, client.Preconditions{UID: &binding.UID, ResourceVersion: &binding.ResourceVersion})
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return false, false
	case apierrors.IsConflict(err):
		_, _ = fmt.Fprintf(out, "[cleanup] Keeping RoleBinding %s/%s: another run adopted it during cleanup.\n",
			namespace, rbac.RunBindingName)
		return false, false
	}
	_, _ = fmt.Fprintf(out, "[cleanup] Warning: delete RoleBinding %s/%s: %v\n", namespace, rbac.RunBindingName, err)
	return true, true
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

// grantsRunRoleToSomeServiceAccount reports whether binding binds
// nvcre-run-role to a single ServiceAccount, whichever it is.
func grantsRunRoleToSomeServiceAccount(binding *rbacv1.RoleBinding) bool {
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != rbacv1.ServiceAccountKind {
		return false
	}
	s := binding.Subjects[0]
	return rbac.GrantsRunRole(binding, rbac.ServiceAccount{Namespace: s.Namespace, Name: s.Name})
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
// under another name is still bound correctly. It reports whether it could:
// when the binding cannot be read it returns the ServiceAccount `nvcrectl
// setup init` installs, which is only a guess, and a binding nvcrectl creates
// must not be given a guessed subject.
func controllerServiceAccount(ctx context.Context, c client.Client) (rbac.ServiceAccount, bool) {
	crb := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: rbac.ManagerRoleBindingName}, crb); err == nil {
		for _, s := range crb.Subjects {
			if s.Kind == rbacv1.ServiceAccountKind && s.Name != "" && s.Namespace != "" {
				return rbac.ServiceAccount{Namespace: s.Namespace, Name: s.Name}, true
			}
		}
	}
	return rbac.DefaultServiceAccount(), false
}

func adoptRunBinding(
	ctx context.Context, c client.Client, existing *rbacv1.RoleBinding, sa rbac.ServiceAccount, saResolved bool,
	out io.Writer,
) (gone bool, err error) {
	// With the ServiceAccount unresolved only the role can be checked: the
	// subject would be compared against a guess. A binding for the wrong
	// account shows as the controller's wait, which names the right one, and
	// the account it grants is printed below.
	if !saResolved {
		if !grantsRunRoleToSomeServiceAccount(existing) {
			return false, fmt.Errorf("RoleBinding %s/%s exists but does not bind ClusterRole %s to a single "+
				"ServiceAccount; fix or delete it (nvcrectl rbac print --namespace %s --controller-namespace "+
				"<namespace> --service-account <name> shows the expected binding)",
				existing.Namespace, existing.Name, rbac.RunRoleName, existing.Namespace)
		}
		sa = rbac.ServiceAccount{Namespace: existing.Subjects[0].Namespace, Name: existing.Subjects[0].Name}
	} else if !rbac.GrantsRunRole(existing, sa) {
		return false, fmt.Errorf("RoleBinding %s/%s exists but does not bind ClusterRole %s to ServiceAccount %s; "+
			"fix or delete it (nvcrectl rbac print --namespace %s shows the expected binding)",
			existing.Namespace, existing.Name, rbac.RunRoleName, sa, existing.Namespace)
	}
	// Only a binding nvcrectl created is ever released by a run's cleanup, so
	// only such a binding is marked: the mark tells that cleanup this run is
	// about to submit under it (see ReleaseRunBinding).
	if existing.Labels[rbac.ManagedByLabel] == managedByNvcrectl {
		patch := client.MergeFrom(existing.DeepCopy())
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[runBindingAdoptedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		switch err := c.Patch(ctx, existing, patch); {
		case err == nil:
		case apierrors.IsNotFound(err):
			return true, nil
		case apierrors.IsForbidden(err):
			// The default edit role cannot modify RoleBindings, so stopping
			// here would lock ordinary users out of a shared namespace for as
			// long as an nvcrectl-created binding exists, mostly to guard
			// against a cleanup that never comes. Submit unmarked instead: if
			// the creating run's cleanup does delete it first, this run's
			// Workflow waits with NamespacePermissionsMissing and fails at the
			// access timeout naming the binding, so the loss is loud.
			_, _ = fmt.Fprintf(out, "[rbac] Warning: cannot mark RoleBinding %s/%s as in use (%v); if the run "+
				"that created it is cleaning up now, it may delete the binding before this run is submitted, "+
				"and this run would then wait for access. A binding listed in the chart's rbac.runNamespaces "+
				"or applied by hand (nvcrectl rbac print --namespace %s) is never deleted by a run's cleanup.\n",
				existing.Namespace, existing.Name, err, existing.Namespace)
		default:
			return false, fmt.Errorf("mark RoleBinding %s/%s as in use: %w", existing.Namespace, existing.Name, err)
		}
	}
	_, _ = fmt.Fprintf(out, "[rbac] Using existing RoleBinding %s/%s (grants ServiceAccount %s).\n",
		existing.Namespace, existing.Name, sa)
	return false, nil
}
