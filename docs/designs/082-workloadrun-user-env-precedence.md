# ADR-082: Preserve WorkloadRun User Environment Precedence

## Context

`WorkloadRun.spec.env` is rendered into the generated TrainingRuntime
container environment, while platform WorkloadRun overrides may set
`TrainJob.trainer.env`. Kubeflow Trainer applies the latter after the runtime
environment, so a platform value can silently replace a user value with the
same name.

## Decision

When platform overrides are rendered for a WorkloadRun, merge the user-supplied
environment variables into every override that provides `trainer.env`, with
user values replacing platform values by name. Keep the existing generic
Workflow override merge semantics unchanged.

## Implementation

The WorkloadRun override renderer receives the user environment and merges it
into the raw `trainer.env` patch before the override is stored in the Workflow.
The controller and offline renderer pass the same `spec.env` values, so both
paths produce the same effective environment.

## Rationale

The override list is intentionally an unnamed list and therefore replaces the
base list during strategic merge. Adding the user values to each relevant
WorkloadRun override preserves platform values that do not conflict while
ensuring the user value is the final value for conflicting names. Changing the
generic merge behavior would affect unrelated Workflow resources.

## Consequences

User environment values consistently win over platform-provided values in the
generated workload trainer environment. Platform values remain unchanged when
the user did not specify the same name. No API or CRD fields are added.

## Alternatives Considered

- Moving all user values into `trainer.env` after override application would
  require carrying the original WorkloadRun input through the Workflow API.
- Changing generic strategic merge behavior would alter unrelated Workflow
  override contracts.

## Notes

The runtime container continues to receive the existing merged environment;
this decision additionally protects the trainer-level environment consumed by
Kubeflow Trainer.

## References

- Issue #421: WorkloadRun platform overrides silently override user `spec.env`
