// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
)

const metricLabelNamespace = "namespace"

func init() {
	_ = nvcrev1alpha1.AddToScheme(scheme.Scheme)
	_ = trainerv1alpha1.AddToScheme(scheme.Scheme)
}

func TestIntegration(t *testing.T) {
	logf.SetLogger(zap.New(zap.WriteTo(testWriter{t}), zap.UseDevMode(true)))

	suite := &testutil.IntegrationTestSuite{}
	suite.Environment.CRDDirectoryPaths = []string{
		"../../helm/nvcre/crds",
		"../../hack/crds",
	}
	suite.Environment.ErrorIfCRDPathMissing = true
	// Enable the resource.k8s.io API group so ResourceClaimTemplate is available.
	suite.Environment.ControlPlane.GetAPIServer().Configure().Append("runtime-config", "resource.k8s.io/v1=true")
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)

	parser := &testutil.TestCaseParser{
		Subdir:         "reconcile",
		ExpectedSuffix: ".json",
	}

	parser.TestDir(t, func(tc *testutil.TestCase) error {
		tt := tc.T.(*testing.T)

		// Install any per-test-case CRDs from input_crd_*.yaml files.
		installTestCRDs(tt, suite.Config, tc)

		cfg := parseWaitConfig(tc)

		// Generate GPU nodes programmatically if configured.
		if cfg.GenerateNodes != nil {
			generateTestNodes(tt, suite.Client, cfg.GenerateNodes)
		}

		// Create objects manually (SetupTest has a bug with status updates).
		objs := createTestObjects(tt, suite.Client, tc)

		fakeFetcher := buildFakeLogFetcher(tc)
		mgr, cancel := startManager(tt, suite.Config, fakeFetcher)

		// Ensure cleanup always runs, even on test failure/timeout.
		// Without this, a timed-out test leaks objects (e.g., cluster-scoped
		// LogProfiles) into subsequent subtests, causing cascade failures.
		defer func() {
			cancel()
			time.Sleep(200 * time.Millisecond)
			deleteTestObjects(tt, suite.Client, objs)
		}()

		waitForCondition(tt, mgr.GetClient(), cfg)

		// Verify a Complete GoodputMeasurement stays frozen across a terminal
		// re-entry (ADR-072). Runs before collection so the golden pins it.
		var frozenGoodput map[string]any
		if cfg.VerifyFrozenGoodput != nil {
			frozenGoodput = verifyFrozenGoodput(tt, suite.Client, mgr.GetClient(), cfg)
		}

		// Delete resources after the initial wait (e.g., to test deletion cascade).
		if len(cfg.DeleteAfterWait) > 0 {
			deleteAfterWait(tt, mgr.GetClient(), cfg.DeleteAfterWait)
		}
		// Wait for specified resources to be fully deleted.
		if len(cfg.WaitForDeletion) > 0 {
			waitForDeletion(tt, mgr.GetClient(), cfg)
		}

		tc.Actual = collectAndSerialize(tt, mgr.GetClient(), cfg, frozenGoodput)
		return nil
	})
}

// testWriter adapts *testing.T to io.Writer for zap logger.
type testWriter struct {
	t *testing.T
}

func (tw testWriter) Write(p []byte) (n int, err error) {
	tw.t.Log(string(p))
	return len(p), nil
}

// installTestCRDs detects input_crd_*.yaml files in the test case, writes them
// to a temp directory, and installs them into the envtest API server.
func installTestCRDs(t *testing.T, cfg *rest.Config, tc *testutil.TestCase) {
	t.Helper()

	var crdFiles []string
	for name, content := range tc.Inputs {
		if strings.HasPrefix(name, "input_crd_") && strings.HasSuffix(name, ".yaml") {
			tmpDir := filepath.Join(os.TempDir(), "cre-integration-crds", tc.Name)
			require.NoError(t, os.MkdirAll(tmpDir, 0o755))
			path := filepath.Join(tmpDir, name)
			require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			crdFiles = append(crdFiles, tmpDir)
			t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
		}
	}
	if len(crdFiles) == 0 {
		return
	}

	// Deduplicate directory paths.
	seen := make(map[string]bool)
	var dirs []string
	for _, d := range crdFiles {
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}

	_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		Paths: dirs,
	})
	require.NoError(t, err)

	// Brief pause for API server to register new resources.
	time.Sleep(500 * time.Millisecond)
}

// generateTestNodes creates N GPU nodes with the specified labels.
// Node names are prefixed with "gen-" and a hash of the test name to avoid
// collisions with nodes defined in other test cases' input_client_objects.yaml.
func generateTestNodes(t *testing.T, c client.Client, spec *generateNodesSpec) {
	t.Helper()
	ctx := context.Background()
	// Use a short hash of the test name as prefix for uniqueness.
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name())))[:6]
	for i := range spec.Count {
		labels := map[string]string{
			"nvidia.com/gpu.present": "true",
		}
		name := fmt.Sprintf("gen-%s-node-%02d", h, i+1)
		labels["kubernetes.io/hostname"] = name
		maps.Copy(labels, spec.Labels)
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: labels,
			},
			Spec: corev1.NodeSpec{
				ProviderID: spec.ProviderID,
			},
		}
		require.NoError(t, c.Create(ctx, node), "failed to create generated node %s", name)
		t.Cleanup(func() {
			_ = c.Delete(ctx, node)
		})
	}
}

