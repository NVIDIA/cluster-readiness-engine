// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// testDefaultImage is the image a test run uses when neither --image nor its
// values choose one: the CLI-version image.
const testDefaultImage = testDefaultRepository + ":v0.8.0"

// testDefaultRepository is the published controller image repository.
const testDefaultRepository = defaultImageRegistry + "/" + defaultImageRepository

// helmReleaseNotFound is a fake helm script line that answers like Helm for a
// release it has no record of.
const helmReleaseNotFound = "printf 'Error: release: not found\\n' >&2; exit 1"

// valuesCaseOptions are the flags of a chart-values test case that decide
// nvcrectl's own values.
type valuesCaseOptions struct {
	Reset      bool   `json:"reset"`
	Image      string `json:"image"`
	PullSecret bool   `json:"pullSecret"`
	RBACScope  string `json:"rbacScope"`
}

func (o valuesCaseOptions) valuesOptions() valuesOptions {
	return valuesOptions{
		reset:              o.Reset,
		imageFlag:          o.Image,
		defaultImage:       testDefaultImage,
		defaultImageSource: sourceCLIVersion,
		pullSecret:         o.PullSecret,
		rbacScope:          o.RBACScope,
	}
}

// valuesCaseResult is the structured output of a chart-values test case.
type valuesCaseResult struct {
	Error    string          `json:"error,omitempty"`
	Values   map[string]any  `json:"values,omitempty"`
	Image    string          `json:"image,omitempty"`
	Summary  []string        `json:"summary,omitempty"`
	Rendered *renderedFields `json:"rendered,omitempty"`
}

func summaryLines(out string) []string {
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}

// assertNoValuesPrinted fails when the summary prints a value other than the
// image, the pull-secret names and rbac.scope: the summary reaches CI logs,
// and a value can hold a credential (ADR-098 decision 6).
func assertNoValuesPrinted(merged map[string]any, summary string) error {
	for _, path := range leafPaths(merged) {
		if hasPrefix(path, pathImageRepository) || hasPrefix(path, pathImageTag) ||
			hasPrefix(path, pathImageDigest) || hasPrefix(path, pathImagePullSecrets) ||
			hasPrefix(path, pathRBACScope) {
			continue
		}
		v, _ := valueAt(merged, path...)
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		for _, s := range []string{string(data), strings.Trim(string(data), `"`)} {
			if len(s) >= 4 && strings.Contains(summary, s) {
				return fmt.Errorf("the summary prints the value of %s: %s", strings.Join(path, "."), s)
			}
		}
	}
	return nil
}

