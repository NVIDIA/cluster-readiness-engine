// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// ChartValues are the chart values `setup init` passes to the NVCRE release
// (ADR-098): Helm's --values, --set-json and --set, in Helm's syntax, and
// --reset-values. They never reach the Kubeflow Trainer release.
type ChartValues struct {
	Files   []string
	SetJSON []string
	Set     []string
	// Reset drops the installed release's values except rbac.scope and
	// rbac.runNamespaces.
	Reset bool
}

// valuesStdin is the --values argument that reads stdin.
const valuesStdin = "-"

// Sources the preflight summary names for a value (ADR-098 decision 6).
const (
	sourceInstalled  = "installed release"
	sourceThisRun    = "this run"
	sourceImageFlag  = "--image"
	sourceVersion    = "--version"
	sourceCLIVersion = "CLI version"
	sourcePullSecret = "--image-pull-secret" // #nosec G101 -- a flag name, not a credential
	sourceRBACFlag   = "--rbac-scope"
)

// Top-level chart value keys under which nvcrectl sets or checks values.
const (
	valuesKeyManager = "manager"
	valuesKeyImage   = "image"
	valuesKeyRBAC    = "rbac"
	// valuesKeyName is the key of an imagePullSecrets entry.
	valuesKeyName = "name"
)

// Paths of the values nvcrectl sets or checks itself.
var (
	pathImageRepository  = []string{valuesKeyManager, valuesKeyImage, "repository"}
	pathImageTag         = []string{valuesKeyManager, valuesKeyImage, "tag"}
	pathImageDigest      = []string{valuesKeyManager, valuesKeyImage, "digest"}
	pathImagePullSecrets = []string{valuesKeyManager, "imagePullSecrets"}
	pathRBACScope        = []string{valuesKeyRBAC, "scope"}
	pathRBACRunNS        = []string{valuesKeyRBAC, "runNamespaces"}
)

func (v ChartValues) hasFlags() bool {
	return len(v.Files)+len(v.SetJSON)+len(v.Set) > 0
}

// validate rejects what setup init cannot honour before anything is read or
// installed: values for a helm phase that is skipped, stdin without
// --auto-approve (stdin answers the confirmation prompt then), and URLs, which
// Helm would fetch again on every run.
func (v ChartValues) validate(skipHelm, autoApprove bool) error {
	if skipHelm {
		var given []string
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"--values", len(v.Files) > 0},
			{"--set-json", len(v.SetJSON) > 0},
			{"--set", len(v.Set) > 0},
			{"--reset-values", v.Reset},
		} {
			if f.set {
				given = append(given, f.name)
			}
		}
		if len(given) > 0 {
			verb := "applies"
			if len(given) > 1 {
				verb = "apply"
			}
			return fmt.Errorf("%s %s to the NVCRE release, but --skip-phases skips the helm phase that installs it",
				strings.Join(given, ", "), verb)
		}
	}
	stdin := 0
	for _, f := range v.Files {
		lower := strings.ToLower(f)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return fmt.Errorf("--values %s: URLs are not accepted, because setup init runs Helm more than once "+
				"and each run would fetch it again; download the file and pass its path", f)
		}
		if f == valuesStdin {
			stdin++
		}
	}
	if stdin > 1 {
		return fmt.Errorf("--values %s may be given only once", valuesStdin)
	}
	if stdin == 1 && !autoApprove {
		return fmt.Errorf("--values %s (stdin) needs --auto-approve: without it, stdin answers the confirmation prompt",
			valuesStdin)
	}
	return nil
}

// valuesWorkspace is the private temporary directory that holds the copies of
// the values sources, the kept values, the probe chart and the merged values
// file. Everything in it is written with mode 0600.
type valuesWorkspace struct {
	dir string
}

func newValuesWorkspace() (*valuesWorkspace, error) {
	dir, err := os.MkdirTemp("", "nvcrectl-values-") // mode 0700
	if err != nil {
		return nil, fmt.Errorf("create a temporary directory for chart values: %w", err)
	}
	return &valuesWorkspace{dir: dir}, nil
}

