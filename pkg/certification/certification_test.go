// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
)

func newCertificationFakeClient(t testing.TB, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, crev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

type certificationGetErrorClient struct {
	client.WithWatch
	err error
}

func (c certificationGetErrorClient) Get(
	context.Context, client.ObjectKey, client.Object, ...client.GetOption,
) error {
	return c.err
}

type certificationDeadlineClient struct {
	client.WithWatch
	sawDeadline     bool
	sawLiveDeadline bool
}

func (c *certificationDeadlineClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.sawDeadline = true
		c.sawLiveDeadline = ctx.Err() == nil && time.Now().Before(deadline)
	}
	return c.WithWatch.Get(ctx, key, obj, opts...)
}

func normalizeCertificationReportOutput(output string) string {
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

func TestCatalogListCategories(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "list-categories",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		categories := catalog.List()

		data, err := json.MarshalIndent(categories, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCertificationRender(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		certPath := filepath.Join(t.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}

		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, "")
		if err != nil {
			return err
		}

		type workflowInfo struct {
			Name                   string            `json:"name"`
			APIVersion             string            `json:"apiVersion"`
			Kind                   string            `json:"kind"`
			Labels                 map[string]string `json:"labels"`
			HasOrchestrationTarget bool              `json:"hasOrchestrationTarget"`
			HasJobTemplate         bool              `json:"hasJobTemplate"`
			NumDependencies        int               `json:"numDependencies"`
		}

		var result []workflowInfo
		for _, wf := range workflows {
			result = append(result, workflowInfo{
				Name:                   wf.Name,
				APIVersion:             wf.APIVersion,
				Kind:                   wf.Kind,
				Labels:                 wf.Labels,
				HasOrchestrationTarget: wf.Spec.Orchestration.Target != nil,
				HasJobTemplate:         wf.Spec.JobTemplate.Spec.Workload.TrainJob != nil,
				NumDependencies:        len(wf.Spec.Dependencies),
			})
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCategoryWatchLabels(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "category-watch-labels",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cert crev1alpha1.Certification
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_certification.yaml"]), &cert); err != nil {
			return err
		}

		data, err := json.MarshalIndent(categoryWatchLabels(&cert), "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestCertificationRenderErrors(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-errors",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			CertificationFile string `yaml:"certificationFile"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_config.yaml"]), &cfg); err != nil {
			return err
		}

		certPath := cfg.CertificationFile
		if certData, ok := tc.Inputs["input_certification.yaml"]; ok {
			certPath = filepath.Join(t.TempDir(), "certification.yaml")
			if err := os.WriteFile(certPath, []byte(certData), 0o644); err != nil {
				return err
			}
		}

		cert, readErr := readCertification(certPath)
		var err error
		if readErr != nil {
			err = readErr
		} else {
			_, err = renderCertification(cert, "")
		}

		type result struct {
			Error string `json:"error"`
		}
		var r result
		if err != nil {
			r.Error = err.Error()
		}

		data, marshalErr := json.MarshalIndent(r, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// ---------------------------------------------------------------------------
// Tests for nvcrectl certification run helpers
// ---------------------------------------------------------------------------

func TestParseCategories(t *testing.T) {
	t.Run("valid single", func(t *testing.T) {
		cats, err := parseCategories([]string{"communication/nccl-all-reduce"})
		require.NoError(t, err)
		require.Len(t, cats, 1)
		assert.Equal(t, "communication", cats[0].Domain)
		assert.Equal(t, "nccl-all-reduce", cats[0].Variant)
	})

	t.Run("valid multiple", func(t *testing.T) {
		cats, err := parseCategories([]string{
			"communication/nccl-all-reduce",
			"training/nemotron5-8b",
		})
		require.NoError(t, err)
		require.Len(t, cats, 2)
	})

	t.Run("invalid format no slash", func(t *testing.T) {
		_, err := parseCategories([]string{"nccl-all-reduce"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected domain/variant")
	})

	t.Run("invalid format empty domain", func(t *testing.T) {
		_, err := parseCategories([]string{"/nccl-all-reduce"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected domain/variant")
	})

	t.Run("unknown category", func(t *testing.T) {
		_, err := parseCategories([]string{"nonexistent/nope"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown category")
		assert.Contains(t, err.Error(), "Available categories")
	})
}

func TestGenerateCertName(t *testing.T) {
	name := generateCertName()
	assert.True(t, strings.HasPrefix(name, "nvcrectl-"),
		"expected prefix nvcrectl-, got %s", name)
	// Format: nvcrectl-YYYYMMDD-HHMMSS (24 chars)
	assert.Len(t, name, 24, "expected 24 chars, got %d: %s", len(name), name)
}

func TestCertificationNamespace(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-namespace",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			FlagNamespace string `yaml:"flagNamespace"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_config.yaml"]), &cfg); err != nil {
			return err
		}

		certPath := filepath.Join(t.TempDir(), "cert.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}

		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}

		// Simulate runCertificationFromFile namespace defaulting.
		if cert.Namespace == "" {
			if cfg.FlagNamespace != "" {
				cert.Namespace = cfg.FlagNamespace
			} else {
				cert.Namespace = generateCertNamespace()
			}
		}

		// Normalize auto-generated timestamps for golden file stability.
		ns := cert.Namespace
		if strings.HasPrefix(ns, "nvcrectl-") && cfg.FlagNamespace == "" {
			ns = "nvcrectl-<generated>"
		}

		type result struct {
			Namespace        string `json:"namespace"`
			AutoGenerated    bool   `json:"autoGenerated"`
			HasXcalctlPrefix bool   `json:"hasXcalctlPrefix"`
		}
		r := result{
			Namespace:        ns,
			AutoGenerated:    cfg.FlagNamespace == "" && cert.Namespace != "",
			HasXcalctlPrefix: strings.HasPrefix(cert.Namespace, "nvcrectl-"),
		}

		data, marshalErr := json.MarshalIndent(r, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestGenerateCertNamespace(t *testing.T) {
	ns := generateCertNamespace()
	assert.True(t, strings.HasPrefix(ns, "nvcrectl-"),
		"expected prefix nvcrectl-, got %s", ns)
	assert.Len(t, ns, 24, "expected 24 chars, got %d: %s", len(ns), ns)
}

func TestGenerateCertNameAndNamespaceAreIndependent(t *testing.T) {
	// Both use the same timestamp format, but they're generated independently.
	name := generateCertName()
	ns := generateCertNamespace()
	assert.True(t, strings.HasPrefix(name, "nvcrectl-"))
	assert.True(t, strings.HasPrefix(ns, "nvcrectl-"))
}

func TestPlatformToProviderID(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "platform-to-provider-id",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `yaml:"platform"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		result := platformToProviderID(cfg.Platform)

		type resultJSON struct {
			Platform string `json:"platform"`
			Result   string `json:"result"`
			Empty    bool   `json:"empty"`
		}
		r := resultJSON{
			Platform: cfg.Platform,
			Result:   result,
			Empty:    result == "",
		}

		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// runCertificationRender validates --platform before doing anything else, so
// an invalid name must fail with the full list of valid names, and every name
// platform detection can return must be accepted. For accepted platforms the
// case also records what detection reports for the synthetic render node,
// which is what override matching actually sees (nscale, for example, is only
// detected when the node carries the nscale.com/rdmashare allocatable).
func TestRenderPlatformFlag(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render-platform-flag",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `yaml:"platform"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		// Cases with a certification file exercise the full render path; cases
		// without one must fail on flag validation before the file is read.
		certPath := filepath.Join(t.TempDir(), "certification.yaml")
		if certData, ok := tc.Inputs["input_certification.yaml"]; ok {
			if err := os.WriteFile(certPath, []byte(certData), 0o644); err != nil {
				return err
			}
		}

		configFlags := kubeconfig.NewConfigFlags(true)
		*configFlags.Namespace = defaultKubeNamespace
		renderErr := runCertificationRender(certPath, "yaml", false, configFlags, cfg.Platform)

		type result struct {
			Error            string `json:"error"`
			DetectedPlatform string `json:"detectedPlatform"`
		}
		var r result
		if renderErr != nil {
			r.Error = renderErr.Error()
		} else if cfg.Platform != "" {
			node := syntheticRenderNode(cfg.Platform, map[string]string{})
			r.DetectedPlatform = controller.DetectPlatform([]corev1.Node{node})
		}

		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestDerefBoolPtr(t *testing.T) {
	t.Run("nil returns false", func(t *testing.T) {
		assert.False(t, derefBoolPtr(nil))
	})

	t.Run("true pointer", func(t *testing.T) {
		v := true
		assert.True(t, derefBoolPtr(&v))
	})

	t.Run("false pointer", func(t *testing.T) {
		v := false
		assert.False(t, derefBoolPtr(&v))
	})
}

func TestDerefInt32Ptr(t *testing.T) {
	t.Run("nil returns zero", func(t *testing.T) {
		assert.Equal(t, int32(0), derefInt32Ptr(nil))
	})

	t.Run("non-nil", func(t *testing.T) {
		v := int32(42)
		assert.Equal(t, int32(42), derefInt32Ptr(&v))
	})

	t.Run("zero value pointer", func(t *testing.T) {
		v := int32(0)
		assert.Equal(t, int32(0), derefInt32Ptr(&v))
	})
}

func TestReadCertification(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		content := `
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: test-cert
  namespace: test-ns
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3
  categories:
    - domain: communication
      variant: nccl-all-reduce
`
		path := filepath.Join(t.TempDir(), "cert.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		cert, err := readCertification(path)
		require.NoError(t, err)
		assert.Equal(t, "test-cert", cert.Name)
		assert.Equal(t, "test-ns", cert.Namespace)
		require.Len(t, cert.Spec.Categories, 1)
		assert.Equal(t, "communication", cert.Spec.Categories[0].Domain)
		assert.Equal(t, "nccl-all-reduce", cert.Spec.Categories[0].Variant)
	})

	t.Run("nonexistent file", func(t *testing.T) {
		_, err := readCertification("/tmp/does-not-exist-nvcrectl-test.yaml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read certification")
	})

	t.Run("invalid yaml", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.yaml")
		require.NoError(t, os.WriteFile(path, []byte("not: [valid: yaml: {"), 0o644))

		_, err := readCertification(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse certification")
	})
}

func TestWatchCertificationImmediateTimeout(t *testing.T) {
	wc := newCertificationFakeClient(t)
	var out bytes.Buffer

	cert, err := watchCertification(context.Background(), wc, "timeout-cert", "test-ns", 0, &out)

	assert.Nil(t, cert)
	require.Error(t, err)
	assert.True(t, isCertificationWaitTimeout(err))
	assert.Equal(t, "certification did not complete within 0s (ran for 0s)", err.Error())
}

func TestCertificationWaitContextErrorCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := certificationWaitContextError(ctx, time.Minute, time.Now())

	assert.EqualError(t, err, "interrupted")
}

func TestProcessWatchEventsContextDoneDuringActiveWatch(t *testing.T) {
	watcher := watch.NewRaceFreeFake()
	defer watcher.Stop()
	heartbeat := time.NewTicker(time.Hour)
	defer heartbeat.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var out bytes.Buffer

	cert, done, err := processWatchEvents(
		ctx, newCertificationFakeClient(t), watcher, time.Now(),
		map[string]string{}, heartbeat, &out,
	)

	assert.Nil(t, cert)
	assert.True(t, done)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestFinishCertificationWaitTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "finish-certification-wait-timeout",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Name       string `json:"name"`
			DoCleanup  bool   `json:"doCleanup"`
			CertExists *bool  `json:"certExists"`
			GetError   string `json:"getError"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		if input.Name == "" {
			input.Name = "timeout-cert"
		}

		cert := &crev1alpha1.Certification{
			ObjectMeta: metav1.ObjectMeta{Name: input.Name, Namespace: "test-ns"},
			Spec: crev1alpha1.CertificationSpec{
				Categories: []crev1alpha1.CertificateCategory{{
					Domain: "communication", Variant: "nccl-all-reduce",
				}},
			},
			Status: crev1alpha1.CertificationStatus{
				CategoryStatuses: []crev1alpha1.CertificationCategoryStatus{{
					Domain: "communication", Variant: "nccl-all-reduce", Status: "InProgress",
				}},
			},
		}
		certExists := input.CertExists == nil || *input.CertExists
		var wc client.WithWatch
		if certExists {
			wc = newCertificationFakeClient(tc.T, cert)
		} else {
			wc = newCertificationFakeClient(tc.T)
		}
		if input.GetError != "" {
			wc = certificationGetErrorClient{WithWatch: wc, err: errors.New(input.GetError)}
		}
		resultsFile := filepath.Join(tc.T.TempDir(), "results.json")
		var out bytes.Buffer
		cfg := &certRunConfig{
			cert: cert.DeepCopy(), namespace: cert.Namespace, doCleanup: input.DoCleanup,
			resultsFile: resultsFile, out: &out,
		}
		waitErr := &certificationWaitTimeoutError{timeout: 15 * time.Minute, elapsed: 15 * time.Minute}

		gotErr := finishCertificationWait(context.Background(), wc, cfg, nil, waitErr)

		assert.Same(tc.T, waitErr, gotErr)
		if certExists && input.GetError == "" {
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

		tc.Actual = normalizeCertificationReportOutput(
			strings.ReplaceAll(out.String(), resultsFile, "<results-file>"),
		)
		return nil
	})
}

func TestFinishCertificationWaitBoundsPostTimeoutReads(t *testing.T) {
	cert := &crev1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{Name: "timeout-cert", Namespace: "test-ns"},
	}
	wc := &certificationDeadlineClient{WithWatch: newCertificationFakeClient(t, cert)}
	var out bytes.Buffer
	cfg := &certRunConfig{cert: cert.DeepCopy(), namespace: cert.Namespace, out: &out}
	waitErr := &certificationWaitTimeoutError{timeout: time.Minute, elapsed: time.Minute}

	gotErr := finishCertificationWait(context.Background(), wc, cfg, nil, waitErr)

	assert.Same(t, waitErr, gotErr)
	assert.True(t, wc.sawDeadline)
	assert.True(t, wc.sawLiveDeadline)
}

func TestExecuteCertificationRunReportsBeforeCleanup(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}}
	wc := newCertificationFakeClient(t, namespace)
	cert := &crev1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{Name: "timeout-cert", Namespace: namespace.Name},
		Spec: crev1alpha1.CertificationSpec{
			Categories: []crev1alpha1.CertificateCategory{{
				Domain: "communication", Variant: "nccl-all-reduce",
			}},
		},
	}
	var out bytes.Buffer
	cfg := &certRunConfig{
		cert: cert, namespace: namespace.Name,
		doWait: true, doCleanup: true, timeout: 0,
		out: &out, watchClient: wc,
	}

	err := executeCertificationRun(cfg)

	require.Error(t, err)
	assert.True(t, isCertificationWaitTimeout(err))
	output := out.String()
	reportIndex := strings.Index(output, "Certification Report")
	cleanupIndex := strings.Index(output, "[cleanup] Deleting certification...")
	assert.GreaterOrEqual(t, reportIndex, 0)
	assert.Greater(t, cleanupIndex, reportIndex)

	gotCert := &crev1alpha1.Certification{}
	err = wc.Get(context.Background(), client.ObjectKeyFromObject(cert), gotCert)
	assert.True(t, apierrors.IsNotFound(err))

	gotNamespace := &corev1.Namespace{}
	require.NoError(t, wc.Get(context.Background(), client.ObjectKeyFromObject(namespace), gotNamespace))
}

func TestFinishCertificationWaitTerminalAtTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "finish-certification-wait-terminal-at-timeout",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			ConditionType string `json:"conditionType"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		cert := &crev1alpha1.Certification{
			ObjectMeta: metav1.ObjectMeta{Name: "timeout-cert", Namespace: "test-ns"},
			Status: crev1alpha1.CertificationStatus{Conditions: []metav1.Condition{{
				Type: input.ConditionType, Status: metav1.ConditionTrue,
			}}},
		}
		wc := newCertificationFakeClient(tc.T, cert)
		var out bytes.Buffer
		cfg := &certRunConfig{cert: cert.DeepCopy(), namespace: cert.Namespace, out: &out}
		waitErr := &certificationWaitTimeoutError{timeout: time.Minute, elapsed: time.Minute}

		gotErr := finishCertificationWait(context.Background(), wc, cfg, nil, waitErr)

		assert.Same(tc.T, waitErr, gotErr)
		tc.Actual = normalizeCertificationReportOutput(out.String())
		return nil
	})
}

func TestFinishCertificationWaitDoesNotMaskOtherWatchErrors(t *testing.T) {
	cert := &crev1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cert", Namespace: "test-ns"},
	}
	wc := newCertificationFakeClient(t, cert)
	resultsFile := filepath.Join(t.TempDir(), "results.json")
	var out bytes.Buffer
	cfg := &certRunConfig{
		cert: cert.DeepCopy(), namespace: cert.Namespace, resultsFile: resultsFile, out: &out,
	}
	waitErr := errors.New("watch disconnected")

	gotErr := finishCertificationWait(context.Background(), wc, cfg, nil, waitErr)

	assert.Same(t, waitErr, gotErr)
	assert.Empty(t, out.String())
	_, err := os.Stat(resultsFile)
	assert.True(t, os.IsNotExist(err))
}

func TestParseCategoriesEdgeCases(t *testing.T) {
	t.Run("empty variant", func(t *testing.T) {
		_, err := parseCategories([]string{"communication/"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected domain/variant")
	})

	t.Run("triple slash", func(t *testing.T) {
		// SplitN with n=2 means "communication" and "a/b"
		_, err := parseCategories([]string{"communication/a/b"})
		require.Error(t, err)
		// "a/b" is not a valid variant in the catalog
		assert.Contains(t, err.Error(), "unknown category")
	})

	t.Run("empty input slice", func(t *testing.T) {
		cats, err := parseCategories(nil)
		require.NoError(t, err)
		assert.Nil(t, cats)
	})
}

// ---------------------------------------------------------------------------
// Tests for newRunCommand flag parsing and validation
// ---------------------------------------------------------------------------

func TestNewRunCommandFlagDefaults(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "new-run-command-flag-defaults",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		cmd := newRunCommand("dev")

		// Serialize the FULL flag set (name -> default, sorted by name) so a
		// newly added flag shows up as a golden-file diff.
		defaults := map[string]string{}
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			defaults[f.Name] = f.DefValue
		})

		names := make([]string, 0, len(defaults))
		for name := range defaults {
			names = append(names, name)
		}
		sort.Strings(names)

		type flagDefault struct {
			Name    string `json:"name"`
			Default string `json:"default"`
		}
		result := make([]flagDefault, 0, len(names))
		for _, name := range names {
			result = append(result, flagDefault{Name: name, Default: defaults[name]})
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestNewRunCommandValidation(t *testing.T) {
	t.Run("cert-file and category are mutually exclusive", func(t *testing.T) {
		cmd := newRunCommand("dev")
		cmd.SetArgs([]string{
			"--cert-file", "cert.yaml",
			"--category", "communication/nccl-all-reduce",
		})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("neither cert-file nor category fails", func(t *testing.T) {
		cmd := newRunCommand("dev")
		cmd.SetArgs([]string{})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "either --cert-file or at least one --category")
	})

	t.Run("setup wait cleanup are independent flags", func(t *testing.T) {
		cmd := newRunCommand("dev")
		for _, flag := range []string{"setup", "wait", "cleanup"} {
			f := cmd.Flags().Lookup(flag)
			require.NotNil(t, f, "--%s should exist", flag)
			assert.Equal(t, "false", f.DefValue)
		}
		// --watch should not exist.
		assert.Nil(t, cmd.Flags().Lookup("watch"), "--watch should not exist")
	})
}

func TestNewRunCommandCategoryRunOptsWiring(t *testing.T) {
	// Test that bool flags use Changed() semantics: omitted = nil, explicit = *bool.
	t.Run("bool flags omitted are nil", func(t *testing.T) {
		cmd := newRunCommand("dev")
		// Override RunE to capture the opts before they hit the network.
		var capturedOpts categoryRunOpts
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			capturedOpts = categoryRunOpts{
				maxSteps:         0,
				exitDurationMins: 0,
				gpusPerNode:      0,
				storageClass:     "",
			}
			if cmd.Flags().Changed("enable-checkpoint") {
				v, _ := cmd.Flags().GetBool("enable-checkpoint")
				capturedOpts.enableCheckpoint = &v
			}
			if cmd.Flags().Changed("enable-mnnvl") {
				v, _ := cmd.Flags().GetBool("enable-mnnvl")
				capturedOpts.enableMNNVL = &v
			}
			return nil
		}
		cmd.SetArgs([]string{"--category", "communication/nccl-all-reduce"})
		require.NoError(t, cmd.Execute())
		assert.Nil(t, capturedOpts.enableCheckpoint, "omitted --enable-checkpoint should be nil")
		assert.Nil(t, capturedOpts.enableMNNVL, "omitted --enable-mnnvl should be nil")
	})

	t.Run("bool flags explicitly set are non-nil", func(t *testing.T) {
		cmd := newRunCommand("dev")
		var capturedOpts categoryRunOpts
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("enable-checkpoint") {
				v, _ := cmd.Flags().GetBool("enable-checkpoint")
				capturedOpts.enableCheckpoint = &v
			}
			if cmd.Flags().Changed("enable-mnnvl") {
				v, _ := cmd.Flags().GetBool("enable-mnnvl")
				capturedOpts.enableMNNVL = &v
			}
			return nil
		}
		cmd.SetArgs([]string{
			"--category", "communication/nccl-all-reduce",
			"--enable-checkpoint",
			"--enable-mnnvl",
		})
		require.NoError(t, cmd.Execute())
		require.NotNil(t, capturedOpts.enableCheckpoint)
		assert.True(t, *capturedOpts.enableCheckpoint)
		require.NotNil(t, capturedOpts.enableMNNVL)
		assert.True(t, *capturedOpts.enableMNNVL)
	})

	t.Run("bool flags explicitly false are non-nil false", func(t *testing.T) {
		cmd := newRunCommand("dev")
		var capturedOpts categoryRunOpts
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("enable-mnnvl") {
				v, _ := cmd.Flags().GetBool("enable-mnnvl")
				capturedOpts.enableMNNVL = &v
			}
			return nil
		}
		cmd.SetArgs([]string{
			"--category", "communication/nccl-all-reduce",
			"--enable-mnnvl=false",
		})
		require.NoError(t, cmd.Execute())
		require.NotNil(t, capturedOpts.enableMNNVL, "explicit --enable-mnnvl=false should be non-nil")
		assert.False(t, *capturedOpts.enableMNNVL)
	})
}

// ---------------------------------------------------------------------------
// Tests for certification object building from categoryRunOpts
// ---------------------------------------------------------------------------

func TestCategoryRunOptsWiringIntoCert(t *testing.T) {
	t.Run("all opts set", func(t *testing.T) {
		enableCP := true
		enableMNNVL := true
		opts := categoryRunOpts{
			enableCheckpoint: &enableCP,
			maxSteps:         100,
			exitDurationMins: 15,
			gpusPerNode:      8,
			enableMNNVL:      &enableMNNVL,
			storageClass:     "gp3",
		}

		cert := &crev1alpha1.Certification{}

		// Simulate the wiring logic from runCertificationRun.
		if opts.enableCheckpoint != nil {
			cert.Spec.EnableCheckpoint = opts.enableCheckpoint
		}
		if opts.maxSteps > 0 {
			cert.Spec.MaxSteps = &opts.maxSteps
		}
		if opts.exitDurationMins > 0 {
			cert.Spec.ExitDurationMins = &opts.exitDurationMins
		}
		if opts.gpusPerNode > 0 {
			cert.Spec.GpusPerNode = &opts.gpusPerNode
		}
		if opts.enableMNNVL != nil {
			cert.Spec.EnableMNNVL = opts.enableMNNVL
		}
		if opts.storageClass != "" {
			cert.Spec.StorageClassName = &opts.storageClass
		}

		require.NotNil(t, cert.Spec.EnableCheckpoint)
		assert.True(t, *cert.Spec.EnableCheckpoint)
		require.NotNil(t, cert.Spec.MaxSteps)
		assert.Equal(t, int32(100), *cert.Spec.MaxSteps)
		require.NotNil(t, cert.Spec.ExitDurationMins)
		assert.Equal(t, int32(15), *cert.Spec.ExitDurationMins)
		require.NotNil(t, cert.Spec.GpusPerNode)
		assert.Equal(t, int32(8), *cert.Spec.GpusPerNode)
		require.NotNil(t, cert.Spec.EnableMNNVL)
		assert.True(t, *cert.Spec.EnableMNNVL)
		require.NotNil(t, cert.Spec.StorageClassName)
		assert.Equal(t, "gp3", *cert.Spec.StorageClassName)
	})

	t.Run("zero opts leave fields nil", func(t *testing.T) {
		opts := categoryRunOpts{}
		cert := &crev1alpha1.Certification{}

		if opts.enableCheckpoint != nil {
			cert.Spec.EnableCheckpoint = opts.enableCheckpoint
		}
		if opts.maxSteps > 0 {
			cert.Spec.MaxSteps = &opts.maxSteps
		}
		if opts.exitDurationMins > 0 {
			cert.Spec.ExitDurationMins = &opts.exitDurationMins
		}
		if opts.gpusPerNode > 0 {
			cert.Spec.GpusPerNode = &opts.gpusPerNode
		}
		if opts.enableMNNVL != nil {
			cert.Spec.EnableMNNVL = opts.enableMNNVL
		}
		if opts.storageClass != "" {
			cert.Spec.StorageClassName = &opts.storageClass
		}

		assert.Nil(t, cert.Spec.EnableCheckpoint)
		assert.Nil(t, cert.Spec.MaxSteps)
		assert.Nil(t, cert.Spec.ExitDurationMins)
		assert.Nil(t, cert.Spec.GpusPerNode)
		assert.Nil(t, cert.Spec.EnableMNNVL)
		assert.Nil(t, cert.Spec.StorageClassName)
	})

	t.Run("nodesPerJob wiring", func(t *testing.T) {
		cert := &crev1alpha1.Certification{}
		nodesPerJob := int32(4)
		if nodesPerJob > 0 {
			cert.Spec.NodesPerJob = &nodesPerJob
		}
		require.NotNil(t, cert.Spec.NodesPerJob)
		assert.Equal(t, int32(4), *cert.Spec.NodesPerJob)
	})

	t.Run("nodesPerJob zero leaves nil", func(t *testing.T) {
		cert := &crev1alpha1.Certification{}
		nodesPerJob := int32(0)
		if nodesPerJob > 0 {
			cert.Spec.NodesPerJob = &nodesPerJob
		}
		assert.Nil(t, cert.Spec.NodesPerJob)
	})

}
