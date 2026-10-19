// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// workspacePath matches a path inside a values workspace, which differs on
// every run.
var workspacePath = regexp.MustCompile(`\S*/nvcrectl-values-[^/\s]+/`)

// initRunResult is what one `setup init` run of a TestInitValuesAcrossRuns
// case did.
type initRunResult struct {
	Args  []string `json:"args"`
	Error string   `json:"error,omitempty"`
	// Summary is the preflight values summary, the [preflight] lines after
	// "Checking prerequisites".
	Summary []string `json:"summary,omitempty"`
	// HelmCalls are the Helm invocations in order, with temporary paths
	// replaced.
	HelmCalls []string `json:"helmCalls"`
	// UpgradeValues is the values file the upgrade was given, which the fake
	// Helm stores as the release's values for the next run.
	UpgradeValues map[string]any `json:"upgradeValues,omitempty"`
	// WorkspaceRemoved is true when the values file is gone after the run.
	WorkspaceRemoved bool `json:"workspaceRemoved,omitempty"`
	// PullSecret is true when nvcrectl-pull-secret exists after the run.
	PullSecret bool `json:"pullSecret"`
}

// TestInitValuesAcrossRuns runs the real `setup init` command several times
// in a row against envtest, the way an operator or a harness re-runs it, and
// records what each run gave Helm. Helm's own `template` parses the values;
// the fake Helm around it stores the values file each upgrade receives as the
// release's values and returns them to the next run's `helm get values`, as
// Helm does. This pins ADR-098 end to end: values survive a re-run with no
// flags, the image is worked out on every run, a conflict changes nothing,
// and the upgrade gets one -f and no --set of nvcrectl's own.
func TestInitValuesAcrossRuns(t *testing.T) {
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
	createStableManagerPod(t, suite.Client)

	p := testutil.TestCaseParser{Subdir: "init-values-runs", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			// Release is the release's stored values before the first run,
			// as if set with `helm upgrade`; omitted means not installed.
			Release map[string]any `json:"release"`
			// Runs are the setup init runs in order. Every run also gets
			// --kubeconfig, --skip-phases=deps and --auto-approve.
			Runs []struct {
				Args  []string `json:"args"`
				Stdin string   `json:"stdin"`
			} `json:"runs"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		caseDir, err := filepath.Abs(filepath.Join("testdata", "init-values-runs", tc.Name))
		if err != nil {
			return err
		}
		ctx := context.Background()
		secret := &corev1.Secret{}
		secret.Name, secret.Namespace = pullSecretName, nvcreNamespace
		if err := client.IgnoreNotFound(suite.Client.Delete(ctx, secret)); err != nil {
			return err
		}

		dir := t.TempDir()
		kubeconfigPath := filepath.Join(dir, "kubeconfig")
		store := filepath.Join(dir, "release.json")
		log := filepath.Join(dir, "helm.log")
		crdFile := filepath.Join(dir, "crds.yaml")
		for path, data := range map[string][]byte{kubeconfigPath: kubeconfigData, crdFile: crds} {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return err
			}
		}
		if in.Release != nil {
			data, err := marshalValues(in.Release)
			if err != nil {
				return err
			}
			if err := os.WriteFile(store, data, 0o600); err != nil {
				return err
			}
		}
		writeFakeHelm(t, dir, fmt.Sprintf(`
echo "$*" >> %[1]q
case "$1" in
get)
  if [ -f %[2]q ]; then cat %[2]q; exit 0; fi
  echo 'Error: release: not found' >&2; exit 1 ;;
template) exec %[3]q "$@" ;;
show) cat %[4]q; exit 0 ;;
upgrade)
  while [ $# -gt 0 ]; do
    if [ "$1" = -f ]; then shift; cp "$1" %[2]q; fi
    shift
  done
  exit 0 ;;
registry) cat >/dev/null; exit 0 ;;
esac
exit 1`, log, store, realHelm, crdFile))
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

		var results []initRunResult
		for _, run := range in.Runs {
			_ = os.Remove(log)
			stored, _ := os.ReadFile(store)

			cmd := newInitCommand("v0.8.0")
			var out bytes.Buffer
			cmd.SetIn(strings.NewReader(run.Stdin))
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			args := []string{"--kubeconfig", kubeconfigPath, "--skip-phases=deps", "--auto-approve"}
			for _, arg := range run.Args {
				// {case} names the case directory, for values files kept beside
				// input.yaml.
				args = append(args, strings.ReplaceAll(arg, "{case}", caseDir))
			}
			cmd.SetArgs(args)
			result := initRunResult{Args: run.Args}
			if err := cmd.Execute(); err != nil {
				result.Error = err.Error()
			}

			calls, valuesFile, err := readHelmLog(log, kubeconfigPath)
			if err != nil {
				return err
			}
			result.HelmCalls = calls
			result.Summary = preflightSummary(out.String())
			if valuesFile != "" {
				data, err := os.ReadFile(store)
				if err != nil {
					return err
				}
				if result.UpgradeValues, err = decodeValues(data); err != nil {
					return err
				}
				_, statErr := os.Stat(valuesFile)
				result.WorkspaceRemoved = errors.Is(statErr, fs.ErrNotExist)
			} else if after, _ := os.ReadFile(store); !bytes.Equal(stored, after) {
				return errors.New("the release's values changed without an upgrade")
			}

			getErr := suite.Client.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{})
			if getErr != nil && !apierrors.IsNotFound(getErr) {
				return getErr
			}
			result.PullSecret = getErr == nil
			if err := assertNoValuesPrinted(result.UpgradeValues, out.String()); err != nil {
				return err
			}
			results = append(results, result)
		}

		b, err := marshalValues(results)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// readHelmLog returns the Helm calls the fake Helm logged, with temporary
// paths replaced, and the values file the upgrade was given, if any.
func readHelmLog(log, kubeconfigPath string) ([]string, string, error) {
	logged, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
	var calls []string
	var valuesFile string
	for line := range strings.SplitSeq(strings.TrimSpace(string(logged)), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "upgrade ") {
			if _, after, ok := strings.Cut(line, " -f "); ok {
				valuesFile, _, _ = strings.Cut(after, " ")
			}
		}
		line = strings.ReplaceAll(line, kubeconfigPath, "<kubeconfig>")
		calls = append(calls, workspacePath.ReplaceAllString(line, "<workspace>/"))
	}
	return calls, valuesFile, nil
}

// stableManagerImage is the stand-in controller pod's image; nothing pulls it.
const stableManagerImage = "nvcre-manager:test"

// createStableManagerPod stands in for the controller a successful upgrade
// leaves running. The fake helm's upgrade creates nothing and envtest runs no
// kubelet, so without it init's post-install check would wait out its
// timeout on every run. The pod has been Ready for an hour, so the check
// passes on its first read.
func createStableManagerPod(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{Name: nvcreNamespace}
	require.NoError(t, client.IgnoreAlreadyExists(c.Create(ctx, ns)))

	pod := &corev1.Pod{
		Name:      "nvcre-manager-stable",
		Namespace: nvcreNamespace,
		Labels:    map[string]string{managerPodLabel: managerPodLabelValue, releaseInstanceLabel: helmReleaseName},
		Spec:      corev1.PodSpec{Containers: []corev1.Container{{Name: managerContainerName, Image: stableManagerImage}}},
	}
	require.NoError(t, c.Create(ctx, pod))
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  managerContainerName,
		Image: stableManagerImage,
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
		}},
	}}
	require.NoError(t, c.Status().Update(ctx, pod))
}

// preflightSummary returns the preflight values summary lines from init's
// output.
func preflightSummary(out string) []string {
	var summary []string
	for line := range strings.SplitSeq(out, "\n") {
		if (strings.HasPrefix(line, "[preflight] ") || strings.HasPrefix(line, "  ")) &&
			!strings.Contains(line, "Checking prerequisites") && !strings.HasPrefix(line, "  CRD ") {
			summary = append(summary, line)
		}
	}
	return summary
}
