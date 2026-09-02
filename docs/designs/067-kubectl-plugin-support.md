# ADR-067: `kubectl nvcrectl` Plugin Support and Full kubectl Flag Parity

## Context

`nvcrectl` is invoked directly today (`nvcrectl certification run ...`). kubectl supports a plugin
mechanism ([Extend kubectl with plugins](https://kubernetes.io/docs/tasks/extend-kubectl/kubectl-plugins/)):
any executable named `kubectl-<name>` on `$PATH` becomes invokable as `kubectl <name> ...` —
kubectl finds it by filename and execs it, forwarding the remaining arguments verbatim. There is
no registry, manifest, or handshake involved.

We want `kubectl nvcrectl certification run ...` to work as an alternative to `nvcrectl
certification run ...`, with no changes to `nvcrectl`'s own commands — same subcommands, same
output, in both invocation forms.

Running as a genuine kubectl plugin surfaces a second, related gap: `nvcrectl` only supports a
hand-rolled subset of kubectl's connection/auth flags (`--kubeconfig`, `--context`, and
`--namespace`/`-n` on some commands), declared as ad-hoc local flag vars per command
(`certification.go`, `workflow_render.go`, `workloadrun.go`, `setup.go`, `cluster.go`), each
feeding a hand-built `clientcmd.ConfigOverrides` in `newK8sClient`/`newK8sWatchClient`/
`newSetupClient`. Users reasonably expect a kubectl plugin to accept the same flags kubectl
itself does (`--as`, `--token`, `--server`/`-s`, `--insecure-skip-tls-verify`, etc.).

Both goals are delivered together in this ADR: making `nvcrectl` discoverable as a kubectl
plugin, and making its flag surface match kubectl's own.

## Decision

**Part A — plugin discoverability.** Cobra (which `nvcrectl` uses) parses `os.Args[1:]` and never
inspects `os.Args[0]`, so the exact same compiled binary already behaves identically regardless
of what it is named or invoked as. We exploit this directly: install a relative symlink
`kubectl-nvcrectl -> nvcrectl` next to the real binary, instead of publishing or maintaining a
second compiled artifact. Because the symlink references the target by filename, not by inode,
it keeps resolving correctly across future `nvcrectl upgrade` runs (which replace the file at the
same path via `os.Rename`), with no extra tracking logic.

**Part B — kubectl-standard flags.** Adopt `k8s.io/cli-runtime/pkg/genericclioptions.ConfigFlags`
— the same package `kubectl` itself is built on — as a full swap across every command, replacing
the hand-rolled `--kubeconfig`/`--context`/`--namespace` flags and the three near-duplicate
client-builder functions (`newK8sClient`, `newK8sWatchClient`, `newSetupClient`) entirely. This
is a new direct dependency (not currently in `go.mod`; only `k8s.io/klog/v2` is present today, as
an indirect dependency).

Together, `kubectl nvcrectl ...` becomes not just name-compatible with `nvcrectl ...`, but also
flag-compatible with `kubectl` itself — the same `--context`, `-n`, `--as`, `--token`, etc. work
identically on every command that talks to a cluster.

## Implementation

### 1. Installer script (`installer`, repo root)

After the existing `mv`/`sudo mv` step that places the binary at `${INSTALL_DIR}/${BIN_NAME}`
and before the "Verify" section, add a symlink step using the same permission-fallback pattern:

```bash
PLUGIN_NAME="kubectl-nvcrectl"
if [[ -w "${INSTALL_DIR}" ]]; then
  ln -sf "${BIN_NAME}" "${INSTALL_DIR}/${PLUGIN_NAME}"
else
  sudo ln -sf "${BIN_NAME}" "${INSTALL_DIR}/${PLUGIN_NAME}"
fi
```

The target is the relative name `nvcrectl`, not an absolute path, so it resolves correctly
regardless of `INSTALL_DIR` (default `/usr/local/bin`, or overridden via `-d`).

### 2. Self-upgrade (`pkg/upgrade/upgrade.go`)

Add `ensureKubectlPlugin()`, called at the end of `installBinary()` as
`ensureKubectlPlugin(filepath.Dir(destPath))`, so users who installed before this feature existed
get the plugin automatically on their next upgrade.

Failure handling mirrors `installBinary`'s existing sudo-fallback pattern (live
`stdout`/`stderr` passthrough so a password prompt or error is visible), but — unlike the real
binary swap, which must fail loudly since a failed upgrade is a real failure — never returns a
hard error: the symlink is a convenience, not a requirement, so a failure only prints a warning
and `nvcrectl upgrade` still exits 0.

