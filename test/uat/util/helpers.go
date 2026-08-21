// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat

// Package util provides shared helpers for UAT tests.
package util

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
)

// Polling configuration.
var (
	PollInterval = 2 * time.Second
	PollTimeout  = 2 * time.Minute
)

// Scheme returns a runtime.Scheme with all required types registered.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = crev1alpha1.AddToScheme(s)
	_ = trainerv1alpha1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

// NewClient creates a controller-runtime client with the UAT scheme.
func NewClient(cfg *envconf.Config) (client.Client, error) {
	return client.New(cfg.Client().RESTConfig(), client.Options{Scheme: Scheme()})
}

// ── YAML helpers ──

// ApplyYAML reads a multi-document YAML file and creates each object in the cluster.
// Nodes (cluster-scoped) are created directly. Namespaced resources use the given namespace.
func ApplyYAML(ctx context.Context, t *testing.T, c client.Client, path string, namespace string) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "failed to read %s", path)

	reader := yamlutil.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}

		obj := &unstructured.Unstructured{}
		jsonBytes, jsonErr := yamlutil.ToJSON(doc)
		require.NoError(t, jsonErr)
		// Skip null/empty documents (comment-only blocks before ---).
		if string(bytes.TrimSpace(jsonBytes)) == "null" || len(bytes.TrimSpace(jsonBytes)) == 0 {
			continue
		}
		require.NoError(t, obj.UnmarshalJSON(jsonBytes))

		if obj.GetKind() != "Node" && namespace != "" {
			obj.SetNamespace(namespace)
		}

		require.NoError(t, c.Create(ctx, obj), "failed to create %s/%s", obj.GetKind(), obj.GetName())
		t.Logf("Created %s/%s", obj.GetKind(), obj.GetName())
	}
}

// CleanupYAML reads a multi-document YAML and deletes each object.
func CleanupYAML(ctx context.Context, c client.Client, path string, namespace string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	reader := yamlutil.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err != nil {
			break
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		jsonBytes, jsonErr := yamlutil.ToJSON(doc)
		if jsonErr != nil {
			continue
		}
		if string(bytes.TrimSpace(jsonBytes)) == "null" || len(bytes.TrimSpace(jsonBytes)) == 0 {
			continue
		}
		if err := obj.UnmarshalJSON(jsonBytes); err != nil {
			continue
		}
		if obj.GetKind() != "Node" && namespace != "" {
			obj.SetNamespace(namespace)
		}
		_ = c.Delete(ctx, obj)
	}
}

// ── Wait helpers ──

// WaitForCertification polls until the Certification has the given condition True.
func WaitForCertification(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	key types.NamespacedName,
	conditionType string,
	timeout time.Duration,
) *crev1alpha1.Certification {
	t.Helper()
	cert := &crev1alpha1.Certification{}
	require.Eventually(t, func() bool {
		if err := c.Get(ctx, key, cert); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(cert.Status.Conditions, conditionType)
	}, timeout, PollInterval, "timed out waiting for Certification %s condition %s", key.Name, conditionType)
	return cert
}

// WaitForCleanNamespace polls until no non-terminating pods exist in the namespace.
// Call at the start of each test to ensure pods from previous tests are gone.
func WaitForCleanNamespace(ctx context.Context, t *testing.T, c client.Client, namespace string) {
	t.Helper()
	require.Eventually(t, func() bool {
		list := &corev1.PodList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return false
		}
		for _, p := range list.Items {
			if p.DeletionTimestamp == nil {
				return false
			}
		}
		return true
	}, 60*time.Second, PollInterval, "timed out waiting for clean namespace %s", namespace)
}

// WaitForPods polls until at least minCount pods exist in the namespace.
func WaitForPods(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	namespace string,
	minCount int,
	timeout time.Duration,
) []corev1.Pod {
	t.Helper()
	var pods []corev1.Pod
	require.Eventually(t, func() bool {
		list := &corev1.PodList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return false
		}
		// Filter out terminating pods from previous tests.
		pods = pods[:0]
		for _, p := range list.Items {
			if p.DeletionTimestamp == nil {
				pods = append(pods, p)
			}
		}
		return len(pods) >= minCount
	}, timeout, PollInterval, "timed out waiting for %d pods in %s", minCount, namespace)
	return pods
}

