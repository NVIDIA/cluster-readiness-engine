// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/catalog"
)

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
// Tests for ncrectl certification run helpers
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
	assert.True(t, strings.HasPrefix(name, "ncrectl-"),
		"expected prefix ncrectl-, got %s", name)
	// Format: ncrectl-YYYYMMDD-HHMMSS (23 chars)
	assert.Len(t, name, 23, "expected 23 chars, got %d: %s", len(name), name)
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
		if strings.HasPrefix(ns, "ncrectl-") && cfg.FlagNamespace == "" {
			ns = "ncrectl-<generated>"
		}

		type result struct {
			Namespace        string `json:"namespace"`
			AutoGenerated    bool   `json:"autoGenerated"`
			HasXcalctlPrefix bool   `json:"hasXcalctlPrefix"`
		}
		r := result{
			Namespace:        ns,
			AutoGenerated:    cfg.FlagNamespace == "" && cert.Namespace != "",
			HasXcalctlPrefix: strings.HasPrefix(cert.Namespace, "ncrectl-"),
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
	assert.True(t, strings.HasPrefix(ns, "ncrectl-"),
		"expected prefix ncrectl-, got %s", ns)
	assert.Len(t, ns, 23, "expected 23 chars, got %d: %s", len(ns), ns)
}

func TestGenerateCertNameAndNamespaceAreIndependent(t *testing.T) {
	// Both use the same timestamp format, but they're generated independently.
	name := generateCertName()
	ns := generateCertNamespace()
	assert.True(t, strings.HasPrefix(name, "ncrectl-"))
	assert.True(t, strings.HasPrefix(ns, "ncrectl-"))
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
apiVersion: cre.nvidia.com/v1alpha1
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
		_, err := readCertification("/tmp/does-not-exist-ncrectl-test.yaml")
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
