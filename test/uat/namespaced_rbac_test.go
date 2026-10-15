// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat

package uat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/rbac"
	"github.com/NVIDIA/cluster-readiness-engine/test/uat/util"
)

// TestNamespacedRBAC runs the controller under rbac.scope: namespaced on a
// cluster that enforces RBAC, which envtest does not (ADR-092). It proves the
// two paths a run namespace gets its grant by: nvcrectl creating the
// RoleBinding in a namespace it creates, and a Certification applied without
// one waiting with NamespacePermissionsMissing until the binding is applied.
func TestNamespacedRBAC(t *testing.T) {
	const (
		nodesDir     = "testdata/aws/h100"
		cliCertName  = "rbac-nvcrectl"
		applyCert    = "testdata/rbac/certification.yaml"
		applyName    = "rbac-kubectl-apply"
		waitedReason = "NamespacePermissionsMissing"
	)
	suffix := time.Now().Format("150405")
	cliNS := "uat-rbac-cli-" + suffix
	applyNS := "uat-rbac-apply-" + suffix

	feature := features.New("rbac/namespaced").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)

			setRBACScope(ctx, t, "namespaced")
			t.Cleanup(func() { setRBACScope(context.Background(), t, "cluster") })

			util.RestartController(ctx, t, c)
			util.ApplyYAML(ctx, t, c, nodesDir+"/nodes.yaml", "")
			t.Cleanup(func() {
				util.CleanupYAML(context.Background(), c, nodesDir+"/nodes.yaml", "")
			})
			return ctx
		}).
		Assess("nvcrectl grants access in the namespace it creates", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			t.Cleanup(func() {
				util.DeleteCertification(context.Background(), c, cliCertName, cliNS)
				_ = util.DeleteNamespace(context.Background(), c, cliNS)
			})

			util.RunNvcrectl(ctx, t,
				"--category", "communication/nccl-all-reduce",
				"--name", cliCertName,
				"--namespace", cliNS,
				"--nodes-per-job", "2",
			)

			binding := &rbacv1.RoleBinding{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: cliNS, Name: rbac.RunBindingName}, binding))
			require.Equal(t, rbac.RunRoleName, binding.RoleRef.Name)
			require.Equal(t, "nvcrectl", binding.Labels[rbac.ManagedByLabel])

			util.WaitForCertification(ctx, t, c, util.CertificationKey(cliCertName, cliNS),
				"Succeeded", util.PollTimeout)
			return ctx
		}).
		Assess("an applied Certification waits for the binding, then runs", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := &corev1.Namespace{}
			ns.Name = applyNS
			require.NoError(t, c.Create(ctx, ns))
			t.Cleanup(func() {
				util.DeleteCertification(context.Background(), c, applyName, applyNS)
				_ = util.DeleteNamespace(context.Background(), c, applyNS)
			})

			util.ApplyYAML(ctx, t, c, applyCert, applyNS)

			workflow := waitForWorkflowReason(ctx, t, c, applyNS, waitedReason)
			cond := meta.FindStatusCondition(workflow.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess)
			require.NotNil(t, cond)
			require.Contains(t, cond.Message, "nvcre/nvcre-manager")
			waitForEvent(ctx, t, c, applyNS, workflow.Name, "NamespaceAccessDenied")

			// Nothing may be written in the namespace while it waits.
			cms := &corev1.ConfigMapList{}
			require.NoError(t, c.List(ctx, cms, client.InNamespace(applyNS),
				client.MatchingLabels{"nvcre.nvidia.com/workflow": workflow.Name}))
			require.Empty(t, cms.Items)

			require.NoError(t, c.Create(ctx, rbac.RunBinding(applyNS, rbac.DefaultServiceAccount())))
			util.WaitForCertification(ctx, t, c, util.CertificationKey(applyName, applyNS),
				"Succeeded", util.PollTimeout)

			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(workflow), workflow))
			require.True(t, meta.IsStatusConditionTrue(workflow.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess))
			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}

// setRBACScope upgrades the UAT release in place with rbac.scope set, keeping
// the values Tilt installed it with.
func setRBACScope(ctx context.Context, t *testing.T, scope string) {
	t.Helper()
	dir := util.ProjectDir(t)

	values, err := exec.CommandContext(ctx, "helm", "get", "values", "nvcre", "-n", "nvcre", "-o", "yaml").Output()
	require.NoError(t, err, "read the UAT release values")
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(valuesFile, values, 0o600))

	cmd := exec.CommandContext(ctx, "helm", "upgrade", "nvcre", filepath.Join(dir, "helm", "cluster-readiness-engine"),
		"-n", "nvcre", "-f", valuesFile, "--set", "rbac.scope="+scope, "--wait", "--timeout", "5m")
	out, err := cmd.CombinedOutput()
	t.Logf("helm upgrade rbac.scope=%s:\n%s", scope, out)
	require.NoError(t, err)
}

func waitForWorkflowReason(
	ctx context.Context, t *testing.T, c client.Client, namespace, reason string,
) *nvcrev1alpha1.Workflow {
	t.Helper()
	var found *nvcrev1alpha1.Workflow
	require.Eventually(t, func() bool {
		list := &nvcrev1alpha1.WorkflowList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return false
		}
		for i := range list.Items {
			cond := meta.FindStatusCondition(list.Items[i].Status.Conditions, nvcrev1alpha1.WorkflowInProgress)
			if cond != nil && cond.Status == "True" && cond.Reason == reason {
				found = &list.Items[i]
				return true
			}
		}
		return false
	}, util.PollTimeout, util.PollInterval, "no Workflow in %s reached InProgress/%s", namespace, reason)
	return found
}

func waitForEvent(ctx context.Context, t *testing.T, c client.Client, namespace, regarding, reason string) {
	t.Helper()
	require.Eventually(t, func() bool {
		list := &eventsv1.EventList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return false
		}
		for _, e := range list.Items {
			if e.Regarding.Name == regarding && e.Reason == reason {
				return true
			}
		}
		return false
	}, util.PollTimeout, util.PollInterval, fmt.Sprintf("no %s event regarding %s", reason, regarding))
}
