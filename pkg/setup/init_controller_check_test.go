// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// flagSkipDeps skips the Kubeflow Trainer phase in the envtest init runs.
const flagSkipDeps = "--skip-phases=deps"

// crashingManagerPod is the controller pod TestInitChecksTheController
// creates: OOMKilled three times and now in CrashLoopBackOff.
const crashingManagerPod = "nvcre-manager-crashing"

// createCrashLoopingManagerPod stands in for a controller the kubelet keeps
// OOMKilling, as in issue #504.
func createCrashLoopingManagerPod(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{Name: nvcreNamespace}
	require.NoError(t, client.IgnoreAlreadyExists(c.Create(ctx, ns)))

	pod := &corev1.Pod{
		Name:      crashingManagerPod,
		Namespace: nvcreNamespace,
		Labels:    map[string]string{managerPodLabel: managerPodLabelValue, releaseInstanceLabel: helmReleaseName},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  managerContainerName,
			Image: stableManagerImage,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("96Mi"),
			}},
		}}},
	}
	require.NoError(t, c.Create(ctx, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         managerContainerName,
		Image:        stableManagerImage,
		RestartCount: 3,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: reasonOOMKilled, ExitCode: 137,
		}},
	}}
	require.NoError(t, c.Status().Update(ctx, pod))
}

// TestInitChecksTheController pins that setup init runs both controller
// checks (issue #504): after helm --wait passes it fails on a crash-looping
// controller, and when helm --wait fails it prints the controller's crash
// next to Helm's error.
func TestInitChecksTheController(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping the envtest setup init runs")
	}
	realHelm := requireHelm(t)

	suite := &testutil.IntegrationTestSuite{}
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)
	user, err := suite.Environment.ControlPlane.AddUser(
		envtest.User{Name: "setup-init", Groups: []string{"system:masters"}}, nil)
	require.NoError(t, err)
	kubeconfigData, err := user.KubeConfig()
	require.NoError(t, err)
	crds, err := os.ReadFile(filepath.Join("testdata", "apply-chart-crds", "fresh-install-creates", "input_crds.yaml"))
	require.NoError(t, err)
	createCrashLoopingManagerPod(t, suite.Client)

	crash := fmt.Sprintf("the NVCRE controller is crash-looping: pod %s/%s restarted 3 times, "+
		"last exit OOMKilled (exit code 137).", nvcreNamespace, crashingManagerPod)
	tests := []struct {
		name        string
		upgradeExit int
		wantErr     string
		wantOut     string
	}{
		{"helm --wait passes and the check finds the crash", 0, crash, ""},
		{"helm --wait fails and init names the crash", 1, "helm upgrade: exit status 1", "[helm] " + crash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			kubeconfigPath := filepath.Join(dir, "kubeconfig")
			crdFile := filepath.Join(dir, "crds.yaml")
			require.NoError(t, os.WriteFile(kubeconfigPath, kubeconfigData, 0o600))
			require.NoError(t, os.WriteFile(crdFile, crds, 0o600))
			writeFakeHelm(t, dir, fmt.Sprintf(`
case "$1" in
get) echo 'Error: release: not found' >&2; exit 1 ;;
template) exec %[1]q "$@" ;;
show) cat %[2]q; exit 0 ;;
upgrade) exit %[3]d ;;
registry) cat >/dev/null; exit 0 ;;
esac
exit 1`, realHelm, crdFile, tt.upgradeExit))
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			cmd := newInitCommand("v0.8.0")
			var out bytes.Buffer
			cmd.SetIn(strings.NewReader(""))
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs([]string{"--kubeconfig", kubeconfigPath, flagSkipDeps, "--auto-approve"})

			err := cmd.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			if tt.wantOut != "" {
				assert.Contains(t, out.String(), tt.wantOut)
			}
			assert.NotContains(t, out.String(), "initialized successfully")
		})
	}
}