// createTestObjects parses and creates objects from test case input files,
// properly handling status subresource updates.
func createTestObjects(t *testing.T, c client.Client, tc *testutil.TestCase) []client.Object {
	t.Helper()
	ctx := context.Background()

	objs, _, err := tc.GetObjects(scheme.Scheme)
	require.NoError(t, err)

	created := make([]client.Object, 0, len(objs))
	for _, obj := range objs {
		// Save a deep copy with the original status before creating.
		statusCopy := obj.DeepCopyObject().(client.Object)

		// Create the object (this clears the status field).
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		require.NoError(t, c.Create(ctx, obj),
			"failed to create %s/%s", kind, obj.GetName())
		created = append(created, obj)

		// Re-fetch the created object to get the resourceVersion, then update status.
		// This is necessary because the apiserver clears status on create.
		freshObj := obj.DeepCopyObject().(client.Object)
		err := c.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, freshObj)
		require.NoError(t, err)

		// Copy status from the original into the fresh object that has resourceVersion.
		if copyStatus(freshObj, statusCopy) {
			err := c.Status().Update(ctx, freshObj)
			if err != nil && !strings.Contains(err.Error(), "not found") &&
				!strings.Contains(err.Error(), "the server could not find the requested resource") {
				// Ignore errors for resources without status subresource.
				t.Logf("Warning: status update failed for %s/%s: %v",
					freshObj.GetObjectKind().GroupVersionKind().Kind, freshObj.GetName(), err)
			}
		}
	}
	return created
}

// copyStatus copies status from src to dst. Returns true if status was non-empty.
func copyStatus(dst, src client.Object) bool { //nolint:gocyclo
	switch d := dst.(type) {
	case *nvcrev1alpha1.Job:
		s := src.(*nvcrev1alpha1.Job)
		hasStatus := len(s.Status.Conditions) > 0 || s.Status.WorkloadRef != nil ||
			len(s.Status.FailedNodes) > 0 || s.Status.RestartCount > 0
		if hasStatus {
			d.Status = s.Status
			return true
		}
	case *nvcrev1alpha1.Workflow:
		s := src.(*nvcrev1alpha1.Workflow)
		if len(s.Status.Conditions) > 0 || s.Status.Orchestration != nil ||
			s.Status.SucceededNodesRef != nil || s.Status.FailedNodesRef != nil ||
			len(s.Status.DependencyRefs) > 0 {
			d.Status = s.Status
			return true
		}
	case *nvcrev1alpha1.Certification:
		s := src.(*nvcrev1alpha1.Certification)
		if len(s.Status.Conditions) > 0 || len(s.Status.CategoryStatuses) > 0 {
			d.Status = s.Status
			return true
		}
	case *nvcrev1alpha1.WorkloadRun:
		s := src.(*nvcrev1alpha1.WorkloadRun)
		if len(s.Status.Conditions) > 0 || s.Status.WorkflowRef != nil {
			d.Status = s.Status
			return true
		}
	case *nvcrev1alpha1.GoodputMeasurement:
		s := src.(*nvcrev1alpha1.GoodputMeasurement)
		if len(s.Status.Conditions) > 0 || s.Status.Result != "" ||
			s.Status.CurrentStep > 0 || s.Status.PendingInterruption != nil ||
			s.Status.StartTime != nil || s.Status.InterruptionCount > 0 ||
			s.Status.LastCheckpointStep > 0 || s.Status.HighestStep > 0 ||
			s.Status.LastStepTimestamp != nil || s.Status.AvgStepTimeSec != "" ||
			s.Status.ApplicationStartTime != nil {
			d.Status = s.Status
			return true
		}
	case *nvcrev1alpha1.BandwidthMeasurement:
		s := src.(*nvcrev1alpha1.BandwidthMeasurement)
		if len(s.Status.Conditions) > 0 || len(s.Status.Results) > 0 ||
			s.Status.StartTime != nil || s.Status.CompletionTime != nil {
			d.Status = s.Status
			return true
		}
	case *corev1.Pod:
		s := src.(*corev1.Pod)
		if s.Status.Phase != "" {
			d.Status = s.Status
			return true
		}
	case *corev1.Node:
		// Node fixtures carry status.allocatable so tests can model differing
		// GPU capacity (issue #82). The API server allows status on Node
		// create, but re-applying it here keeps fixtures working even where
		// that changes.
		s := src.(*corev1.Node)
		if len(s.Status.Allocatable) > 0 || len(s.Status.Capacity) > 0 {
			d.Status = s.Status
			return true
		}
	case *trainerv1alpha1.TrainJob:
		s := src.(*trainerv1alpha1.TrainJob)
		if len(s.Status.Conditions) > 0 {
			d.Status = s.Status
			return true
		}
	case *batchv1.Job:
		s := src.(*batchv1.Job)
		if len(s.Status.Conditions) > 0 || s.Status.Active > 0 ||
			s.Status.Succeeded > 0 || s.Status.Failed > 0 {
			d.Status = s.Status
			return true
		}
	}
	return false
}

