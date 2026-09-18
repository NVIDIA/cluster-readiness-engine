//go:build uat

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:goconst // Lifecycle fixtures intentionally repeat CLI flags and Kubernetes resource identities.
package setupuat

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	trainerChart = "oci://ghcr.io/kubeflow/charts/kubeflow-trainer"
	jobSetChart  = "oci://registry.k8s.io/jobset/charts/jobset"
)

func TestSetupJobSetLifecycle(t *testing.T) {
	nvcrectl := requiredEnv(t, "NVCRECTL")
	kubeconfig := requiredEnv(t, "KUBECONFIG")
	contextName := requiredEnv(t, "KUBE_CONTEXT")
	cli := func(t *testing.T, args ...string) string {
		t.Helper()
		base := make([]string, 0, len(args)+6)
		base = append(base, "setup")
		base = append(base, args...)
		base = append(base, "--kubeconfig", kubeconfig, "--context", contextName, "--auto-approve")
		return run(t, nvcrectl, base...)
	}

	t.Run("bundled-upgrade-retains-crd", func(t *testing.T) {
		run(t, "helm", "upgrade", "--install", "kubeflow-trainer", trainerChart,
			"--version", "2.1.0", "--namespace", "kubeflow-system", "--create-namespace", "--wait", "--timeout", "5m",
			"--kubeconfig", kubeconfig, "--kube-context", contextName)
		waitDeployment(t, kubeconfig, contextName, "kubeflow-system", "jobset-controller")
		uid := crdUID(t, kubeconfig, contextName)
		ownership := objectOwnership(t, kubeconfig, contextName, "crd", "jobsets.jobset.x-k8s.io", "")
		if len(ownership) != 0 {
			t.Fatalf("seed JobSet CRD must have no Helm ownership metadata: %v", ownership)
		}
		assertNoJobSets(t, kubeconfig, contextName)
		output := cli(t, "init", "--skip-phases=helm")
		if !strings.Contains(output, "[helm] Skipped.") {
			t.Fatalf("init did not execute the requested phase boundary:\n%s", output)
		}
		waitDeployment(t, kubeconfig, contextName, "kubeflow-system", "jobset-controller")
		if got := crdUID(t, kubeconfig, contextName); got != uid {
			t.Fatalf("JobSet CRD UID changed during bundled upgrade: %s -> %s", uid, got)
		}
		assertRelease(t, kubeconfig, contextName, "kubeflow-trainer", "kubeflow-system", "2.2.1")
	})

	t.Run("bundled-reset-init-retains-crd", func(t *testing.T) {
		assertNoJobSets(t, kubeconfig, contextName)
		uid := crdUID(t, kubeconfig, contextName)
		output := cli(t, "reset", "--skip-phases=cr,helm")
		if !strings.Contains(output, "JobSet CRD") || strings.Contains(output, "kubectl delete crd jobsets.") {
			t.Fatalf("reset retained-resource diagnostic is unsafe:\n%s", output)
		}
		assertNotFound(t, kubeconfig, contextName, "deployment", "jobset-controller", "kubeflow-system")
		if got := crdUID(t, kubeconfig, contextName); got != uid {
			t.Fatalf("JobSet CRD UID changed during reset: %s -> %s", uid, got)
		}
		cli(t, "init", "--skip-phases=helm")
		waitDeployment(t, kubeconfig, contextName, "kubeflow-system", "jobset-controller")
		if got := crdUID(t, kubeconfig, contextName); got != uid {
			t.Fatalf("JobSet CRD UID changed during reset/init: %s -> %s", uid, got)
		}
	})

	t.Run("external-controller-survives-init-reset", func(t *testing.T) {
		cli(t, "reset", "--skip-phases=cr,helm")
		postRenderer := filepath.Join(t.TempDir(), "rename-webhooks.sh")
		script := "#!/bin/sh\nsed " +
			"-e 's/name: jobset-mutating-webhook-configuration/name: external-jobset-mutating-webhook-configuration/' " +
			"-e 's/name: jobset-validating-webhook-configuration/name: external-jobset-validating-webhook-configuration/'\n"
		if err := os.WriteFile(postRenderer, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		rendered := runStdout(t, "helm", "template", "external-jobset", jobSetChart, "--version", "0.10.1",
			"--namespace", "external-jobset-system", "--set", "fullnameOverride=external-jobset",
			"--post-renderer", postRenderer)
		bundled := runStdout(t, "helm", "template", "kubeflow-trainer", trainerChart, "--version", "2.2.1",
			"--namespace", "kubeflow-system", "--set", "jobset.install=true")
		assertDisjointRenderedIdentities(t, bundled, rendered)
		run(t, "helm", "upgrade", "--install", "external-jobset", jobSetChart,
			"--version", "0.10.1", "--namespace", "external-jobset-system", "--create-namespace",
			"--set", "fullnameOverride=external-jobset", "--post-renderer", postRenderer, "--wait", "--timeout", "5m",
			"--kubeconfig", kubeconfig, "--kube-context", contextName)
		waitDeployment(t, kubeconfig, contextName, "external-jobset-system", "external-jobset-controller")
		crdBefore := crdUID(t, kubeconfig, contextName)
		assertNoJobSets(t, kubeconfig, contextName)
		crdOwnership := objectOwnership(t, kubeconfig, contextName, "crd", "jobsets.jobset.x-k8s.io", "")
		controllerOwnership := objectOwnership(t, kubeconfig, contextName,
			"deployment", "external-jobset-controller", "external-jobset-system")
		releaseRevision := assertRelease(t, kubeconfig, contextName, "external-jobset", "external-jobset-system", "0.10.1")
		assertExternalPreserved := func(t *testing.T, crdPresent bool) {
			t.Helper()
			got := assertRelease(t, kubeconfig, contextName, "external-jobset", "external-jobset-system", "0.10.1")
			if got != releaseRevision {
				t.Fatalf("external release revision changed: %d -> %d", releaseRevision, got)
			}
			assertOwnershipUnchanged(t, kubeconfig, contextName,
				"deployment", "external-jobset-controller", "external-jobset-system", controllerOwnership)
			if crdPresent {
				assertOwnershipUnchanged(t, kubeconfig, contextName, "crd", "jobsets.jobset.x-k8s.io", "", crdOwnership)
			}
		}
		controllerBefore := objectUID(t, kubeconfig, contextName,
			"deployment", "external-jobset-controller", "external-jobset-system")
		clusterUIDs := captureExternalClusterUIDs(t, kubeconfig, contextName)
		assertHelmOwner(t, kubeconfig, contextName, "deployment", "external-jobset-controller", "external-jobset-system")
		cli(t, "init", "--skip-phases=helm")
		assertRelease(t, kubeconfig, contextName, "kubeflow-trainer", "kubeflow-system", "2.2.1")
		assertJobSetSubchartDisabled(t, kubeconfig, contextName)
		assertExternalPreserved(t, true)
		assertNotFound(t, kubeconfig, contextName, "deployment", "jobset-controller", "kubeflow-system")
		assertUIDs(t, kubeconfig, contextName, crdBefore, controllerBefore)
		assertExternalClusterUIDs(t, kubeconfig, contextName, clusterUIDs)
		waitDeployment(t, kubeconfig, contextName, "external-jobset-system", "external-jobset-controller")
		cli(t, "reset", "--skip-phases=cr,helm")
		assertExternalPreserved(t, true)
		assertUIDs(t, kubeconfig, contextName, crdBefore, controllerBefore)
		assertExternalClusterUIDs(t, kubeconfig, contextName, clusterUIDs)
		waitDeployment(t, kubeconfig, contextName, "external-jobset-system", "external-jobset-controller")

		run(t, "kubectl", "--kubeconfig", kubeconfig, "--context", contextName, "delete", "crd", "jobsets.jobset.x-k8s.io")
		cmd := exec.Command(nvcrectl, "setup", "init", "--skip-phases=helm", "--kubeconfig", kubeconfig,
			"--context", contextName, "--auto-approve")
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "JobSet-related resources remain") {
			t.Fatalf("missing-CRD external-controller case did not refuse with fingerprint evidence (err=%v):\n%s", err, output)
		}
		for _, fingerprint := range []string{
			"ClusterRole/external-jobset-controller",
			"MutatingWebhookConfiguration/external-jobset-mutating-webhook-configuration",
			"ValidatingWebhookConfiguration/external-jobset-validating-webhook-configuration",
		} {
			if !strings.Contains(string(output), fingerprint) {
				t.Fatalf("refusal omits detected fingerprint %s: %s", fingerprint, output)
			}
		}
		assertNotFound(t, kubeconfig, contextName, "deployment", "jobset-controller", "kubeflow-system")
		assertExternalPreserved(t, false)
		assertNotFound(t, kubeconfig, contextName, "crd", "jobsets.jobset.x-k8s.io", "")
		releases := runStdout(t, "helm", "--kubeconfig", kubeconfig, "--kube-context", contextName,
			"list", "--all", "--namespace", "kubeflow-system", "--filter", "^kubeflow-trainer$", "--output", "json")
		var listedReleases []json.RawMessage
		if err := json.Unmarshal([]byte(releases), &listedReleases); err != nil {
			t.Fatalf("cannot parse Helm release list after refusal: %v: %s", err, releases)
		}
		if len(listedReleases) != 0 {
			t.Fatalf("Trainer release was unexpectedly created after refusal: %s", releases)
		}
		got := objectUID(t, kubeconfig, contextName,
			"deployment", "external-jobset-controller", "external-jobset-system")
		if got != controllerBefore {
			t.Fatalf("external controller UID changed after refusal: %s -> %s", controllerBefore, got)
		}
		assertExternalClusterUIDs(t, kubeconfig, contextName, clusterUIDs)
	})
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	return runCommand(t, false, name, args...)
}