// TestResolveValues drives ADR-098 decisions 2 to 6 without Helm: the kept
// values taken from the installed release, the conflict checks, the merge of
// the kept values under P, nvcrectl's own values, and the summary. operator
// is the probe of this run's flags alone (O); probed is the probe of the kept
// file followed by them (P), as Helm reads it. A case that leaves probed out
// gets what prepareNVCREValues would use: the kept values when there are no
// flags, O when nothing is kept.
func TestResolveValues(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "chart-values-merge", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Release  json.RawMessage   `json:"release"`
			Operator json.RawMessage   `json:"operator"`
			Probed   json.RawMessage   `json:"probed"`
			Options  valuesCaseOptions `json:"options"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		release, err := decodeValues(in.Release)
		if err != nil {
			return fmt.Errorf("release: %w", err)
		}
		operator, err := decodeValues(in.Operator)
		if err != nil {
			return fmt.Errorf("operator: %w", err)
		}
		probed, err := decodeValues(in.Probed)
		if err != nil {
			return fmt.Errorf("probed: %w", err)
		}

		kept := keptValues(release, in.Options.Reset)
		if in.Probed == nil {
			switch {
			case operator == nil:
				probed = kept
			case len(kept) == 0:
				probed = operator
			default:
				return errors.New("a case with kept values and operator values must give probed, as Helm reads it")
			}
		}

		var result valuesCaseResult
		resolved, err := resolveValues(kept, operator, probed, in.Options.valuesOptions())
		if err != nil {
			result.Error = err.Error()
		} else {
			var out bytes.Buffer
			printValuesSummary(&out, resolved)
			if err := assertNoValuesPrinted(resolved.merged, out.String()); err != nil {
				return err
			}
			result.Values = resolved.merged
			result.Image = resolved.image.String()
			result.Summary = summaryLines(out.String())
		}
		b, err := marshalValues(result)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// requireHelm returns the helm binary. CI installs Helm for the test job, so
// a missing binary fails there rather than skipping the probe tests silently;
// a local run without Helm skips them. It mirrors test/helm's requireHelm.
func requireHelm(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI but not on PATH; the test job must install helm (azure/setup-helm)")
		}
		t.Skip("helm not available; skipping the values probe tests")
	}
	return path
}

// chartPath is the in-repo NVCRE chart.
func chartPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "helm", "cluster-readiness-engine"))
	require.NoError(t, err)
	return path
}

// writeFakeHelm writes an executable helm script into dir and returns its
// path.
func writeFakeHelm(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "helm")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return path
}

// renderedFields are the parts of the rendered chart the probe cases check.
type renderedFields struct {
	Error            string         `json:"error,omitempty"`
	Image            string         `json:"image,omitempty"`
	ImagePullSecrets []any          `json:"imagePullSecrets,omitempty"`
	Replicas         any            `json:"replicas,omitempty"`
	Resources        map[string]any `json:"resources,omitempty"`
	Tolerations      []any          `json:"tolerations,omitempty"`
	RunRoleBindings  []string       `json:"runRoleBindings,omitempty"`
}

// renderChart renders the NVCRE chart with the values file and picks out the
// controller Deployment's image, pull secrets, replicas, resources and
// tolerations, and every binding to nvcre-run-role.
func renderChart(helmPath, chart, valuesFile string) renderedFields {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(helmPath, "template", helmReleaseName, chart, // #nosec G204 -- test helper
		helmFlagNamespace, nvcreNamespace, helmFlagValues, valuesFile)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return renderedFields{Error: strings.TrimSpace(stderr.String())}
	}

	var r renderedFields
	reader := k8syaml.NewYAMLReader(bufio.NewReader(&stdout))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return renderedFields{Error: err.Error()}
		}
		var obj map[string]any
		if err := sigsyaml.Unmarshal(doc, &obj); err != nil || obj == nil {
			continue
		}
		kind, _ := obj["kind"].(string)
		name, _ := valueAt(obj, "metadata", valuesKeyName)
		switch kind {
		case testKindCRB, rbacKindRoleBinding:
			if role, _ := valueAt(obj, "roleRef", valuesKeyName); role == "nvcre-run-role" {
				ref := fmt.Sprintf("%s/%v", kind, name)
				if ns, ok := valueAt(obj, "metadata", "namespace"); ok {
					ref = fmt.Sprintf("%s %v/%v", kind, ns, name)
				}
				r.RunRoleBindings = append(r.RunRoleBindings, ref)
			}
		case kindDeployment:
			r.Replicas, _ = valueAt(obj, "spec", "replicas")
			secrets, _ := valueAt(obj, "spec", "template", "spec", "imagePullSecrets")
			r.ImagePullSecrets, _ = secrets.([]any)
			tolerations, _ := valueAt(obj, "spec", "template", "spec", "tolerations")
			r.Tolerations, _ = tolerations.([]any)
			containers, _ := valueAt(obj, "spec", "template", "spec", "containers")
			list, _ := containers.([]any)
			for _, c := range list {
				cm, _ := c.(map[string]any)
				if cm[valuesKeyName] != managerContainerName {
					continue
				}
				r.Image, _ = cm["image"].(string)
				r.Resources, _ = cm["resources"].(map[string]any)
			}
		}
	}
	sort.Strings(r.RunRoleBindings)
	return r
}

// TestPrepareNVCREValuesWithHelm runs preflight's values handling against the
// real Helm binary: the probes parse this run's flags, the merge keeps the
// installed release's values, and the merged file, rendered against the
// in-repo chart, gives the Deployment and RBAC bindings ADR-098 promises. The
// release read is faked; every other Helm call is real.
func TestPrepareNVCREValuesWithHelm(t *testing.T) {
	realHelm := requireHelm(t)
	chart := chartPath(t)

	p := testutil.TestCaseParser{Subdir: "chart-values-probe", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Release json.RawMessage   `json:"release"`
			Files   []string          `json:"files"`
			SetJSON []string          `json:"setJSON"`
			Set     []string          `json:"set"`
			Options valuesCaseOptions `json:"options"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		dir := t.TempDir()
		getValues := helmReleaseNotFound
		if in.Release != nil {
			releaseFile := filepath.Join(dir, "release.json")
			if err := os.WriteFile(releaseFile, in.Release, 0o600); err != nil {
				return err
			}
			getValues = fmt.Sprintf("cat %q; exit 0", releaseFile)
		}
		helm := writeFakeHelm(t, dir, fmt.Sprintf(
			"if [ \"$1\" = get ]; then %s; fi\nexec %q \"$@\"", getValues, realHelm))

		files := make([]string, 0, len(in.Files))
		for _, name := range in.Files {
			content, ok := tc.Inputs[name]
			if !ok {
				return fmt.Errorf("values file %s is not in the case directory", name)
			}
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				return err
			}
			files = append(files, path)
		}

		ws, err := newValuesWorkspace()
		if err != nil {
			return err
		}
		defer ws.cleanup()

		var out bytes.Buffer
		var result valuesCaseResult
		valuesFile, image, err := prepareNVCREValues(helm, "", "", ws, nvcreValuesInput{
			values: ChartValues{Files: files, SetJSON: in.SetJSON, Set: in.Set, Reset: in.Options.Reset},
			opts:   in.Options.valuesOptions(),
		}, &out)
		if err != nil {
			result.Error = err.Error()
		} else {
			data, err := os.ReadFile(valuesFile)
			if err != nil {
				return err
			}
			if result.Values, err = decodeValues(data); err != nil {
				return err
			}
			if err := assertNoValuesPrinted(result.Values, out.String()); err != nil {
				return err
			}
			result.Image = image.String()
			result.Summary = summaryLines(out.String())
			rendered := renderChart(realHelm, chart, valuesFile)
			result.Rendered = &rendered
		}
		b, err := marshalValues(result)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestChartValuesValidate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		values      ChartValues
		skipHelm    bool
		autoApprove bool
		wantErr     string
	}{
		{name: "no values"},
		{name: "no values with helm skipped", skipHelm: true},
		{name: "values file", values: ChartValues{Files: []string{"values.yaml"}}},
		{name: "values with helm skipped", values: ChartValues{Files: []string{"values.yaml"}}, skipHelm: true,
			wantErr: "--values applies to the NVCRE release, but --skip-phases skips the helm phase"},
		{name: "set with helm skipped", values: ChartValues{Set: []string{"a=b"}}, skipHelm: true,
			wantErr: "--set applies to the NVCRE release"},
		{name: "set-json with helm skipped", values: ChartValues{SetJSON: []string{"a=1"}}, skipHelm: true,
			wantErr: "--set-json applies to the NVCRE release"},
		{name: "reset-values with helm skipped", values: ChartValues{Reset: true}, skipHelm: true,
			wantErr: "--reset-values applies to the NVCRE release"},
		{name: "several with helm skipped", values: ChartValues{Set: []string{"a=b"}, Reset: true}, skipHelm: true,
			wantErr: "--set, --reset-values apply to the NVCRE release"},
		{name: "stdin with auto-approve", values: ChartValues{Files: []string{"-"}}, autoApprove: true},
		{name: "stdin without auto-approve", values: ChartValues{Files: []string{"-"}},
			wantErr: "--values - (stdin) needs --auto-approve"},
		{name: "stdin twice", values: ChartValues{Files: []string{"-", "-"}}, autoApprove: true,
			wantErr: "--values - may be given only once"},
		{name: "https URL", values: ChartValues{Files: []string{"https://example.com/values.yaml"}},
			wantErr: "download the file and pass its path"},
		{name: "http URL", values: ChartValues{Files: []string{"HTTP://example.com/values.yaml"}},
			wantErr: "URLs are not accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.values.validate(tc.skipHelm, tc.autoApprove)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestReadReleaseValues runs the read against a fake helm. Only a release
// Helm has no record of counts as a first install; any other failure stops
// init, because upgrading without the installed values would reset them.
func TestReadReleaseValues(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		want    map[string]any
		wantErr string
	}{
		{
			name:   "installed values",
			script: `printf '%s' '{"manager":{"replicas":2}}'`,
			want:   map[string]any{valuesKeyManager: map[string]any{"replicas": json.Number("2")}},
		},
		{name: "release without values", script: "printf 'null\\n'"},
		{name: "no release yet", script: helmReleaseNotFound},
		{
			name:    "cluster unreachable",
			script:  "printf 'Error: Kubernetes cluster unreachable\\n' >&2; exit 1",
			wantErr: "Kubernetes cluster unreachable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readReleaseValues(writeFakeHelm(t, t.TempDir(), tc.script), "", "")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// probeLogHelm is a fake helm whose `get values` prints kept and whose
// `template` appends the content of every -f file it is given to log, one
// "--- run" line per call, and prints an empty probe result.
func probeLogHelm(t *testing.T, dir, kept, log string) string {
	return writeFakeHelm(t, dir, fmt.Sprintf(`
if [ "$1" = get ]; then printf '%%s' '%s'; exit 0; fi
if [ "$1" = template ]; then
  echo "--- run" >> %q
  while [ $# -gt 0 ]; do
    if [ "$1" = -f ]; then shift; cat "$1" >> %q; echo >> %q; fi
    shift
  done
  printf -- '---\n# Source: nvcre-values-probe/templates/values.json\n{}\n'
  exit 0
fi
exit 1`, kept, log, log, log))
}

// TestValuesSourcesReadOnce pins that a values source is read exactly once
// and both probes see the same bytes, for a named pipe and for stdin, and
// that every file preflight writes is private and removed afterwards.
func TestValuesSourcesReadOnce(t *testing.T) {
	const content = "manager:\n  replicas: 2\n"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string) (string, io.Reader)
	}{
		{
			name: "named pipe",
			setup: func(t *testing.T, dir string) (string, io.Reader) {
				pipe := filepath.Join(dir, "values.fifo")
				require.NoError(t, syscall.Mkfifo(pipe, 0o600))
				go func() {
					// Opening blocks until the reader opens the pipe; a second
					// read would find no writer and block until the test times out.
					if f, err := os.OpenFile(pipe, os.O_WRONLY, 0); err == nil {
						_, _ = f.WriteString(content)
						_ = f.Close()
					}
				}()
				return pipe, nil
			},
		},
		{
			name: "stdin",
			setup: func(t *testing.T, dir string) (string, io.Reader) {
				return valuesStdin, strings.NewReader(content)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "probe.log")
			helm := probeLogHelm(t, dir, `{"manager":{"resources":{"limits":{"memory":"8Gi"}}}}`, log)
			source, in := tc.setup(t, dir)

			ws, err := newValuesWorkspace()
			require.NoError(t, err)
			valuesFile, _, err := prepareNVCREValues(helm, "", "", ws, nvcreValuesInput{
				values: ChartValues{Files: []string{source}},
				opts:   valuesCaseOptions{}.valuesOptions(),
				in:     in,
			}, io.Discard)
			require.NoError(t, err)

			logged, err := os.ReadFile(log)
			require.NoError(t, err)
			runs := strings.Split(strings.TrimPrefix(string(logged), "--- run\n"), "--- run\n")
			require.Len(t, runs, 2, "want the operator probe and the probe with the kept file in front")
			assert.Equal(t, content+"\n", runs[0], "the operator probe")
			assert.True(t, strings.HasSuffix(runs[1], content+"\n"), "the second probe: %q", runs[1])

			require.FileExists(t, valuesFile)
			require.NoError(t, filepath.WalkDir(ws.dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				want := fs.FileMode(0o600)
				if d.IsDir() {
					want = 0o700
				}
				assert.Equal(t, want, info.Mode().Perm(), path)
				return nil
			}))
			ws.cleanup()
			assert.NoDirExists(t, ws.dir)
		})
	}
}