// deleteTestObjects cleans up created objects.
func deleteTestObjects(t *testing.T, c client.Client, objs []client.Object) {
	t.Helper()
	ctx := context.Background()
	// Delete in reverse order.
	for _, v := range slices.Backward(objs) {
		obj := v
		// Re-fetch to get latest resourceVersion.
		fresh := obj.DeepCopyObject().(client.Object)
		key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
		if err := c.Get(ctx, key, fresh); err != nil {
			continue // Already deleted.
		}
		// Remove finalizers to avoid blocking deletion.
		if len(fresh.GetFinalizers()) > 0 {
			fresh.SetFinalizers(nil)
			if err := c.Update(ctx, fresh); err != nil {
				t.Logf("Warning: failed to remove finalizers from %s/%s: %v",
					fresh.GetObjectKind().GroupVersionKind().Kind, fresh.GetName(), err)
			}
		}
		_ = c.Delete(ctx, fresh)
	}
}

// startManager creates and starts a controller manager in-process.
func startManager(
	t *testing.T, cfg *rest.Config, fetcher podlogs.PodLogFetcher,
) (ctrl.Manager, context.CancelFunc) {
	t.Helper()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // Disable metrics server — tests read from the global registry directly.
		},
		// Use production's cache configuration so the harness exercises the same
		// informer scoping (notably: only GPU-labelled Nodes are cached).
		Cache: controller.CacheOptions(),
		Controller: crconfig.Controller{
			SkipNameValidation:      func() *bool { v := true; return &v }(),
			MaxConcurrentReconciles: 5, // Prevent stale objects from blocking the reconcile queue.
		},
	})
	require.NoError(t, err)

	// Register field indexes through the same entry point production uses, so the
	// harness cannot drift from cmd/manager.
	require.NoError(t, controller.RegisterFieldIndexes(context.Background(), mgr.GetFieldIndexer()))

	// Register all controllers with short requeue intervals for test speed.
	err = (&controller.JobReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		WorkloadRequeueInterval: 1 * time.Second,
		MeasurementTimeout:      3 * time.Second,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	err = (&controller.WorkflowReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Recorder:           mgr.GetEventRecorder("workflow-controller"),
		JobRequeueInterval: 1 * time.Second,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	err = (&controller.CertificationReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		WorkflowRequeueInterval: 1 * time.Second,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	err = (&controller.GoodputMeasurementReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		LogFetcher: fetcher,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	err = (&controller.BandwidthMeasurementReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		LogFetcher: fetcher,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	err = (&controller.WorkloadRunReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("Manager stopped: %v", err)
		}
	}()

	// Wait for informer caches to sync before returning.
	// Without this, the controller may reconcile before pods/nodes
	// are visible in the cache, causing flaky test results.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		t.Fatal("timed out waiting for informer cache sync")
	}

	return mgr, cancel
}

// fakeLogFetcher returns pre-loaded log lines keyed by pod name.
type fakeLogFetcher struct {
	logs map[string][]string
}

func (f *fakeLogFetcher) FetchLogs(_ context.Context, _, podName string, opts podlogs.LogOptions) ([]string, error) {
	lines, ok := f.logs[podName]
	if !ok {
		return nil, fmt.Errorf("no fake logs for pod %s", podName)
	}
	if opts.SinceTime == nil {
		return lines, nil
	}
	// Mirror the real fetcher: SinceTime is passed server-side and the kubelet
	// returns only records at or after it (pkg/podlogs/fetcher.go). Filter the
	// canned lines by their leading RFC3339 timestamp; lines without a parseable
	// timestamp are kept so fixtures without timestamps behave as before.
	since := opts.SinceTime.Time
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.SplitN(line, " ", 2)
		if ts, err := time.Parse(time.RFC3339Nano, fields[0]); err == nil && ts.Before(since) {
			continue
		}
		filtered = append(filtered, line)
	}
	return filtered, nil
}

func buildFakeLogFetcher(tc *testutil.TestCase) podlogs.PodLogFetcher {
	logs := make(map[string][]string)
	for name, content := range tc.Inputs {
		if strings.HasPrefix(name, "input_logs_") && strings.HasSuffix(name, ".txt") {
			podName := strings.TrimSuffix(strings.TrimPrefix(name, "input_logs_"), ".txt")
			logs[podName] = strings.Split(content, "\n")
		}
	}
	return &fakeLogFetcher{logs: logs}
}

// waitConfig specifies what condition to wait for and what objects to collect.
type waitConfig struct {
	WaitFor struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Condition string `json:"condition"`
		Reason    string `json:"reason,omitempty"` // optional: wait for specific reason
	} `json:"waitFor"`
	Collect                 []collectSpec                `json:"collect"`
	CollectMetrics          *collectMetricsSpec          `json:"collectMetrics,omitempty"`
	CollectBandwidthMetrics *collectBandwidthMetricsSpec `json:"collectBandwidthMetrics,omitempty"`
	CollectJobMetrics       *collectJobMetricsSpec       `json:"collectJobMetrics,omitempty"`
	CollectTopologyMetrics  *collectTopologyMetricsSpec  `json:"collectTopologyMetrics,omitempty"`
	// GenerateNodes creates N GPU nodes programmatically before test objects.
	// Avoids repeating Node YAML in fixtures for multi-node tests.
	GenerateNodes *generateNodesSpec `json:"generateNodes,omitempty"`
	// VerifyFrozenGoodput drives the ADR-072 determinism check: after the
	// initial wait it snapshots the named GoodputMeasurement's status, touches
	// the referenced Job, strips the Complete condition to force the
	// controller back through the full terminal path (the stale-cache /
	// restart re-entry from issue #177), waits for Complete to be restored,
	// and asserts the regenerated status is byte-identical (modulo condition
	// transition timestamps, which re-adding a condition necessarily moves).
	VerifyFrozenGoodput *verifyFrozenGoodputSpec `json:"verifyFrozenGoodput,omitempty"`
	// DeleteAfterWait lists resources to delete after the initial wait completes.
	// The manager remains running so controllers can process the deletion cascade.
	DeleteAfterWait []collectSpec `json:"deleteAfterWait,omitempty"`
	// WaitForDeletion lists resources that must be fully deleted before collection.
	WaitForDeletion []collectSpec `json:"waitForDeletion,omitempty"`
	TimeoutSeconds  int           `json:"timeoutSeconds"`
}

