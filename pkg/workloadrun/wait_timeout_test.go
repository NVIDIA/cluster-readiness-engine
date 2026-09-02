// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Name/namespace literals reused across this file's fixtures.
const (
	testWorkloadRunTimeoutRun = "timeout-run"
	testWorkloadRunNamespace  = "test-ns"
)

// newWorkloadRunFakeClient is defined in run_cleanup_test.go and shared by
// the tests in this file.

type workloadRunGetErrorClient struct {
	client.WithWatch
	err error
}

func (c workloadRunGetErrorClient) Get(
	context.Context, client.ObjectKey, client.Object, ...client.GetOption,
) error {
	return c.err
}

type workloadRunDeadlineClient struct {
	client.WithWatch
	sawDeadline     bool
	sawLiveDeadline bool
}

func (c *workloadRunDeadlineClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.sawDeadline = true
		c.sawLiveDeadline = ctx.Err() == nil && time.Now().Before(deadline)
	}
	return c.WithWatch.Get(ctx, key, obj, opts...)
}

func normalizeWorkloadRunReportOutput(output string) string {
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

func TestWatchWorkloadRunImmediateTimeout(t *testing.T) {
	wc := newWorkloadRunFakeClient(t)
	var out bytes.Buffer

	run, err := watchWorkloadRun(context.Background(), wc, testWorkloadRunTimeoutRun, testWorkloadRunNamespace, 0, &out)

	assert.Nil(t, run)
	require.Error(t, err)
	assert.True(t, isWorkloadRunWaitTimeout(err))
	assert.Equal(t, "timeout waiting for WorkloadRun timeout-run", err.Error())
}

// TestFinishWorkloadRunWaitTimeout covers the wait-timeout reporting path
// (issue #219): a timed-out watch returns a nil WorkloadRun, so the finish
// step retrieves the still-live object with a bounded context, prints a
// partial report, writes --results-file, and explains how to monitor or stop
// the run — mirroring finishCertificationWait (PR #207).
func TestFinishWorkloadRunWaitTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "finish-workloadrun-wait-timeout",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		// json tags, not yaml: sigs.k8s.io/yaml converts YAML to JSON and
		// encoding/json ignores yaml tags, silently zeroing mistyped keys.
		var input struct {
			Name      string `json:"name"`
			RunExists *bool  `json:"runExists"`
			GetError  string `json:"getError"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		if input.Name == "" {
			input.Name = testWorkloadRunTimeoutRun
		}

		run := &nvcrev1alpha1.WorkloadRun{
			Name: input.Name, Namespace: testWorkloadRunNamespace,
			Status: nvcrev1alpha1.WorkloadRunStatus{
				Conditions: []metav1.Condition{{
					Type:   nvcrev1alpha1.WorkloadRunInProgress,
					Status: metav1.ConditionTrue,
					Reason: "Running",
				}},
				WorkflowRef: &nvcrev1alpha1.WorkflowReference{Name: input.Name + "-wf"},
			},
		}
		wf := &nvcrev1alpha1.Workflow{
			Name: input.Name + "-wf", Namespace: testWorkloadRunNamespace,
			Status: nvcrev1alpha1.WorkflowStatus{
				Conditions: []metav1.Condition{{
					Type:   nvcrev1alpha1.WorkflowInProgress,
					Status: metav1.ConditionTrue,
					Reason: "Running",
				}},
			},
		}
		runExists := input.RunExists == nil || *input.RunExists
		var wc client.WithWatch
		if runExists {
			wc = newWorkloadRunFakeClient(tc.T, run, wf)
		} else {
			wc = newWorkloadRunFakeClient(tc.T)
		}
		if input.GetError != "" {
			wc = workloadRunGetErrorClient{WithWatch: wc, err: errors.New(input.GetError)}
		}
		resultsFile := filepath.Join(tc.T.TempDir(), "results.json")
		var out bytes.Buffer
		waitErr := &workloadRunWaitTimeoutError{name: input.Name}

		gotErr := finishWorkloadRunWait(
			context.Background(), wc,
			&wrRunConfig{run: run, resultsFile: resultsFile, out: &out},
			nil, waitErr,
		)

		assert.Same(tc.T, waitErr, gotErr)
		if runExists && input.GetError == "" {
			data, err := os.ReadFile(resultsFile)
			if err != nil {
				return err
			}
			var result struct {
				Name   string `json:"name"`
				Result string `json:"result"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}
			assert.Equal(tc.T, input.Name, result.Name)
			assert.Equal(tc.T, "RUNNING", result.Result)
		} else {
			_, err := os.Stat(resultsFile)
			assert.True(tc.T, os.IsNotExist(err))
		}

		tc.Actual = normalizeWorkloadRunReportOutput(
			strings.ReplaceAll(out.String(), resultsFile, "<results-file>"),
		)
		return nil
	})
}

