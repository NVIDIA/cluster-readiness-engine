// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	helmReleaseName    = "cluster-readiness-engine"
	helmChartOCI       = "oci://ghcr.io/nvidia/cluster-readiness-engine"
	ghcrRegistryUser   = "token"
	helmInstallTimeout = 5 * time.Minute

	trainerReleaseName  = "kubeflow-trainer"
	trainerHelmChartOCI = "oci://ghcr.io/kubeflow/charts/kubeflow-trainer"
	trainerNamespace    = "kubeflow-system"
)

// isReleaseBuild returns true if version is a clean semver release tag
// (e.g. "1.2.3" or "v1.2.3") with no pre-release or dirty suffix.
func isReleaseBuild(v string) bool {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, "-+") || strings.IndexFunc(p, func(r rune) bool {
			return r < '0' || r > '9'
		}) >= 0 {
			return false
		}
	}
	return true
}

// helmChartVersion normalises a version string for use as a Helm chart version.
func helmChartVersion(ver string) string {
	return strings.TrimSpace(ver)
}

// resolveHelmChartVersion returns the chart version to pull. Release builds
// default to the CLI version; dev builds require --version.
func resolveHelmChartVersion(version, versionOverride string) (string, error) {
	if versionOverride != "" {
		return helmChartVersion(versionOverride), nil
	}
	if isReleaseBuild(version) {
		return helmChartVersion(version), nil
	}
	return "", fmt.Errorf(
		"dev build %q has no published chart; pass --version <chart-version>", version)
}

// ensureHelm checks that helm is available in PATH.
func ensureHelm() (string, error) {
	path, err := exec.LookPath("helm")
	if err != nil {
		return "", fmt.Errorf("helm not found in PATH: install helm and try again")
	}
	return path, nil
}

type helmInstallParams struct {
	version         string
	kubeconfig      string
	kubeContext     string
	versionOverride string
	registryToken   string
	image           string
	pullSecretName  string
	out             io.Writer
}

// installHelmRelease installs or upgrades CRE via the helm CLI.
func installHelmRelease(p helmInstallParams) error {
	helmPath, err := ensureHelm()
	if err != nil {
		return err
	}

	chartVersion, err := resolveHelmChartVersion(p.version, p.versionOverride)
	if err != nil {
		return err
	}

	if p.registryToken != "" {
		if err := helmRegistryLogin(helmPath, defaultImageRegistry, p.registryToken, p.out); err != nil {
			return err
		}
		defer helmRegistryLogout(helmPath, defaultImageRegistry, p.out)
	}

	imageName, imageTag := parseImage(p.image)
	args := []string{
		"upgrade", "--install", helmReleaseName, helmChartOCI,
		"--namespace", creNamespace,
		"--create-namespace",
		"--version", chartVersion,
		"--set", "manager.image.repository=" + imageName,
		"--set", "manager.image.tag=" + imageTag,
		"--wait",
		"--timeout", helmInstallTimeout.String(),
	}
	if p.pullSecretName != "" {
		args = append(args, "--set", "manager.imagePullSecrets[0].name="+p.pullSecretName)
	}
	args = appendKubeconfigArgs(args, p.kubeconfig, p.kubeContext)

	_, _ = fmt.Fprintf(p.out,
		"[helm] Installing CRE Helm release %q in namespace %s...\n",
		helmReleaseName, creNamespace)
	return runHelm(helmPath, args, p.out)
}

type helmUninstallParams struct {
	kubeconfig  string
	kubeContext string
	out         io.Writer
}

// uninstallHelmRelease removes the CRE Helm release.
func uninstallHelmRelease(p helmUninstallParams) error {
	helmPath, err := ensureHelm()
	if err != nil {
		return err
	}

	args := []string{
		"uninstall", helmReleaseName,
		"--namespace", creNamespace,
		"--ignore-not-found",
		"--wait",
		"--timeout", helmInstallTimeout.String(),
	}
	args = appendKubeconfigArgs(args, p.kubeconfig, p.kubeContext)

	_, _ = fmt.Fprintf(p.out,
		"[helm] Removing CRE Helm release %q from namespace %s...\n",
		helmReleaseName, creNamespace)
	return runHelm(helmPath, args, p.out)
}

// installTrainerHelmRelease installs Kubeflow Trainer via the helm CLI.
// The helm CLI resolves OCI sub-chart dependencies (including JobSet) automatically.
func installTrainerHelmRelease(kubeconfig, kubeContext string, out io.Writer) error {
	helmPath, err := ensureHelm()
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "[deps] Installing Kubeflow Trainer Helm release %q in namespace %s...\n",
		trainerReleaseName, trainerNamespace)
	args := []string{
		"upgrade", "--install", trainerReleaseName, trainerHelmChartOCI,
		"--namespace", trainerNamespace,
		"--create-namespace",
		"--version", strings.TrimPrefix(kubeflowTrainerVersion, "v"),
		"--set", "manager.tolerations[0].operator=Exists",
		"--set", "jobset.controller.tolerations[0].operator=Exists",
		"--wait",
		"--timeout", helmInstallTimeout.String(),
	}
	args = appendKubeconfigArgs(args, kubeconfig, kubeContext)
	return runHelm(helmPath, args, out)
}

// uninstallTrainerHelmRelease removes the Kubeflow Trainer Helm release.
func uninstallTrainerHelmRelease(kubeconfig, kubeContext string, out io.Writer) error {
	helmPath, err := ensureHelm()
	if err != nil {
		return err
	}

	args := []string{
		"uninstall", trainerReleaseName,
		"--namespace", trainerNamespace,
		"--ignore-not-found",
		"--wait",
		"--timeout", helmInstallTimeout.String(),
	}
	args = appendKubeconfigArgs(args, kubeconfig, kubeContext)
	_, _ = fmt.Fprintf(out, "[deps] Removing Helm release %q from namespace %s...\n", trainerReleaseName, trainerNamespace)
	if err := runHelm(helmPath, args, out); err != nil {
		_, _ = fmt.Fprintf(out, "[deps] Warning: failed to uninstall %s: %v\n", trainerReleaseName, err)
	}
	return nil
}

// runHelm executes a helm subcommand, printing output only on failure.
func runHelm(helmPath string, args []string, out io.Writer) error {
	var buf bytes.Buffer
	cmd := exec.Command(helmPath, args...) // #nosec G204 -- helmPath and args come from this CLI, not from untrusted input
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		_, _ = io.Copy(out, &buf)
		return fmt.Errorf("helm %s: %w", args[0], err)
	}
	return nil
}

// helmRegistryLogin logs in to an OCI registry.
func helmRegistryLogin(helmPath, registry, password string, out io.Writer) error {
	return runHelm(helmPath, []string{
		"registry", "login", registry,
		"--username", ghcrRegistryUser,
		"--password", password,
	}, out)
}

func helmRegistryLogout(helmPath, registry string, out io.Writer) {
	_ = runHelm(helmPath, []string{"registry", "logout", registry}, out)
}

// appendKubeconfigArgs appends --kubeconfig and --kube-context flags if set.
func appendKubeconfigArgs(args []string, kubeconfig, kubeContext string) []string {
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	return args
}
