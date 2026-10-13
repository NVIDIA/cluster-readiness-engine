// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/rbac"
)

const runNS = "team-a"

func runRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{Name: rbac.RunRoleName}
}

func clusterModeBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		Name:    rbac.ClusterModeRunBindingName,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: rbac.RunRoleName},
	}
}

// managerBinding is the chart's binding of nvcre-manager-role, whose subject
// names the release's ServiceAccount.
func managerBinding(sa rbac.ServiceAccount) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		Name:    rbac.ManagerRoleBindingName,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: "nvcre-manager-role"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: sa.Namespace,
		}},
	}
}

// testKindCRB keys an injected ClusterRoleBinding read error.
const testKindCRB = "ClusterRoleBinding"

// warnedUnknownScope is in the warning printed when the scope is unknown.
const warnedUnknownScope = "submitting anyway"

// unreadableClusterRBAC makes both cluster-scoped reads forbidden, as for a
// namespace admin.
func unreadableClusterRBAC() map[string]error {
	return map[string]error{
		testKindCRB:     forbidden("clusterrolebindings"),
		kindClusterRole: forbidden("clusterroles"),
	}
}

func forbidden(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName, Resource: resource}, "",
		errors.New("injected"))
}

func TestEnsureRunBinding(t *testing.T) {
	customSA := rbac.ServiceAccount{Namespace: "platform", Name: "readiness-manager"}
	mismatched := rbac.RunBinding(runNS, rbac.ServiceAccount{Namespace: "elsewhere", Name: "someone-else"})

	for _, tc := range []struct {
		name    string
		objects []client.Object
		opts    RunBindingOptions
		// getErr and createErr inject an error for reads of the named kind, or
		// for the RoleBinding create.
		getErr    map[string]error
		createErr error

		wantCreated bool
		wantErr     []string
		wantOut     []string
		// wantBinding is the subject the namespace's binding must name, or
		// nil when the namespace must have no binding.
		wantBinding *rbac.ServiceAccount
		wantLabel   bool
	}{
		{
			name:    "cluster scope needs no binding",
			objects: []client.Object{runRole(), clusterModeBinding()},
		},
		{
			name: "chart from before the role split needs no binding",
		},
		{
			name:        "namespace this run created gets a binding",
			objects:     []client.Object{runRole(), managerBinding(customSA)},
			opts:        RunBindingOptions{NamespaceCreated: true},
			wantCreated: true,
			wantOut:     []string{"Created RoleBinding team-a/nvcre-manager-run", "platform/readiness-manager"},
			wantBinding: &customSA,
			wantLabel:   true,
		},
		{
			name:    "existing namespace is refused without --grant-run-access",
			objects: []client.Object{runRole()},
			wantErr: []string{
				`namespace "team-a", which already existed`,
				"--grant-run-access",
				"name: nvcre-manager-run",
				"name: nvcre-run-role",
			},
		},
		{
			name:        "existing namespace with --grant-run-access",
			objects:     []client.Object{runRole()},
			opts:        RunBindingOptions{Grant: true},
			wantCreated: true,
			wantOut:     []string{"Created RoleBinding", "stays until the run object is gone"},
			wantBinding: new(rbac.DefaultServiceAccount()),
			wantLabel:   true,
		},
		{
			name: "existing binding is adopted and not owned",
			objects: []client.Object{
				runRole(), managerBinding(customSA), rbac.RunBinding(runNS, customSA),
			},
			wantOut:     []string{"Using existing RoleBinding team-a/nvcre-manager-run"},
			wantBinding: &customSA,
		},
		{
			name:        "binding for another subject is refused",
			objects:     []client.Object{runRole(), mismatched},
			opts:        RunBindingOptions{NamespaceCreated: true},
			wantErr:     []string{"does not bind ClusterRole nvcre-run-role to ServiceAccount nvcre/nvcre-manager"},
			wantBinding: &rbac.ServiceAccount{Namespace: "elsewhere", Name: "someone-else"},
		},
		{
			name:      "forbidden create names the missing permission",
			objects:   []client.Object{runRole()},
			opts:      RunBindingOptions{NamespaceCreated: true},
			createErr: forbidden("rolebindings"),
			wantErr:   []string{"the bind verb on clusterroles/nvcre-run-role", "kind: RoleBinding"},
		},
		{
			name:    "unreadable cluster RBAC in an existing namespace submits with a warning",
			getErr:  unreadableClusterRBAC(),
			wantOut: []string{"cannot read ClusterRoleBinding nvcre-run-rolebinding", warnedUnknownScope},
		},
		{
			name:    "unreadable cluster-mode binding with a readable role submits with a warning",
			objects: []client.Object{runRole()},
			getErr:  map[string]error{testKindCRB: forbidden("clusterrolebindings")},
			wantOut: []string{warnedUnknownScope},
		},
		{
			name:      "unreadable cluster RBAC and a forbidden create submit with a warning",
			opts:      RunBindingOptions{Grant: true},
			createErr: forbidden("rolebindings"),
			getErr:    unreadableClusterRBAC(),
			wantOut:   []string{warnedUnknownScope},
		},
		{
			name:        "unreadable cluster RBAC in a namespace this run created still gets a binding",
			opts:        RunBindingOptions{NamespaceCreated: true},
			getErr:      unreadableClusterRBAC(),
			wantCreated: true,
			wantOut:     []string{"Created RoleBinding team-a/nvcre-manager-run"},
			wantBinding: new(rbac.DefaultServiceAccount()),
			wantLabel:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						switch obj.(type) {
						case *rbacv1.ClusterRoleBinding:
							if err := tc.getErr[testKindCRB]; err != nil {
								return err
							}
						case *rbacv1.ClusterRole:
							if err := tc.getErr[kindClusterRole]; err != nil {
								return err
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if tc.createErr != nil {
							return tc.createErr
						}
						return c.Create(ctx, obj, opts...)
					},
				}).Build()

			var out bytes.Buffer
			created, err := EnsureRunBinding(ctx, c, runNS, tc.opts, &out)
			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, want := range tc.wantErr {
					require.Contains(t, err.Error(), want)
				}
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantCreated, created)
			for _, want := range tc.wantOut {
				require.Contains(t, out.String(), want)
			}
			if len(tc.wantOut) == 0 {
				require.Empty(t, strings.TrimSpace(out.String()))
			}

			got := &rbacv1.RoleBinding{}
			err = c.Get(ctx, client.ObjectKey{Namespace: runNS, Name: rbac.RunBindingName}, got)
			if tc.wantBinding == nil {
				require.True(t, apierrors.IsNotFound(err), "namespace must have no binding, got %v", err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, rbac.RunRoleName, got.RoleRef.Name)
			require.Len(t, got.Subjects, 1)
			require.Equal(t, tc.wantBinding.Name, got.Subjects[0].Name)
			require.Equal(t, tc.wantBinding.Namespace, got.Subjects[0].Namespace)
			if tc.wantLabel {
				require.Equal(t, "nvcrectl", got.Labels[rbac.ManagedByLabel])
			} else {
				require.Empty(t, got.Labels[rbac.ManagedByLabel], "an adopted binding is not nvcrectl's")
			}
		})
	}
}

func TestDeleteRunBinding(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rbac.RunBinding(runNS, rbac.DefaultServiceAccount())).Build()

	var out bytes.Buffer
	require.NoError(t, DeleteRunBinding(ctx, c, runNS, &out))
	err := c.Get(ctx, client.ObjectKey{Namespace: runNS, Name: rbac.RunBindingName}, &rbacv1.RoleBinding{})
	require.True(t, apierrors.IsNotFound(err))
	// Already gone is not an error: cleanup may run twice.
	require.NoError(t, DeleteRunBinding(ctx, c, runNS, &out))
}