type generateNodesSpec struct {
	Count      int               `json:"count"`
	Labels     map[string]string `json:"labels"`
	ProviderID string            `json:"providerID,omitempty"`
}

type verifyFrozenGoodputSpec struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Mode selects the re-entry style. "strip" (default) removes the Complete
	// condition to force a full terminal re-run and asserts byte-identical
	// regeneration — valid where every terminal field is a pure recompute.
	// "intact" leaves Complete in place and asserts the first-write-wins guard
	// keeps every status byte untouched. That is the honest assertion for the
	// Failed path: handleFailed's lostWorkTime/interruptionCount are
	// accumulators, so a strip-and-rerun legitimately re-adds them and
	// byte-identity cannot hold there.
	Mode string `json:"mode,omitempty"`
}

type collectMetricsSpec struct {
	Namespace      string   `json:"namespace"`
	Measurement    string   `json:"measurement"`
	SanitizeFields []string `json:"sanitizeFields"`
}

type collectBandwidthMetricsSpec struct {
	Namespace   string `json:"namespace"`
	Measurement string `json:"measurement"`
}

type collectJobMetricsSpec struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Workflow  string `json:"workflow"`
}

type collectTopologyMetricsSpec struct {
	Namespace   string `json:"namespace"`
	Workflow    string `json:"workflow"`
	TopologyKey string `json:"topologyKey"`
}

type collectSpec struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func parseWaitConfig(tc *testutil.TestCase) waitConfig {
	data, ok := tc.Inputs["input_config.yaml"]
	if !ok {
		panic("test case missing input_config.yaml")
	}
	var cfg waitConfig
	if err := sigyaml.Unmarshal([]byte(data), &cfg); err != nil {
		panic(fmt.Sprintf("failed to parse input_config.yaml: %v", err))
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 30
	}
	return cfg
}

func waitForCondition(t *testing.T, c client.Client, cfg waitConfig) {
	t.Helper()
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	interval := 500 * time.Millisecond

	require.Eventually(t, func() bool {
		ctx := context.Background()
		obj := getObject(ctx, t, c, collectSpec{
			Kind:      cfg.WaitFor.Kind,
			Name:      cfg.WaitFor.Name,
			Namespace: cfg.WaitFor.Namespace,
		})
		if obj == nil {
			return false
		}

		reason := cfg.WaitFor.Reason
		switch o := obj.(type) {
		case *nvcrev1alpha1.Job:
			return hasConditionWithReason(o.Status.Conditions, cfg.WaitFor.Condition, reason)
		case *nvcrev1alpha1.Workflow:
			return hasConditionWithReason(o.Status.Conditions, cfg.WaitFor.Condition, reason)
		case *nvcrev1alpha1.Certification:
			return hasConditionWithReason(o.Status.Conditions, cfg.WaitFor.Condition, reason)
		case *nvcrev1alpha1.WorkloadRun:
			return hasConditionWithReason(o.Status.Conditions, cfg.WaitFor.Condition, reason)
		case *nvcrev1alpha1.GoodputMeasurement:
			return hasConditionWithReason(o.Status.Conditions, cfg.WaitFor.Condition, reason)
		case *nvcrev1alpha1.BandwidthMeasurement:
			return hasConditionWithReason(o.Status.Conditions, cfg.WaitFor.Condition, reason)
		}
		return false
	}, timeout, interval, "timed out waiting for condition %s on %s/%s",
		cfg.WaitFor.Condition, cfg.WaitFor.Kind, cfg.WaitFor.Name)
}

// deleteAfterWait deletes the specified resources while the manager is still running,
// allowing controllers to process the deletion cascade (finalizers, child cleanup, etc.).
func deleteAfterWait(t *testing.T, c client.Client, specs []collectSpec) {
	t.Helper()
	ctx := context.Background()
	for _, spec := range specs {
		obj := getObject(ctx, t, c, spec)
		require.NotNil(t, obj, "deleteAfterWait: %s/%s not found", spec.Kind, spec.Name)
		require.NoError(t, c.Delete(ctx, obj),
			"deleteAfterWait: failed to delete %s/%s", spec.Kind, spec.Name)
		t.Logf("deleteAfterWait: deleted %s/%s", spec.Kind, spec.Name)
	}
}

