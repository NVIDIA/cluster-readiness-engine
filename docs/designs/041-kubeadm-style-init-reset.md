# ADR-041: kubeadm-Style Init/Reset with Phases

## Context

ADRs 036-040 introduced four separate setup commands: `install-deps`, `install`, `uninstall-deps`, and `uninstall`. While functional, this design has two problems:

1. **Fragmented workflow**: In 95% of deployments, operators need deps before the controller. Having them as separate commands forces operators to run two commands in sequence and know the correct order.
2. **Poor discoverability**: Four commands with overlapping flags is harder to learn than one command with phases.

kubeadm — the standard Kubernetes cluster bootstrap tool — solves this with a phase-based architecture: `kubeadm init` runs sequential phases (preflight → certs → kubeconfig → ...), and `kubeadm reset` reverses them. Phases can be skipped with `--skip-phases=<name>` for users who don't need specific steps.

This pattern is well-established in the Kubernetes ecosystem and maps naturally to our use case:
- **init**: deps → CRDs → controller → logprofiles
- **reset**: logprofiles → controller → CRDs → deps

## Decision

Replace the four separate commands with two phase-based commands:

- `nvcrectl setup init` — runs all init phases sequentially
- `nvcrectl setup reset` — runs all reset phases (reverse order)

Both support `--skip-phases=<comma-separated>` to skip specific phases.

### Init Phases

| Phase | Description |
|-------|-------------|
| `preflight` | Validate kubectl in PATH, cluster reachable |
| `deps` | Install Kubeflow Trainer v2.1.0 |
| `crds` | Install CRE CRDs |
| `controller` | Install CRE controller (Deployment, RBAC, metrics) |
| `logprofiles` | Install CRE LogProfiles |

### Reset Phases (reverse order)

| Phase | Description |
|-------|-------------|
| `preflight` | Validate kubectl in PATH, cluster reachable |
| `logprofiles` | Remove CRE LogProfiles |
| `controller` | Remove CRE controller |
| `crds` | Remove CRE CRDs |
| `deps` | Remove Kubeflow Trainer v2.1.0 |

### Output Format (kubeadm bracketed style)

```
[preflight] Checking prerequisites...
[deps] Installing Kubeflow Trainer v2.1.0...
[crds] Installing CRE CRDs...
[controller] Installing CRE controller...
[logprofiles] Installing CRE LogProfiles...

CRE initialized successfully.
```

Skipped phases show: `[deps] Skipped.`

### Phase Skipping

```bash
# Skip deps (already installed separately)
nvcrectl setup init --skip-phases=deps

# Skip deps on reset (leave Kubeflow Trainer installed)
nvcrectl setup reset --skip-phases=deps

# Skip multiple phases
nvcrectl setup init --skip-phases=deps,logprofiles
```

## Implementation

Supersedes ADRs 036-040. The four existing commands are replaced by `init` and `reset`. All helper functions (`ensureKubectl`, `getClusterInfo`, `promptForConfirmation`, `runKubectlApply`, `runKubectlDelete`, `writeTempOverlay`, `installController`, `uninstallController`, `parseImage`, `creKustomizeURL`, `defaultImage`) are reused.

New functions:
- `newInitCommand()`, `newResetCommand()` — cobra commands
- `runInit()`, `runReset()` — phase runners
- `parseSkipPhases()` — parses `--skip-phases` comma-separated string into a set

## Rationale

- **kubeadm is the reference**: Operators managing Kubernetes clusters are already familiar with this UX pattern. Following it reduces cognitive load.
- **Phases over subcommands**: A single `init` with skip is simpler than remembering which of 4 commands to run and in what order.
- **`--skip-phases` over separate commands**: Users who don't need deps can skip with one flag instead of running `install` without `install-deps`.
- **Reverse order for reset**: Matches kubeadm's approach — teardown is the reverse of setup. The controller must stop before CRDs are removed.

## Consequences

### Positive
- Single command for full deployment: `nvcrectl setup init --auto-approve`
- Single command for full teardown: `nvcrectl setup reset --auto-approve`
- Familiar kubeadm UX pattern
- `--skip-phases` handles all edge cases (pre-installed deps, keep deps on reset, etc.)

### Negative
- Replaces 4 commands that were just introduced (no external users yet, so no backward compat concern)
- Slightly more complex implementation (phase runner loop vs direct function calls)

### Supersedes
- ADR-036 (install-deps) → init phase `deps`
- ADR-037 (install controller) → init phases `crds`, `controller`, `logprofiles`
- ADR-040 (uninstall) → reset phases