func (w *valuesWorkspace) cleanup() {
	_ = os.RemoveAll(w.dir)
}

func (w *valuesWorkspace) write(name string, data []byte) (string, error) {
	path := filepath.Join(w.dir, filepath.Base(name))
	if err := os.WriteFile(path, data, 0o600); err != nil { // #nosec G703 -- a base name inside the private temporary directory
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// copyValuesFiles reads each --values source exactly once and returns the
// paths of the copies, in order. Helm runs more than once on a single init, so
// a source that can be read only once (stdin, a named pipe, process
// substitution) would otherwise reach only the first run.
func (w *valuesWorkspace) copyValuesFiles(files []string, in io.Reader) ([]string, error) {
	copies := make([]string, 0, len(files))
	for i, f := range files {
		var data []byte
		var err error
		base := filepath.Base(f)
		if f == valuesStdin {
			base = "stdin"
			if in == nil {
				return nil, fmt.Errorf("--values %s: no stdin to read", valuesStdin)
			}
			data, err = io.ReadAll(in)
		} else {
			data, err = os.ReadFile(f) // #nosec G304 -- the operator names the file to read
		}
		if err != nil {
			return nil, fmt.Errorf("read --values %s: %w", f, err)
		}
		path, err := w.write(fmt.Sprintf("values-%d-%s", i, base), data)
		if err != nil {
			return nil, err
		}
		copies = append(copies, path)
	}
	return copies, nil
}

// probeChart writes a chart with no values.yaml whose only template prints
// .Values, so `helm template` on it returns Helm's own reading of the flags it
// is given and nothing else.
func (w *valuesWorkspace) probeChart() (string, error) {
	dir := filepath.Join(w.dir, "probe")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o700); err != nil {
		return "", fmt.Errorf("create the values probe chart: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("apiVersion: v2\nname: nvcre-values-probe\nversion: 0.0.0\n"), 0o600); err != nil {
		return "", fmt.Errorf("create the values probe chart: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "values.json"),
		[]byte("{{ toJson .Values }}\n"), 0o600); err != nil {
		return "", fmt.Errorf("create the values probe chart: %w", err)
	}
	return dir, nil
}

// probeArgs returns the `helm template` arguments that render the probe chart
// with the given values files and the operator's --set-json and --set. Helm
// applies files, then --set-json, then --set, whatever the order on the
// command line.
func probeArgs(chartDir string, files, setJSON, set []string) []string {
	args := make([]string, 0, 3+2*(len(files)+len(setJSON)+len(set)))
	args = append(args, "template", "nvcre-values-probe", chartDir)
	for _, f := range files {
		args = append(args, helmFlagValues, f)
	}
	for _, s := range setJSON {
		args = append(args, helmFlagSetJSON, s)
	}
	for _, s := range set {
		args = append(args, helmFlagSet, s)
	}
	return args
}

// runProbe renders the probe chart and returns the values Helm read.
func runProbe(helmPath, chartDir string, files, setJSON, set []string) (map[string]any, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(helmPath, probeArgs(chartDir, files, setJSON, set)...) // #nosec G204 -- helmPath comes from this CLI; the values are the operator's own
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read the chart values with helm template: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return decodeProbeOutput(stdout.Bytes())
}

// decodeProbeOutput drops the document separator and "# Source:" comment that
// `helm template` prints around the probe's single JSON line.
func decodeProbeOutput(output []byte) (map[string]any, error) {
	var body bytes.Buffer
	for line := range strings.SplitSeq(string(output), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	values, err := decodeValues(body.Bytes())
	if err != nil {
		return nil, fmt.Errorf("parse the values probe output: %w", err)
	}
	return values, nil
}

// decodeValues parses a JSON values object. Numbers stay json.Number so they
// are written back exactly as Helm read them. Empty input and null are no
// values.
func decodeValues(data []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return nil, err
	}
	return values, nil
}

// readReleaseValues reads the values the installed NVCRE release was
// installed or upgraded with, not the chart defaults. A release Helm has no
// record of is a first install and yields no values. Any other failure is
// returned rather than guessed at: upgrading without the installed values
// would reset them, which is the change this read exists to prevent.
func readReleaseValues(helmPath, kubeconfig, kubeContext string) (map[string]any, error) {
	args := []string{helmSubcommandGet, "values", helmReleaseName, helmFlagNamespace, nvcreNamespace, helmFlagOutput, outputJSON}
	args = appendKubeconfigArgs(args, kubeconfig, kubeContext)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(helmPath, args...) // #nosec G204 -- helmPath and args come from this CLI, not from untrusted input
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "release: not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("read the installed %s release's values to keep them: %w: %s",
			helmReleaseName, err, strings.TrimSpace(stderr.String()))
	}
	values, err := decodeValues(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("parse the installed %s release's values: %w", helmReleaseName, err)
	}
	return values, nil
}

// keptValues returns the installed release's values that this run keeps. The
// image's repository, tag and digest are never kept: they hold whatever
// nvcrectl set on an earlier run, which cannot be told apart from an
// operator's choice (ADR-098 decision 4). With reset, only rbac.scope and
// rbac.runNamespaces are kept, so a reset never widens a namespaced grant.
func keptValues(release map[string]any, reset bool) map[string]any {
	if reset {
		kept := map[string]any{}
		for _, path := range [][]string{pathRBACScope, pathRBACRunNS} {
			if v, ok := valueAt(release, path...); ok {
				setPath(kept, deepCopy(v), path...)
			}
		}
		return nonEmpty(kept)
	}
	kept, _ := deepCopy(release).(map[string]any)
	removed := false
	for _, path := range [][]string{pathImageRepository, pathImageTag, pathImageDigest} {
		removed = deletePath(kept, path...) || removed
	}
	if removed {
		pruneEmpty(kept, valuesKeyManager, valuesKeyImage)
	}
	return nonEmpty(kept)
}

// imageSetBy reports whether values choose the controller image: they set its
// repository, tag or digest, or replace a parent of them with something that
// is not an object. Other keys under manager.image, such as pullPolicy, do
// not choose the image.
func imageSetBy(values map[string]any) bool {
	return setsPath(values, pathImageRepository...) ||
		setsPath(values, pathImageTag...) ||
		setsPath(values, pathImageDigest...)
}

// imageChoice is the controller image a run uses and where it came from.
type imageChoice struct {
	repository string
	tag        string
	digest     string
	source     string
}

// String renders the image reference. Parts the chart fills itself are shown
// as placeholders.
func (c imageChoice) String() string {
	repo := c.repository
	if repo == "" {
		repo = "<chart default repository>"
	}
	if c.digest != "" {
		return repo + "@" + c.digest
	}
	tag := c.tag
	if tag == "" {
		tag = "<chart appVersion>"
	}
	return repo + ":" + tag
}

// valuesOptions are the flags of this run that decide nvcrectl's own values.
type valuesOptions struct {
	reset bool
	// imageFlag is --image; empty when not given.
	imageFlag string
	// defaultImage is the image used when neither --image nor the operator's
	// values choose one, and defaultImageSource names it.
	defaultImage       string
	defaultImageSource string
	// pullSecret is true when --image-pull-secret was given.
	pullSecret bool
	// rbacScope is --rbac-scope; empty when not given.
	rbacScope string
}

// valuePath is a merged value's path and where it came from, for the summary.
type valuePath struct {
	path   string
	source string
}

// resolvedValues is what the NVCRE upgrade is given, and how each part was
// decided.
type resolvedValues struct {
	merged            map[string]any
	image             imageChoice
	paths             []valuePath
	rbacScopeSource   string
	pullSecretSources []string
	reset             bool
}

// resolveValues applies ADR-098 decisions 2 to 5. kept is the installed
// release's kept values, operator is the probe of this run's flags alone (O)
// and probed is the probe of the kept values followed by this run's flags
// (P). It rejects a value set both by its flag and by this run's values, lays
// P over the kept values, and adds nvcrectl's own values.
func resolveValues(kept, operator, probed map[string]any, opts valuesOptions) (resolvedValues, error) {
	operatorImage := imageSetBy(operator)
	if opts.imageFlag != "" && operatorImage {
		return resolvedValues{}, fmt.Errorf(
			"--image and this run's values both set the controller image (manager.image.repository, tag or digest); " +
				"give --image or the values, not both")
	}
	if opts.rbacScope != "" && setsPath(operator, pathRBACScope...) {
		return resolvedValues{}, fmt.Errorf(
			"--rbac-scope and this run's values both set rbac.scope; give --rbac-scope or the value, not both")
	}

	merged := mergeUnder(kept, probed)
	r := resolvedValues{merged: merged, reset: opts.reset}

	// The image (decision 4).
	switch {
	case operatorImage:
		r.image = imageChoice{source: sourceThisRun}
		r.image.repository = stringAt(merged, pathImageRepository...)
		r.image.tag = stringAt(merged, pathImageTag...)
		r.image.digest = stringAt(merged, pathImageDigest...)
	default:
		ref, source := opts.defaultImage, opts.defaultImageSource
		if opts.imageFlag != "" {
			ref, source = opts.imageFlag, sourceImageFlag
		}
		name, tag, digest := parseImage(ref)
		r.image = imageChoice{repository: name, tag: tag, digest: digest, source: source}
		setPath(merged, name, pathImageRepository...)
		if digest != "" {
			setPath(merged, digest, pathImageDigest...)
		} else {
			setPath(merged, tag, pathImageTag...)
		}
	}

	// The pull-secret list (decision 5).
	if setsPath(operator, pathImagePullSecrets...) {
		r.pullSecretSources = append(r.pullSecretSources, sourceThisRun)
	}
	if setsPath(kept, pathImagePullSecrets...) {
		o, _ := valueAt(operator, pathImagePullSecrets...)
		p, _ := valueAt(probed, pathImagePullSecrets...)
		if !setsPath(operator, pathImagePullSecrets...) || !reflect.DeepEqual(o, p) {
			r.pullSecretSources = append([]string{sourceInstalled}, r.pullSecretSources...)
		}
	}
	if opts.pullSecret {
		list, _ := valueAt(merged, pathImagePullSecrets...)
		if _, isList := list.([]any); list != nil && !isList {
			// Appending to anything but a list would silently replace it.
			return resolvedValues{}, fmt.Errorf(
				"manager.imagePullSecrets is %s, not a list; --image-pull-secret adds nvcrectl-pull-secret "+
					"to a list of {name: …} entries, so give a list or null", describeValueType(list))
		}
		if withSecret, added := appendPullSecret(list, pullSecretName); added {
			setPath(merged, withSecret, pathImagePullSecrets...)
			r.pullSecretSources = append(r.pullSecretSources, sourcePullSecret)
		}
	}

	// rbac.scope (decision 3).
	switch {
	case opts.rbacScope != "":
		setPath(merged, opts.rbacScope, pathRBACScope...)
		r.rbacScopeSource = sourceRBACFlag
	case setsPath(operator, pathRBACScope...):
		r.rbacScopeSource = sourceThisRun
	default:
		r.rbacScopeSource = sourceInstalled
	}

	// Every other value, by path and source (decision 6).
	for _, path := range leafPaths(merged) {
		if hasPrefix(path, pathImageRepository) || hasPrefix(path, pathImageTag) ||
			hasPrefix(path, pathImageDigest) || hasPrefix(path, pathImagePullSecrets) ||
			hasPrefix(path, pathRBACScope) {
			continue
		}
		source := sourceInstalled
		if setsPath(operator, path...) {
			source = sourceThisRun
		}
		r.paths = append(r.paths, valuePath{path: joinPath(path), source: source})
	}
	return r, nil
}

// appendPullSecret returns list with {name: name} at the end, unless an entry
// with that name is already there. A null or absent list starts empty;
// resolveValues rejects any other value that is not a list before calling it.
func appendPullSecret(list any, name string) ([]any, bool) {
	entries, _ := list.([]any)
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && m[valuesKeyName] == name {
			return entries, false
		}
	}
	out := make([]any, 0, len(entries)+1)
	out = append(out, entries...)
	return append(out, map[string]any{valuesKeyName: name}), true
}

