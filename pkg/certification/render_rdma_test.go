// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// rdmaContainer records the host-access surface of one runtime container.
// Capabilities are projected next to Privileged because the two must coexist:
// the hostPath patch merges a privileged flag into a securityContext that
// already carries IPC_LOCK, and a merge that replaced the block instead of
// recursing into it would drop the capability silently.
type rdmaContainer struct {
	Name         string   `json:"name"`
	Privileged   *bool    `json:"privileged"`
	Capabilities []string `json:"capabilities"`
	// Resources renders limits and requests as sorted "limits/<name>=<qty>"
	// and "requests/<name>=<qty>" entries, the same way the mlnx suite
	// projects them. The hostPath patch merges into containers that already
	// carry an EFA or GPU resources block, and a merge that replaced the
	// container instead of recursing would drop those requests while every
	// other field here still looked right.
	Resources []string `json:"resources"`
	// VolumeMounts renders in declaration order as "<name>=<mountPath>", with
	// ":ro" appended when the mount is read-only. The suffix matters for
	// /dev/infiniband specifically: the verbs character devices are opened
	// read-write, so a read-only mount would reproduce the EPERM the opt-in
	// exists to fix, and would do it while every other field in this
	// projection still looked correct.
	VolumeMounts []string `json:"volumeMounts"`
}

// rdmaReplicatedJob records one replicatedJob's containers and the pod volumes
// backing their mounts. A mount without its volume is a pod that never starts,
// so the two are pinned together.
type rdmaReplicatedJob struct {
	Dependency    string          `json:"dependency"`
	ReplicatedJob string          `json:"replicatedJob"`
	Containers    []rdmaContainer `json:"containers"`
	// Volumes renders as "<name>=<source>", where source is the hostPath path
	// and type for a hostPath volume and the source kind otherwise.
	Volumes []string `json:"volumes"`
}

// rdmaWorkflow is the per-Workflow projection written to the golden file.
// TrainerArgs and TrainerEnv carry the issue #456 half: the RoCE environment
// reaches the ranks through mpirun -x args on the MPI entries and through the
// container env on the torch entries, and the launcher reads its own env.
type rdmaWorkflow struct {
	Workflow       string              `json:"workflow"`
	TrainerArgs    []string            `json:"trainerArgs"`
	TrainerEnv     []string            `json:"trainerEnv"`
	ReplicatedJobs []rdmaReplicatedJob `json:"replicatedJobs"`
}

