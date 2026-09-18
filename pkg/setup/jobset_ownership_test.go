// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:goconst // Ownership fixtures intentionally repeat Kubernetes field and kind literals.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestSupportedNonHelmEvidenceConflictsWithBundledOrPartialHelmMetadata(t *testing.T) {
	raw, err := os.ReadFile("testdata/jobset-ownership/supported-nonhelm-controller/input_objects.yaml")
	if err != nil {
		t.Fatal(err)
	}
	build := func(t *testing.T, mutate func([]client.Object)) client.Client {
		t.Helper()
		objects := make([]client.Object, 0, len(splitYAMLDocuments(raw)))
		for _, doc := range splitYAMLDocuments(raw) {
			object, decodeErr := decodeUnstructured(doc)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			objects = append(objects, object)
		}
		mutate(objects)
		scheme := newSetupScheme(t)
		registerTrainerKinds(scheme)
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	}

	t.Run("bundled manifest conflicts", func(t *testing.T) {
		c := build(t, func([]client.Object) {})
		observation := observeJobSetOwnership(context.Background(), c, true, "deployed", func() (string, error) {
			return "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: jobset-controller\n", nil
		})
		if observation.mode != jobSetModeUnknown {
			t.Fatalf("bundled manifest plus non-Helm controller must be unknown, got %s", observation.mode)
		}
	})

	t.Run("partial Helm metadata is not non-Helm", func(t *testing.T) {
		c := build(t, func(objects []client.Object) {
			objects[0].SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
		})
		observation := observeJobSetOwnership(context.Background(), c, true, helmStateNotInstalled, nil)
		if observation.mode != jobSetModeUnknown {
			t.Fatalf("partial Helm metadata must be unknown, got %s", observation.mode)
		}
	})
}

func TestFingerprintScanConsumesEveryPageAndFailsClosed(t *testing.T) {
	role := func(name string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
			"metadata": map[string]any{"name": name},
			"rules":    []any{map[string]any{"apiGroups": []any{jobsetAPIGroup}, "resources": []any{"jobsets"}, "verbs": []any{"get"}}},
		}}
	}
	scheme := newSetupScheme(t)
	listCalls := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind != "ClusterRoleList" {
				return underlying.List(ctx, list, opts...)
			}
			listCalls++
			target := list.(*unstructured.UnstructuredList)
			if listCalls == 1 {
				target.Items = []unstructured.Unstructured{role("first-page")}
				target.SetContinue("next-page")
				return nil
			}
			target.Items = []unstructured.Unstructured{role("second-page")}
			target.SetContinue("")
			return nil
		},
	}).Build()
	findings, err := scanJobSetFingerprints(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 2 || fingerprintNames(findings) != "ClusterRole/first-page, ClusterRole/second-page" {
		t.Fatalf("pagination not consumed: calls=%d findings=%s", listCalls, fingerprintNames(findings))
	}

	partialCalls := 0
	partial := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind != "ClusterRoleList" {
				return underlying.List(ctx, list, opts...)
			}
			partialCalls++
			listOptions := (&client.ListOptions{}).ApplyOptions(opts)
			if partialCalls == 1 {
				if listOptions.Continue != "" {
					t.Fatalf("first page unexpectedly supplied continuation %q", listOptions.Continue)
				}
				target := list.(*unstructured.UnstructuredList)
				target.Items = []unstructured.Unstructured{role("partial-first-page")}
				target.SetContinue("partial-next-page")
				return nil
			}
			if listOptions.Continue != "partial-next-page" {
				t.Fatalf("second page did not receive continuation: %q", listOptions.Continue)
			}
			return fmt.Errorf("simulated second-page failure")
		},
	}).Build()
	partialObservation := observeJobSetOwnership(context.Background(), partial, false, helmStateNotInstalled, nil)
	if partialCalls != 2 || partialObservation.mode != jobSetModeUnknown ||
		!strings.Contains(strings.Join(partialObservation.evidence, " "), "second-page failure") ||
		strings.Contains(strings.Join(partialObservation.evidence, " "), "partial-first-page") {
		t.Fatalf("partial pagination must discard findings and select unknown: calls=%d observation=%#v", partialCalls, partialObservation)
	}

	denied := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "ClusterRoleList" {
				return fmt.Errorf("simulated cluster-role list denial")
			}
			return underlying.List(ctx, list, opts...)
		},
	}).Build()
	observation := observeJobSetOwnership(context.Background(), denied, false, helmStateNotInstalled, nil)
	if observation.mode != jobSetModeUnknown || !strings.Contains(strings.Join(observation.evidence, " "), "list denial") {
		t.Fatalf("scan denial must select unknown with cause: %#v", observation)
	}
}

