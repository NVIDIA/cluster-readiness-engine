// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:goconst // Structured recovery fixtures intentionally repeat Kubernetes field and kind literals.
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	sigsyaml "sigs.k8s.io/yaml"
)

// testAPIVersionV1Alpha1 is the "v1alpha1" version string shared by the
// NVCRE and Trainer GroupVersionKinds registered for tests in this package.
const testAPIVersionV1Alpha1 = "v1alpha1"

// The external JobSet release the external-mode recovery fixtures seed. The
// names are chart-derived: release "external-jobset" with
// fullnameOverride=external-jobset renders <fullname>-controller and
// <fullname>-webhook-service.
const (
	externalJobSetNamespace  = "external-jobset-system"
	externalJobSetController = "external-jobset-controller"
)

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
	StateError         string `yaml:"stateError"`
	ReleaseManifest    string `yaml:"releaseManifest"`
	JobSetCRD          string `yaml:"jobSetCRD"`
	UninstallFail      bool   `yaml:"uninstallFail"`
	SeedRecovery       bool   `yaml:"seedRecovery"`
	ConfirmMutation    string `yaml:"confirmMutation"`
	AfterCRDDelete     int    `yaml:"afterCRDDelete"`
	DeleteMutation     string `yaml:"deleteMutation"`
	DiscoveryEmptyCall int    `yaml:"discoveryEmptyCall"`
	DiscoveryErrorCall int    `yaml:"discoveryErrorCall"`
	ListFailureKind    string `yaml:"listFailureKind"`
	UnservedCRD        string `yaml:"unservedCRD"`
	// ExtendedDiscovery makes the discovery stub return the full protected
	// kind set (workloads, storage, leases, and the excluded kinds) instead
	// of the minimal Secret/ConfigMap pair the ADR-073 cases were written for.
	ExtendedDiscovery bool `yaml:"extendedDiscovery"`
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
		if in.SeedRecovery {
			objs = append(objs, recoverySeedObjects()...)
		}
		if err := markCRDUnserved(objs, in.UnservedCRD); err != nil {
			return err
		}
		var c client.Client
		crdDeletes := 0
		c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
			List: recoveryListInterceptor(in.ListFailureKind, &crdDeletes),
			Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
				err := underlying.Delete(ctx, object, opts...)
				if err == nil && strings.HasSuffix(object.GetName(), "."+trainerAPIGroup) {
					crdDeletes++
					if crdDeletes == in.AfterCRDDelete {
						if mutationErr := createRecoveryMutation(ctx, underlying, in.DeleteMutation); mutationErr != nil {
							return mutationErr
						}
					}
				}
				return err
			},
		}).Build()
		if in.JobSetCRD != "absent" && in.ReleaseState != helmStateNotInstalled && in.ReleaseState != helmStateUninstalled &&
			in.ReleaseState != helmStateUnknown {
			present, err := jobSetCRDExists(context.Background(), c)
			if err != nil {
				return err
			}
			if !present {
				crd := &unstructured.Unstructured{}
				crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
				crd.SetName(jobSetCRDName)
				if err := c.Create(context.Background(), crd); err != nil {
					return err
				}
			}
		}

		installCalls, uninstallCalls := 0, 0
		var installModes []jobSetMode
		trainer := trainerHelm{
			state: func() trainerReleaseState {
				var stateErr error
				if in.StateError != "" {
					stateErr = errors.New(in.StateError)
				}
				return trainerReleaseState{state: in.ReleaseState, chartVersion: in.ChartVersion, err: stateErr}
			},
			install: func(mode jobSetMode, out io.Writer) (string, error) {
				installCalls++
				installModes = append(installModes, mode)
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
				if in.UninstallFail {
					return errors.New("helm uninstall: exit status 1")
				}
				// A real helm uninstall removes every resource it rendered,
				// which includes the controller Deployment, not just the
				// conflicting webhook Secrets. Deleting the Deployment leaves
				// its ReplicaSet and Pod as descendants of a now-absent owner,
				// so the final gate has to re-verify them from the preserved
				// pre-uninstall evidence instead of re-deriving them from a
				// still-live owner chain.
				for _, listKind := range []struct{ apiVersion, kind string }{
					{"v1", "SecretList"}, {"apps/v1", "DeploymentList"},
				} {
					rendered := &unstructured.UnstructuredList{}
					rendered.SetAPIVersion(listKind.apiVersion)
					rendered.SetKind(listKind.kind)
					if err := c.List(context.Background(), rendered, client.InNamespace(trainerNamespace)); err != nil {
						return err
					}
					for i := range rendered.Items {
						if !completeHelmOwner(&rendered.Items[i]).bundled() {
							continue
						}
						if err := c.Delete(context.Background(), &rendered.Items[i]); err != nil {
							return err
						}
					}
				}
				return nil
			},
			manifest: func() (string, error) { return in.ReleaseManifest, nil },
		}

		autoApprove := true
		if in.AutoApprove != nil {
			autoApprove = *in.AutoApprove
		}

		var buf bytes.Buffer
		inputReader := io.Reader(strings.NewReader(in.ConfirmInput))
		if in.ConfirmMutation != "" {
			inputReader = &mutationReader{reader: inputReader, mutate: func() error {
				return createRecoveryMutation(context.Background(), c, in.ConfirmMutation)
			}}
		}
		discoveryCalls := 0
		sp := setupPhaseParams{
			ctx:                context.Background(),
			c:                  c,
			skip:               map[string]bool{},
			in:                 inputReader,
			autoApprove:        autoApprove,
			trainer:            trainer,
			discoverNamespaced: recoveryDiscoveryStub(in, &discoveryCalls),
			out:                &buf,
		}
		err := installDepsPhase(sp)

		buf.WriteString("\n--- result ---\n")
		_, _ = fmt.Fprintf(&buf, "error: %v\ninstallCalls: %d\ninstallModes: %v\nuninstallCalls: %d\ncrdDeletes: %d\n",
			err, installCalls, installModes, uninstallCalls, crdDeletes)
		_, _ = fmt.Fprintf(&buf, "jobSetCRDPresent: %t\nnamespacePresent: %t\ntrainerCRDsPresent: %v\n",
			objectPresent(t, c, schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, "", jobSetCRDName),
			objectPresent(t, c, schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, "", trainerNamespace),
			presentTrainerCRDs(t, c))
		tc.Actual = buf.String()
		return nil
	})
}