```go
func ensureKubectlPlugin(installDir string) {
	if runtime.GOOS == "windows" {
		return // symlinks need elevated privileges on Windows; out of scope
	}
	linkPath := filepath.Join(installDir, "kubectl-nvcrectl")
	_ = os.Remove(linkPath) // best-effort; clears any stale file/symlink
	if err := os.Symlink(binaryName, linkPath); err == nil {
		return
	}
	cmd := exec.Command("sudo", "ln", "-sf", binaryName, linkPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: could not set up 'kubectl nvcrectl' plugin symlink: %v\n", err)
	}
}
```

### 3. Local dev builds (`Makefile`)

Extend `build-nvcrectl` to also create the local symlink:

```makefile
.PHONY: build-nvcrectl
build-nvcrectl: $(LOCALBIN) ## Build nvcrectl CLI tool.
	go build -ldflags "$(LDFLAGS)" -o bin/nvcrectl ./cmd/nvcrectl/
	ln -sf nvcrectl bin/kubectl-nvcrectl
```

No change to `build-nvcrectl-cross` or `.github/workflows/release.yml` — cross-compiled release artifacts and the
publish job are untouched. Only install-time symlinking changes; no new published artifacts.

### 4. Adopt `genericclioptions.ConfigFlags` across every command

Add `k8s.io/cli-runtime` to `go.mod`.

**Client builders.** Collapse `newK8sClient`/`newK8sWatchClient` (`workflow_render.go`) and
`newSetupClient` (`setup.go`) — each currently building its own `clientcmd.ConfigOverrides` from
raw strings — into builders that take a `*genericclioptions.ConfigFlags` and call
`.ToRESTConfig()`:

```go
func newK8sClientFromFlags(cf *genericclioptions.ConfigFlags) (client.Client, error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return client.New(restConfig, client.Options{Scheme: s})
}

func newK8sWatchClientFromFlags(cf *genericclioptions.ConfigFlags) (client.WithWatch, error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return client.NewWithWatch(restConfig, client.Options{Scheme: s})
}
```

**Per-command flags.** Every command that talks to a cluster (`certification
render`/`run`/`report`, `workflow render`, `workloadrun render`/`run`/`report`/`status`/`cancel`,
`cluster info`, `setup init`/`reset`) replaces its own `--kubeconfig`/`--context`/`--namespace`
declarations:

```go
// Before: per-command local vars, each command redeclaring the same three flags
var kubeconfig, kubeContext, namespace string
cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "...")
cmd.Flags().StringVar(&kubeContext, "context", "", "...")
cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "...")

// After: one shared struct, ~20 kubectl-identical flags for free
configFlags := genericclioptions.NewConfigFlags(true)
configFlags.AddFlags(cmd.Flags())
```

`AddFlags()` registers, in one call, matching kubectl exactly: `--kubeconfig`, `--context`,
`--cluster`, `--user`, `-n`/`--namespace`, `-s`/`--server`, `--certificate-authority`,
`--client-certificate`, `--client-key`, `--token`, `--as`, `--as-group`, `--as-uid`,
`--insecure-skip-tls-verify`, `--tls-server-name`, `--cache-dir`, `--disable-compression`,
`--request-timeout` (`--username`/`--password` are a separate opt-in — see Gap 3).