// TestAbsentProbeSkipsNamespacedAndJobSetLists pins the CRD-first probe's
// minimality (ADR-078 decision 1). The fresh-absent golden asserts the mode,
// the evidence and manifestCalls, but installs no List interceptor, so it
// cannot tell a probe that stops at the cluster-scoped scan from one that goes
// on to read namespaced controller resources and every JobSet in the cluster.
// Those reads need permissions the fresh path is documented not to require.
func TestAbsentProbeSkipsNamespacedAndJobSetLists(t *testing.T) {
	scanned := map[string]int{}
	forbidden := map[string]bool{
		"ServiceAccountList": true, "ServiceList": true, "ConfigMapList": true,
		"SecretList": true, "DeploymentList": true, "RoleList": true,
		"RoleBindingList": true, "JobSetList": true,
	}
	scheme := newSetupScheme(t)
	registerTrainerKinds(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			kind := list.GetObjectKind().GroupVersionKind().Kind
			scanned[kind]++
			if forbidden[kind] {
				t.Errorf("absent path must not list %s", kind)
			}
			return underlying.List(ctx, list, opts...)
		},
	}).Build()

	observation := observeJobSetOwnership(context.Background(), c, false, helmStateNotInstalled, nil)
	if observation.mode != jobSetModeAbsent {
		t.Fatalf("clean scan with an absent CRD must be absent, got %s: %v", observation.mode, observation.evidence)
	}
	// Exactly the three cluster-scoped kinds the fingerprint scan covers.
	for _, kind := range []string{"ValidatingWebhookConfigurationList", "MutatingWebhookConfigurationList", "ClusterRoleList"} {
		if scanned[kind] == 0 {
			t.Errorf("fingerprint scan did not list %s", kind)
		}
	}
	if len(scanned) != 3 {
		t.Errorf("absent path listed more than the fingerprint kinds: %v", scanned)
	}
}

func TestInstallDepsCRDReadFailureRefusesBeforePinnedSkip(t *testing.T) {
	installCalls := 0
	c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, object client.Object, _ ...client.GetOption) error {
			if object.GetObjectKind().GroupVersionKind().Kind == kindCustomResourceDefinition {
				return errors.New("simulated CRD read denial")
			}
			return errors.New("unexpected get")
		},
	}).Build()
	var out bytes.Buffer
	err := installDepsPhase(setupPhaseParams{
		ctx: context.Background(), c: c, out: &out, skip: map[string]bool{},
		trainer: trainerHelm{
			state: func() trainerReleaseState {
				return trainerReleaseState{state: helmStateDeployed, chartVersion: strings.TrimPrefix(kubeflowTrainerVersion, "v")}
			},
			install: func(jobSetMode, io.Writer) (string, error) { installCalls++; return "", nil },
		},
	})
	if err == nil || !strings.Contains(err.Error(), "simulated CRD read denial") {
		t.Fatalf("expected preserved CRD read error, got %v", err)
	}
	if installCalls != 0 {
		t.Fatalf("CRD read failure must stop before Helm mutation, calls=%d", installCalls)
	}
	for _, want := range []string{"JobSet ownership inspection is inconclusive", "read JobSet CRD: simulated CRD read denial", "--skip-phases=deps"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing fallback evidence %q:\n%s", want, out.String())
		}
	}
}

func TestObserveJobSetOwnership(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "jobset-ownership", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			CRDPresent      bool   `yaml:"crdPresent"`
			ReleaseState    string `yaml:"releaseState"`
			ReleaseManifest string `yaml:"releaseManifest"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		var objects []client.Object
		for _, doc := range splitYAMLDocuments([]byte(tc.Inputs["input_objects.yaml"])) {
			object, err := decodeUnstructured(doc)
			if err != nil {
				return fmt.Errorf("decode input object: %w", err)
			}
			objects = append(objects, object)
		}
		scheme := newSetupScheme(t)
		registerTrainerKinds(scheme)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
		manifestCalls := 0
		observation := observeJobSetOwnership(context.Background(), c, in.CRDPresent, in.ReleaseState,
			func() (string, error) { manifestCalls++; return in.ReleaseManifest, nil })
		actual, err := json.MarshalIndent(struct {
			Mode          jobSetMode `json:"mode"`
			Evidence      []string   `json:"evidence"`
			InstallJobSet bool       `json:"installJobSet"`
			ManifestCalls int        `json:"manifestCalls"`
		}{observation.mode, observation.evidence, observation.mode != jobSetModeExternal, manifestCalls}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(actual) + "\n"
		return nil
	})
}
