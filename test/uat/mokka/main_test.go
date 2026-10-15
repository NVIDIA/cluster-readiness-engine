// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat_mokka

// Package mokka is the UAT runtime lane (ADR-096). It runs against a cluster
// brought up by setup.sh, where Mokka simulates the GPUs and the real device
// plugin, GFD and DRA driver run on top of it, so NVCRE's pods schedule, start
// and log. Tests assert behaviour (terminal status, reasons, measurements,
// lifecycle ordering, cleanup), never pod specs; those stay with the KWOK
// render lane in test/uat.
//
// Every test names the profiles it applies to and skips on any other, so the
// same package runs once per profile.
package mokka

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/test/uat/util"
)

const (
	pollInterval = time.Second
	// A stub run takes under a minute; the margin covers image pulls and
	// MPI bootstrap on a loaded CI runner.
	certTimeout    = 5 * time.Minute
	cleanupTimeout = 3 * time.Minute
	// A launcher that exits non-zero is restarted in place: the
	// nccl-all-reduce catalog entry sets restartPolicy OnFailure and leaves
	// backoffLimit at the Kubernetes default of 6, so the kubelet's
	// crash-loop backoff runs before the Job fails and NVCRE sees it. That
	// measured about seven minutes locally; the budget leaves room for a
	// loaded CI runner.
	workloadFailureTimeout = 15 * time.Minute

	nvcreNamespace = "nvcre"
)

var (
	kubeContext string
	profile     string
	c           client.Client
	clientset   kubernetes.Interface

	computeDomainGVK = schema.GroupVersionKind{Group: "resource.nvidia.com", Version: "v1beta1", Kind: "ComputeDomainList"}
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "mokka lane setup: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// setup connects to the cluster setup.sh created. The context is always named
// explicitly, so the lane never runs against whatever context is current.
func setup() error {
	cluster := os.Getenv("MOKKA_CLUSTER")
	if cluster == "" {
		cluster = "nvcre-mokka"
	}
	kubeContext = "kind-" + cluster

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig context %s: %w", kubeContext, err)
	}
	scheme := util.Scheme()
	if err := resourcev1.AddToScheme(scheme); err != nil {
		return err
	}
	if c, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		return err
	}
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "kube-system", Name: "nvcre-mokka"}, cm); err != nil {
		return fmt.Errorf("read the profile setup.sh records (is %s a Mokka cluster?): %w", cluster, err)
	}
	profile = cm.Data["profile"]
	if profile == "" {
		return fmt.Errorf("configmap kube-system/nvcre-mokka has no profile")
	}
	fmt.Printf("mokka lane: cluster %s, profile %s\n", cluster, profile)
	return nil
}

// pollUntil calls done on the test goroutine until it returns true, failing
// the test after timeout. Unlike require.Eventually, which runs its condition
// on another goroutine, done may call require itself, so a sampled invariant
// can fail the test the moment it breaks.
//
// describe is called only on timeout, so it can report state the polling
// gathered.
func pollUntil(t *testing.T, timeout, interval time.Duration, done func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %s", timeout, describe())
		}
		time.Sleep(interval)
	}
}

// requireProfile skips the test unless the cluster runs one of the profiles.
func requireProfile(t *testing.T, profiles ...string) {
	t.Helper()
	for _, p := range profiles {
		if p == profile {
			return
		}
	}
	t.Skipf("applies to profiles %v; cluster runs %s", profiles, profile)
}

// testNamespace creates a namespace for one test, so the leftover checks see
// only what that test created, and deletes it afterwards.
func testNamespace(ctx context.Context, t *testing.T) string {
	t.Helper()
	name := "mokka-" + strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "-"))
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, client.IgnoreAlreadyExists(c.Create(ctx, ns)))
	t.Cleanup(func() {
		ctx := context.Background()
		_ = c.DeleteAllOf(ctx, &nvcrev1alpha1.Certification{}, client.InNamespace(name))
		_ = util.DeleteNamespace(ctx, c, name)
	})
	return name
}

// createCertification creates the Certification in testdata/<file> in the
// namespace and returns its name.
func createCertification(ctx context.Context, t *testing.T, namespace, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	require.NoError(t, err)
	cert := &nvcrev1alpha1.Certification{}
	require.NoError(t, yaml.UnmarshalStrict(data, cert), "decode %s", file)
	cert.Namespace = namespace
	require.NoError(t, c.Create(ctx, cert))
	t.Logf("created Certification %s/%s from %s", namespace, cert.Name, file)
	return cert.Name
}

// waitTerminal waits for the Certification to reach Succeeded or Failed and
// returns it, so a test can assert the outcome rather than time out on the
// wrong one.
func waitTerminal(ctx context.Context, t *testing.T, namespace, name string) *nvcrev1alpha1.Certification {
	t.Helper()
	return waitTerminalWithin(ctx, t, namespace, name, certTimeout)
}

func waitTerminalWithin(ctx context.Context, t *testing.T, namespace, name string, timeout time.Duration) *nvcrev1alpha1.Certification {
	t.Helper()
	cert := &nvcrev1alpha1.Certification{}
	pollUntil(t, timeout, pollInterval, func() bool {
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cert); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationSucceeded) ||
			meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed)
	}, func() string {
		return fmt.Sprintf("Certification %s/%s never reached a terminal condition", namespace, name)
	})
	return cert
}

// onlyJob returns the single NVCRE Job in the namespace.
func onlyJob(ctx context.Context, t *testing.T, namespace string) *nvcrev1alpha1.Job {
	t.Helper()
	list := &nvcrev1alpha1.JobList{}
	require.NoError(t, c.List(ctx, list, client.InNamespace(namespace)))
	require.Len(t, list.Items, 1, "expected one NVCRE Job in %s", namespace)
	return &list.Items[0]
}