**Reverse check.** `nvcrectl`'s only current cluster-connection flags are `--kubeconfig`,
`--context`, `--namespace` — a strict subset of `ConfigFlags`, so nothing needs to stay
hand-written. One unrelated detail is unaffected: `appendKubeconfigArgs()` (`setup_helm.go`)
translates `--context` into `--kube-context` when shelling out to the real `helm` CLI (its flag
naming differs from kubectl's) — still fed from `*configFlags.Context`.

#### Known gaps (found by reading `k8s.io/cli-runtime@v0.31.0` source directly)

**Gap 1 — `--namespace` default changes from `"default"` to `""` on several commands.**
`NewConfigFlags(true)` seeds `Namespace` to `""`. `certification run` already defaults to `""`
(auto-generates a namespace name when unset), but `certification render`/`report`, `workflow
render`, and `workloadrun report`/`status`/`cancel` currently default to `"default"`. An empty
namespace is a real regression either way: a `Get` (e.g. `certification report`'s lookup) hits a
malformed REST path and 404s — visible, but misleading, reading as "not found" even when the
resource exists elsewhere; a `List` (e.g. discovering related Workflows/Jobs) is treated as an
explicit "all namespaces" query by client-go/controller-runtime — either a 403 if RBAC is scoped
to one namespace, or a silent, unintentionally cluster-wide result set otherwise.

Workaround: `AddFlags()` reads `*f.Namespace` at bind time to seed the flag's own default, so
mutate the pointed-to value *before* calling `AddFlags` rather than adding fallback logic after
parsing:
```go
configFlags := genericclioptions.NewConfigFlags(true)
*configFlags.Namespace = "default" // seed the command's own default, pre-AddFlags
configFlags.AddFlags(cmd.Flags())
```
This reproduces today's exact default with zero downstream changes. `certification run` needs no
change since `""` is already its native default.

**Gap 2 — commands with no real namespace concept would gain a silently-ignored flag.**
`cluster info` operates on cluster-scoped `Node` objects, and `setup init`/`reset` install into a
hardcoded `nvcre` constant namespace — neither has a `--namespace` flag today, and
`AddFlags()` would add one that looks functional but does nothing, which is worse than not having
it. Concretely: `setup init`/`reset` already ignore `--namespace`, so an `nvcrectl`-driven install
can never land anywhere but the hardcoded namespace — but the raw Helm path documented in
`installation.md` does let a user pick an arbitrary namespace. Someone who installed that way
into `my-team-ns`, then ran `setup reset -n my-team-ns --auto-approve` expecting `-n` to target
that install, would instead silently reset the hardcoded `nvcre` namespace.

Workaround: set `configFlags.Namespace = nil` before `AddFlags` on exactly these commands (`nil`
fields are skipped):
```go
configFlags := genericclioptions.NewConfigFlags(true)
configFlags.Namespace = nil // cluster info / setup init / setup reset: no namespace concept
configFlags.AddFlags(cmd.Flags())
```

**Gap 3 — `--username`/`--password` are not included by default.** `NewConfigFlags` leaves them
`nil`, and `AddFlags` skips nil fields, so they need the separate opt-in
`WithDeprecatedPasswordFlag()` ("deprecated" in the library itself — HTTP basic auth to the API
server is a legacy mode). Since `nvcrectl` doesn't support basic auth today either, this is not a
regression: intentionally exclude them rather than opting in, documented as a deliberate
omission. The opt-in call remains available later if full `kubectl options` parity is ever
required.

### 5. Environment variable parity — no additional work needed

kubectl currently documents 9 environment variables:

| Variable | `nvcrectl` support | Reason |
|---|---|---|
| `KUBECONFIG` | Supported | Already reads it via `clientcmd.NewDefaultClientConfigLoadingRules()`, the same function `ConfigFlags` uses internally |
| `KUBECACHEDIR` | Out of scope | Only read inside `ConfigFlags.ToDiscoveryClient()`/`.ToRESTMapper()`, which this ADR's client builders never call (only `.ToRESTConfig()`) — no current need for response/discovery caching |
| `KUBECTL_EXTERNAL_DIFF` | Out of scope | Controls `kubectl diff`, a command `nvcrectl` doesn't have |
| `KUBECTL_EXPLAIN_OPENAPIV3` | Out of scope | Controls `kubectl explain`, a command `nvcrectl` doesn't have |
| `KUBECTL_PORT_FORWARD_WEBSOCKETS` | Out of scope | Controls `kubectl port-forward`, a command `nvcrectl` doesn't have |
| `KUBECTL_REMOTE_COMMAND_WEBSOCKETS` | Out of scope | Controls `kubectl exec`/`cp`/`attach`, commands `nvcrectl` doesn't have |
| `KUBECTL_KUBERC` | Out of scope | Controls kubectl's own preferences-file feature, matching `--kuberc` already excluded above |
| `KUBECTL_KYAML` | Out of scope | Controls a kubectl-specific YAML print dialect; unrelated to connection/auth |
| `KUBECTL_ENABLE_CMD_SHADOW` | Not applicable | Read by kubectl itself, before a plugin is ever invoked — not something a plugin process can act on |

"Supporting" any of the last 8 without first building the unrelated feature they control would be
the same phantom-flag problem as Gap 2, for env vars.

### 6. Docs (`site/content/docs/getting-started/installation.md`)

Add a note after "Install nvcrectl" documenting that both forms work interchangeably
(`kubectl nvcrectl setup init` / `nvcrectl setup init`), the manual fallback
(`ln -s nvcrectl /usr/local/bin/kubectl-nvcrectl`) for manual/air-gapped/Windows installs, and the
expanded flag surface (`--as`, `--token`, `-s`/`--server`, etc.) now available on every
cluster-facing command.

### 7. Tests

- `internal/upgrade_test.go`: unit tests for `ensureKubectlPlugin()` — symlink created,
  refreshed when stale or pointing elsewhere, skipped on Windows.
- Per-command flag tests: `ConfigFlags.AddFlags()` registers without shorthand collisions on
  every affected command (a duplicate shorthand panics at flag-parse time, not compile time), and
  existing `--namespace`/`-n`/`--kubeconfig`/`--context` usage in current tests continues to parse
  identically post-swap.

## References

- [Extend kubectl with plugins](https://kubernetes.io/docs/tasks/extend-kubectl/kubectl-plugins/)
- [kubectl reference (flags and environment variables)](https://kubernetes.io/docs/reference/kubectl/kubectl/)
- [`k8s.io/cli-runtime/pkg/genericclioptions`](https://pkg.go.dev/k8s.io/cli-runtime/pkg/genericclioptions) — `ConfigFlags`, used by `kubectl` itself
