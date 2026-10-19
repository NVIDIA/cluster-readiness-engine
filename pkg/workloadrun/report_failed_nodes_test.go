// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestBuildWorkloadRunReportFailedNodes pins that a WorkloadRun report carries
// the per-node reason and message behind its failed-node names, not just the
// names. The WorkloadRun path builds its CertReport directly rather than
// through report.Build, so it is the one surface that can silently fall behind
// the certification report and print bare names (ADR-099).
func TestBuildWorkloadRunReportFailedNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "report-failed-nodes",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var run nvcrev1alpha1.WorkloadRun
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_workloadrun.yaml"]), &run); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}
		objs, _, err := tc.GetObjects(scheme)
		if err != nil {
			return err
		}
		// The WorkloadRun itself is parsed above; seed only the rest.
		var seeded []client.Object
		for _, o := range objs {
			if _, isRun := o.(*nvcrev1alpha1.WorkloadRun); isRun {
				continue
			}
			seeded = append(seeded, o)
		}
		c := newWorkloadRunFakeClient(t, seeded...)

		r := buildWorkloadRunReport(context.Background(), c, &run)

		var buf bytes.Buffer
		report.Print(&buf, r)
		buf.WriteString("--- results.json ---\n")
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteString("\n")
		tc.Actual = buf.String()
		return nil
	})
}
