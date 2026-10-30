// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const (
	renderAnnotationPrefix  = "nvcrectl.nvidia.com/"
	appliedOverridesAnnoKey = renderAnnotationPrefix + "applied-overrides"
)

// annotationsWorkflow is the per-Workflow projection written to the golden
// file.
type annotationsWorkflow struct {
	Workflow string `json:"workflow"`
	// Annotations holds every nvcrectl.nvidia.com/ annotation except
	// applied-overrides, so a missing or unexpected render annotation shows up
	// in the golden.
	Annotations map[string]string `json:"annotations"`
	// AppliedOverrides is the applied-overrides annotation decoded from the API
	// type it carries, null when the annotation is absent (no override
	// matched).
	AppliedOverrides []matchedOverride `json:"appliedOverrides"`
}

// matchedOverride is one decoded AppliedOverride without its index. The
// index is the override's position in the catalog entry's overrides list,
// so it shifts whenever a block is inserted earlier in that file; when and
// patches already identify which block matched.
type matchedOverride struct {
	When    string `json:"when"`
	Patches string `json:"patches"`
	NoOp    bool   `json:"noOp,omitempty"`
}

// TestCertificationRenderAnnotations covers the render annotations offline
// "nvcrectl certification render" writes: the platform and GPU architecture
// detected from the synthetic render node, and the overrides that matched,
// the same three annotations the --dry-run path and "nvcrectl workflow render"
// write. The cases drive resolveWorkflowsOffline, the same function
// runCertificationRender calls without --dry-run. They pin that --platform
// and --gpu-arch reach the annotations, that a render without --platform
// reports the onprem detection its overrides actually matched against, and
// that applied-overrides is omitted when nothing matched.
func TestCertificationRenderAnnotations(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-annotations",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `json:"platform"`
			GPUArch  string `json:"gpuArch"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}
		gpuArch, err := catalog.ParseGPUArchFlag(cfg.GPUArch)
		if err != nil {
			return err
		}

		certPath := filepath.Join(tc.T.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}

		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, cfg.Platform, gpuArch, nil, "", catalog.DefaultOCISecondaryNetwork)
		if err != nil {
			return err
		}
		if err := resolveWorkflowsOffline(cert, workflows, cfg.Platform, gpuArch); err != nil {
			return err
		}

		result := make([]annotationsWorkflow, 0, len(workflows))
		for i := range workflows {
			out := annotationsWorkflow{
				Workflow:    workflows[i].Name,
				Annotations: map[string]string{},
			}
			for k, v := range workflows[i].Annotations {
				switch {
				case k == appliedOverridesAnnoKey:
					var applied []nvcrev1alpha1.AppliedOverride
					if err := json.Unmarshal([]byte(v), &applied); err != nil {
						return err
					}
					for _, a := range applied {
						out.AppliedOverrides = append(out.AppliedOverrides, matchedOverride{
							When: a.When, Patches: a.Patches, NoOp: a.NoOp,
						})
					}
				case strings.HasPrefix(k, renderAnnotationPrefix):
					out.Annotations[k] = v
				}
			}
			result = append(result, out)
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
