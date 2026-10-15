// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/rbac"
)

const runNS = "team-a"

func runRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{Name: rbac.RunRoleName}
}

// namespacedChart is what the chart installs under rbac.scope: namespaced:
// the run role and the manager binding that names the controller's
// ServiceAccount, with no cluster-wide run binding.
func namespacedChart() []client.Object {
	return []client.Object{runRole(), managerBinding(rbac.DefaultServiceAccount())}
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
		patchErr  error

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
			objects: namespacedChart(),
			wantErr: []string{
				`namespace "team-a", which already existed`,
				"--grant-run-access",
				"name: nvcre-manager-run",
				"name: nvcre-run-role",
			},
		},
		{
			name:        "existing namespace with --grant-run-access",
			objects:     namespacedChart(),
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
			objects:     append(namespacedChart(), mismatched),
			opts:        RunBindingOptions{NamespaceCreated: true},
			wantErr:     []string{"does not bind ClusterRole nvcre-run-role to ServiceAccount nvcre/nvcre-manager"},
			wantBinding: &rbac.ServiceAccount{Namespace: "elsewhere", Name: "someone-else"},
		},
		{
			name:      "forbidden create names the missing permission",
			objects:   namespacedChart(),
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
			objects: namespacedChart(),
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
			name:    "unreadable cluster RBAC in a namespace this run created submits without a guessed subject",
			opts:    RunBindingOptions{NamespaceCreated: true},
			getErr:  unreadableClusterRBAC(),
			wantOut: []string{warnedUnknownScope},
		},
		{
			name:        "unknown scope and an unreadable existing binding submit with a warning",
			objects:     append(namespacedChart(), rbac.RunBinding(runNS, rbac.DefaultServiceAccount())),
			opts:        RunBindingOptions{Grant: true},
			getErr:      map[string]error{kindClusterRole: forbidden("clusterroles"), "RoleBinding": forbidden("rolebindings")},
			wantOut:     []string{warnedUnknownScope},
			wantBinding: new(rbac.DefaultServiceAccount()),
		},
		{
			name:        "known namespaced scope and an unreadable existing binding still stop",
			objects:     append(namespacedChart(), rbac.RunBinding(runNS, rbac.DefaultServiceAccount())),
			opts:        RunBindingOptions{Grant: true},
			getErr:      map[string]error{"RoleBinding": forbidden("rolebindings")},
			wantErr:     []string{"read RoleBinding team-a/nvcre-manager-run"},
			wantBinding: new(rbac.DefaultServiceAccount()),
		},
		{
			name:    "unresolvable ServiceAccount with the binding known to be needed stops",
			objects: []client.Object{runRole()},
			opts:    RunBindingOptions{NamespaceCreated: true},
			wantErr: []string{"will not guess the subject", "--service-account <name>"},
		},
		{
			name:    "unresolvable ServiceAccount with known namespaced scope adopts an existing binding and names its account",
			objects: []client.Object{runRole(), rbac.RunBinding(runNS, customSA)},
			wantOut: []string{
				"Using existing RoleBinding team-a/nvcre-manager-run (grants ServiceAccount platform/readiness-manager)",
			},
			wantBinding: &customSA,
		},
		{
			name:    "unresolvable ServiceAccount with unknown scope adopts an existing binding and names its account",
			objects: []client.Object{runRole(), rbac.RunBinding(runNS, customSA)},
			getErr:  unreadableClusterRBAC(),
			wantOut: []string{
				"Using existing RoleBinding team-a/nvcre-manager-run (grants ServiceAccount platform/readiness-manager)",
			},
			wantBinding: &customSA,
		},
		{
			name: "unresolvable ServiceAccount refuses a binding with no single ServiceAccount subject",
			objects: []client.Object{runRole(), func() client.Object {
				b := rbac.RunBinding(runNS, customSA)
				b.Subjects = []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "someone"}}
				return b
			}()},
			wantErr:     []string{"does not bind ClusterRole nvcre-run-role to a single ServiceAccount"},
			wantBinding: &rbac.ServiceAccount{Name: "someone"},
		},
		{
			name:        "an nvcrectl binding that cannot be marked is used with a warning",
			objects:     append(namespacedChart(), nvcrectlBinding(time.Time{})),
			patchErr:    forbidden("rolebindings"),
			wantOut:     []string{"cannot mark RoleBinding", "Using existing RoleBinding"},
			wantBinding: new(rbac.DefaultServiceAccount()),
			wantLabel:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			c := interceptor.NewClient(backing, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					switch obj.(type) {
					case *rbacv1.RoleBinding:
						if err := tc.getErr["RoleBinding"]; err != nil {
							return err
						}
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
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if tc.patchErr != nil {
						return tc.patchErr
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})

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
			err = backing.Get(ctx, client.ObjectKey{Namespace: runNS, Name: rbac.RunBindingName}, got)
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

// nvcrectlBinding is a run binding nvcrectl created, optionally adopted by
// another run at adoptedAt.
func nvcrectlBinding(adoptedAt time.Time) *rbacv1.RoleBinding {
	b := rbac.RunBinding(runNS, rbac.DefaultServiceAccount())
	b.Labels = map[string]string{rbac.ManagedByLabel: managedByNvcrectl}
	if !adoptedAt.IsZero() {
		b.Annotations = map[string]string{runBindingAdoptedAtAnnotation: adoptedAt.UTC().Format(time.RFC3339Nano)}
	}
	return b
}

func newRunBindingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))
	return scheme
}