func runStdout(t *testing.T, name string, args ...string) string {
	t.Helper()
	return runCommand(t, true, name, args...)
}

func runCommand(t *testing.T, stdoutOnly bool, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s failed: %v\n%s\n%s", name, strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	t.Logf("%s %s\n%s\n%s", name, strings.Join(args, " "), stdout.String(), stderr.String())
	if stdoutOnly {
		return stdout.String()
	}
	return stdout.String() + stderr.String()
}

func waitDeployment(t *testing.T, kubeconfig, contextName, namespace, name string) {
	t.Helper()
	run(t, "kubectl", "--kubeconfig", kubeconfig, "--context", contextName, "--namespace", namespace,
		"rollout", "status", "deployment/"+name, "--timeout", (5 * time.Minute).String())
}

func crdUID(t *testing.T, kubeconfig, contextName string) string {
	t.Helper()
	return objectUID(t, kubeconfig, contextName, "crd", "jobsets.jobset.x-k8s.io", "")
}

func objectUID(t *testing.T, kubeconfig, contextName, kind, name, namespace string) string {
	t.Helper()
	args := []string{"--kubeconfig", kubeconfig, "--context", contextName}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind, name, "-o", "jsonpath={.metadata.uid}")
	uid := strings.TrimSpace(run(t, "kubectl", args...))
	if uid == "" {
		t.Fatalf("%s %s has no UID", kind, name)
	}
	return uid
}