// ── ncrectl integration ──

// RunNcrectl runs `ncrectl certification run` with the given args.
func RunNcrectl(ctx context.Context, t *testing.T, args ...string) {
	t.Helper()

	ncrectl := os.Getenv("NCRECTL")
	if ncrectl == "" {
		ncrectl = filepath.Join(ProjectDir(t), "bin", "ncrectl")
	}

	fullArgs := append([]string{"certification", "run"}, args...)
	cmd := exec.CommandContext(ctx, ncrectl, fullArgs...)
	cmd.Dir = ProjectDir(t)
	t.Logf("running: %s %s", ncrectl, strings.Join(fullArgs, " "))

	output, err := cmd.CombinedOutput()
	t.Logf("ncrectl output:\n%s", string(output))
	require.NoError(t, err, "ncrectl certification run failed")
}

// ProjectDir returns the CRE project root.
func ProjectDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	// Walk up from any test/uat/**/... directory to find go.mod.
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find project root from %s", wd)
		}
		dir = parent
	}
}

// ── YAML golden file comparison ──

// ComparePods serializes pods to YAML and compares against a golden file.
// On mismatch, writes the actual output to <golden>.actual for easy review.
func ComparePods(t *testing.T, goldenPath string, pods []corev1.Pod) {
	t.Helper()

	var buf bytes.Buffer
	for i, pod := range pods {
		StripPodVolatile(&pod)
		data, err := yaml.Marshal(pod)
		require.NoError(t, err)
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(data)
	}

	actual := buf.String()

	expected, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file %s not found", goldenPath)

	if string(expected) != actual {
		actualPath := goldenPath + ".actual"
		_ = os.WriteFile(actualPath, []byte(actual), 0o644)
		t.Errorf("pods do not match golden file %s\nActual output written to %s\n\n--- DIFF ---\n%s",
			goldenPath, actualPath, lineDiff(string(expected), actual))
	}
}

// CompareCertification gets the Certification, strips volatile fields,
// serializes to YAML, and compares against a golden file.
func CompareCertification(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	goldenPath string,
	key types.NamespacedName,
) {
	t.Helper()

	cert := &crev1alpha1.Certification{}
	require.NoError(t, c.Get(ctx, key, cert))
	StripCertVolatile(cert)

	actual, err := yaml.Marshal(cert)
	require.NoError(t, err)

	expected, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file %s not found", goldenPath)

	if string(expected) != string(actual) {
		actualPath := goldenPath + ".actual"
		_ = os.WriteFile(actualPath, actual, 0o644)
		t.Errorf("certification does not match golden file %s\nActual output written to %s\n\n--- DIFF ---\n%s",
			goldenPath, actualPath, lineDiff(string(expected), string(actual)))
	}
}

// lineDiff produces a simple line-by-line diff showing only changed lines.
func lineDiff(expected, actual string) string {
	expectedLines := strings.Split(expected, "\n")
	actualLines := strings.Split(actual, "\n")

	var diff bytes.Buffer
	maxLines := len(expectedLines)
	if len(actualLines) > maxLines {
		maxLines = len(actualLines)
	}

	for i := 0; i < maxLines; i++ {
		var expLine, actLine string
		if i < len(expectedLines) {
			expLine = expectedLines[i]
		}
		if i < len(actualLines) {
			actLine = actualLines[i]
		}
		if expLine != actLine {
			fmt.Fprintf(&diff, "line %d:\n  expected: %s\n  actual:   %s\n", i+1, expLine, actLine)
		}
	}

	if diff.Len() == 0 {
		return "(no differences)"
	}
	return diff.String()
}

// ── Volatile field stripping ──