// TestEnsureRunBindingMarksAdoption pins the mark a run leaves on a binding it
// adopts, so that the cleanup of the run that created it does not delete it
// before this run is submitted. Only nvcrectl's own bindings are marked;
// others are never released, and are left untouched.
func TestEnsureRunBindingMarksAdoption(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		existing *rbacv1.RoleBinding
		wantMark bool
	}{
		{name: "nvcrectl binding is marked", existing: nvcrectlBinding(time.Time{}), wantMark: true},
		{name: "foreign binding is not touched", existing: rbac.RunBinding(runNS, rbac.DefaultServiceAccount())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newRunBindingScheme(t)).
				WithObjects(append(namespacedChart(), tc.existing)...).Build()
			var out bytes.Buffer
			created, err := EnsureRunBinding(ctx, c, runNS, RunBindingOptions{}, &out)
			require.NoError(t, err)
			require.False(t, created)
			got := &rbacv1.RoleBinding{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: runNS, Name: rbac.RunBindingName}, got))
			_, marked := got.Annotations[runBindingAdoptedAtAnnotation]
			require.Equal(t, tc.wantMark, marked)
		})
	}
}

// TestEnsureRunBindingAdoptionRace covers a binding released by its creator's
// cleanup between this run's read and its mark: the run grants access afresh
// where it may, instead of submitting with no binding.
func TestEnsureRunBindingAdoptionRace(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(newRunBindingScheme(t)).
		WithObjects(append(namespacedChart(), nvcrectlBinding(time.Time{}))...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// The creator's cleanup deletes it first.
				require.NoError(t, c.Delete(ctx, obj))
				return apierrors.NewNotFound(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, obj.GetName())
			},
		}).Build()
	var out bytes.Buffer
	created, err := EnsureRunBinding(ctx, c, runNS, RunBindingOptions{NamespaceCreated: true}, &out)
	require.NoError(t, err)
	require.True(t, created, "the binding must be granted afresh")
	require.Contains(t, out.String(), "Created RoleBinding")
}

// TestReleaseRunBinding pins when cleanup deletes the binding a run created:
// only when the run is gone, no other run is in the namespace, no other run
// adopted it recently, and it is unchanged since cleanup read it. It also pins
// when the run keeps ownership for a later retry: only when the release could
// not complete, never when another run uses or adopted the binding.
func TestReleaseRunBinding(t *testing.T) {
	ctx := context.Background()
	conflict := apierrors.NewConflict(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"},
		rbac.RunBindingName, errors.New("resourceVersion changed"))
	for _, tc := range []struct {
		name        string
		binding     *rbacv1.RoleBinding
		runGone     bool
		deleteErr   error
		wantWarn    bool
		wantRetry   bool
		wantDeleted bool
		wantOut     string
	}{
		{name: "deleted when nothing else uses it", binding: nvcrectlBinding(time.Time{}), runGone: true,
			wantDeleted: true, wantOut: "Deleting RoleBinding"},
		{name: "kept while the run is not gone", binding: nvcrectlBinding(time.Time{}),
			wantWarn: true, wantRetry: true, wantOut: "is not gone yet"},
		{name: "kept with a warning after a recent adoption", binding: nvcrectlBinding(time.Now().Add(-10 * time.Second)),
			runGone: true, wantWarn: true, wantOut: "another nvcrectl run adopted it"},
		{name: "deleted once the adoption lease ran out", binding: nvcrectlBinding(time.Now().Add(-runBindingAdoptionLease - time.Minute)),
			runGone: true, wantDeleted: true},
		{name: "kept when an adoption lands during cleanup", binding: nvcrectlBinding(time.Time{}), runGone: true,
			deleteErr: conflict, wantOut: "adopted it during cleanup"},
		{name: "kept for a retry when the delete fails", binding: nvcrectlBinding(time.Time{}), runGone: true,
			deleteErr: errors.New("connection refused"), wantWarn: true, wantRetry: true, wantOut: "Warning: delete RoleBinding"},
		{name: "already gone", runGone: true, wantDeleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(newRunBindingScheme(t))
			if tc.binding != nil {
				builder = builder.WithObjects(tc.binding)
			}
			c := builder.WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if tc.deleteErr != nil {
						return tc.deleteErr
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			var out bytes.Buffer
			warned, retry := ReleaseRunBinding(ctx, c, runNS, tc.runGone, "WorkloadRun", &out)
			require.Equal(t, tc.wantWarn, warned)
			require.Equal(t, tc.wantRetry, retry)
			require.Contains(t, out.String(), tc.wantOut)
			err := c.Get(ctx, client.ObjectKey{Namespace: runNS, Name: rbac.RunBindingName}, &rbacv1.RoleBinding{})
			require.Equal(t, tc.wantDeleted, apierrors.IsNotFound(err), "binding state: %v", err)
		})
	}
}