// printValuesSummary prints what the NVCRE release will be given. Values are
// printed by path and source only: the summary reaches CI logs, and a value
// can hold anything, including a credential under a key the chart never
// reads. The image, the pull-secret names and rbac.scope are printed with
// their values, because nvcrectl sets or checks them and none is a credential.
func printValuesSummary(out io.Writer, r resolvedValues) {
	if r.reset {
		_, _ = fmt.Fprintf(out, "[preflight] --reset-values: the installed release's values are not kept, "+
			"except rbac.scope and rbac.runNamespaces.\n")
	}
	if len(r.paths) > 0 {
		_, _ = fmt.Fprintf(out, "[preflight] Values for release %q (paths only; values are not printed):\n", helmReleaseName)
		width := 0
		for _, p := range r.paths {
			width = max(width, len(p.path))
		}
		for _, p := range r.paths {
			_, _ = fmt.Fprintf(out, "  %-*s  (%s)\n", width, p.path, p.source)
		}
	}
	if scope, ok := valueAt(r.merged, pathRBACScope...); ok {
		_, _ = fmt.Fprintf(out, "[preflight] rbac.scope: %s (%s)\n", displayScalar(scope), r.rbacScopeSource)
	}
	_, _ = fmt.Fprintf(out, "[preflight] Controller image: %s (%s)\n", r.image, r.image.source)
	if list, ok := valueAt(r.merged, pathImagePullSecrets...); ok && len(r.pullSecretSources) > 0 {
		entries, _ := list.([]any)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			name := "<no name>"
			if m, ok := e.(map[string]any); ok {
				if n, ok := m[valuesKeyName]; ok {
					name = displayScalar(n)
				}
			}
			names = append(names, name)
		}
		_, _ = fmt.Fprintf(out, "[preflight] Image pull secrets: [%s] (%s)\n",
			strings.Join(names, ", "), strings.Join(r.pullSecretSources, ", "))
	}
}