// StripPodVolatile removes timestamps and UIDs from a Pod.
func StripPodVolatile(pod *corev1.Pod) {
	pod.ResourceVersion = ""
	pod.UID = ""
	pod.Generation = 0
	pod.CreationTimestamp = metav1.Time{}
	pod.ManagedFields = nil
	pod.Namespace = ""

	for i := range pod.OwnerReferences {
		pod.OwnerReferences[i].UID = ""
	}

	// Strip random UIDs from annotations and labels.
	stripAnnotation(pod, "jobset.sigs.k8s.io/jobset-uid")
	stripAnnotation(pod, "jobset.sigs.k8s.io/job-key")
	stripCNIAnnotations(pod)
	stripLabel(pod, "jobset.sigs.k8s.io/jobset-uid")
	stripLabel(pod, "jobset.sigs.k8s.io/job-key")
	stripLabel(pod, "batch.kubernetes.io/controller-uid")
	stripLabel(pod, "controller-uid")

	// Strip random pod name suffix (keep generateName which is stable).
	pod.Name = ""

	// Strip nodeName (scheduling order varies between runs).
	pod.Spec.NodeName = ""

	// Strip kube-api-access-* volume names (random suffix).
	for i := range pod.Spec.Volumes {
		if strings.HasPrefix(pod.Spec.Volumes[i].Name, "kube-api-access-") {
			pod.Spec.Volumes[i].Name = "kube-api-access"
		}
	}
	for i := range pod.Spec.Containers {
		for j := range pod.Spec.Containers[i].VolumeMounts {
			if strings.HasPrefix(pod.Spec.Containers[i].VolumeMounts[j].Name, "kube-api-access-") {
				pod.Spec.Containers[i].VolumeMounts[j].Name = "kube-api-access"
			}
		}
	}
	for i := range pod.Spec.InitContainers {
		for j := range pod.Spec.InitContainers[i].VolumeMounts {
			if strings.HasPrefix(pod.Spec.InitContainers[i].VolumeMounts[j].Name, "kube-api-access-") {
				pod.Spec.InitContainers[i].VolumeMounts[j].Name = "kube-api-access"
			}
		}
	}

	pod.Status = corev1.PodStatus{}
}

func stripAnnotation(pod *corev1.Pod, key string) {
	if pod.Annotations != nil {
		delete(pod.Annotations, key)
	}
}

// cniAnnotationPrefixes are annotation keys a CNI plugin writes onto a pod
// after the pod is created. The values hold a container ID and the assigned
// pod IP, so they differ on every run. Whether they are present at all also
// varies, because the test may read the pod before the plugin annotates it.
//
// KWOK runs no real CNI, so these never appear in the simulated tests. They do
// appear on any real cluster, and they make a golden file unusable.
var cniAnnotationPrefixes = []string{
	"cni.projectcalico.org/", // Calico
	"k8s.v1.cni.cncf.io/",    // Multus network-status
	"io.cilium/",             // Cilium
}

// stripCNIAnnotations removes every annotation written by a CNI plugin.
func stripCNIAnnotations(pod *corev1.Pod) {
	for key := range pod.Annotations {
		for _, prefix := range cniAnnotationPrefixes {
			if strings.HasPrefix(key, prefix) {
				delete(pod.Annotations, key)
				break
			}
		}
	}
}

func stripLabel(pod *corev1.Pod, key string) {
	if pod.Labels != nil {
		delete(pod.Labels, key)
	}
}

// StripCertVolatile removes timestamps and UIDs from a Certification.
func StripCertVolatile(cert *crev1alpha1.Certification) {
	cert.ResourceVersion = ""
	cert.UID = ""
	cert.Generation = 0
	cert.CreationTimestamp = metav1.Time{}
	cert.ManagedFields = nil
	cert.Namespace = ""

	for i := range cert.OwnerReferences {
		cert.OwnerReferences[i].UID = ""
	}

	for i := range cert.Status.Conditions {
		cert.Status.Conditions[i].LastTransitionTime = metav1.Time{}
	}

	// Normalize node-results ConfigMap refs (UID-based names change per run).
	for i := range cert.Status.CategoryStatuses {
		if ref := cert.Status.CategoryStatuses[i].SucceededNodesRef; ref != nil && ref.Name != "" {
			ref.Name = "succeeded-nodes"
		}
		if ref := cert.Status.CategoryStatuses[i].FailedNodesRef; ref != nil && ref.Name != "" {
			ref.Name = "failed-nodes"
		}
	}
}

