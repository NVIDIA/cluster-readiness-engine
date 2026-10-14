// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Framework names a WorkloadRun's spec.framework selects between. The
// controller and the render path hand them to the builders and to
// ValidateWorkloadVolumes, so the two agree on which runtime a spec gets and
// which names that runtime reserves.
const (
	FrameworkTorch = "torch"
	FrameworkMPI   = "mpi"
	FrameworkExec  = "exec"
)

// ValidateWorkloadVolumes rejects a spec.volumes or spec.volumeMounts entry
// that collides with a volume or mount the builders, or a Kubeflow Trainer
// plugin, add to the same pod themselves. Both call sites run it before
// anything is created, so the controller and `nvcrectl workloadrun render`
// refuse the same specs.
//
// A duplicate volume name or mount path is otherwise caught at pod creation,
// after the Workflow and the Job exist, and two cases are not caught at all,
// because Trainer upserts its own entries by name and by path: the MPI
// plugin's mpi-ssh-auth volume on every worker and launcher pod, and the
// TrainJobStatus plugin's kubeflow-trainer-token volume on the pod whose job
// carries the "trainer" ancestor label, which is the node pod on torch and
// exec. A user volume of either name is replaced by Trainer's, and the user's
// mount of it ends up holding SSH keys or a service-account token. A user
// mount at /dev/shm, /tmp/mpi-ssh-raw or /root/.ssh would likewise shadow the
// built-in memory volume, the Secret the worker copies its keys from, or the
// directory it copies them into, and break the worker or its sshd before the
// workload ran. The two SSH directories and the token directory,
// /var/run/secrets/kubeflow/trainer, are reserved together with every path
// below them: a mount at /root/.ssh/authorized_keys hides that file as surely
// as one at /root/.ssh does. /dev/shm and /config are reserved exactly, since
// nothing built in reads a named file under them, so a mount below either
// collides with nothing of ours. A mountPath that is not absolute is refused
// outright, since none of the comparisons here mean anything for one.
//
// framework is one of FrameworkTorch, FrameworkMPI and FrameworkExec. The
// mpi-only names and paths are checked on FrameworkMPI alone, since the torch
// and exec runtimes run no sshd and have no launcher; the Trainer token is
// checked on the other two, since on mpi it lands on the launcher, which user
// volumes never reach. hasConfig says whether spec.config is set, which is
// when /config is taken and when a mount may name config-volume at all. The
// config-volume name itself is reserved either way, so that adding
// spec.config later never turns a valid spec into an invalid one.
func ValidateWorkloadVolumes(volumes []corev1.Volume, mounts []corev1.VolumeMount, framework string, hasConfig bool) error {
	mpi := framework == FrameworkMPI
	tokenOnNode := framework == FrameworkTorch || framework == FrameworkExec
	for i, v := range volumes {
		switch {
		case v.Name == volumeNameDSHM:
			return fmt.Errorf("spec.volumes[%d].name: %q is reserved: the built-in memory-backed %s volume has that name; rename the volume",
				i, v.Name, dshmMountPath)
		case v.Name == ConfigVolumeName:
			return fmt.Errorf("spec.volumes[%d].name: %q is reserved: spec.config is mounted through a volume of that name; rename the volume",
				i, v.Name)
		case mpi && v.Name == mpiSSHAuthName:
			return fmt.Errorf("spec.volumes[%d].name: %q is reserved on the mpi framework: Kubeflow Trainer replaces a volume of that name with its SSH key Secret on every worker and launcher pod; rename the volume",
				i, v.Name)
		case tokenOnNode && v.Name == trainerStatusVolumeName:
			return fmt.Errorf("spec.volumes[%d].name: %q is reserved on the %s framework: Kubeflow Trainer's TrainJobStatus plugin replaces a volume of that name with its status token on the node pod; rename the volume",
				i, v.Name, framework)
		}
	}
	for i, m := range mounts {
		if !path.IsAbs(m.MountPath) {
			return fmt.Errorf("spec.volumeMounts[%d].mountPath: %q must be an absolute path", i, m.MountPath)
		}
		p := path.Clean(m.MountPath)
		switch {
		case p == dshmMountPath:
			return fmt.Errorf("spec.volumeMounts[%d].mountPath: %q is reserved: the built-in %s volume is mounted there; choose another path",
				i, m.MountPath, volumeNameDSHM)
		case hasConfig && p == ConfigMountPath:
			return fmt.Errorf("spec.volumeMounts[%d].mountPath: %q is where spec.config is mounted; choose another path or drop spec.config",
				i, m.MountPath)
		case !hasConfig && m.Name == ConfigVolumeName:
			return fmt.Errorf("spec.volumeMounts[%d].name: %q names the volume spec.config creates, and spec.config is not set; set spec.config or mount another volume",
				i, m.Name)
		case mpi && underDir(p, mpiSSHMountPath):
			return fmt.Errorf("spec.volumeMounts[%d].mountPath: %q is reserved on the mpi framework: Kubeflow Trainer mounts its SSH key Secret at %s for the worker to copy its keys from, and a mount there or below it hides them; choose another path",
				i, m.MountPath, mpiSSHMountPath)
		case mpi && underDir(p, mpiSSHKeyDir):
			return fmt.Errorf("spec.volumeMounts[%d].mountPath: %q is reserved on the mpi framework: the worker writes its SSH keys under %s before starting sshd, and a mount there or below it hides them; choose another path",
				i, m.MountPath, mpiSSHKeyDir)
		case tokenOnNode && underDir(p, trainerStatusMountPath):
			return fmt.Errorf("spec.volumeMounts[%d].mountPath: %q is reserved on the %s framework: Kubeflow Trainer's TrainJobStatus plugin mounts its status token at %s on the node container, and a mount there or below it hides it; choose another path",
				i, m.MountPath, framework, trainerStatusMountPath)
		}
	}
	return nil
}

// underDir reports whether the cleaned path p is dir itself or lies below it.
// The comparison is on path segments, so /root/.ssh-extra is not under
// /root/.ssh.
func underDir(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// ValidateWorkloadInitContainers rejects a spec.initContainers entry that
// takes the workload container's name. Every builder names that container
// "node" on the pod the init containers join, and a pod's container names
// are unique across its init and regular containers, so the duplicate is
// otherwise caught at pod creation, after the Workflow and the Job exist,
// with nothing on the WorkloadRun to say why. The launcher's own init
// container, fix-ssh-permissions, is not reserved: user init containers never
// reach the launcher.
func ValidateWorkloadInitContainers(initContainers []corev1.Container) error {
	for i, c := range initContainers {
		if c.Name == nodeJobName {
			return fmt.Errorf("spec.initContainers[%d].name: %q is reserved: the workload container has that name and a pod's container names must be unique; rename the init container",
				i, c.Name)
		}
	}
	return nil
}
