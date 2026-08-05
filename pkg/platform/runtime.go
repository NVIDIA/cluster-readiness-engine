// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"strconv"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// defaultWorkerMemory is the memory request/limit applied when a WorkloadRun
// does not specify spec.resources. Matches values used by training catalog
// entries (e.g. nemotron5-8b, nemotron5-56b).
const defaultWorkerMemory = "800Gi"

// defaultWorkerResources returns the standard worker resources block when the
// user did not provide spec.resources: GPU count plus a memory default.
func defaultWorkerResources(gpusPerNode int32) map[string]any {
	gpuStr := strconv.Itoa(int(gpusPerNode))
	return map[string]any{
		"limits": map[string]any{
			"nvidia.com/gpu": gpuStr,
			"memory":         defaultWorkerMemory,
		},
		"requests": map[string]any{
			"nvidia.com/gpu": gpuStr,
			"memory":         defaultWorkerMemory,
		},
	}
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
}

// BuildTorchRuntime creates a TrainingRuntime dependency for PyTorch distributed
// training. Generates a runtime with torch mlPolicy and a single "node" replicatedJob.
func BuildTorchRuntime(cfg RuntimeConfig) burninv1alpha1.DependencySpec {
	// Build container spec
	container := map[string]any{
		"name":  "node",
		"image": cfg.Image,
		"env":   cfg.Env,
	}

	if cfg.Resources != nil {
		container["resources"] = cfg.Resources
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
									"labels": map[string]any{
										"trainer.kubeflow.org/trainjob-ancestor-step": "trainer",
										"app": cfg.EntryName,
									},
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
	return burninv1alpha1.DependencySpec{
		RawExtension: runtime.RawExtension{Raw: data},
	}
}

// BuildMPIRuntime creates TrainingRuntime dependencies for MPI-based workloads.
// Generates:
// - A runtime with MPI mlPolicy and launcher+node replicatedJobs
// - Worker nodes with sshd, IPC_LOCK, readiness probe
// - Launcher with mpirun and SSH key setup
func BuildMPIRuntime(cfg RuntimeConfig) burninv1alpha1.DependencySpec {
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
		workerContainer["resources"] = cfg.Resources
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
						// Worker (node) replicatedJob
						map[string]any{
							"name": "node",
							"template": map[string]any{
								"spec": map[string]any{
									"template": map[string]any{
										"spec": map[string]any{
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
										},
									},
								},
							},
						},
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
									"labels": map[string]any{
										"trainer.kubeflow.org/trainjob-ancestor-step": "trainer",
									},
								},
								"spec": map[string]any{
									"template": map[string]any{
										"spec": map[string]any{
											"initContainers": []any{launcherInitContainer},
											"containers":     []any{launcherContainer},
											"restartPolicy":  "OnFailure",
											"volumes": []any{
												map[string]any{"name": "ssh-keys", "emptyDir": map[string]any{}},
											},
										},
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
	return burninv1alpha1.DependencySpec{
		RawExtension: runtime.RawExtension{Raw: data},
	}
}

// BuildExecRuntime creates a TrainingRuntime dependency for arbitrary command execution.
// Uses a simple single-replicatedJob layout with torch mlPolicy.
func BuildExecRuntime(cfg RuntimeConfig) burninv1alpha1.DependencySpec {
	// Exec uses the same runtime shape as torch (simple single-replicatedJob)
	// since the command is injected via the JobTemplate trainer field.
	return BuildTorchRuntime(cfg)
}