// TestNVCREUpgradeArgsAddOnlyAValuesFile pins that nvcrectl's own addition to
// the upgrade is one -f: no --set or --set-json, so it needs nothing beyond
// Helm 3.8 (ADR-098).
func TestNVCREUpgradeArgsAddOnlyAValuesFile(t *testing.T) {
	args := nvcreHelmUpgradeArgs("", "0.8.0", "/tmp/values.json")
	assert.NotContains(t, args, helmFlagSet)
	assert.NotContains(t, args, helmFlagSetJSON)
	assert.Equal(t, []string{helmFlagValues, "/tmp/values.json"}, args[len(args)-2:])
}

// TestChartImagePullSecretsDefaultEmpty pins the chart's empty default list.
// The probe chart has no defaults, so a non-empty chart default would be
// dropped from the list --image-pull-secret builds without anything noticing.
func TestChartImagePullSecretsDefaultEmpty(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(chartPath(t), "values.yaml"))
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, sigsyaml.Unmarshal(data, &values))
	list, ok := valueAt(values, pathImagePullSecrets...)
	require.True(t, ok, "values.yaml has no manager.imagePullSecrets")
	require.Equal(t, []any{}, list)
}

// managerContainerName is the controller container in the chart's Deployment.
const managerContainerName = valuesKeyManager

