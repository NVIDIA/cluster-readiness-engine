// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// ApplyEnvToJobTemplate writes the owner's resolved environment variables onto
// a resolved job template, the sibling of ApplyImageToJobTemplate for the
// Certification tier's spec.env and spec.categories[].options.env (ADR-097).
// It is a no-op when env is empty, and it never creates structure: a template
// without a trainJob workload or without a trainer has nowhere to put a
// variable and is left alone.
//
// Where a variable lands depends on how the entry launches its ranks, and the
// two surfaces are not interchangeable:
//
//   - MPI entries get "-x NAME=VALUE" pairs in trainer.args. Their ranks start
//     under sshd with a fresh session environment and never read container env,
//     and their trainjob-ancestor-step label sits on the launcher replicated
//     job, so a trainer env var would reach mpirun's own container and no rank.
//   - Every other entry gets a trainer env var. Kubeflow upserts it by name
//     onto the container named "node" in the ancestor replicated job, which is
//     where the workload actually runs, and that upsert already wins over the
//     dependency runtime's own value. No dependency rewrite is needed.
//
// The shape is read from the referenced TrainingRuntime's mlPolicy rather than
// guessed from the argument list: "has a -x pair" conflates being MPI with
// having an anchor, and an MPI arm with no -x pairs would then be silently
// written to an environment its ranks never read — the invisible no-op this
// whole field exists to remove.
func ApplyEnvToJobTemplate(
	jt *nvcrev1alpha1.JobTemplateSpec, deps []nvcrev1alpha1.DependencySpec, env []corev1.EnvVar,
) error {
	if len(env) == 0 {
		return nil
	}
	trainJob := jt.Spec.Workload.TrainJob
	if trainJob == nil || trainJob.Trainer == nil {
		return nil
	}

	isMPI, err := runtimeIsMPI(trainJob, deps)
	if err != nil {
		return err
	}
	if !isMPI {
		upsertTrainerEnv(trainJob.Trainer, env)
		return nil
	}
	return applyEnvAsMPIArgs(trainJob.Trainer, env)
}

// runtimeIsMPI reports whether the TrainingRuntime the trainer references
// declares mlPolicy.mpi. A trainJob whose runtimeRef matches no dependency is
// not MPI as far as this rewrite is concerned: the catalog always ships the
// runtime it references, so an unmatched ref is a torch-shaped entry resolved
// from elsewhere rather than an MPI entry whose runtime went missing.
func runtimeIsMPI(
	trainJob *trainerv1alpha1.TrainJobSpec, deps []nvcrev1alpha1.DependencySpec,
) (bool, error) {
	name := trainJob.RuntimeRef.Name
	if name == "" {
		return false, nil
	}
	for i := range deps {
		if len(deps[i].Raw) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := json.Unmarshal(deps[i].Raw, &obj); err != nil {
			return false, fmt.Errorf("unmarshal dependency %d: %w", i, err)
		}
		if kind, _ := obj[keyKind].(string); kind != kindTrainingRuntime {
			continue
		}
		metadata, ok := nestedMap(obj, keyMetadata)
		if !ok {
			continue
		}
		if depName, _ := metadata[keyName].(string); depName != name {
			continue
		}
		return hasMPIPolicy(obj), nil
	}
	return false, nil
}

// upsertTrainerEnv replaces by name and appends otherwise, matching how
// Kubeflow merges trainer env onto the workload container. Replacing rather
// than appending is required, not tidy: the Workflow CRD carries trainer.env
// as a list whose identity is the variable name, so a duplicate name is a
// malformed object rather than a last-one-wins.
func upsertTrainerEnv(trainer *trainerv1alpha1.Trainer, env []corev1.EnvVar) {
	for _, e := range env {
		replaced := false
		for i := range trainer.Env {
			if trainer.Env[i].Name == e.Name {
				trainer.Env[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			trainer.Env = append(trainer.Env, e)
		}
	}
}

// applyEnvAsMPIArgs writes each variable as an "-x NAME=VALUE" pair in
// trainer.args.
//
// Position is load-bearing. The list is [mpirun flags] [-x pairs] [binary]
// [binary flags], so appending puts the pair after the test binary, where the
// binary receives it as its own argument: no error, no effect on the rank
// environment. New pairs are therefore inserted immediately after the last
// existing -x pair, which is also last among the -x flags and so wins if a
// duplicate ever survives.
//
// A name the entry already forwards has its operand replaced in place rather
// than a second pair appended. Besides keeping the list honest, this is what
// lets isMNNVLEnabledInJobTemplate keep reading NCCL_MNNVL_ENABLE back out of
// the rendered template to decide whether the diagnose no-NVL stage runs: with
// two pairs, mpirun takes the last and that scan takes the first, and the
// controller's belief diverges from what the ranks received.
//
// An MPI entry with no -x pair at all leaves no anchor for the insertion, and
// there is no safe way to locate the binary without one. That fails closed:
// guessing would reintroduce the silent no-op.
func applyEnvAsMPIArgs(trainer *trainerv1alpha1.Trainer, env []corev1.EnvVar) error {
	for _, e := range env {
		operand := e.Name + "=" + e.Value
		if i, ok := indexOfMPIEnvArg(trainer.Args, e.Name); ok {
			trainer.Args[i+1] = operand
			continue
		}
		last, ok := lastMPIEnvArgIndex(trainer.Args)
		if !ok {
			return fmt.Errorf(
				"cannot set env %s: the resolved MPI trainer args carry no %q pair to anchor an insertion",
				e.Name, mpiEnvFlag)
		}
		at := last + 2
		args := make([]string, 0, len(trainer.Args)+2)
		args = append(args, trainer.Args[:at]...)
		args = append(args, mpiEnvFlag, operand)
		args = append(args, trainer.Args[at:]...)
		trainer.Args = args
	}
	return nil
}

// indexOfMPIEnvArg returns the index of the "-x" that forwards name.
func indexOfMPIEnvArg(args []string, name string) (int, bool) {
	for i := range args {
		forwarded, ok := mpiEnvArgName(args, i)
		if ok && forwarded == name {
			return i, true
		}
	}
	return 0, false
}

// lastMPIEnvArgIndex returns the index of the final "-x" in args.
func lastMPIEnvArgIndex(args []string) (int, bool) {
	last, found := 0, false
	for i := range args {
		if _, ok := mpiEnvArgName(args, i); ok {
			last, found = i, true
		}
	}
	return last, found
}

// mpiEnvFlag is mpirun's environment-forwarding flag.
const mpiEnvFlag = "-x"