func markCRDUnserved(objects []client.Object, name string) error {
	if name == "" {
		return nil
	}
	for _, object := range objects {
		if object.GetName() != name {
			continue
		}
		unstructuredObject, ok := object.(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("test object %s is not unstructured", object.GetName())
		}
		return unstructured.SetNestedSlice(unstructuredObject.Object,
			[]any{map[string]any{"name": testAPIVersionV1Alpha1, "served": false}}, "spec", "versions")
	}
	return fmt.Errorf("test CRD %s was not found", name)
}

// recoveryDiscoveryStub returns the discovery function the init-recovery
// cases inject. It counts calls so a case can fail or empty a specific call,
// and returns either the minimal ADR-073 kind set or the extended protected
// kind set when the case opts in with extendedDiscovery.
func recoveryDiscoveryStub(in initRecoveryInput, discoveryCalls *int) func() ([]*metav1.APIResourceList, error) {
	return func() ([]*metav1.APIResourceList, error) {
		*discoveryCalls++
		if *discoveryCalls == in.DiscoveryErrorCall {
			return nil, errors.New("simulated partial discovery failure")
		}
		if *discoveryCalls == in.DiscoveryEmptyCall {
			return []*metav1.APIResourceList{}, nil
		}
		if !in.ExtendedDiscovery {
			return []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: []metav1.APIResource{
				{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: metav1.Verbs{"list"}},
			}}}, nil
		}
		// The protected kinds a real discovery call returns for the
		// namespace, including the excluded kinds so the exclusion is
		// exercised on the inventory cases that opt in.
		return []*metav1.APIResourceList{
			{GroupVersion: "v1", APIResources: []metav1.APIResource{
				{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "services", Kind: "Service", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "serviceaccounts", Kind: "ServiceAccount", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "persistentvolumeclaims", Kind: "PersistentVolumeClaim", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "events", Kind: "Event", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "endpoints", Kind: "Endpoints", Namespaced: true, Verbs: metav1.Verbs{"list"}},
			}},
			{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true, Verbs: metav1.Verbs{"list"}},
			}},
			{GroupVersion: "coordination.k8s.io/v1", APIResources: []metav1.APIResource{
				{Name: "leases", Kind: "Lease", Namespaced: true, Verbs: metav1.Verbs{"list"}},
			}},
			{GroupVersion: "discovery.k8s.io/v1", APIResources: []metav1.APIResource{
				{Name: "endpointslices", Kind: "EndpointSlice", Namespaced: true, Verbs: metav1.Verbs{"list"}},
			}},
			// Production discovery returns the namespaced Trainer resources
			// too, and recovery deletes their CRDs mid-run. Without them the
			// trainerAPIsRemoved arms in collectRecoveryEvidence and
			// compareRecoveryEvidence are evaluated but never taken, so the
			// accounting for intentionally removed APIs is untested even
			// though it runs on every real recovery.
			{GroupVersion: trainerAPIGroup + "/" + testAPIVersionV1Alpha1, APIResources: []metav1.APIResource{
				{Name: "trainingruntimes", Kind: "TrainingRuntime", Namespaced: true, Verbs: metav1.Verbs{"list"}},
			}},
		}, nil
	}
}

type mutationReader struct {
	reader io.Reader
	mutate func() error
	done   bool
}

func (r *mutationReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		if err := r.mutate(); err != nil {
			return 0, err
		}
	}
	return r.reader.Read(p)
}

