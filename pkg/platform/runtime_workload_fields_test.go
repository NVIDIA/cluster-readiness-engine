// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Framework names each case's input.yaml selects between.
const (
	wfFrameworkMPI   = "mpi"
	wfFrameworkTorch = "torch"
	wfFrameworkExec  = "exec"
)

// workloadFieldsInput is each case's input.yaml: the builder to call and the
// user-level fields a WorkloadRun hands to it through RuntimeConfig.
type workloadFieldsInput struct {
	FrameworkType     string               `json:"frameworkType"`
	GangSchedulerName string               `json:"gangSchedulerName,omitempty"`
	Volumes           []corev1.Volume      `json:"volumes,omitempty"`
	VolumeMounts      []corev1.VolumeMount `json:"volumeMounts,omitempty"`
	InitContainers    []corev1.Container   `json:"initContainers,omitempty"`
}

// workloadFieldsContainer records one rendered container: its name, whether it
// is an init container, and its mounts as "name at /path" (with " (ro)" when
// readOnly), so the golden shows order as well as presence.
type workloadFieldsContainer struct {
	Name         string   `json:"name"`
	Init         bool     `json:"init,omitempty"`
	VolumeMounts []string `json:"volumeMounts"`
}

// workloadFieldsJob records one replicatedJob: its pod volumes as
// "name: <source>" and every init container and container in pod order.
type workloadFieldsJob struct {
	ReplicatedJob string                    `json:"replicatedJob"`
	Volumes       []string                  `json:"volumes"`
	Containers    []workloadFieldsContainer `json:"containers"`
}

// workloadFieldsProjection is the golden. violations is always empty in a
// recorded golden: a non-empty list fails the case before the golden is
// compared, so regeneration cannot record a regression. userItemsChecked is
// the number of placement lookups made for the input entries (each once on
// the worker and, on mpi, once more on the launcher); 0 marks a baseline case.
type workloadFieldsProjection struct {
	UserItemsChecked int                 `json:"userItemsChecked"`
	ReplicatedJobs   []workloadFieldsJob `json:"replicatedJobs"`
	Violations       []string            `json:"violations"`
}

// TestBuildRuntimeWorkloadFields pins where a WorkloadRun's spec.volumes,
// spec.volumeMounts and spec.initContainers land in each rendered runtime.
//
// BuildTorchRuntime (and BuildExecRuntime through it) always honoured the three
// fields. BuildMPIRuntime never read them, so on the mpi framework all of
// them, and spec.config with them, rendered as a silent no-op (issue #455).
// The fix puts them on the worker pod and its "node" container only, after the
// built-in mounts and volumes; the launcher is a coordinator with fixed
// resources and hosts no rank, so it is left exactly as it was.
//
// It is a projection golden, not a verbatim one: each case renders one
// builder, projects every replicatedJob's volumes and containers, and computes
// violations from the same data the projection records. The golden documents
// the shape a reviewer should expect, and the violation check keeps a
// regeneration from recording a regression in either direction (entries
// missing from the worker, or entries leaking onto the launcher).
func TestBuildRuntimeWorkloadFields(t *testing.T) {
	p := &testutil.TestCaseParser{Subdir: "build-runtime-workload-fields"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in workloadFieldsInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		cfg := baseRuntimeConfig()
		cfg.Volumes = in.Volumes
		cfg.VolumeMounts = in.VolumeMounts
		cfg.InitContainers = in.InitContainers
		cfg.GangSchedulerName = in.GangSchedulerName
		var dep nvcrev1alpha1.DependencySpec
		switch in.FrameworkType {
		case wfFrameworkMPI:
			dep = BuildMPIRuntime(cfg)
		case wfFrameworkTorch:
			dep = BuildTorchRuntime(cfg)
		case wfFrameworkExec:
			dep = BuildExecRuntime(cfg)
		default:
			return fmt.Errorf("unknown frameworkType %q", in.FrameworkType)
		}

		var rt trainerv1alpha1.TrainingRuntime
		if err := json.Unmarshal(dep.Raw, &rt); err != nil {
			return err
		}

		proj := projectWorkloadFields(in, rt)
		require.Empty(tc.T, proj.Violations, "workload field placement violated")

		out, err := json.MarshalIndent(proj, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(out) + "\n"
		return nil
	})
}

