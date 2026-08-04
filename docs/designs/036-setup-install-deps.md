# ADR-036: CLI Command for Dependency Installation

## Context

CRE's controller requires external CRDs to be present in the cluster before it can start. With ADR-035 making legacy Kubeflow v1 optional, the only required dependency is **Kubeflow Trainer v2** (the `TrainJob` CRD and its controller). Today, operators must manually discover and run the correct `kubectl apply` command with the right URL and version — a process that is error-prone, undiscoverable, and version-sensitive.

As the project matures, operators need a single command that installs all required dependencies safely. This command must work both interactively (with a confirmation prompt showing the target cluster) and non-interactively (for CI/CD pipelines).

### Requirements

1. **Discoverability**: Operators should not need to know the Kubeflow Trainer installation URL or version.
2. **Safety**: Before modifying a cluster, the command must show the target cluster context and server URL and require explicit confirmation.
3. **Automation**: CI pipelines must be able to skip the interactive prompt via a flag.
4. **Fail-fast**: If `kubectl` is not available, fail immediately with a clear message before any prompting or cluster interaction.
5. **Idempotency**: Running the command on a cluster that already has the dependency installed must succeed without harm.

## Decision

Add a new `ncrectl setup install-deps` command that:

1. Checks for `kubectl` in PATH (fail-fast via `exec.LookPath`).
2. Resolves the target cluster from kubeconfig and displays context name + server URL.
3. Prompts for confirmation with Terraform-style `yes` input (not just `y`).
4. Executes `kubectl apply --server-side -k <kustomize-url>` to install the dependency.
5. Supports `--auto-approve` to skip the prompt for CI/automation.

The confirmation prompt follows Terraform's pattern: only the exact string `yes` is accepted, empty input or any other value cancels the operation.

## Implementation

### Command Structure

```
ncrectl setup install-deps [flags]

Flags:
  --auto-approve    Skip interactive confirmation prompt
  --kubeconfig      Path to kubeconfig (defaults to KUBECONFIG env or ~/.kube/config)
  --context         Kubeconfig context to use
```

### Execution Flow

```
1. ensureKubectl()      — exec.LookPath("kubectl"), fail if missing
2. getClusterInfo()     — resolve context name + server URL from kubeconfig
3. Display summary      — show cluster info + dependencies to install
4. promptForConfirmation() — unless --auto-approve, require "yes" from stdin
5. runKubectlApply()    — execute kubectl apply --server-side -k <url>
6. Report result        — success or failure message
```

### Dependency Definition

```go
const (
    kubeflowTrainerVersion      = "v2.1.0"
    kubeflowTrainerKustomizeURL = "https://github.com/kubeflow/trainer.git/manifests/overlays/manager?ref=" + kubeflowTrainerVersion
)
```

The version is compiled into the binary, ensuring version consistency between ncrectl and the controller.

### Confirmation Prompt (Terraform-style)

```
  Target cluster
  Context:  my-production-cluster
  Server:   https://10.0.1.100:6443

  Dependencies to install:
    - Kubeflow Trainer v2.1.0 (TrainJob CRD + controller)

Do you want to install these dependencies? Only 'yes' will be accepted to confirm.
  Enter a value:
```

All output goes to stderr. The prompt function accepts `io.Reader`/`io.Writer` parameters for testability.

### New Files

- `pkg/setup/setup.go` — command implementation
- `internal/setup_test.go` — unit tests for prompt, cluster info, error cases

### Modified Files

- `cmd/ncrectl/main.go` — register `newSetupCommand()`

## Rationale

- **Terraform-style `yes`**: Requires full word to prevent accidental confirmation. This is an industry standard for commands that modify cluster state.
- **`--auto-approve` over `--yes`/`-y`**: Follows Terraform naming convention which is widely recognized. The longer name makes the intent unambiguous.
- **kubectl delegation**: Rather than using the Go Kubernetes client directly for kustomize installation, delegating to `kubectl` avoids reimplementing kustomize rendering logic and leverages the user's existing authentication setup.
- **Fail-fast kubectl check**: `exec.LookPath` before any cluster interaction avoids wasting the user's time on confirmation prompts only to fail at the apply step.
- **Compiled-in version**: The Trainer version is a constant, not a flag, ensuring compatibility between the CLI tool and the controller binary which depends on specific Trainer API types.

## Consequences

### Positive

- Single-command dependency installation — no need to find URLs or versions.
- Safe by default with cluster confirmation.
- CI-friendly with `--auto-approve`.
- Extensible — future dependencies can be added to the same command.

### Negative

- Requires `kubectl` in PATH — adds an external dependency. Acceptable since operators managing Kubernetes clusters universally have kubectl available.
- Version is compiled-in — updating the Trainer version requires a new ncrectl release. This is intentional for version safety.

## Alternatives Considered

1. **Direct Go client for kustomize apply**: Would eliminate the kubectl dependency but requires reimplementing kustomize rendering and server-side apply logic. Significantly more complex for the same result.
2. **`--yes` / `-y` flag**: Simpler but ambiguous. `--auto-approve` is clearer and matches Terraform's well-known convention.
3. **No confirmation prompt**: Too risky for a command that modifies cluster state. Default-safe is the right choice.