// recoveryListInterceptor denies one kind on request, and stops serving the
// Trainer group once recovery has deleted its CRDs. The fake client keeps
// serving a kind after its CRD is gone while a real API server stops, so
// without this the accounting for the APIs recovery just removed would be
// satisfied by empty lists instead of having to be correct.
func recoveryListInterceptor(
	failureKind string, crdDeletes *int,
) func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
	return func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		gvk := list.GetObjectKind().GroupVersionKind()
		if failureKind != "" && gvk.Kind == failureKind+"List" {
			return errors.New("simulated required list denial")
		}
		if gvk.Group == trainerAPIGroup && *crdDeletes >= len(trainerRecoveryCRDs) {
			resource := strings.ToLower(strings.TrimSuffix(gvk.Kind, "List")) + "s"
			return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: resource}, "")
		}
		return underlying.List(ctx, list, opts...)
	}
}

func recoverySeedObjects() []client.Object {
	var result []client.Object
	for _, name := range append(append([]string{}, trainerRecoveryCRDs...), jobSetCRDName) {
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		object.SetName(name)
		group := trainerAPIGroup
		kind, plural, version := "TrainJob", "trainjobs", testAPIVersionV1Alpha1
		switch name {
		case "trainingruntimes." + trainerAPIGroup:
			kind, plural = "TrainingRuntime", "trainingruntimes"
		case "clustertrainingruntimes." + trainerAPIGroup:
			kind, plural = "ClusterTrainingRuntime", "clustertrainingruntimes"
		case jobSetCRDName:
			group, kind, plural, version = jobsetAPIGroup, "JobSet", "jobsets", "v1alpha2"
		}
		object.Object["spec"] = map[string]any{
			"group":    group,
			"names":    map[string]any{"kind": kind, "plural": plural},
			"versions": []any{map[string]any{"name": version, "served": true}},
		}
		result = append(result, object)
	}
	namespace := &unstructured.Unstructured{}
	namespace.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	namespace.SetName(trainerNamespace)
	namespace.SetUID("namespace-uid")
	return append(result, namespace)
}

func createRecoveryMutation(ctx context.Context, c client.Client, mutation string) error {
	switch mutation {
	case "foreign-configmap":
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "foreign-config", "namespace": trainerNamespace, "uid": "foreign-config-uid"},
			"data":     map[string]any{"owner": "external"},
		}}
		return c.Create(ctx, object)
	case "trainingruntime":
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": trainerAPIGroup + "/" + testAPIVersionV1Alpha1, "kind": "TrainingRuntime",
			"metadata": map[string]any{"name": "late-runtime", "namespace": "team-a"},
		}}
		return c.Create(ctx, object)
	case "replace-namespace":
		namespace := &unstructured.Unstructured{}
		namespace.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
		namespace.SetName(trainerNamespace)
		if err := c.Delete(ctx, namespace); err != nil {
			return err
		}
		namespace.SetResourceVersion("")
		namespace.SetUID("replacement-namespace-uid")
		return c.Create(ctx, namespace)
	case "jobset":
		// A JobSet appearing mid-recovery. Outside external mode every
		// instance blocks, because the cleanup removes the controller that
		// would reconcile it (ADR-078 decision 4).
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": jobsetAPIGroup + "/v1alpha2", "kind": "JobSet",
			"metadata": map[string]any{"name": "late-jobset", "namespace": "team-a", "uid": "late-jobset-uid"},
		}}
		return c.Create(ctx, object)
	case "replace-external-controller":
		// The external JobSet controller is replaced by an identically named
		// Deployment with a new UID. Every ownership-token part embeds a UID,
		// so this must be caught before the release is uninstalled; a token
		// built from names alone would wave it through.
		deployment := &unstructured.Unstructured{}
		deployment.SetGroupVersionKind(schema.GroupVersionKind{Group: appsAPIGroup, Version: "v1", Kind: kindDeployment})
		if err := c.Get(ctx, client.ObjectKey{Namespace: externalJobSetNamespace, Name: externalJobSetController}, deployment); err != nil {
			return err
		}
		if err := c.Delete(ctx, deployment); err != nil {
			return err
		}
		deployment.SetResourceVersion("")
		deployment.SetUID("replacement-external-controller-uid")
		return c.Create(ctx, deployment)
	default:
		return fmt.Errorf("unknown recovery mutation %q", mutation)
	}
}

func objectPresent(t *testing.T, c client.Client, gvk schema.GroupVersionKind, namespace, name string) bool {
	t.Helper()
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, object)
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get %s %s/%s: %v", gvk.Kind, namespace, name, err)
	}
	return err == nil
}

func presentTrainerCRDs(t *testing.T, c client.Client) []string {
	t.Helper()
	var present []string
	for _, name := range trainerRecoveryCRDs {
		if objectPresent(t, c, schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, "", name) {
			present = append(present, name)
		}
	}
	return present
}