// DeleteCertification deletes a Certification and ignores not-found errors.
func DeleteCertification(ctx context.Context, c client.Client, name, namespace string) {
	cert := &crev1alpha1.Certification{}
	cert.Name = name
	cert.Namespace = namespace
	_ = client.IgnoreNotFound(c.Delete(ctx, cert))
}

// RestartController restarts the CRE controller by deleting its pods.
// The Deployment recreates them with a fresh informer cache.
// Waits for the new pod to be Ready.
func RestartController(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()

	// Delete all controller pods.
	podList := &corev1.PodList{}
	require.NoError(t, c.List(ctx, podList,
		client.InNamespace("cluster-readiness-engine"),
		client.MatchingLabels{"control-plane": "manager"}))
	for i := range podList.Items {
		_ = c.Delete(ctx, &podList.Items[i])
	}

	// Wait for a new Ready pod.
	require.Eventually(t, func() bool {
		list := &corev1.PodList{}
		if err := c.List(ctx, list,
			client.InNamespace("cluster-readiness-engine"),
			client.MatchingLabels{"control-plane": "manager"}); err != nil {
			return false
		}
		for _, pod := range list.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true
				}
			}
		}
		return false
	}, 60*time.Second, PollInterval, "controller pod did not become ready after restart")

	// Wait for pods from previous tests to be fully cleaned up.
	WaitForCleanNamespace(ctx, t, c, "default")
}

// ── Namespace helpers ──

// DeleteNamespace deletes a namespace. Ignores not-found errors.
func DeleteNamespace(ctx context.Context, c client.Client, name string) error {
	ns := &corev1.Namespace{}
	ns.Name = name
	return client.IgnoreNotFound(c.Delete(ctx, ns))
}

// CertificationKey returns a NamespacedName for a Certification.
func CertificationKey(name, namespace string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: namespace}
}

// ── WorkloadRun helpers ──

// RunNcrectlWorkloadRun runs `ncrectl workloadrun run` with a YAML file.
func RunNcrectlWorkloadRun(ctx context.Context, t *testing.T, yamlFile string) {
	t.Helper()

	ncrectl := os.Getenv("NCRECTL")
	if ncrectl == "" {
		ncrectl = filepath.Join(ProjectDir(t), "bin", "ncrectl")
	}

	// Resolve relative paths against test/uat/ (where tests run from).
	if !filepath.IsAbs(yamlFile) {
		yamlFile = filepath.Join(ProjectDir(t), "test", "uat", yamlFile)
	}
	fullArgs := []string{"workloadrun", "run", yamlFile}
	cmd := exec.CommandContext(ctx, ncrectl, fullArgs...)
	cmd.Dir = ProjectDir(t)
	t.Logf("running: %s %s", ncrectl, strings.Join(fullArgs, " "))

	output, err := cmd.CombinedOutput()
	t.Logf("ncrectl output:\n%s", string(output))
	require.NoError(t, err, "ncrectl workloadrun run failed")
}

// WaitForWorkloadRun polls until the WorkloadRun has the given condition True.
func WaitForWorkloadRun(
	ctx context.Context,
	t *testing.T,
	c client.Client,
	key types.NamespacedName,
	conditionType string,
	timeout time.Duration,
) *crev1alpha1.WorkloadRun {
	t.Helper()
	run := &crev1alpha1.WorkloadRun{}
	require.Eventually(t, func() bool {
		if err := c.Get(ctx, key, run); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(run.Status.Conditions, conditionType)
	}, timeout, PollInterval,
		"timed out waiting for WorkloadRun %s condition %s",
		key.Name, conditionType)
	return run
}

// DeleteWorkloadRun deletes a WorkloadRun and ignores not-found errors.
func DeleteWorkloadRun(ctx context.Context, c client.Client, name, namespace string) {
	run := &crev1alpha1.WorkloadRun{}
	run.Name = name
	run.Namespace = namespace
	_ = client.IgnoreNotFound(c.Delete(ctx, run))
}
