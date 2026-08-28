// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"strconv"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
)

// gpuResourceName is the extended resource a container must request to be given
// a GPU. A container that does not name it is scheduled without one, however
// many GPUs the node has.
const gpuResourceName = corev1.ResourceName("nvidia.com/gpu")

// defaultWorkerResources returns the worker resources block when the user did
// not provide spec.resources.
//
// This used to add memory: 800Gi as well, copied from the training catalog
// entries, which set their own. NVCRE cannot know what an arbitrary WorkloadRun
// needs, and 800Gi is satisfiable only on a DGX-sized node, so omitting
// spec.resources left the pod Pending forever anywhere else. The GPU count is
// the part NVCRE does know, so it is the only part it fills in.
func defaultWorkerResources(gpusPerNode int32) map[string]any {
	gpuStr := strconv.Itoa(int(gpusPerNode))
	return map[string]any{
		"limits":   map[string]any{gpuResourceName.String(): gpuStr},
		"requests": map[string]any{gpuResourceName.String(): gpuStr},
	}
}

// withGPURequest returns res with nvidia.com/gpu filled in from gpusPerNode.
//
// The user's block used to be taken exactly as written, so a WorkloadRun that
// set spec.resources to avoid the memory default also lost its GPU request.
// The pod then scheduled onto a node it could not use, while numProcPerNode was
// still set from gpusPerNode, and the run died inside CUDA reporting a driver
// problem rather than a missing GPU.
//
// An explicit value is never overwritten, including an explicit zero, so
// asking for no GPU stays possible. If the user named the resource in either
// limits or requests, the block is left entirely alone.
func withGPURequest(res *corev1.ResourceRequirements, gpusPerNode int32) *corev1.ResourceRequirements {
	if res == nil || gpusPerNode <= 0 {
		return res
	}
	if _, ok := res.Limits[gpuResourceName]; ok {
		return res
	}
	if _, ok := res.Requests[gpuResourceName]; ok {
		return res
	}

	out := res.DeepCopy()
	qty := *resource.NewQuantity(int64(gpusPerNode), resource.DecimalSI)
	if out.Limits == nil {
		out.Limits = corev1.ResourceList{}
	}
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	out.Limits[gpuResourceName] = qty
	out.Requests[gpuResourceName] = qty
	return out
}

// RuntimeConfig holds all parameters needed to build a TrainingRuntime dependency.
type RuntimeConfig struct {
	// EntryName is the WorkloadRun name (used as prefix for resource names).
	// Matches catalog template variable .EntryName.
	EntryName string
	// Image is the container image.
	Image string
	// NodesPerJob is the number of nodes.
	NodesPerJob int32
	// GpusPerNode is the number of GPUs per node.
	GpusPerNode int32
	// Env is the merged env vars (base NCCL + user).
	Env []corev1.EnvVar
	// Volumes are additional volumes.
	Volumes []corev1.Volume
	// VolumeMounts are additional volume mounts.
	VolumeMounts []corev1.VolumeMount
	// InitContainers are user-provided init containers.
	InitContainers []corev1.Container
	// Resources overrides GPU/memory/CPU resources.
	Resources *corev1.ResourceRequirements
	// ImagePullSecrets for container pull.
	ImagePullSecrets []corev1.LocalObjectReference
	// GangSchedulerName is the scheduler name to inject into pod specs (e.g. "kai-scheduler").
	// Empty means no gang scheduler is configured.
	GangSchedulerName string
	// GangSchedulerQueue is the queue label value for the gang scheduler.
	// Defaults to "default-queue" when GangSchedulerName is set and Queue is empty.
	GangSchedulerQueue string
}

// gangSchedulerQueue returns the effective queue name, defaulting to "default-queue".
func gangSchedulerQueue(cfg RuntimeConfig) string {
	if cfg.GangSchedulerQueue != "" {
		return cfg.GangSchedulerQueue
	}
	return "default-queue"
}

// applyGangScheduler injects schedulerName into the pod spec and the queue label
// into the pod template metadata labels when a gang scheduler is configured.
// podSpec is the pod spec map (sets schedulerName).
// podLabels is the pod template metadata labels map (sets kai.scheduler/queue).
func applyGangScheduler(cfg RuntimeConfig, podSpec, podLabels map[string]any) {
	if cfg.GangSchedulerName == "" {
		return
	}
	podSpec["schedulerName"] = cfg.GangSchedulerName
	podLabels["kai.scheduler/queue"] = gangSchedulerQueue(cfg)
}