// onlyBandwidthMeasurement waits for the namespace's single BandwidthMeasurement
// to reach Complete and returns it.
func onlyBandwidthMeasurement(ctx context.Context, t *testing.T, namespace string) *nvcrev1alpha1.BandwidthMeasurement {
	t.Helper()
	var bwm *nvcrev1alpha1.BandwidthMeasurement
	pollUntil(t, cleanupTimeout, pollInterval, func() bool {
		list := &nvcrev1alpha1.BandwidthMeasurementList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil || len(list.Items) != 1 {
			return false
		}
		bwm = &list.Items[0]
		return meta.FindStatusCondition(bwm.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete) != nil
	}, func() string { return fmt.Sprintf("no completed BandwidthMeasurement in %s", namespace) })
	return bwm
}

// requireCondition asserts a condition is True with the given reason.
func requireCondition(t *testing.T, conds []metav1.Condition, condType, reason string) {
	t.Helper()
	cond := meta.FindStatusCondition(conds, condType)
	require.NotNil(t, cond, "condition %s missing; have %s", condType, describeConditions(conds))
	require.Equal(t, metav1.ConditionTrue, cond.Status, "condition %s: %s", condType, cond.Message)
	require.Equal(t, reason, cond.Reason, "condition %s reason; message: %s", condType, cond.Message)
}

func describeConditions(conds []metav1.Condition) string {
	parts := make([]string, 0, len(conds))
	for _, cond := range conds {
		parts = append(parts, fmt.Sprintf("%s=%s(%s)", cond.Type, cond.Status, cond.Reason))
	}
	return strings.Join(parts, ", ")
}

// claimPods returns the namespace's pods that hold a ResourceClaim: on GB200
// and GB300 these are the workload pods using the ComputeDomain's channel.
func claimPods(ctx context.Context, t *testing.T, namespace string) []corev1.Pod {
	t.Helper()
	list := &corev1.PodList{}
	require.NoError(t, c.List(ctx, list, client.InNamespace(namespace)))
	var pods []corev1.Pod
	for _, pod := range list.Items {
		if len(pod.Spec.ResourceClaims) > 0 {
			pods = append(pods, pod)
		}
	}
	return pods
}

func podTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func anyContainerRunning(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil {
			return true
		}
	}
	return false
}

// computeDomains lists the namespace's ComputeDomains.
func computeDomains(ctx context.Context, t *testing.T, namespace string) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(computeDomainGVK)
	require.NoError(t, c.List(ctx, list, client.InNamespace(namespace)))
	return list.Items
}

func computeDomainReady(cd *unstructured.Unstructured) bool {
	status, _, _ := unstructured.NestedString(cd.Object, "status", "status")
	return status == "Ready"
}

// requireNoDRALeftovers waits until the namespace holds no ResourceClaims,
// ResourceClaimTemplates or ComputeDomains.
func requireNoDRALeftovers(ctx context.Context, t *testing.T, namespace string) {
	t.Helper()
	var remaining string
	pollUntil(t, cleanupTimeout, pollInterval, func() bool {
		claims := &resourcev1.ResourceClaimList{}
		templates := &resourcev1.ResourceClaimTemplateList{}
		if c.List(ctx, claims, client.InNamespace(namespace)) != nil ||
			c.List(ctx, templates, client.InNamespace(namespace)) != nil {
			return false
		}
		cds := computeDomains(ctx, t, namespace)
		remaining = fmt.Sprintf("%d ResourceClaims, %d ResourceClaimTemplates, %d ComputeDomains",
			len(claims.Items), len(templates.Items), len(cds))
		return len(claims.Items) == 0 && len(templates.Items) == 0 && len(cds) == 0
	}, func() string { return fmt.Sprintf("DRA objects left in %s: %s", namespace, remaining) })
	t.Logf("no DRA objects left in %s (%s)", namespace, remaining)
}

// controllerLogs returns the NVCRE manager's log lines since the given time.
func controllerLogs(ctx context.Context, t *testing.T, since time.Time) string {
	t.Helper()
	pods := &corev1.PodList{}
	require.NoError(t, c.List(ctx, pods, client.InNamespace(nvcreNamespace),
		client.MatchingLabels{"control-plane": "manager"}))
	var out bytes.Buffer
	sinceTime := metav1.NewTime(since)
	for _, pod := range pods.Items {
		stream, err := clientset.CoreV1().Pods(nvcreNamespace).GetLogs(pod.Name,
			&corev1.PodLogOptions{SinceTime: &sinceTime}).Stream(ctx)
		require.NoError(t, err)
		_, err = io.Copy(&out, stream)
		_ = stream.Close()
		require.NoError(t, err)
	}
	return out.String()
}

// nvcrectl runs bin/nvcrectl against the lane's context and returns stdout.
func nvcrectl(ctx context.Context, t *testing.T, args ...string) []byte {
	t.Helper()
	bin := os.Getenv("NVCRECTL")
	if bin == "" {
		bin = filepath.Join(util.ProjectDir(t), "bin", "nvcrectl")
	}
	full := append([]string{"--context", kubeContext}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "nvcrectl %s: %s", strings.Join(full, " "), stderr.String())
	return out
}

// replayedRows counts the result rows in the log the stub replays, so the
// expected measurement count follows the fixture rather than a constant.
func replayedRows(t *testing.T) int {
	t.Helper()
	f, err := os.Open(filepath.Join("stub", "logs", "allreduce.log"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	row := regexp.MustCompile(`^\s+[0-9]+\s+[0-9]+\s+\S+\s+\S+`)
	n := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if row.MatchString(scanner.Text()) {
			n++
		}
	}
	require.NoError(t, scanner.Err())
	require.Positive(t, n, "no result rows in the stub log")
	return n
}
