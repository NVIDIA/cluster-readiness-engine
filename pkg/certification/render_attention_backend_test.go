// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// nvteAttnVars are the Transformer Engine variables megatron derives from
// --attention-backend, in the order attentionBackendNVTE lists their values.
var nvteAttnVars = []string{"NVTE_FLASH_ATTN", "NVTE_FUSED_ATTN", "NVTE_UNFUSED_ATTN"}

// attentionBackendNVTE is LanguageModule._set_attention_backend in
// megatron-core core_v0.15.2 (language_module.py), the tag the training
// entries check out. Megatron asserts that each variable is either unset or
// already equal to the value here, and every rank fails in GPTModel.__init__
// when one is not.
var attentionBackendNVTE = map[string][]string{
	"local":   {"0", "0", "0"},
	"flash":   {"1", "0", "0"},
	"fused":   {"0", "1", "0"},
	"unfused": {"0", "0", "1"},
	"auto":    {"1", "1", "1"},
}

var (
	// Skips shell comment lines, which may mention the flag in prose.
	attentionBackendArg = regexp.MustCompile(`(?m)^[^#\n]*--attention-backend\s+(\S+)`)
	shellDefaultExpr    = regexp.MustCompile(`^\$\{(\w+):-(\w+)\}$`)
)

// attentionBackendResult is one rendered training Workflow: the backend
// train.sh resolves to under the Workflow's effective env, and the NVTE
// attention variables that env sets (absent when unset, as megatron wants).
type attentionBackendResult struct {
	Target  string            `json:"target"`
	Backend string            `json:"backend,omitempty"`
	NVTE    map[string]string `json:"nvte,omitempty"`
}