// waitForDeletion polls until all specified resources are fully removed from the API server.
func waitForDeletion(t *testing.T, c client.Client, cfg waitConfig) {
	t.Helper()
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	interval := 500 * time.Millisecond

	for _, spec := range cfg.WaitForDeletion {
		require.Eventually(t, func() bool {
			obj := getObject(context.Background(), t, c, spec)
			return obj == nil
		}, timeout, interval, "timed out waiting for deletion of %s/%s", spec.Kind, spec.Name)
	}
}

// verifyFrozenGoodput pins the ADR-072 freeze: a Complete GoodputMeasurement's
// status must be a pure function of its persisted status, the Job, and the
// final log parse. It snapshots the status, touches the referenced Job, strips
// the Complete condition so the controller replays the entire terminal path —
// exactly what a stale informer cache or a controller restart before the
// Complete write causes — waits for Complete to be restored, and requires the
// regenerated status (including result, trainingTimeSec, and completionTime)
// to be byte-identical. Only condition transition timestamps are cleared
// before comparing: re-adding the stripped condition necessarily refreshes its
// LastTransitionTime.
// direct must be an uncached API client: the check strips a condition and then
// waits for the controller to restore it, and a cached read could satisfy the
// wait with the stale pre-strip object. cached is the manager's client, polled
// at the end so the subsequent collection sees the restored state.
func verifyFrozenGoodput(t *testing.T, direct, cached client.Client, cfg waitConfig) map[string]any {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Name: cfg.VerifyFrozenGoodput.Name, Namespace: cfg.VerifyFrozenGoodput.Namespace}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second

	gm := &nvcrev1alpha1.GoodputMeasurement{}
	require.NoError(t, direct.Get(ctx, key, gm))
	before := frozenStatusJSON(t, gm)

	// Touch the referenced Job: reconciling a terminal Job must not move the
	// measurement.
	if jobName := gm.Spec.JobRef.Name; jobName != "" {
		job := &nvcrev1alpha1.Job{}
		require.NoError(t, direct.Get(ctx, types.NamespacedName{Name: jobName, Namespace: key.Namespace}, job))
		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations["test.nvcre.nvidia.com/touched"] = "true"
		require.NoError(t, direct.Update(ctx, job))
	}

	// Intact mode: the GM controller has no Job watch, so the Job touch above
	// does not itself trigger a GM reconcile — and any GM reconcile that does
	// run (GM watch events, requeues) returns at the Complete gate. The
	// require.Never below therefore pins the ADR-072 convention directly:
	// nothing writes to a GoodputMeasurement whose Complete condition is True,
	// so nothing in the status — condition timestamps included — may move
	// during the window.
	if cfg.VerifyFrozenGoodput.Mode == "intact" {
		beforeRaw := rawStatusJSON(t, gm)
		require.Never(t, func() bool {
			fresh := &nvcrev1alpha1.GoodputMeasurement{}
			if err := direct.Get(ctx, key, fresh); err != nil {
				return false
			}
			return rawStatusJSON(t, fresh) != beforeRaw
		}, 4*time.Second, 250*time.Millisecond,
			"status moved during a replay with Complete intact (first-write-wins violated, ADR-072)")
		return map[string]any{
			"untouchedWithCompleteIntact": true,
			"result":                      gm.Status.Result,
			"trainingTimeSec":             gm.Status.TrainingTimeSec,
			"startTime":                   gm.Status.StartTime,
			"completionTime":              gm.Status.CompletionTime,
		}
	}

	// Strip Complete to force the controller back through the terminal path.
	apimeta.RemoveStatusCondition(&gm.Status.Conditions, nvcrev1alpha1.GoodputMeasurementComplete)
	require.NoError(t, direct.Status().Update(ctx, gm))

	require.Eventually(t, func() bool {
		fresh := &nvcrev1alpha1.GoodputMeasurement{}
		if err := direct.Get(ctx, key, fresh); err != nil {
			return false
		}
		return hasConditionWithReason(fresh.Status.Conditions, nvcrev1alpha1.GoodputMeasurementComplete, "")
	}, timeout, 250*time.Millisecond, "Complete was not restored after terminal re-entry")

	after := &nvcrev1alpha1.GoodputMeasurement{}
	require.NoError(t, direct.Get(ctx, key, after))
	afterJSON := frozenStatusJSON(t, after)

	require.Equal(t, before, afterJSON,
		"terminal re-entry must regenerate a byte-identical status (ADR-072)")

	// Let the manager's cache catch up so collection sees the restored state.
	require.Eventually(t, func() bool {
		fresh := &nvcrev1alpha1.GoodputMeasurement{}
		if err := cached.Get(ctx, key, fresh); err != nil {
			return false
		}
		return hasConditionWithReason(fresh.Status.Conditions, nvcrev1alpha1.GoodputMeasurementComplete, "")
	}, timeout, 250*time.Millisecond, "manager cache did not observe the restored Complete condition")

	return map[string]any{
		"byteIdenticalAfterReentry": before == afterJSON,
		"result":                    after.Status.Result,
		"trainingTimeSec":           after.Status.TrainingTimeSec,
		"startTime":                 after.Status.StartTime,
		"completionTime":            after.Status.CompletionTime,
	}
}

// rawStatusJSON marshals the full status, timestamps included — used by the
// intact-mode check where nothing at all may change.
func rawStatusJSON(t *testing.T, gm *nvcrev1alpha1.GoodputMeasurement) string {
	t.Helper()
	b, err := json.Marshal(gm.Status)
	require.NoError(t, err)
	return string(b)
}

