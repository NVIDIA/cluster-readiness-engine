// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

// testAPIVersionV1Alpha1 is the "v1alpha1" version string shared by the
// NVCRE and Trainer GroupVersionKinds registered for tests in this package.
const testAPIVersionV1Alpha1 = "v1alpha1"

// registerTrainerKinds registers the Trainer-family kinds the recovery gate
// lists, so the fake client can serve them as unstructured objects — the
// same registration shape newSetupScheme uses for LogProfile.
func registerTrainerKinds(s *runtime.Scheme) {
	kinds := []schema.GroupVersionKind{
		{Group: trainerAPIGroup, Version: testAPIVersionV1Alpha1, Kind: "TrainJob"},
		{Group: trainerAPIGroup, Version: testAPIVersionV1Alpha1, Kind: "TrainingRuntime"},
		{Group: trainerAPIGroup, Version: testAPIVersionV1Alpha1, Kind: "ClusterTrainingRuntime"},
		{Group: jobsetAPIGroup, Version: "v1alpha2", Kind: "JobSet"},
	}
	for _, gvk := range kinds {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"),
			&unstructured.UnstructuredList{})
	}
}

// initRecoveryInput is the input.yaml shape for the init-recovery cases.
type initRecoveryInput struct {
	// ReleaseState is what the trainer state stub reports, both before the
	// attempt and when re-queried after a failure.
	ReleaseState string `yaml:"releaseState"`
	// ChartVersion is the installed chart version the state stub reports.
	ChartVersion string `yaml:"chartVersion"`
	// AutoApprove defaults to true; set false to exercise the recovery
	// confirmation prompt.
	AutoApprove *bool `yaml:"autoApprove"`
	// ConfirmInput is the stdin fed to the recovery confirmation prompt.
	ConfirmInput string `yaml:"confirmInput"`
	// InstallResults are consumed in order, one per install attempt. An
	// attempt beyond the list reports a loud sentinel failure, so a
	// recovery loop shows up as a golden mismatch.
	InstallResults []struct {
		// Output is the captured helm transcript for the attempt.
		Output string `yaml:"output"`
		// Fail makes the attempt return an error (with Output printed, the
		// way runHelmCapture prints the transcript on failure).
		Fail bool `yaml:"fail"`
	} `yaml:"installResults"`
}

// TestInstallDepsPhaseRecovery drives the [deps] state machine (ADR-073)
// against a fake cluster built from input_objects.yaml and a stubbed trainer
// helm. The golden file holds the full printed transcript plus the phase
// result and the install/uninstall call counts, so a recovery loop or a
// skipped safety gate shows up as a diff.
func TestInstallDepsPhaseRecovery(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "init-recovery",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in initRecoveryInput
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		var objs []client.Object
		for _, doc := range splitYAMLDocuments([]byte(tc.Inputs["input_objects.yaml"])) {
			obj, err := decodeUnstructured(doc)
			if err != nil {
				return fmt.Errorf("decode input object: %w", err)
			}
			objs = append(objs, obj)
		}

		scheme := newSetupScheme(t)
		registerTrainerKinds(scheme)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

		installCalls, uninstallCalls := 0, 0
		trainer := trainerHelm{
			state: func() (string, string) { return in.ReleaseState, in.ChartVersion },
			install: func(out io.Writer) (string, error) {
				installCalls++
				if installCalls > len(in.InstallResults) {
					output := "UNEXPECTED EXTRA INSTALL ATTEMPT — the phase must attempt recovery at most once"
					_, _ = io.WriteString(out, output+"\n")
					return output, errors.New("unexpected extra install attempt")
				}
				r := in.InstallResults[installCalls-1]
				_, _ = fmt.Fprintf(out, "[deps] Installing Kubeflow Trainer Helm release %q in namespace %s...\n",
					trainerReleaseName, trainerNamespace)
				if r.Fail {
					// runHelmCapture prints the transcript on failure.
					_, _ = io.WriteString(out, r.Output)
					return r.Output, errors.New("helm upgrade: exit status 1")
				}
				return r.Output, nil
			},
			uninstall: func(out io.Writer) error {
				uninstallCalls++
				_, _ = fmt.Fprintf(out, "[deps] Removing Helm release %q from namespace %s...\n",
					trainerReleaseName, trainerNamespace)
				return nil
			},
		}

		autoApprove := true
		if in.AutoApprove != nil {
			autoApprove = *in.AutoApprove
		}

		var buf bytes.Buffer
		sp := setupPhaseParams{
			ctx:         context.Background(),
			c:           c,
			skip:        map[string]bool{},
			in:          strings.NewReader(in.ConfirmInput),
			autoApprove: autoApprove,
			trainer:     trainer,
			out:         &buf,
		}
		err := installDepsPhase(sp)

		buf.WriteString("\n--- result ---\n")
		_, _ = fmt.Fprintf(&buf, "error: %v\ninstallCalls: %d\nuninstallCalls: %d\n",
			err, installCalls, uninstallCalls)
		tc.Actual = buf.String()
		return nil
	})
}