// TestCertificationRenderAttentionBackend guards issue #486: on AWS H100 the
// nemotron5 entries set NVTE_FUSED_ATTN=1 while train.sh passed
// --attention-backend flash, so megatron's consistency assert killed every
// rank before the first step. The reconcile goldens stored that conflict as
// expected output, and no CI lane runs pretrain_gpt.py, so nothing failed.
//
// Every training entry is rendered for every platform in platform.Names()
// crossed with the input's GPU architectures, through the same path as
// "nvcrectl certification render". A combination whose effective env
// contradicts its backend fails the test outright, independent of the golden.
// So does one that fails to render: recording the error in the golden would let
// a regeneration accept a target this guard never checked.
// The golden pins which backend each combination resolves to, so changing a
// platform's kernel choice is a reviewed diff.
func TestCertificationRenderAttentionBackend(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-attention-backend",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			GPUArchs    []string `json:"gpuArchs"`
			NodesPerJob int32    `json:"nodesPerJob"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}
		for _, arch := range cfg.GPUArchs {
			if !catalog.KnownGPUArch(arch) {
				return fmt.Errorf("gpuArchs lists %q, which gpu-defaults.yaml does not define", arch)
			}
		}

		var results []attentionBackendResult
		var conflicts []error
		for _, cat := range catalog.List() {
			if cat.Domain != "training" {
				continue
			}
			for _, platformName := range platform.Names() {
				for _, arch := range cfg.GPUArchs {
					target := fmt.Sprintf("%s/%s/%s", cat.Variant, platformName, arch)
					res, err := renderAttentionBackend(cat, platformName, arch, cfg.NodesPerJob)
					if err != nil {
						return fmt.Errorf("%s: render: %w", target, err)
					}
					res.Target = target
					results = append(results, res)
					if err := checkAttentionBackend(res); err != nil {
						conflicts = append(conflicts, fmt.Errorf("%s: %w", target, err))
					}
				}
			}
		}
		if len(conflicts) > 0 {
			return errors.Join(conflicts...)
		}

		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// renderAttentionBackend renders one training category for one platform and
// GPU architecture the way "nvcrectl certification render" does, then projects
// the resolved Workflow's attention backend and NVTE attention env.
func renderAttentionBackend(
	cat catalog.CategoryInfo, platformName, arch string, nodesPerJob int32,
) (attentionBackendResult, error) {
	cert := &nvcrev1alpha1.Certification{
		Name: "attn",
		Spec: nvcrev1alpha1.CertificationSpec{
			Target: nvcrev1alpha1.TargetSpec{
				NodeSelector: map[string]string{"nvidia.com/gpu.product": "NVIDIA-" + arch},
			},
			Categories: []nvcrev1alpha1.CertificateCategory{{Domain: cat.Domain, Variant: cat.Variant}},
			CategoryOptions: nvcrev1alpha1.CategoryOptions{
				NodesPerJob: new(nodesPerJob),
			},
		},
	}
	workflows, err := renderCertification(cert, platformName, arch, nil, "")
	if err != nil {
		return attentionBackendResult{}, err
	}
	if err := resolveWorkflowsOffline(cert, workflows, platformName, arch); err != nil {
		return attentionBackendResult{}, err
	}
	if len(workflows) != 1 {
		return attentionBackendResult{}, fmt.Errorf("rendered %d workflows, want 1", len(workflows))
	}
	return projectAttentionBackend(&workflows[0])
}

// projectAttentionBackend computes the env the trainer container starts with
// (runtime "node" container env, with trainer env upserted over it by name,
// which is how Kubeflow Trainer composes them), then resolves the
// --attention-backend argument in the entry's scripts against that env.
func projectAttentionBackend(wf *nvcrev1alpha1.Workflow) (attentionBackendResult, error) {
	env := map[string]string{}
	var scripts []string
	for i := range wf.Spec.Dependencies {
		raw := wf.Spec.Dependencies[i].Raw
		var typeMeta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return attentionBackendResult{}, err
		}
		switch typeMeta.Kind {
		case trainerv1alpha1.TrainingRuntimeKind:
			var rt trainerv1alpha1.TrainingRuntime
			if err := json.Unmarshal(raw, &rt); err != nil {
				return attentionBackendResult{}, err
			}
			for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
				for _, c := range rj.Template.Spec.Template.Spec.Containers {
					if c.Name == "node" {
						addEnv(env, c.Env)
					}
				}
			}
		case "ConfigMap":
			var cm corev1.ConfigMap
			if err := json.Unmarshal(raw, &cm); err != nil {
				return attentionBackendResult{}, err
			}
			for _, v := range cm.Data {
				scripts = append(scripts, v)
			}
		}
	}
	if tj := wf.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
		addEnv(env, tj.Trainer.Env)
	}

	res := attentionBackendResult{}
	for _, name := range nvteAttnVars {
		if v, ok := env[name]; ok {
			if res.NVTE == nil {
				res.NVTE = map[string]string{}
			}
			res.NVTE[name] = v
		}
	}
	for _, s := range scripts {
		for _, m := range attentionBackendArg.FindAllStringSubmatch(s, -1) {
			backend, err := resolveShellWord(m[1], env)
			if err != nil {
				return res, err
			}
			if res.Backend != "" && res.Backend != backend {
				return res, fmt.Errorf("scripts pass both --attention-backend %s and %s", res.Backend, backend)
			}
			res.Backend = backend
		}
	}
	return res, nil
}

// addEnv upserts env into dst by name. ValueFrom entries are recorded as
// empty, which still counts as set for the NVTE check.
func addEnv(dst map[string]string, env []corev1.EnvVar) {
	for _, e := range env {
		dst[e.Name] = e.Value
	}
}

// resolveShellWord expands the two shapes train.sh uses for the backend: a
// literal word, or ${VAR:-default}, which takes VAR when it is set and
// non-empty.
func resolveShellWord(word string, env map[string]string) (string, error) {
	if !strings.Contains(word, "$") {
		return word, nil
	}
	m := shellDefaultExpr.FindStringSubmatch(word)
	if m == nil {
		return "", fmt.Errorf("cannot resolve --attention-backend %s; use a literal or ${VAR:-default}", word)
	}
	if v := env[m[1]]; v != "" {
		return v, nil
	}
	return m[2], nil
}

// checkAttentionBackend reports every NVTE attention variable whose value
// contradicts what megatron requires for the resolved backend. An unset
// variable never conflicts, since megatron sets it itself.
func checkAttentionBackend(res attentionBackendResult) error {
	if res.Backend == "" {
		if len(res.NVTE) > 0 {
			return fmt.Errorf("sets %v but passes no --attention-backend to check it against", res.NVTE)
		}
		return nil
	}
	want, ok := attentionBackendNVTE[res.Backend]
	if !ok {
		return fmt.Errorf("--attention-backend %q is not one megatron accepts", res.Backend)
	}
	var errs []error
	for i, name := range nvteAttnVars {
		if got, set := res.NVTE[name]; set && got != want[i] {
			errs = append(errs, fmt.Errorf(
				"%s=%q conflicts with --attention-backend %s, which requires %q; "+
					"select the kernel with ATTENTION_BACKEND instead of setting %s",
				name, got, res.Backend, want[i], name))
		}
	}
	return errors.Join(errs...)
}