// frozenStatusJSON marshals a measurement status for byte-level comparison
// across terminal re-entries. Conditions are a set keyed by type: stripping
// and re-adding one necessarily re-appends it and refreshes its transition
// timestamp, so conditions are sorted by type and their timestamps cleared.
// Every other byte must match.
func frozenStatusJSON(t *testing.T, gm *nvcrev1alpha1.GoodputMeasurement) string {
	t.Helper()
	s := gm.Status.DeepCopy()
	clearConditionTimestamps(s.Conditions)
	slices.SortFunc(s.Conditions, func(a, b metav1.Condition) int {
		return strings.Compare(a.Type, b.Type)
	})
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

func hasConditionWithReason(conditions []metav1.Condition, condType, reason string) bool {
	for _, c := range conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			if reason == "" || c.Reason == reason {
				return true
			}
		}
	}
	return false
}

func getObject(ctx context.Context, t *testing.T, c client.Client, spec collectSpec) client.Object { //nolint:gocyclo
	t.Helper()
	key := types.NamespacedName{Name: spec.Name, Namespace: spec.Namespace}

	switch spec.Kind {
	case "Job":
		obj := &nvcrev1alpha1.Job{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "Workflow":
		obj := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "Certification":
		obj := &nvcrev1alpha1.Certification{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "WorkloadRun":
		obj := &nvcrev1alpha1.WorkloadRun{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "GoodputMeasurement":
		obj := &nvcrev1alpha1.GoodputMeasurement{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "BandwidthMeasurement":
		obj := &nvcrev1alpha1.BandwidthMeasurement{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "PersistentVolumeClaim":
		obj := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "PersistentVolume":
		obj := &corev1.PersistentVolume{}
		if err := c.Get(ctx, types.NamespacedName{Name: spec.Name}, obj); err != nil {
			return nil
		}
		return obj
	case "ConfigMap":
		obj := &corev1.ConfigMap{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "Node":
		obj := &corev1.Node{}
		if err := c.Get(ctx, types.NamespacedName{Name: spec.Name}, obj); err != nil {
			return nil
		}
		return obj
	case "Namespace":
		obj := &corev1.Namespace{}
		if err := c.Get(ctx, types.NamespacedName{Name: spec.Name}, obj); err != nil {
			return nil
		}
		return obj
	case "TrainJob":
		obj := &trainerv1alpha1.TrainJob{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	case "BatchJob":
		obj := &batchv1.Job{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil
		}
		return obj
	default:
		t.Fatalf("unknown kind: %s", spec.Kind)
		return nil
	}
}

func collectAndSerialize(t *testing.T, c client.Client, cfg waitConfig, frozenGoodput map[string]any) string {
	t.Helper()
	ctx := context.Background()

	results := make(map[string]any)
	if frozenGoodput != nil {
		results["frozenGoodput"] = frozenGoodput
	}
	for _, spec := range cfg.Collect {
		obj := getObject(ctx, t, c, spec)
		require.NotNil(t, obj, "failed to collect %s/%s in namespace %s", spec.Kind, spec.Name, spec.Namespace)
		sanitizeObject(obj)
		key := fmt.Sprintf("%s/%s", spec.Kind, spec.Name)
		results[key] = obj
	}

	// Include Prometheus gauge values when configured.
	if cm := cfg.CollectMetrics; cm != nil {
		metrics := collectGoodputMetrics(t, cm.Namespace, cm.Measurement)
		for _, field := range cm.SanitizeFields {
			metrics[field] = 0
		}
		results["metrics"] = metrics
	}

	// Include NCCL bandwidth Prometheus gauge values when configured.
	if bm := cfg.CollectBandwidthMetrics; bm != nil {
		results["bandwidthMetrics"] = collectBandwidthMetrics(t, bm.Namespace, bm.Measurement)
	}

	// Include job-level Prometheus metrics when configured.
	if jm := cfg.CollectJobMetrics; jm != nil {
		results["jobMetrics"] = collectJobMetrics(t, jm.Namespace, jm.Job, jm.Workflow)
	}

	// Include topology Prometheus gauge values when configured.
	if tm := cfg.CollectTopologyMetrics; tm != nil {
		results["topologyMetrics"] = collectTopologyMetrics(t, tm.Namespace, tm.Workflow, tm.TopologyKey)
	}

	data, err := json.MarshalIndent(results, "", "  ")
	require.NoError(t, err)
	return string(data) + "\n"
}

// sanitizeObject removes volatile metadata fields for stable golden file comparison.
func sanitizeObject(obj client.Object) {
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetGeneration(0)
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetManagedFields(nil)
	obj.SetSelfLink("")

	// Clear owner reference UIDs (they are non-deterministic).
	refs := obj.GetOwnerReferences()
	for i := range refs {
		refs[i].UID = ""
	}
	obj.SetOwnerReferences(refs)

	// Clear condition timestamps.
	switch o := obj.(type) {
	case *nvcrev1alpha1.Job:
		clearConditionTimestamps(o.Status.Conditions)
		// Sanitize stall detection messages (contain non-deterministic elapsed time).
		for i := range o.Status.Conditions {
			if o.Status.Conditions[i].Reason == "WorkloadStalled" {
				o.Status.Conditions[i].Message = "Workload stalled"
			}
		}
	case *nvcrev1alpha1.Workflow:
		clearConditionTimestamps(o.Status.Conditions)
		// Node-result ConfigMaps use generateName, so their names are
		// non-deterministic; normalize the refs to stable placeholders.
		sanitizeNodeResultsRef(o.Status.SucceededNodesRef, o.Status.FailedNodesRef)
		// Clear time-dependent fields in group statuses.
		if o.Status.Orchestration != nil {
			for i := range o.Status.Orchestration.Groups {
				o.Status.Orchestration.Groups[i].StartTime = nil
				o.Status.Orchestration.Groups[i].CompletionTime = nil
			}
			for i := range o.Status.Orchestration.IterationHistory {
				for j := range o.Status.Orchestration.IterationHistory[i].Groups {
					o.Status.Orchestration.IterationHistory[i].Groups[j].StartTime = nil
					o.Status.Orchestration.IterationHistory[i].Groups[j].CompletionTime = nil
				}
			}
		}
	case *nvcrev1alpha1.Certification:
		clearConditionTimestamps(o.Status.Conditions)
		for i := range o.Status.CategoryStatuses {
			sanitizeNodeResultsRef(o.Status.CategoryStatuses[i].SucceededNodesRef, o.Status.CategoryStatuses[i].FailedNodesRef)
		}
	case *nvcrev1alpha1.WorkloadRun:
		clearConditionTimestamps(o.Status.Conditions)
		sanitizeNodeResultsRef(o.Status.SucceededNodesRef, o.Status.FailedNodesRef)
	case *nvcrev1alpha1.GoodputMeasurement:
		clearConditionTimestamps(o.Status.Conditions)
		// Clear time-dependent fields.
		o.Status.StartTime = nil
		o.Status.CompletionTime = nil
		o.Status.LastStepTimestamp = nil
		o.Status.ApplicationStopTime = nil
		// Clear time-dependent numeric fields (training time changes with wall clock).
		o.Status.TrainingTimeSec = ""
		o.Status.Result = ""
		o.Status.AvgTFLOPSPerGPU = ""
		// Clear pending interruption timestamps (they contain wall-clock times).
		if o.Status.PendingInterruption != nil {
			o.Status.PendingInterruption.TCheckpoint = nil
			o.Status.PendingInterruption.TInterrupt = nil
		}
	case *nvcrev1alpha1.BandwidthMeasurement:
		clearConditionTimestamps(o.Status.Conditions)
		o.Status.StartTime = nil
		o.Status.CompletionTime = nil
	case *corev1.PersistentVolume:
		o.Status.LastPhaseTransitionTime = nil
	case *corev1.Node:
		clearNodeConditionTimestamps(o.Status.Conditions)
	case *trainerv1alpha1.TrainJob:
		clearConditionTimestamps(o.Status.Conditions)
	case *batchv1.Job:
		clearBatchJobConditionTimestamps(o.Status.Conditions)
	}
}

func clearBatchJobConditionTimestamps(conditions []batchv1.JobCondition) {
	for i := range conditions {
		conditions[i].LastTransitionTime = metav1.Time{}
		conditions[i].LastProbeTime = metav1.Time{}
	}
}

// sanitizeNodeResultsRef normalizes the UID-based ConfigMap names in node-result
// references to stable placeholders so golden files are deterministic (the Workflow
// UID changes every envtest run). The ref structure and kind are preserved.
func sanitizeNodeResultsRef(succeeded, failed *corev1.TypedLocalObjectReference) {
	if succeeded != nil && succeeded.Name != "" {
		succeeded.Name = "succeeded-nodes"
	}
	if failed != nil && failed.Name != "" {
		failed.Name = "failed-nodes"
	}
}

func clearConditionTimestamps(conditions []metav1.Condition) {
	for i := range conditions {
		conditions[i].LastTransitionTime = metav1.Time{}
	}
}

func clearNodeConditionTimestamps(conditions []corev1.NodeCondition) {
	for i := range conditions {
		conditions[i].LastTransitionTime = metav1.Time{}
		conditions[i].LastHeartbeatTime = metav1.Time{}
	}
}

// collectGoodputMetrics gathers from the controller-runtime metrics registry
// (the same data source the /metrics HTTP endpoint serves) and returns the
// gauge values for the given namespace/measurement labels keyed by metric name.
func collectGoodputMetrics(t *testing.T, namespace, measurementName string) map[string]float64 {
	t.Helper()

	gathered, err := crmetrics.Registry.Gather()
	require.NoError(t, err)

	idx := make(map[string]int, len(gathered))
	for i, mf := range gathered {
		idx[mf.GetName()] = i
	}

	names := []string{
		"nvcre_goodput_avg_step_time_seconds",
		"nvcre_goodput_avg_tflops_per_gpu",
		"nvcre_goodput_checkpoint_save_time_seconds",
		"nvcre_goodput_lost_work_time_seconds",
		"nvcre_goodput_non_warmup_time_seconds",
		"nvcre_goodput_ratio",
		"nvcre_goodput_reschedule_time_seconds",
		"nvcre_goodput_resume_time_seconds",
		"nvcre_goodput_training_time_seconds",
		"nvcre_goodput_warmup_time_seconds",
	}

	result := make(map[string]float64, len(names))
	for _, name := range names {
		famIdx, ok := idx[name]
		fam := gathered[famIdx]
		require.True(t, ok, "metric %s not found in /metrics output", name)

		for _, m := range fam.GetMetric() {
			nsMatch, measMatch := false, false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == metricLabelNamespace && lp.GetValue() == namespace {
					nsMatch = true
				}
				if lp.GetName() == "measurement" && lp.GetValue() == measurementName {
					measMatch = true
				}
			}
			if nsMatch && measMatch {
				result[name] = m.GetGauge().GetValue()
				break
			}
		}
		_, found := result[name]
		require.True(t, found, "metric %s missing labels namespace=%s measurement=%s", name, namespace, measurementName)
	}
	return result
}

// collectBandwidthMetrics gathers NCCL bandwidth gauge metrics from the controller-runtime
// metrics registry. Returns a map of "metric_name:message_size_bytes" → gauge value.
func collectBandwidthMetrics(t *testing.T, namespace, measurementName string) map[string]float64 {
	t.Helper()

	gathered, err := crmetrics.Registry.Gather()
	require.NoError(t, err)

	idx := make(map[string]int, len(gathered))
	for i, mf := range gathered {
		idx[mf.GetName()] = i
	}

	names := []string{
		"nvcre_nccl_algbw_gbps",
		"nvcre_nccl_busbw_gbps",
	}

	result := make(map[string]float64)
	for _, name := range names {
		famIdx, ok := idx[name]
		fam := gathered[famIdx]
		if !ok {
			continue // Metrics may not exist yet if no data points parsed
		}

		for _, m := range fam.GetMetric() {
			nsMatch, measMatch := false, false
			sizeLabel := ""
			for _, lp := range m.GetLabel() {
				if lp.GetName() == metricLabelNamespace && lp.GetValue() == namespace {
					nsMatch = true
				}
				if lp.GetName() == "measurement" && lp.GetValue() == measurementName {
					measMatch = true
				}
				if lp.GetName() == "message_size_bytes" {
					sizeLabel = lp.GetValue()
				}
			}
			if nsMatch && measMatch && sizeLabel != "" {
				key := fmt.Sprintf("%s:%s", name, sizeLabel)
				result[key] = m.GetGauge().GetValue()
			}
		}
	}
	return result
}

// collectJobMetrics gathers job-level gauge metrics from the controller-runtime
// metrics registry, matching by namespace, job, and workflow labels.
func collectJobMetrics(t *testing.T, namespace, jobName, workflow string) map[string]float64 {
	t.Helper()

	gathered, err := crmetrics.Registry.Gather()
	require.NoError(t, err)

	idx := make(map[string]int, len(gathered))
	for i, mf := range gathered {
		idx[mf.GetName()] = i
	}

	// Only collect gauge metrics (not counters or histograms) for stable golden output.
	names := []string{
		"nvcre_job_status",
		"nvcre_job_failed_nodes",
	}

	result := make(map[string]float64, len(names))
	for _, name := range names {
		famIdx, ok := idx[name]
		fam := gathered[famIdx]
		if !ok {
			continue
		}

		for _, m := range fam.GetMetric() {
			nsMatch, jobMatch, wfMatch := false, false, false
			statusLabel := ""
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case metricLabelNamespace:
					nsMatch = lp.GetValue() == namespace
				case "job":
					jobMatch = lp.GetValue() == jobName
				case "workflow":
					wfMatch = lp.GetValue() == workflow
				case "status":
					statusLabel = lp.GetValue()
				}
			}
			if nsMatch && jobMatch && wfMatch {
				key := name
				if statusLabel != "" {
					key = name + "{status=" + statusLabel + "}"
				}
				result[key] = m.GetGauge().GetValue()
			}
		}
	}
	return result
}

// collectTopologyMetrics gathers topology validated/failed node gauges from the
// controller-runtime metrics registry. Returns a sorted map of
// "metric_name{domain=...,node=...}" → gauge value for stable golden comparison.
func collectTopologyMetrics(t *testing.T, namespace, workflow, topologyKey string) map[string]float64 {
	t.Helper()

	gathered, err := crmetrics.Registry.Gather()
	require.NoError(t, err)

	idx := make(map[string]int, len(gathered))
	for i, mf := range gathered {
		idx[mf.GetName()] = i
	}

	names := []string{
		"nvcre_topology_validated_nodes",
		"nvcre_topology_failed_nodes",
	}

	result := make(map[string]float64)
	for _, name := range names {
		famIdx, ok := idx[name]
		fam := gathered[famIdx]
		if !ok {
			continue
		}

		for _, m := range fam.GetMetric() {
			nsMatch, wfMatch, tkMatch := false, false, false
			domainLabel, nodeLabel := "", ""
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case metricLabelNamespace:
					nsMatch = lp.GetValue() == namespace
				case "workflow":
					wfMatch = lp.GetValue() == workflow
				case "topology_key":
					tkMatch = lp.GetValue() == topologyKey
				case "domain":
					domainLabel = lp.GetValue()
				case "node":
					nodeLabel = lp.GetValue()
				}
			}
			if nsMatch && wfMatch && tkMatch && domainLabel != "" && nodeLabel != "" {
				key := fmt.Sprintf("%s{domain=%s,node=%s}", name, domainLabel, nodeLabel)
				result[key] = m.GetGauge().GetValue()
			}
		}
	}
	return result
}
