// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

// unresolvedRenderInput is the WorkloadRun the standalone integration fixture
// was rendered from. The override deliberately carries a queue-changing
// mutation — it redirects the runtime's queue label to queue-b while the
// intent says queue-a — because that mutation is what the fixture relies on
// resolving later, under the controller.
const unresolvedRenderInput = `
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: standalone-render
  namespace: default
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3
  image: nvcr.io/nvidia/pytorch:25.08-py3
  numNodes: 2
  framework:
    torch:
      script: /workspace/train.py
  gangScheduler:
    schedulerName: kai-scheduler
    queue: queue-a
  overrides:
    - when:
        platform:
          equals: aws
      dependencies:
        - apiVersion: trainer.kubeflow.org/v1alpha1
          kind: TrainingRuntime
          metadata:
            name: standalone-render-runtime
            namespace: default
          spec:
            template:
              spec:
                replicatedJobs:
                  - name: node
                    template:
                      metadata:
                        labels:
                          kai.scheduler/queue: queue-b
`

// TestUnresolvedRenderRetainsIntentAndOverrides guards the integration fixture
// cmd/integration/testdata/reconcile/workflow-standalone-unresolved-render,
// whose input is saved `nvcrectl workloadrun render` output produced without
// --platform.
//
// That fixture proves a template submitted on its own still enforces its
// owner's gang-scheduling contract once its conditional overrides resolve. But
// saved output goes stale silently: if the CLI ever stopped emitting
// spec.gangScheduler on the unresolved path, or stopped carrying the user's
// override through, the fixture would keep passing against a document the CLI
// no longer produces and would be testing nothing.
//
// So this drives runWorkloadRunRender itself rather than the builder beneath
// it, and parses what the command actually prints. Going through the builder
// alone would miss the CLI dropping either field afterwards — it nils
// spec.overrides on the resolved path, which is precisely the kind of step
// that could grow an unresolved-path bug.
//
// A failure here means the fixture needs regenerating, not that the assertion
// is wrong.
func TestUnresolvedRenderRetainsIntentAndOverrides(t *testing.T) {
	const (
		wantQueue    = "queue-a"
		wantQueueKey = "kai.scheduler/queue"
		// The value the user's override redirects the runtime queue to. It
		// appears nowhere else, so finding it identifies that specific
		// override rather than any of the platform overrides the builder adds
		// alongside it.
		overrideQueue = "queue-b"
	)

	path := filepath.Join(t.TempDir(), "workloadrun.yaml")
	require.NoError(t, os.WriteFile(path, []byte(unresolvedRenderInput), 0o600))

	// No --platform, so the overrides stay conditional and unresolved.
	emitted := captureStdout(t, func() error {
		return runWorkloadRunRender(path, "yaml", "")
	})

	var workflow nvcrev1alpha1.Workflow
	require.NoError(t, yaml.Unmarshal([]byte(emitted), &workflow),
		"the command's output is not a Workflow document")

	// The intent must survive into unresolved output: it is the only thing
	// that lets a standalone Workflow enforce the contract later.
	require.NotNilf(t, workflow.Spec.GangScheduler,
		"rendered output dropped spec.gangScheduler; the standalone fixture cannot "+
			"enforce anything without it, so regenerate it once the field is restored")
	if got := workflow.Spec.GangScheduler.Queue; got != wantQueue {
		t.Errorf("persisted queue = %q, want %q", got, wantQueue)
	}
	if got := workflow.Spec.GangScheduler.QueueLabelKey; got != wantQueueKey {
		t.Errorf("persisted queueLabelKey = %q, want it resolved to the KAI default", got)
	}

	// The user's own override must survive, identified by its mutation rather
	// than by counting: the builder appends platform overrides of its own, so
	// a non-empty list says nothing about whether the user's one is still
	// there.
	found := 0
	for _, o := range workflow.Spec.Overrides {
		if !overrideRedirectsQueue(o, overrideQueue) {
			continue
		}
		found++
		require.NotNilf(t, o.When.Platform,
			"the user's override lost its platform condition, so it would no longer "+
				"resolve on a target and the fixture would prove nothing")
		if got := o.When.Platform.Equals; got != "aws" {
			t.Errorf("user override platform condition = %q, want %q", got, "aws")
		}
	}
	if found != 1 {
		t.Errorf("found %d override(s) redirecting the runtime queue to %q, want exactly 1; "+
			"the user's conditional override is what the standalone fixture depends on",
			found, overrideQueue)
	}

	// The construction-time merge must already have placed the configured
	// queue on the workload object, so the emitted template is internally
	// consistent before any override resolves.
	labels := workload.LabelsOf(workflow.Spec.JobTemplate.Spec.WorkloadMetadata)
	if got := labels[wantQueueKey]; got != wantQueue {
		t.Errorf("workload object queue label = %q, want %q", got, wantQueue)
	}
}

// overrideRedirectsQueue reports whether o carries a dependency mutation
// setting a queue label to queue. The dependency payloads are opaque JSON, so
// this matches on the rendered text rather than decoding a runtime shape the
// override only partially specifies.
func overrideRedirectsQueue(o nvcrev1alpha1.OverrideSpec, queue string) bool {
	for _, dep := range o.Dependencies {
		if strings.Contains(string(dep.Raw), `"`+queue+`"`) {
			return true
		}
	}
	return false
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
// The render command writes to stdout directly, and its output is the artifact
// under test here, not a return value.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	original := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = original
	require.NoError(t, w.Close())

	var buf bytes.Buffer
	_, copyErr := io.Copy(&buf, r)
	require.NoError(t, copyErr)
	require.NoError(t, r.Close())
	require.NoError(t, runErr, "rendering without --platform must succeed: "+
		"unresolved output is a template, not a validated manifest")

	return buf.String()
}