// TestCertificationRenderRDMA covers both halves of ADR-088 through the same
// path "nvcrectl certification render --platform <csp>" uses.
//
// Issue #456: OCI GB200 rendered none of the OCI RoCE environment. On the five
// communication entries the platform: oci block was discarded by the later
// gpuArchitecture in [gb200, gb300] block, which replaces trainer.args (and
// trainer.env, which has no patchMergeKey tags) atomically. On the two training
// entries no OCI GB200 arm existed at all. The oci-gb200-* goldens pin the
// composed result; oci-gb300-nccl-control and oci-l40s-nccl-control pin that
// the two architectures that already worked are untouched, which is what
// catches an arm inserted at the wrong point in the document.
//
// The rdmaDeviceAccess opt-in: hostPath mounts /dev/infiniband and runs the
// workload container privileged, because kubelet builds the device cgroup
// allowlist from requested resources and a mount alone leaves open() failing
// with EPERM. The device-plugin golden is the mutation test for the
// {{- if eq .RDMADeviceAccess "hostPath" }} guard: deleting the guard renders
// the patch at every value of the field and turns that case red.
func TestCertificationRenderRDMA(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-rdma",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `json:"platform"`
			GPUArch  string `json:"gpuArch"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		certPath := filepath.Join(tc.T.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}

		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		gpuArch, err := catalog.ParseGPUArchFlag(cfg.GPUArch)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, cfg.Platform, gpuArch, nil, "")
		if err != nil {
			return err
		}
		if err := resolveWorkflowsOffline(cert, workflows, cfg.Platform, gpuArch); err != nil {
			return err
		}

		result := make([]rdmaWorkflow, 0, len(workflows))
		for i := range workflows {
			projected, projectErr := projectRDMA(&workflows[i])
			if projectErr != nil {
				return projectErr
			}
			result = append(result, projected)
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// projectRDMA walks a resolved Workflow in declaration order throughout:
// trainer args and env as the overrides composed them, then dependencies as the
// catalog lists them and replicatedJobs as the runtime lists them. Nothing is
// sorted, because the order of an mpirun arg list and of an env array is part
// of what these goldens pin.
func projectRDMA(wf *nvcrev1alpha1.Workflow) (rdmaWorkflow, error) {
	out := rdmaWorkflow{
		Workflow:       wf.Name,
		TrainerArgs:    []string{},
		TrainerEnv:     []string{},
		ReplicatedJobs: []rdmaReplicatedJob{},
	}

	if tj := wf.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
		out.TrainerArgs = append(out.TrainerArgs, tj.Trainer.Args...)
		for _, e := range tj.Trainer.Env {
			out.TrainerEnv = append(out.TrainerEnv, fmt.Sprintf("%s=%s", e.Name, e.Value))
		}
	}

	for i := range wf.Spec.Dependencies {
		raw := wf.Spec.Dependencies[i].Raw
		if len(raw) == 0 {
			continue
		}

		var typeMeta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return out, err
		}
		if typeMeta.Kind != trainerv1alpha1.TrainingRuntimeKind {
			continue
		}

		var rt trainerv1alpha1.TrainingRuntime
		if err := json.Unmarshal(raw, &rt); err != nil {
			return out, err
		}
		for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
			podSpec := rj.Template.Spec.Template.Spec
			projected := rdmaReplicatedJob{
				Dependency:    rt.Name,
				ReplicatedJob: rj.Name,
				Containers:    []rdmaContainer{},
				Volumes:       []string{},
			}
			for _, c := range podSpec.Containers {
				projected.Containers = append(projected.Containers, projectRDMAContainer(c))
			}
			for _, v := range podSpec.Volumes {
				projected.Volumes = append(projected.Volumes, fmt.Sprintf("%s=%s", v.Name, volumeSource(v)))
			}
			out.ReplicatedJobs = append(out.ReplicatedJobs, projected)
		}
	}
	return out, nil
}

func projectRDMAContainer(c corev1.Container) rdmaContainer {
	pc := rdmaContainer{
		Name:         c.Name,
		Capabilities: []string{},
		Resources:    []string{},
		VolumeMounts: []string{},
	}
	for name, qty := range c.Resources.Limits {
		pc.Resources = append(pc.Resources, fmt.Sprintf("limits/%s=%s", name, qty.String()))
	}
	for name, qty := range c.Resources.Requests {
		pc.Resources = append(pc.Resources, fmt.Sprintf("requests/%s=%s", name, qty.String()))
	}
	sort.Strings(pc.Resources)
	if sc := c.SecurityContext; sc != nil {
		pc.Privileged = sc.Privileged
		if sc.Capabilities != nil {
			for _, add := range sc.Capabilities.Add {
				pc.Capabilities = append(pc.Capabilities, string(add))
			}
		}
	}
	for _, m := range c.VolumeMounts {
		entry := fmt.Sprintf("%s=%s", m.Name, m.MountPath)
		if m.ReadOnly {
			entry += ":ro"
		}
		pc.VolumeMounts = append(pc.VolumeMounts, entry)
	}
	return pc
}

// volumeSource renders a volume's backing store. hostPath is spelled out with
// its path and type because those are the two fields the opt-in sets; every
// other source collapses to its kind, which is enough to pin that an unrelated
// volume neither appeared nor vanished.
func volumeSource(v corev1.Volume) string {
	switch {
	case v.HostPath != nil:
		hostType := ""
		if v.HostPath.Type != nil {
			hostType = string(*v.HostPath.Type)
		}
		return fmt.Sprintf("hostPath:%s:%s", v.HostPath.Path, hostType)
	case v.EmptyDir != nil:
		return fmt.Sprintf("emptyDir:%s", v.EmptyDir.Medium)
	case v.ConfigMap != nil:
		return "configMap:" + v.ConfigMap.Name
	case v.PersistentVolumeClaim != nil:
		return "pvc:" + v.PersistentVolumeClaim.ClaimName
	case v.Secret != nil:
		return "secret:" + v.Secret.SecretName
	default:
		return "other"
	}
}