// BuildTorchRuntime creates a TrainingRuntime dependency for PyTorch distributed
// training. Generates a runtime with torch mlPolicy and a single "node" replicatedJob.
func BuildTorchRuntime(cfg RuntimeConfig) nvcrev1alpha1.DependencySpec {
	// Build container spec
	container := map[string]any{
		"name":  "node",
		"image": cfg.Image,
		"env":   cfg.Env,
	}

	if cfg.Resources != nil {
		container["resources"] = withGPURequest(cfg.Resources, cfg.GpusPerNode)
	} else {
		container["resources"] = defaultWorkerResources(cfg.GpusPerNode)
	}

	// Volume mounts: shared memory + user-provided.
	mounts := make([]corev1.VolumeMount, 0, 1+len(cfg.VolumeMounts))
	mounts = append(mounts, corev1.VolumeMount{Name: "dshm", MountPath: "/dev/shm"})
	mounts = append(mounts, cfg.VolumeMounts...)
	container["volumeMounts"] = mounts

	// Volumes: shared memory + user-provided.
	volumes := make([]any, 0, 1+len(cfg.Volumes))
	volumes = append(volumes, map[string]any{
		"name":     "dshm",
		"emptyDir": map[string]any{"medium": "Memory"},
	})
	for _, v := range cfg.Volumes {
		volumes = append(volumes, v)
	}

	// Pod spec
	podSpec := map[string]any{
		"terminationGracePeriodSeconds": 30,
		"securityContext": map[string]any{
			"seccompProfile": map[string]any{
				"type": "RuntimeDefault",
			},
		},
		"containers": []any{container},
		"volumes":    volumes,
	}

	// Add init containers if provided
	if len(cfg.InitContainers) > 0 {
		podSpec["initContainers"] = cfg.InitContainers
	}

	podLabels := map[string]any{
		"trainer.kubeflow.org/trainjob-ancestor-step": "trainer",
		"app": cfg.EntryName,
	}
	applyGangScheduler(cfg, podSpec, podLabels)

	rt := map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "TrainingRuntime",
		"metadata": map[string]any{
			"name": fmt.Sprintf("%s-runtime", cfg.EntryName),
			"labels": map[string]any{
				"trainer.kubeflow.org/framework": "torch",
				"app":                            cfg.EntryName,
			},
		},
		"spec": map[string]any{
			"mlPolicy": map[string]any{
				"numNodes": cfg.NodesPerJob,
				"torch": map[string]any{
					"numProcPerNode": cfg.GpusPerNode,
				},
			},
			"template": map[string]any{
				"spec": map[string]any{
					"replicatedJobs": []any{
						map[string]any{
							"name": "node",
							"template": map[string]any{
								"metadata": map[string]any{
									"labels": podLabels,
								},
								"spec": map[string]any{
									"template": map[string]any{
										"spec": podSpec,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	data, _ := json.Marshal(rt)
	return nvcrev1alpha1.DependencySpec{
		RawExtension: runtime.RawExtension{Raw: data},
	}
}

// BuildMPIRuntime creates TrainingRuntime dependencies for MPI-based workloads.
// Generates:
// - A runtime with MPI mlPolicy and launcher+node replicatedJobs
// - Worker nodes with sshd, IPC_LOCK, readiness probe
// - Launcher with mpirun and SSH key setup
func BuildMPIRuntime(cfg RuntimeConfig) nvcrev1alpha1.DependencySpec {
	// Worker (node) container: runs sshd
	workerContainer := map[string]any{
		"name":    "node",
		"image":   cfg.Image,
		"command": []string{"sh", "-c"},
		"args": []string{
			"set -x && " +
				"apt-get update && " +
				"apt-get install -y --no-install-recommends openssh-server && " +
				"mkdir -p /var/run/sshd && " +
				"chmod 0755 /var/run/sshd && " +
				"mkdir -p /root/.ssh && " +
				"chmod 700 /root/.ssh && " +
				"cp /tmp/mpi-ssh-raw/* /root/.ssh/ && " +
				"chmod 600 /root/.ssh/id_rsa && " +
				"chmod 644 /root/.ssh/id_rsa.pub /root/.ssh/authorized_keys && " +
				"/usr/sbin/sshd -De",
		},
		"readinessProbe": map[string]any{
			"initialDelaySeconds": 5,
			"tcpSocket": map[string]any{
				"port": 22,
			},
		},
		"securityContext": map[string]any{
			"capabilities": map[string]any{
				"add": []string{"IPC_LOCK"},
			},
		},
		"volumeMounts": []map[string]any{
			{"name": "mpi-ssh-auth", "mountPath": "/tmp/mpi-ssh-raw", "readOnly": true},
			{"name": "dshm", "mountPath": "/dev/shm"},
		},
	}

	if cfg.Resources != nil {
		workerContainer["resources"] = withGPURequest(cfg.Resources, cfg.GpusPerNode)
	} else {
		workerContainer["resources"] = defaultWorkerResources(cfg.GpusPerNode)
	}

	// Launcher init container: fix SSH permissions
	launcherInitContainer := map[string]any{
		"name":    "fix-ssh-permissions",
		"image":   cfg.Image,
		"command": []string{"sh", "-c"},
		"args": []string{
			"set -x && " +
				"cp /tmp/mpi-ssh-raw/* /root/.ssh/ && " +
				"chmod 600 /root/.ssh/id_rsa && " +
				"chmod 644 /root/.ssh/id_rsa.pub /root/.ssh/authorized_keys",
		},
		"volumeMounts": []map[string]any{
			{"name": "mpi-ssh-auth", "mountPath": "/tmp/mpi-ssh-raw", "readOnly": true},
			{"name": "ssh-keys", "mountPath": "/root/.ssh"},
		},
	}

	// Launcher container
	launcherContainer := map[string]any{
		"name":  "node",
		"image": cfg.Image,
		"resources": map[string]any{
			"limits": map[string]any{
				"cpu":    "2",
				"memory": "1Gi",
			},
		},
		"volumeMounts": []map[string]any{
			{"name": "mpi-ssh-auth", "mountPath": "/tmp/mpi-ssh-raw", "readOnly": true},
			{"name": "ssh-keys", "mountPath": "/root/.ssh"},
		},
	}

	workerPodSpec := map[string]any{
		"containers":    []any{workerContainer},
		"restartPolicy": "OnFailure",
		"volumes": []any{
			map[string]any{
				"name": "dshm",
				"emptyDir": map[string]any{
					"medium": "Memory",
				},
			},
		},
	}
	workerPodLabels := map[string]any{}
	applyGangScheduler(cfg, workerPodSpec, workerPodLabels)

	workerReplicatedJob := map[string]any{
		"name": "node",
		"template": map[string]any{
			"spec": map[string]any{
				"template": map[string]any{
					"spec": workerPodSpec,
				},
			},
		},
	}
	if len(workerPodLabels) > 0 {
		workerReplicatedJob["template"].(map[string]any)["metadata"] = map[string]any{
			"labels": workerPodLabels,
		}
	}

	launcherPodSpec := map[string]any{
		"initContainers": []any{launcherInitContainer},
		"containers":     []any{launcherContainer},
		"restartPolicy":  "OnFailure",
		"volumes": []any{
			map[string]any{"name": "ssh-keys", "emptyDir": map[string]any{}},
		},
	}
	launcherPodLabels := map[string]any{
		"trainer.kubeflow.org/trainjob-ancestor-step": "trainer",
	}
	applyGangScheduler(cfg, launcherPodSpec, launcherPodLabels)

	rt := map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "TrainingRuntime",
		"metadata": map[string]any{
			"name": fmt.Sprintf("%s-runtime", cfg.EntryName),
			"labels": map[string]any{
				"trainer.kubeflow.org/framework": "mpi",
				"app":                            cfg.EntryName,
			},
		},
		"spec": map[string]any{
			"mlPolicy": map[string]any{
				"numNodes": 1, // MPI launcher runs on 1 node; workers scale via replicatedJobs
				"mpi": map[string]any{
					"mpiImplementation": "OpenMPI",
					"numProcPerNode":    cfg.GpusPerNode,
					"sshAuthMountPath":  "/tmp/mpi-ssh-raw",
				},
			},
			"template": map[string]any{
				"spec": map[string]any{
					"network": map[string]any{
						"publishNotReadyAddresses": true,
					},
					"replicatedJobs": []any{
						workerReplicatedJob,
						// Launcher replicatedJob
						map[string]any{
							"name": "launcher",
							"dependsOn": []any{
								map[string]any{
									"name":   "node",
									"status": "Ready",
								},
							},
							"template": map[string]any{
								"metadata": map[string]any{
									"labels": launcherPodLabels,
								},
								"spec": map[string]any{
									"template": map[string]any{
										"spec": launcherPodSpec,
									},
								},
							},
						},
					},
					"successPolicy": map[string]any{
						"operator":             "All",
						"targetReplicatedJobs": []string{"launcher"},
					},
				},
			},
		},
	}

	data, _ := json.Marshal(rt)
	return nvcrev1alpha1.DependencySpec{
		RawExtension: runtime.RawExtension{Raw: data},
	}
}

// BuildExecRuntime creates a TrainingRuntime dependency for arbitrary command execution.
// Uses a simple single-replicatedJob layout with torch mlPolicy.
func BuildExecRuntime(cfg RuntimeConfig) nvcrev1alpha1.DependencySpec {
	// Exec uses the same runtime shape as torch (simple single-replicatedJob)
	// since the command is injected via the JobTemplate trainer field.
	return BuildTorchRuntime(cfg)
}