func assertUIDs(t *testing.T, kubeconfig, contextName, crdUIDWant, controllerUIDWant string) {
	t.Helper()
	if got := crdUID(t, kubeconfig, contextName); got != crdUIDWant {
		t.Fatalf("external JobSet CRD UID changed: %s -> %s", crdUIDWant, got)
	}
	got := objectUID(t, kubeconfig, contextName,
		"deployment", "external-jobset-controller", "external-jobset-system")
	if got != controllerUIDWant {
		t.Fatalf("external JobSet controller UID changed: %s -> %s", controllerUIDWant, got)
	}
}

func assertNotFound(t *testing.T, kubeconfig, contextName, kind, name, namespace string) {
	t.Helper()
	args := []string{"--kubeconfig", kubeconfig, "--context", contextName}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind, name)
	cmd := exec.Command("kubectl", args...)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(strings.ToLower(string(output)), "not found") {
		t.Fatalf("expected %s %s to be absent (err=%v): %s", kind, name, err, output)
	}
}

func captureExternalClusterUIDs(t *testing.T, kubeconfig, contextName string) map[string]string {
	t.Helper()
	objects := map[string]string{
		"clusterrole":                    "external-jobset-controller",
		"clusterrolebinding":             "external-jobset-controller",
		"mutatingwebhookconfiguration":   "external-jobset-mutating-webhook-configuration",
		"validatingwebhookconfiguration": "external-jobset-validating-webhook-configuration",
	}
	uids := map[string]string{}
	for kind, name := range objects {
		uids[kind+"/"+name] = objectUID(t, kubeconfig, contextName, kind, name, "")
		assertHelmOwner(t, kubeconfig, contextName, kind, name, "")
	}
	return uids
}

func assertExternalClusterUIDs(t *testing.T, kubeconfig, contextName string, want map[string]string) {
	t.Helper()
	for identity, uid := range want {
		kind, name, _ := strings.Cut(identity, "/")
		if got := objectUID(t, kubeconfig, contextName, kind, name, ""); got != uid {
			t.Fatalf("external %s UID changed: %s -> %s", identity, uid, got)
		}
		assertHelmOwner(t, kubeconfig, contextName, kind, name, "")
	}
}

func assertHelmOwner(t *testing.T, kubeconfig, contextName, kind, name, namespace string) {
	t.Helper()
	want := map[string]string{
		"app.kubernetes.io/managed-by":   "Helm",
		"meta.helm.sh/release-name":      "external-jobset",
		"meta.helm.sh/release-namespace": "external-jobset-system",
	}
	assertOwnershipUnchanged(t, kubeconfig, contextName, kind, name, namespace, want)
}