func TestFinishWorkloadRunWaitBoundsPostTimeoutReads(t *testing.T) {
	run := &nvcrev1alpha1.WorkloadRun{
		Name: testWorkloadRunTimeoutRun, Namespace: testWorkloadRunNamespace,
	}
	wc := &workloadRunDeadlineClient{WithWatch: newWorkloadRunFakeClient(t, run)}
	var out bytes.Buffer
	waitErr := &workloadRunWaitTimeoutError{name: run.Name}

	gotErr := finishWorkloadRunWait(
		context.Background(), wc,
		&wrRunConfig{run: run, out: &out},
		nil, waitErr,
	)

	assert.Same(t, waitErr, gotErr)
	assert.True(t, wc.sawDeadline)
	assert.True(t, wc.sawLiveDeadline)
}

// TestFinishWorkloadRunWaitTerminalAtTimeout covers the race where the
// WorkloadRun reaches a terminal condition between the watch deadline and the
// post-timeout Get: the report shows the fresh terminal state, and the exit
// still reflects the elapsed wait deadline.
func TestFinishWorkloadRunWaitTerminalAtTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "finish-workloadrun-wait-terminal-at-timeout",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			ConditionType string `json:"conditionType"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		run := &nvcrev1alpha1.WorkloadRun{
			Name: testWorkloadRunTimeoutRun, Namespace: testWorkloadRunNamespace,
			Status: nvcrev1alpha1.WorkloadRunStatus{Conditions: []metav1.Condition{{
				Type: input.ConditionType, Status: metav1.ConditionTrue,
			}}},
		}
		wc := newWorkloadRunFakeClient(tc.T, run)
		var out bytes.Buffer
		waitErr := &workloadRunWaitTimeoutError{name: run.Name}

		gotErr := finishWorkloadRunWait(
			context.Background(), wc,
			&wrRunConfig{run: run, out: &out},
			nil, waitErr,
		)

		assert.Same(tc.T, waitErr, gotErr)
		tc.Actual = normalizeWorkloadRunReportOutput(out.String())
		return nil
	})
}

func TestFinishWorkloadRunWaitDoesNotMaskOtherWatchErrors(t *testing.T) {
	run := &nvcrev1alpha1.WorkloadRun{
		Name: "test-run", Namespace: testWorkloadRunNamespace,
	}
	wc := newWorkloadRunFakeClient(t, run)
	resultsFile := filepath.Join(t.TempDir(), "results.json")
	var out bytes.Buffer
	waitErr := errors.New("watch disconnected")

	gotErr := finishWorkloadRunWait(
		context.Background(), wc,
		&wrRunConfig{run: run, resultsFile: resultsFile, out: &out},
		nil, waitErr,
	)

	assert.Same(t, waitErr, gotErr)
	assert.Empty(t, out.String())
	_, err := os.Stat(resultsFile)
	assert.True(t, os.IsNotExist(err))
}