// rbacKindRoleBinding is the kind of the chart's per-namespace run binding.
const rbacKindRoleBinding = "RoleBinding"

// preflightKept is the installed release's values the RunInit tests' fake helm
// prints.
const preflightKept = `{"manager":{"resources":{"limits":{"memory":"8Gi"}}}}`

// runInitWithFakeHelm runs RunInit against a dummy kubeconfig with a fake
// helm first on PATH. The fake logs every call's first two arguments, answers
// `get values` with preflightKept and `template` with probe, and prints nothing for
// anything else.
func runInitWithFakeHelm(
	t *testing.T, probe string, image, skipPhases string, autoApprove bool, values ChartValues, in string,
) (string, []string, error) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "helm.log")
	writeFakeHelm(t, dir, fmt.Sprintf(`
echo "$1 $2" >> %q
if [ "$1" = get ]; then printf '%%s' '%s'; exit 0; fi
if [ "$1" = template ]; then printf -- '---\n# Source: p/templates/values.json\n%%s\n' '%s'; exit 0; fi
exit 0`, log, preflightKept, probe))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")

	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfigPath, []byte(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`), 0o600))

	var out bytes.Buffer
	err := RunInitWithValues("v0.8.0", image, "", skipPhases, autoApprove, newTestConfigFlags(kubeconfigPath, ""),
		"", "", "", "", values, strings.NewReader(in), &out)
	logged, readErr := os.ReadFile(log)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		require.NoError(t, readErr)
	}
	var calls []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(logged)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return out.String(), calls, err
}

// TestRunInitValuesPreflight pins where preflight's values handling sits: a
// conflict stops init before the deps phase calls Helm, the summary is printed
// before the prompt and with --auto-approve, and nothing is read when the helm
// phase is skipped.
func TestRunInitValuesPreflight(t *testing.T) {
	t.Run("conflict stops before deps", func(t *testing.T) {
		_, calls, err := runInitWithFakeHelm(t, `{"manager":{"image":{"tag":"v1"}}}`,
			"registry.example.com/manager:v1", "", true, ChartValues{Set: []string{"manager.image.tag=v1"}}, "")
		require.ErrorContains(t, err, "--image and this run's values both set the controller image")
		assert.Equal(t, []string{"get values", "template nvcre-values-probe", "template nvcre-values-probe"}, calls)
	})

	t.Run("summary before the prompt", func(t *testing.T) {
		out, calls, err := runInitWithFakeHelm(t, `{}`, "", "", false, ChartValues{}, "no\n")
		require.NoError(t, err)
		assert.Equal(t, []string{"get values"}, calls)
		summary := strings.Index(out, "[preflight] Values for release \"nvcre\"")
		prompt := strings.Index(out, "Do you want to proceed?")
		require.GreaterOrEqual(t, summary, 0, out)
		require.Greater(t, prompt, summary, out)
		assert.Contains(t, out, "  manager.resources.limits.memory  (installed release)\n")
		assert.Contains(t, out, "[preflight] Controller image: "+testDefaultImage+" (CLI version)\n")
		assert.Contains(t, out, "image "+testDefaultImage+" (CLI version)")
		assert.NotContains(t, out, "8Gi")
		assert.Contains(t, out, "Init cancelled.")
	})

	t.Run("summary with auto-approve", func(t *testing.T) {
		out, _, err := runInitWithFakeHelm(t, `{}`, "", "deps", true, ChartValues{}, "")
		// The fake helm prints no CRDs, so the helm phase fails after preflight.
		require.ErrorContains(t, err, "no CustomResourceDefinition")
		assert.Contains(t, out, "  manager.resources.limits.memory  (installed release)\n")
		assert.NotContains(t, out, "8Gi")
	})

	t.Run("helm phase skipped reads nothing", func(t *testing.T) {
		out, calls, err := runInitWithFakeHelm(t, `{}`, "", "deps,helm", true, ChartValues{}, "")
		require.NoError(t, err)
		assert.Empty(t, calls)
		assert.NotContains(t, out, "Values for release")
	})

	t.Run("values with helm skipped", func(t *testing.T) {
		_, calls, err := runInitWithFakeHelm(t, `{}`, "", "helm", true,
			ChartValues{Set: []string{"manager.replicas=2"}}, "")
		require.ErrorContains(t, err, "--set applies to the NVCRE release")
		assert.Empty(t, calls)
	})
}

// TestPrepareNVCREValuesErrors pins that a values source that cannot be read
// and a value Helm rejects stop preflight with an error naming the cause,
// before the summary is printed or a values file is written.
func TestPrepareNVCREValuesErrors(t *testing.T) {
	realHelm := requireHelm(t)
	for _, tc := range []struct {
		name    string
		values  ChartValues
		in      io.Reader
		wantErr string
	}{
		{
			name:    "missing values file",
			values:  ChartValues{Files: []string{filepath.Join(t.TempDir(), "missing.yaml")}},
			wantErr: "read --values",
		},
		{
			name:    "stdin with no reader",
			values:  ChartValues{Files: []string{valuesStdin}},
			wantErr: "no stdin to read",
		},
		{
			name:    "values file Helm cannot parse",
			values:  ChartValues{Files: []string{"-"}},
			in:      strings.NewReader("manager: [unclosed\n"),
			wantErr: "read the chart values with helm template",
		},
		{
			name:    "set-json Helm cannot parse",
			values:  ChartValues{SetJSON: []string{"manager={bad"}},
			wantErr: "read the chart values with helm template",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			helm := writeFakeHelm(t, dir, fmt.Sprintf(
				"if [ \"$1\" = get ]; then printf 'null'; exit 0; fi\nexec %q \"$@\"", realHelm))
			ws, err := newValuesWorkspace()
			require.NoError(t, err)
			defer ws.cleanup()

			var out bytes.Buffer
			_, _, err = prepareNVCREValues(helm, "", "", ws, nvcreValuesInput{
				values: tc.values,
				opts:   valuesCaseOptions{}.valuesOptions(),
				in:     tc.in,
			}, &out)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Empty(t, out.String(), "nothing is printed before a values error")
			assert.NoFileExists(t, filepath.Join(ws.dir, "values.json"))
		})
	}
}

// TestDecodeErrors pins that malformed release values and probe output are
// errors rather than empty values, which would silently reset the release.
func TestDecodeErrors(t *testing.T) {
	_, err := readReleaseValues(writeFakeHelm(t, t.TempDir(), "printf '{not json'"), "", "")
	require.ErrorContains(t, err, "parse the installed nvcre release's values")

	_, err = decodeProbeOutput([]byte("---\n# Source: p/templates/values.json\n[1, 2]\n"))
	require.ErrorContains(t, err, "parse the values probe output")

	values, err := decodeProbeOutput([]byte("---\n# Source: p/templates/values.json\n{}\n"))
	require.NoError(t, err)
	require.Empty(t, values)
}

// imageValues is a manager.image object with a repository and a tag.
func imageValues(repository, tag string) map[string]any {
	return map[string]any{pathImageRepository[2]: repository, pathImageTag[2]: tag}
}

// TestKeptValuesPrunesWhatItEmpties pins that dropping the image leaves no
// empty manager object behind, so a release whose only values are an earlier
// run's image keeps nothing and skips the second probe, while an empty object
// the operator set is kept.
func TestKeptValuesPrunesWhatItEmpties(t *testing.T) {
	imageOnly := map[string]any{valuesKeyManager: map[string]any{valuesKeyImage: imageValues(testDefaultRepository, "v0.7.0")}}
	require.Nil(t, keptValues(imageOnly, false))
	require.Equal(t, map[string]any{valuesKeyManager: map[string]any{valuesKeyImage: imageValues(testDefaultRepository, "v0.7.0")}},
		imageOnly,
		"the release's values are not modified")

	emptyAffinity := map[string]any{valuesKeyManager: map[string]any{"affinity": map[string]any{}}}
	require.Equal(t, emptyAffinity, keptValues(emptyAffinity, false))

	require.Nil(t, keptValues(nil, false))
	require.Nil(t, keptValues(nil, true))
}

// previousSetupInitArgs are the value arguments `setup init` gave the upgrade
// before ADR-098 (nvcreHelmUpgradeArgs and releaseRBACArgs on main): the image
// as --set, the pull secret at index 0, and rbac.* carried from the installed
// release with --rbac-scope over it. Every other kept value was dropped.
func previousSetupInitArgs(release map[string]any, image string, pullSecret bool, rbacScope string) []string {
	name, tag, digest := parseImage(image)
	if digest != "" {
		tag = digest // the bug ADR-098 fixes; no compatibility case uses a digest
	}
	args := []string{
		helmFlagSet, "manager.image.repository=" + name,
		helmFlagSet, "manager.image.tag=" + tag,
	}
	if pullSecret {
		args = append(args, helmFlagSet, "manager.imagePullSecrets[0].name="+pullSecretName)
	}
	scope := stringAt(release, pathRBACScope...)
	if rbacScope != "" {
		scope = rbacScope
	}
	if scope != "" {
		args = append(args, helmFlagSet, "rbac.scope="+scope)
	}
	if namespaces, ok := valueAt(release, pathRBACRunNS...); ok {
		data, _ := json.Marshal(namespaces)
		args = append(args, helmFlagSetJSON, "rbac.runNamespaces="+string(data))
	}
	return args
}

// TestRenderMatchesPreviousSetupInit pins backward compatibility: for every
// run the previous `setup init` supported, the values file ADR-098 builds
// renders the chart byte for byte as the previous --set arguments did. The
// one intended difference is pinned too: a pull-secret binding a previous run
// stored is kept by a later run without --image-pull-secret, where the
// previous `setup init` dropped it.
func TestRenderMatchesPreviousSetupInit(t *testing.T) {
	realHelm := requireHelm(t)
	chart := chartPath(t)
	// A release a previous `setup init --image-pull-secret --rbac-scope
	// namespaced` stored, with run namespaces set through Helm.
	previousRelease := map[string]any{
		valuesKeyManager: map[string]any{
			valuesKeyImage:     imageValues(testDefaultRepository, "v0.7.0"),
			"imagePullSecrets": []any{map[string]any{valuesKeyName: pullSecretName}},
		},
		"rbac": map[string]any{"scope": rbacScopeNamespaced, "runNamespaces": []any{runNS, "team-b"}},
	}

	for _, tc := range []struct {
		name       string
		release    map[string]any
		image      string
		pullSecret bool
		rbacScope  string
		// previousPullSecret renders the previous arguments as if
		// --image-pull-secret had been given: the pinned difference.
		previousPullSecret bool
	}{
		{name: "first install"},
		{name: "first install with --image", image: "registry.example.com/mirror/manager:v0.8.0"},
		{name: "first install with --image-pull-secret", pullSecret: true},
		{name: "first install with --rbac-scope", rbacScope: rbacScopeNamespaced},
		{name: "re-run with --image-pull-secret", release: previousRelease, pullSecret: true},
		{name: "re-run changing --rbac-scope", release: previousRelease, pullSecret: true, rbacScope: rbacScopeCluster},
		{name: "re-run without --image-pull-secret keeps the binding", release: previousRelease, previousPullSecret: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			getValues := helmReleaseNotFound
			if tc.release != nil {
				data, err := json.Marshal(tc.release)
				require.NoError(t, err)
				releaseFile := filepath.Join(dir, "release.json")
				require.NoError(t, os.WriteFile(releaseFile, data, 0o600))
				getValues = fmt.Sprintf("cat %q; exit 0", releaseFile)
			}
			helm := writeFakeHelm(t, dir, fmt.Sprintf(
				"if [ \"$1\" = get ]; then %s; fi\nexec %q \"$@\"", getValues, realHelm))
			ws, err := newValuesWorkspace()
			require.NoError(t, err)
			defer ws.cleanup()

			opts := valuesCaseOptions{Image: tc.image, PullSecret: tc.pullSecret, RBACScope: tc.rbacScope}.valuesOptions()
			valuesFile, _, err := prepareNVCREValues(helm, "", "", ws, nvcreValuesInput{opts: opts}, io.Discard)
			require.NoError(t, err)

			image := tc.image
			if image == "" {
				image = testDefaultImage
			}
			render := func(valueArgs ...string) string {
				args := append([]string{"template", helmReleaseName, chart, helmFlagNamespace, nvcreNamespace}, valueArgs...)
				out, err := exec.Command(realHelm, args...).CombinedOutput() // #nosec G204 -- test helper
				require.NoError(t, err, string(out))
				return string(out)
			}
			previous := render(previousSetupInitArgs(tc.release, image,
				tc.pullSecret || tc.previousPullSecret, tc.rbacScope)...)
			require.Equal(t, previous, render(helmFlagValues, valuesFile))
		})
	}
}