// projectWorkloadFields projects every replicatedJob in order and evaluates
// the placement predicates against the worker and, when present, the launcher.
func projectWorkloadFields(in workloadFieldsInput, rt trainerv1alpha1.TrainingRuntime) workloadFieldsProjection {
	proj := workloadFieldsProjection{ReplicatedJobs: []workloadFieldsJob{}, Violations: []string{}}
	sawNode, sawLauncher := false, false
	for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
		pod := rj.Template.Spec.Template.Spec
		job := workloadFieldsJob{ReplicatedJob: rj.Name, Volumes: []string{}, Containers: []workloadFieldsContainer{}}
		for _, v := range pod.Volumes {
			job.Volumes = append(job.Volumes, v.Name+": "+describeVolumeSource(v.VolumeSource))
		}
		for _, c := range pod.InitContainers {
			job.Containers = append(job.Containers, projectWorkloadFieldsContainer(c, true))
		}
		for _, c := range pod.Containers {
			job.Containers = append(job.Containers, projectWorkloadFieldsContainer(c, false))
		}
		proj.ReplicatedJobs = append(proj.ReplicatedJobs, job)

		switch rj.Name {
		case nodeJobName:
			sawNode = true
			checked, violations := workerPlacementViolations(in, pod)
			proj.UserItemsChecked += checked
			proj.Violations = append(proj.Violations, violations...)
		case launcherJobName:
			sawLauncher = true
			checked, violations := launcherPlacementViolations(in, pod)
			proj.UserItemsChecked += checked
			proj.Violations = append(proj.Violations, violations...)
		}
	}
	// A runtime with no "node" job, or on mpi no "launcher" job, gives the
	// predicates nothing to look at, and an empty violation list would then
	// prove nothing. Say so rather than pass vacuously.
	if userItems := len(in.Volumes) + len(in.VolumeMounts) + len(in.InitContainers); userItems > 0 {
		if !sawNode {
			proj.Violations = append(proj.Violations, "no replicatedJob named "+nodeJobName+" to check")
		}
		if in.FrameworkType == wfFrameworkMPI && !sawLauncher {
			proj.Violations = append(proj.Violations, "no replicatedJob named "+launcherJobName+" to check")
		}
	}
	sort.Strings(proj.Violations)
	return proj
}

func projectWorkloadFieldsContainer(c corev1.Container, init bool) workloadFieldsContainer {
	out := workloadFieldsContainer{Name: c.Name, Init: init, VolumeMounts: []string{}}
	for _, m := range c.VolumeMounts {
		out.VolumeMounts = append(out.VolumeMounts, describeMount(m))
	}
	return out
}

func describeMount(m corev1.VolumeMount) string {
	s := m.Name + " at " + m.MountPath
	if m.ReadOnly {
		s += " (ro)"
	}
	return s
}

// describeVolumeSource names the one VolumeSource field that is set, with the
// detail a reviewer needs to tell the user's volume from a built-in one.
func describeVolumeSource(src corev1.VolumeSource) string {
	switch {
	case src.HostPath != nil:
		s := "hostPath " + src.HostPath.Path
		if src.HostPath.Type != nil {
			s += " type=" + string(*src.HostPath.Type)
		}
		return s
	case src.EmptyDir != nil:
		if src.EmptyDir.Medium != "" {
			return "emptyDir medium=" + string(src.EmptyDir.Medium)
		}
		return "emptyDir"
	case src.ConfigMap != nil:
		return "configMap " + src.ConfigMap.Name
	case src.PersistentVolumeClaim != nil:
		return "persistentVolumeClaim " + src.PersistentVolumeClaim.ClaimName
	case src.Secret != nil:
		return "secret " + src.Secret.SecretName
	default:
		return "other"
	}
}

