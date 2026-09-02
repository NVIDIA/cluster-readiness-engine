// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

func newWorkloadRunFakeClient(t testing.TB, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// wrDeleteErrorClient fails deletion of WorkloadRun objects to exercise the
// cleanup warning path. All other operations pass through.
type wrDeleteErrorClient struct {
	client.WithWatch
	err error
}

func (c wrDeleteErrorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*nvcrev1alpha1.WorkloadRun); ok {
		return c.err
	}
	return c.WithWatch.Delete(ctx, obj, opts...)
}

// wrCompletingClient marks a WorkloadRun as Succeeded at creation time,
// simulating a controller that completes the run instantly, so --wait
// observes a terminal state and prints a report.
type wrCompletingClient struct {
	client.WithWatch
}

func (c wrCompletingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if run, ok := obj.(*nvcrev1alpha1.WorkloadRun); ok {
		run.Status.Conditions = []metav1.Condition{{
			Type:               nvcrev1alpha1.WorkloadRunSucceeded,
			Status:             metav1.ConditionTrue,
			Reason:             "WorkflowSucceeded",
			Message:            "Workflow completed successfully",
			LastTransitionTime: metav1.Now(),
		}}
	}
	return c.WithWatch.Create(ctx, obj, opts...)
}

// wrElapsedRe matches trailing elapsed-time suffixes like "(5s)" or "(1m30s)"
// that watch lines append; they depend on wall-clock timing and must be
// normalized for golden-file stability.
var wrElapsedRe = regexp.MustCompile(`\((\d+h)?(\d+m)?\d+(\.\d+)?s\)`)

// normalizeWorkloadRunRunOutput strips report box-drawing characters and
// elapsed-time suffixes, mirroring the certification run cleanup tests.
func normalizeWorkloadRunRunOutput(output string) string {
	output = wrElapsedRe.ReplaceAllString(output, "(elapsed)")
	lines := make([]string, 0)
	for line := range strings.SplitSeq(output, "\n") {
		if strings.ContainsAny(line, "═─") {
			continue
		}
		if strings.HasPrefix(line, "║") || strings.HasPrefix(line, "│") {
			line = strings.TrimSpace(strings.Trim(line, "║│"))
		} else {
			line = strings.TrimRight(line, " \t")
		}
		if line == "" && (len(lines) == 0 || lines[len(lines)-1] == "") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
}

// TestExecuteWorkloadRunRunCleanup drives executeWorkloadRunRun with a fake
// client and verifies the --cleanup semantics mirror certification run:
// cleanup runs on every exit path, deletes the WorkloadRun only when this
// invocation created it, deletes the namespace only when this invocation
// created it, surfaces failures as warnings, and prints the report before
// the deferred cleanup tears anything down.
func TestExecuteWorkloadRunRunCleanup(t *testing.T) {
	// Shorten the cleanup deletion poll so the fake-client cases don't block
	// on the production 2s ticker.
	oldInterval := wrDeletionPollInterval
	wrDeletionPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { wrDeletionPollInterval = oldInterval })

	p := testutil.TestCaseParser{
		Subdir:         "run-cleanup",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			DoWait                bool   `json:"doWait"`
			DoCleanup             bool   `json:"doCleanup"`
			TimeoutSeconds        int    `json:"timeoutSeconds"`
			NamespaceExists       bool   `json:"namespaceExists"`
			RunPreexists          bool   `json:"runPreexists"`
			DeleteError           string `json:"deleteError"`
			CompleteOnCreate      bool   `json:"completeOnCreate"`
			ExpectRunExists       bool   `json:"expectRunExists"`
			ExpectNamespaceExists bool   `json:"expectNamespaceExists"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_config.yaml"]), &input); err != nil {
			return err
		}

		var run nvcrev1alpha1.WorkloadRun
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_workloadrun.yaml"]), &run); err != nil {
			return err
		}

		node := &corev1.Node{
			Name: "gpu-node-0",
			Labels: map[string]string{
				"nvidia.com/gpu.present": "true",
				"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
			}}
		objects := []client.Object{node}
		if input.NamespaceExists {
			objects = append(objects, &corev1.Namespace{
				Name: run.Namespace,
			})
		}
		if input.RunPreexists {
			objects = append(objects, run.DeepCopy())
		}

		var wc = newWorkloadRunFakeClient(tc.T, objects...)
		if input.CompleteOnCreate {
			wc = wrCompletingClient{WithWatch: wc}
		}
		if input.DeleteError != "" {
			wc = wrDeleteErrorClient{WithWatch: wc, err: errors.New(input.DeleteError)}
		}

		var out bytes.Buffer
		cfg := &wrRunConfig{
			run:         &run,
			doWait:      input.DoWait,
			doCleanup:   input.DoCleanup,
			timeout:     time.Duration(input.TimeoutSeconds) * time.Second,
			out:         &out,
			watchClient: wc,
		}

		execErr := executeWorkloadRunRun(cfg)

		// Post-conditions: the WorkloadRun and namespace exist exactly when the
		// case says they should.
		gotRun := &nvcrev1alpha1.WorkloadRun{}
		getErr := wc.Get(context.Background(),
			client.ObjectKey{Name: run.Name, Namespace: run.Namespace}, gotRun)
		if input.ExpectRunExists {
			assert.NoError(tc.T, getErr, "expected WorkloadRun to still exist")
		} else {
			assert.True(tc.T, apierrors.IsNotFound(getErr),
				"expected WorkloadRun to be deleted, got: %v", getErr)
		}
		gotNS := &corev1.Namespace{}
		nsErr := wc.Get(context.Background(), client.ObjectKey{Name: run.Namespace}, gotNS)
		if input.ExpectNamespaceExists {
			assert.NoError(tc.T, nsErr, "expected namespace to still exist")
		} else {
			assert.True(tc.T, apierrors.IsNotFound(nsErr),
				"expected namespace to be deleted, got: %v", nsErr)
		}

		actual := out.String()
		if execErr != nil {
			actual += "--- error: " + execErr.Error() + "\n"
		}
		tc.Actual = normalizeWorkloadRunRunOutput(actual)
		return nil
	})
}