// describeValueType names the kind of a decoded values entry for an error,
// without printing the value itself.
func describeValueType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	default:
		return fmt.Sprintf("a %T", v)
	}
}

// displayScalar renders a value nvcrectl prints with its value.
func displayScalar(v any) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(v)
}

// nvcreValuesInput is what preflight needs to build the NVCRE values file.
type nvcreValuesInput struct {
	values ChartValues
	opts   valuesOptions
	// in is stdin, read once when a --values source is "-".
	in io.Reader
}

// prepareNVCREValues reads the installed release's values, runs the probes,
// resolves the values, prints the summary, and writes the values file the
// upgrade is given. It runs in preflight, so a conflict stops setup init with
// the cluster unchanged.
func prepareNVCREValues(
	helmPath, kubeconfig, kubeContext string, ws *valuesWorkspace, input nvcreValuesInput, out io.Writer,
) (string, imageChoice, error) {
	// Copy the values sources before Helm runs at all.
	files, err := ws.copyValuesFiles(input.values.Files, input.in)
	if err != nil {
		return "", imageChoice{}, err
	}
	release, err := readReleaseValues(helmPath, kubeconfig, kubeContext)
	if err != nil {
		return "", imageChoice{}, err
	}
	kept := keptValues(release, input.values.Reset)

	var operator map[string]any
	probed := kept
	if input.values.hasFlags() {
		chart, err := ws.probeChart()
		if err != nil {
			return "", imageChoice{}, err
		}
		operator, err = runProbe(helmPath, chart, files, input.values.SetJSON, input.values.Set)
		if err != nil {
			return "", imageChoice{}, err
		}
		probed = operator
		if len(kept) > 0 {
			keptFile, err := writeValuesFile(ws, "kept.json", kept)
			if err != nil {
				return "", imageChoice{}, err
			}
			probed, err = runProbe(helmPath, chart, append([]string{keptFile}, files...),
				input.values.SetJSON, input.values.Set)
			if err != nil {
				return "", imageChoice{}, err
			}
		}
	}

	resolved, err := resolveValues(kept, operator, probed, input.opts)
	if err != nil {
		return "", imageChoice{}, err
	}
	printValuesSummary(out, resolved)
	file, err := writeValuesFile(ws, "values.json", resolved.merged)
	if err != nil {
		return "", imageChoice{}, err
	}
	return file, resolved.image, nil
}