func objectOwnership(t *testing.T, kubeconfig, contextName, kind, name, namespace string) map[string]string {
	t.Helper()
	args := []string{"--kubeconfig", kubeconfig, "--context", contextName}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind, name, "-o", "json")
	var object unstructured.Unstructured
	if err := json.Unmarshal([]byte(runStdout(t, "kubectl", args...)), &object); err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	if value, exists := object.GetLabels()["app.kubernetes.io/managed-by"]; exists {
		result["app.kubernetes.io/managed-by"] = value
	}
	for _, key := range []string{"meta.helm.sh/release-name", "meta.helm.sh/release-namespace"} {
		if value, exists := object.GetAnnotations()[key]; exists {
			result[key] = value
		}
	}
	return result
}

func assertOwnershipUnchanged(
	t *testing.T, kubeconfig, contextName, kind, name, namespace string, want map[string]string,
) {
	t.Helper()
	if got := objectOwnership(t, kubeconfig, contextName, kind, name, namespace); !reflect.DeepEqual(got, want) {
		t.Fatalf("Helm ownership changed for %s/%s: want %v, got %v", kind, name, want, got)
	}
}

func assertNoJobSets(t *testing.T, kubeconfig, contextName string) {
	t.Helper()
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	raw := runStdout(t, "kubectl", "--kubeconfig", kubeconfig, "--context", contextName,
		"get", "jobsets.jobset.x-k8s.io", "--all-namespaces", "-o", "json")
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("scenario requires zero JobSets cluster-wide: %s", raw)
	}
}

func assertRelease(t *testing.T, kubeconfig, contextName, name, namespace, chartVersion string) int {
	t.Helper()
	var status struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status string `json:"status"`
		} `json:"info"`
	}
	raw := runStdout(t, "helm", "--kubeconfig", kubeconfig, "--kube-context", contextName,
		"status", name, "--namespace", namespace, "--output", "json")
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Version string `json:"version"`
	}
	metadataRaw := runStdout(t, "helm", "--kubeconfig", kubeconfig, "--kube-context", contextName,
		"get", "metadata", name, "--namespace", namespace, "--output", "json")
	if err := json.Unmarshal([]byte(metadataRaw), &metadata); err != nil {
		t.Fatal(err)
	}
	if status.Name != name || status.Namespace != namespace || status.Info.Status != "deployed" ||
		metadata.Version != chartVersion || status.Version < 1 {
		t.Fatalf("expected deployed %s/%s at chart %s, got: %s", namespace, name, chartVersion, raw)
	}
	return status.Version
}

func assertJobSetSubchartDisabled(t *testing.T, kubeconfig, contextName string) {
	t.Helper()
	var values struct {
		JobSet struct {
			Install *bool `json:"install"`
		} `json:"jobset"`
	}
	raw := runStdout(t, "helm", "--kubeconfig", kubeconfig, "--kube-context", contextName,
		"get", "values", "kubeflow-trainer", "--namespace", "kubeflow-system", "--all", "--output", "json")
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatal(err)
	}
	if values.JobSet.Install == nil || *values.JobSet.Install {
		t.Fatalf("Trainer must explicitly disable the JobSet subchart: %s", raw)
	}
}

func assertDisjointRenderedIdentities(t *testing.T, bundled, external string) {
	t.Helper()
	identities := func(raw string) map[string]bool {
		result := map[string]bool{}
		decoder := utilyaml.NewYAMLToJSONDecoder(strings.NewReader(raw))
		for {
			var object unstructured.Unstructured
			if err := decoder.Decode(&object); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if len(object.Object) == 0 {
				continue
			}
			// Shared CRDs are deliberately retained/reused, not templated release resources.
			if object.GetKind() == "CustomResourceDefinition" {
				continue
			}
			gv, err := schema.ParseGroupVersion(object.GetAPIVersion())
			if err != nil || object.GetKind() == "" || object.GetName() == "" {
				t.Fatalf("invalid rendered resource identity: %v", object.Object)
			}
			// Compare even across namespaces: stricter than Kubernetes identity, so
			// no fixture can accidentally rely only on a different release namespace.
			result[gv.Group+"/"+object.GetKind()+"/"+object.GetName()] = true
		}
		if len(result) == 0 {
			t.Fatal("rendered chart contains no release resources")
		}
		return result
	}
	base := identities(bundled)
	for identity := range identities(external) {
		if base[identity] {
			t.Fatalf("external rendered resource collides with bundled chart: %s", identity)
		}
	}
}