// workerPlacementViolations checks the "node" replicatedJob, the workload pod
// on every framework: every user entry present, in input order, after the
// built-ins, with the built-ins still in place.
func workerPlacementViolations(in workloadFieldsInput, pod corev1.PodSpec) (int, []string) {
	const where = nodeJobName + "/" + nodeJobName
	var violations []string
	checked := 0

	volumeNames := make([]string, 0, len(pod.Volumes))
	for _, v := range pod.Volumes {
		volumeNames = append(volumeNames, v.Name)
	}
	dshmAt := slices.Index(volumeNames, volumeNameDSHM)
	if dshmAt < 0 {
		violations = append(violations, where+": built-in volume "+volumeNameDSHM+" is missing")
	}
	lastAt := dshmAt
	for _, v := range in.Volumes {
		checked++
		at := slices.Index(volumeNames, v.Name)
		switch {
		case at < 0:
			violations = append(violations, where+": user volume "+v.Name+" is missing from the pod")
		case at <= lastAt:
			violations = append(violations, where+": user volume "+v.Name+" is out of order (built-ins first, then user entries as given)")
		default:
			lastAt = at
		}
	}

	var node *corev1.Container
	for i := range pod.Containers {
		if pod.Containers[i].Name == nodeJobName {
			node = &pod.Containers[i]
			break
		}
	}
	if node == nil {
		violations = append(violations, where+": no container named "+nodeJobName)
	} else {
		mounts := make([]string, 0, len(node.VolumeMounts))
		for _, m := range node.VolumeMounts {
			mounts = append(mounts, describeMount(m))
		}
		if in.FrameworkType == wfFrameworkMPI {
			if len(node.VolumeMounts) == 0 || node.VolumeMounts[0].Name != mpiSSHAuthName {
				violations = append(violations, where+": built-in mount "+mpiSSHAuthName+" is not first")
			}
		}
		dshmMountAt := slices.IndexFunc(node.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == volumeNameDSHM && m.MountPath == "/dev/shm"
		})
		if dshmMountAt < 0 {
			violations = append(violations, where+": built-in mount "+volumeNameDSHM+" at /dev/shm is missing")
		}
		lastMountAt := dshmMountAt
		for _, m := range in.VolumeMounts {
			checked++
			want := describeMount(m)
			at := slices.Index(mounts, want)
			switch {
			case at < 0:
				violations = append(violations, where+": user mount "+want+" is missing from the container")
			case at <= lastMountAt:
				violations = append(violations, where+": user mount "+want+" is out of order (built-ins first, then user entries as given)")
			default:
				lastMountAt = at
			}
		}
	}

	initNames := make([]string, 0, len(pod.InitContainers))
	for _, c := range pod.InitContainers {
		initNames = append(initNames, c.Name)
	}
	lastInitAt := -1
	for _, c := range in.InitContainers {
		checked++
		at := slices.Index(initNames, c.Name)
		switch {
		case at < 0:
			violations = append(violations, where+": user init container "+c.Name+" is missing from the pod")
		case at <= lastInitAt:
			violations = append(violations, where+": user init container "+c.Name+" is out of input order")
		default:
			lastInitAt = at
		}
	}
	return checked, violations
}

// launcherPlacementViolations checks the "launcher" replicatedJob (mpi only):
// none of the user's entries may appear on it. This pins the worker-only
// decision: the launcher hosts no rank and has fixed resources, so a user
// volume there could fail validation or attachment without helping the run.
func launcherPlacementViolations(in workloadFieldsInput, pod corev1.PodSpec) (int, []string) {
	const where = launcherJobName
	var violations []string
	checked := 0

	volumeNames := make([]string, 0, len(pod.Volumes))
	for _, v := range pod.Volumes {
		volumeNames = append(volumeNames, v.Name)
	}
	for _, v := range in.Volumes {
		checked++
		if slices.Contains(volumeNames, v.Name) {
			violations = append(violations, where+": user volume "+v.Name+" leaked onto the launcher pod")
		}
	}

	all := append(slices.Clone(pod.InitContainers), pod.Containers...)
	for _, m := range in.VolumeMounts {
		checked++
		for _, c := range all {
			if slices.ContainsFunc(c.VolumeMounts, func(cm corev1.VolumeMount) bool { return cm.Name == m.Name }) {
				violations = append(violations, where+"/"+c.Name+": user mount "+m.Name+" leaked onto the launcher")
			}
		}
	}

	initNames := make([]string, 0, len(pod.InitContainers))
	for _, c := range pod.InitContainers {
		initNames = append(initNames, c.Name)
	}
	for _, c := range in.InitContainers {
		checked++
		if slices.Contains(initNames, c.Name) {
			violations = append(violations, where+": user init container "+c.Name+" leaked onto the launcher pod")
		}
	}
	return checked, violations
}