// writeValuesFile writes values as JSON, which Helm reads as YAML.
func writeValuesFile(ws *valuesWorkspace, name string, values map[string]any) (string, error) {
	if values == nil {
		values = map[string]any{}
	}
	data, err := marshalValues(values)
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", name, err)
	}
	return ws.write(name, data)
}

// marshalValues encodes values as indented JSON, leaving <, > and & as they
// are rather than escaping them for HTML.
func marshalValues(values any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(values); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Value tree helpers
// ---------------------------------------------------------------------------

// mergeUnder lays over on top of base: maps merge key by key, and wherever
// over has a key its value wins, including a list and null. A base value
// survives under a parent that over replaced with an object, as a chart
// default would. Neither argument is modified.
func mergeUnder(base, over map[string]any) map[string]any {
	out, _ := deepCopy(base).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for k, ov := range over {
		om, overIsMap := ov.(map[string]any)
		bm, baseIsMap := out[k].(map[string]any)
		if overIsMap && baseIsMap {
			out[k] = mergeUnder(bm, om)
			continue
		}
		out[k] = deepCopy(ov)
	}
	return out
}

// valueAt returns the value at path and whether it is present.
func valueAt(m map[string]any, path ...string) (any, bool) {
	var cur any = m
	for _, k := range path {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = cm[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// stringAt returns the value at path as a string, or "" when it is absent or
// null.
func stringAt(m map[string]any, path ...string) string {
	v, ok := valueAt(m, path...)
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// setsPath reports whether values set path: the key itself, or a parent of it
// that is not an object (null or a scalar replaces everything under it).
func setsPath(m map[string]any, path ...string) bool {
	cur := m
	for i, k := range path {
		v, ok := cur[k]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		next, isMap := v.(map[string]any)
		if !isMap {
			return true
		}
		cur = next
	}
	return false
}

// setPath sets path to v, creating objects on the way and replacing anything
// on the way that is not one.
func setPath(m map[string]any, v any, path ...string) {
	cur := m
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}

// deletePath removes path and reports whether it was there.
func deletePath(m map[string]any, path ...string) bool {
	parent, ok := valueAt(m, path[:len(path)-1]...)
	if !ok {
		return false
	}
	pm, ok := parent.(map[string]any)
	if !ok {
		return false
	}
	if _, ok := pm[path[len(path)-1]]; !ok {
		return false
	}
	delete(pm, path[len(path)-1])
	return true
}

// pruneEmpty removes the objects along path, deepest first, that are empty.
func pruneEmpty(m map[string]any, path ...string) {
	for i := len(path); i > 0; i-- {
		v, ok := valueAt(m, path[:i]...)
		if vm, isMap := v.(map[string]any); !ok || !isMap || len(vm) > 0 {
			return
		}
		deletePath(m, path[:i]...)
	}
}

func nonEmpty(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// leafPaths returns the paths of every value that is not a non-empty object,
// sorted. A list is one path; it is never flattened into indices.
func leafPaths(m map[string]any) [][]string {
	var paths [][]string
	var walk func(prefix []string, v any)
	walk = func(prefix []string, v any) {
		vm, isMap := v.(map[string]any)
		if !isMap || (len(vm) == 0 && len(prefix) > 0) {
			paths = append(paths, append([]string(nil), prefix...))
			return
		}
		keys := make([]string, 0, len(vm))
		for k := range vm {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walk(append(prefix, k), vm[k])
		}
	}
	walk(nil, m)
	return paths
}

// joinPath renders a path in Helm's --set syntax, escaping a dot in a key.
func joinPath(path []string) string {
	keys := make([]string, len(path))
	for i, k := range path {
		keys[i] = strings.ReplaceAll(k, ".", `\.`)
	}
	return strings.Join(keys, ".")
}

func hasPrefix(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}

// deepCopy copies a decoded JSON value.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return map[string]any(nil)
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		if t == nil {
			return []any(nil)
		}
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
