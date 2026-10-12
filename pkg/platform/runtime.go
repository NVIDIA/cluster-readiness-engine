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
)

// gpuResourceName is the extended resource a container must request to be given
// a GPU. A container that does not name it is scheduled without one, however
// many GPUs the node has.
const gpuResourceName = corev1.ResourceName("nvidia.com/gpu")

// Repeated map keys and literal values used when building the unstructured
// TrainingRuntime manifests below.
const (
	keyName           = "name"
	keyImage          = "image"
	keyEnv            = "env"
	keyCommand        = "command"
	keyArgs           = "args"
	keyEmptyDir       = "emptyDir"
	keyContainers     = "containers"
	keyInitContainers = "initContainers"
	keyVolumes        = "volumes"
	keyMetadata       = "metadata"
	keyLabels         = "labels"
	keySpec           = "spec"
	keyTemplate       = "template"
	keyVolumeMounts   = "volumeMounts"
	keyMountPath      = "mountPath"

	volumeNameDSHM    = "dshm"
	volumeNameSSHKeys = "ssh-keys"
	labelKeyApp       = "app"
	mpiSSHMountPath   = "/tmp/mpi-ssh-raw"
	mpiSSHKeyDir      = "/root/.ssh"
	// mpiHostfileDir/mpiHostfilePath mirror the Kubeflow Trainer MPI plugin's
	// hostfile contract (constants.MPIHostfileDir / MPIHostfileName): the
	// plugin mounts a ConfigMap holding "<endpoint> slots=<n>" lines into the
	// launcher pod at this path.
	mpiHostfileDir  = "/etc/mpi"
	mpiHostfilePath = "/etc/mpi/hostfile"
	// volumeNameMPIHostfile is the volume the Trainer MPI plugin adds to the
	// launcher pod for that ConfigMap (constants.MPIHostfileVolumeName). The
	// launcher's wait init container mounts it by name.
	volumeNameMPIHostfile = "mpi-hostfile"
	nodeJobName           = "node"
	launcherJobName       = "launcher"
	mpiSSHAuthName        = "mpi-ssh-auth"
	keyReadOnly           = "readOnly"
)

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
	// GangSchedulerQueueLabelKey is the label key the queue value is written
	// under. Defaults to "kai.scheduler/queue" when GangSchedulerName is set
	// and the key is empty.
	GangSchedulerQueueLabelKey string
}

// applyGangScheduler injects schedulerName into the pod spec and the queue
// label into both label maps when a gang scheduler is configured.
// podSpec is the pod spec map (sets schedulerName).
// jobLabels is the Job template metadata labels map and podLabels the pod
// template metadata labels map; the queue label is written to both under
// cfg.GangSchedulerQueueLabelKey ("kai.scheduler/queue" when empty), so the
// pods carry the label without relying on Trainer/JobSet label propagation.
func applyGangScheduler(cfg RuntimeConfig, podSpec, jobLabels, podLabels map[string]any) {
	if cfg.GangSchedulerName == "" {
		return
	}
	podSpec[keySchedulerName] = cfg.GangSchedulerName
	queueKey := gangSchedulerQueueLabelKey(cfg.GangSchedulerQueueLabelKey)
	queue := gangSchedulerQueue(cfg.GangSchedulerQueue)
	jobLabels[queueKey] = queue
	podLabels[queueKey] = queue
}

// BuildTorchRuntime creates a TrainingRuntime dependency for PyTorch distributed
// training. Generates a runtime with torch mlPolicy and a single "node" replicatedJob.
func BuildTorchRuntime(cfg RuntimeConfig) nvcrev1alpha1.DependencySpec {
	// Build container spec
	container := map[string]any{
		keyName:  nodeJobName,
		keyImage: cfg.Image,
		keyEnv:   cfg.Env,
	}

	if cfg.Resources != nil {
		container["resources"] = withGPURequest(cfg.Resources, cfg.GpusPerNode)
	} else {
		container["resources"] = defaultWorkerResources(cfg.GpusPerNode)
	}

	// Volume mounts: shared memory + user-provided.
	mounts := make([]corev1.VolumeMount, 0, 1+len(cfg.VolumeMounts))
	mounts = append(mounts, corev1.VolumeMount{Name: volumeNameDSHM, MountPath: "/dev/shm"})
	mounts = append(mounts, cfg.VolumeMounts...)
	container[keyVolumeMounts] = mounts

	// Volumes: shared memory + user-provided.
	volumes := make([]any, 0, 1+len(cfg.Volumes))
	volumes = append(volumes, map[string]any{
		keyName:     volumeNameDSHM,
		keyEmptyDir: map[string]any{"medium": "Memory"},
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
		keyContainers: []any{container},
		keyVolumes:    volumes,
	}

	// Add init containers if provided
	if len(cfg.InitContainers) > 0 {
		podSpec["initContainers"] = cfg.InitContainers
	}

	jobLabels := map[string]any{
		"trainer.kubeflow.org/trainjob-ancestor-step": "trainer",
		labelKeyApp: cfg.EntryName,
	}
	podLabels := map[string]any{}
	applyGangScheduler(cfg, podSpec, jobLabels, podLabels)

	// Pod template: replicatedJobs[].template.spec.template. Its metadata only
	// exists when gang scheduling put the queue label there, so a runtime
	// without a gang scheduler renders byte-identically to before.
	podTemplate := map[string]any{
		keySpec: podSpec,
	}
	if len(podLabels) > 0 {
		podTemplate[keyMetadata] = map[string]any{
			keyLabels: podLabels,
		}
	}

	rt := map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "TrainingRuntime",
		keyMetadata: map[string]any{
			keyName: fmt.Sprintf("%s-runtime", cfg.EntryName),
			keyLabels: map[string]any{
				"trainer.kubeflow.org/framework": "torch",
				labelKeyApp:                      cfg.EntryName,
			},
		},
		keySpec: map[string]any{
			"mlPolicy": map[string]any{
				"numNodes": cfg.NodesPerJob,
				"torch": map[string]any{
					"numProcPerNode": cfg.GpusPerNode,
				},
			},
			keyTemplate: map[string]any{
				keySpec: map[string]any{
					"replicatedJobs": []any{
						map[string]any{
							keyName: nodeJobName,
							keyTemplate: map[string]any{
								keyMetadata: map[string]any{
									keyLabels: jobLabels,
								},
								keySpec: map[string]any{
									keyTemplate: podTemplate,
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
		Raw: data,
	}
}

// sshdProbeCommand is the worker's readiness check: a real loopback SSH
// session, not a TCP connect.
//
// A tcpSocket probe on port 22 answers "is something listening", which is not
// the question the launcher's dependsOn gate is asking. sshd binds the port
// before it can serve anything and keeps the listener up no matter how badly
// individual sessions fail, so every way a worker can accept connections and
// still refuse work reads as Ready: unreadable host keys, authorized_keys
// written with the wrong mode, and the issue #461 case, where privilege
// separation cannot chroot and sshd kills each session preauth. The gate then
// releases mpirun against workers that refuse every orted, and the only symptom
// is the generic ORTE banner.
//
// A banner-level check would not close this. OpenSSH writes its version string
// in kex_exchange_identification, before privsep_preauth forks and chroots
// (sshd.c; sshd-session.c from 9.8), so a worker failing exactly the way #461
// fails still greets a prober. The chroot is on the path to authentication, so
// authentication is the earliest point that can observe it: the probe has to
// open a real session.
//
// When coreutils `timeout` is present the session runs under `timeout 8`,
// inside the probe's own 10s budget, so a hung connection is reported with
// ssh's stderr rather than the kubelet's generic "command timed out", and the
// probe stays bounded on a kubelet whose ExecProbeTimeout gate is off. Without
// `timeout` the same ssh runs bare and the kubelet's timeoutSeconds is the
// bound; neither helper's absence turns a healthy worker into a failing one.
//
// It dials 127.0.0.1, never a peer, so the probe reports on this worker alone.
// Probing other workers would couple their readiness together and deadlock on
// whichever starts first. `true` runs under the user's login shell, so the
// session is exercised end to end rather than stopping at authentication.
//
// The `command -v ssh` guard keeps the probe honest on an image that ships sshd
// without the client. Every image NVCRE renders today has both (on Debian and
// Ubuntu openssh-server depends on openssh-client, and the fallback path
// installs it), but a user-supplied image in a WorkloadRun need not, and a
// probe that fails because the prober is missing would report a healthy worker
// as broken forever. The fallback reads /proc/net/tcp directly, which needs
// nothing but grep: a listening socket on port 22 has a local address ending
// in :0016 (22 in hex) and state 0A (TCP_LISTEN). That is the same question
// the old tcpSocket probe asked, so such a worker keeps exactly what it had.
const sshdProbeCommand = `command -v ssh >/dev/null 2>&1 || exec grep -q ':0016 [0-9A-F]*:[0-9A-F]* 0A ' /proc/net/tcp /proc/net/tcp6
set -- ssh -n -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 127.0.0.1 true
command -v timeout >/dev/null 2>&1 && set -- timeout 8 "$@"
exec "$@"`

// sshdReadinessProbe returns the probe block for a container running sshd.
//
// timeoutSeconds is 10 so the 8s `timeout` around ssh expires first; the
// field's default of 1s would fail a healthy worker under load.
//
// periodSeconds is 30 rather than the default 10 because the probe keeps
// running for the pod's whole life while nothing consumes its verdict after
// the launcher's dependsOn gate has fired, and sshd -e logs every accepted
// session into the worker's log. Thirty seconds keeps that to two sessions a
// minute. The cost is up to 30s of added gate latency on an image that
// installs openssh-server at start; a prebaked image passes the first probe
// at 5s. failureThreshold keeps its default: readiness starts out failed and
// flips on the first success, so the threshold never delays a worker becoming
// Ready, and after the gate nothing depends on the Ready-to-NotReady edge
// (JobSet publishes not-ready addresses, so DNS does not either).
//
// A worker that never passes this probe never releases the gate, so the
// launcher is never created and the Job stays InProgress until timeoutPerJob.
// The worker's own events carry the reason (`Readiness probe failed` with
// ssh's stderr), which is the trade this probe makes: a slower terminal
// failure in exchange for a direct diagnosis instead of the ORTE banner.
func sshdReadinessProbe() map[string]any {
	return map[string]any{
		"initialDelaySeconds": 5,
		"periodSeconds":       30,
		"timeoutSeconds":      10,
		"exec": map[string]any{
			keyCommand: []string{"sh", "-c", sshdProbeCommand},
		},
	}
}

// BuildMPIRuntime creates TrainingRuntime dependencies for MPI-based workloads.
// Generates:
// - A runtime with MPI mlPolicy and launcher+node replicatedJobs
// - Worker nodes with sshd, capabilities, an SSH readiness probe, and cfg.Env
// - Launcher with mpirun, SSH key setup, and cfg.Env
//
// The worker's openssh-server install is guarded by `test -x /usr/sbin/sshd`,
// so images that already ship sshd start without any package-manager egress
// (issue #319: air-gapped and restricted-egress clusters). The guard tests
// the exact path the chain execs (/usr/sbin/sshd); a PATH lookup could
// disagree with it in either direction.
//
// cfg.Env goes on both containers as container-level env, the same way
// BuildTorchRuntime emits it (issue #68: it used to be dropped here, so
// spec.env behaved differently between the two frameworks). On the launcher
// the variables reach mpirun directly; on the worker they reach the sshd
// process, but sshd gives each SSH session a fresh environment, so ranks
// launched through it may not inherit them. A variable that must reach the
// ranks should be passed as "-x NAME=value" in mpiArgs, which forwards it
// through mpirun itself.
func BuildMPIRuntime(cfg RuntimeConfig) nvcrev1alpha1.DependencySpec {
	// Worker (node) container: runs sshd
	workerContainer := map[string]any{
		keyName:    nodeJobName,
		keyImage:   cfg.Image,
		keyEnv:     cfg.Env,
		keyCommand: []string{"sh", "-c"},
		keyArgs: []string{
			"set -x && " +
				"test -x /usr/sbin/sshd || (apt-get update && apt-get install -y --no-install-recommends openssh-server) && " +
				"mkdir -p /var/run/sshd && " +
				"chmod 0755 /var/run/sshd && " +
				"mkdir -p /root/.ssh && " +
				"chmod 700 /root/.ssh && " +
				"cp /tmp/mpi-ssh-raw/* /root/.ssh/ && " +
				"chmod 600 /root/.ssh/id_rsa && " +
				"chmod 644 /root/.ssh/id_rsa.pub /root/.ssh/authorized_keys && " +
				"/usr/sbin/sshd -De",
		},
		"readinessProbe": sshdReadinessProbe(),
		"securityContext": map[string]any{
			"capabilities": map[string]any{
				// SYS_CHROOT is what sshd needs, not what the workload needs.
				// OpenSSH has made privilege separation mandatory since 7.5, so
				// every inbound session chroots to the compiled-in PRIVSEP_PATH
				// before authenticating. Docker and containerd grant SYS_CHROOT
				// in their default set, which is why this went unnoticed; CRI-O
				// dropped it from default_capabilities, so on OpenShift the
				// chroot returns EPERM and sshd kills the session preauth while
				// the listener stays up (issue #461).
				"add": []string{"IPC_LOCK", "SYS_CHROOT"},
			},
		},
		keyVolumeMounts: []map[string]any{
			{keyName: mpiSSHAuthName, keyMountPath: mpiSSHMountPath, keyReadOnly: true},
			{keyName: volumeNameDSHM, keyMountPath: "/dev/shm"},
		},
	}

	if cfg.Resources != nil {
		workerContainer["resources"] = withGPURequest(cfg.Resources, cfg.GpusPerNode)
	} else {
		workerContainer["resources"] = defaultWorkerResources(cfg.GpusPerNode)
	}

	// Launcher init container: fix SSH permissions, and — under KAI, where the
	// JobSet cannot order the launcher behind the workers (see
	// isKAIGangScheduler) — wait until every worker answers sshd before mpirun
	// dials it. The worker list is Trainer's MPI hostfile, mounted into the
	// launcher pod by the Trainer MPI plugin; its entries are
	// "<endpoint> slots=<n>".
	launcherInitScript := "set -x && " +
		"cp /tmp/mpi-ssh-raw/* /root/.ssh/ && " +
		"chmod 600 /root/.ssh/id_rsa && " +
		"chmod 644 /root/.ssh/id_rsa.pub /root/.ssh/authorized_keys"
	launcherInitMounts := []map[string]any{
		{keyName: mpiSSHAuthName, keyMountPath: mpiSSHMountPath, keyReadOnly: true},
		{keyName: volumeNameSSHKeys, keyMountPath: mpiSSHKeyDir},
	}
	if isKAIGangScheduler(cfg) {
		launcherInitScript += launcherWaitScript()
		launcherInitMounts = append(launcherInitMounts, launcherWaitMount())
	}
	launcherInitContainer := map[string]any{
		keyName:         "fix-ssh-permissions",
		keyImage:        cfg.Image,
		keyCommand:      []string{"sh", "-c"},
		keyArgs:         []string{launcherInitScript},
		keyVolumeMounts: launcherInitMounts,
	}

	// Launcher container
	launcherContainer := map[string]any{
		keyName:  nodeJobName,
		keyImage: cfg.Image,
		keyEnv:   cfg.Env,
		"resources": map[string]any{
			"limits": map[string]any{
				"cpu":    "2",
				"memory": "1Gi",
			},
		},
		keyVolumeMounts: []map[string]any{
			{keyName: mpiSSHAuthName, keyMountPath: mpiSSHMountPath, keyReadOnly: true},
			{keyName: volumeNameSSHKeys, keyMountPath: mpiSSHKeyDir},
		},
	}

	workerPodSpec := map[string]any{
		keyContainers:   []any{workerContainer},
		"restartPolicy": "OnFailure",
		keyVolumes: []any{
			map[string]any{
				keyName: volumeNameDSHM,
				keyEmptyDir: map[string]any{
					"medium": "Memory",
				},
			},
		},
	}
	workerJobLabels := map[string]any{}
	workerPodLabels := map[string]any{}
	applyGangScheduler(cfg, workerPodSpec, workerJobLabels, workerPodLabels)

	// Worker pod template: metadata only exists when gang scheduling put the
	// queue label there, mirroring the Job template metadata below.
	workerPodTemplate := map[string]any{
		keySpec: workerPodSpec,
	}
	if len(workerPodLabels) > 0 {
		workerPodTemplate[keyMetadata] = map[string]any{
			keyLabels: workerPodLabels,
		}
	}

	workerReplicatedJob := map[string]any{
		keyName: nodeJobName,
		keyTemplate: map[string]any{
			keySpec: map[string]any{
				keyTemplate: workerPodTemplate,
			},
		},
	}
	if len(workerJobLabels) > 0 {
		workerReplicatedJob[keyTemplate].(map[string]any)[keyMetadata] = map[string]any{
			keyLabels: workerJobLabels,
		}
	}

	launcherPodSpec := map[string]any{
		"initContainers": []any{launcherInitContainer},
		keyContainers:    []any{launcherContainer},
		"restartPolicy":  "OnFailure",
		keyVolumes: []any{
			map[string]any{keyName: volumeNameSSHKeys, keyEmptyDir: map[string]any{}},
		},
	}
	launcherJobLabels := map[string]any{
		"trainer.kubeflow.org/trainjob-ancestor-step": "trainer",
	}
	launcherPodLabels := map[string]any{}
	applyGangScheduler(cfg, launcherPodSpec, launcherJobLabels, launcherPodLabels)

	// Launcher pod template: metadata only exists when gang scheduling put the
	// queue label there, mirroring the worker above.
	launcherPodTemplate := map[string]any{
		keySpec: launcherPodSpec,
	}
	if len(launcherPodLabels) > 0 {
		launcherPodTemplate[keyMetadata] = map[string]any{
			keyLabels: launcherPodLabels,
		}
	}

	// The launcher waits for the workers before mpirun dials them. Outside KAI
	// that wait is the JobSet's dependsOn gate. Under KAI both the gate and any
	// startup policy are omitted, because KAI refuses to schedule a JobSet whose
	// PodGroup has an empty sub-group and the ordered launcher sub-group is empty
	// until the workers are ready; the wait moves into the launcher init
	// container instead (see schedulerNameKAI and launcherWaitScript).
	launcherReplicatedJob := map[string]any{
		keyName: launcherJobName,
		keyTemplate: map[string]any{
			keyMetadata: map[string]any{
				keyLabels: launcherJobLabels,
			},
			keySpec: map[string]any{
				keyTemplate: launcherPodTemplate,
			},
		},
	}
	// Under KAI the launcher gets no gate at all: KAI requires every sub-group of
	// the PodGroup to have pods before the group is schedulable, so an ordered or
	// gated launcher (whose sub-group is empty until the workers are ready) can
	// never be admitted — measured live on KAI v0.16.4 with both dependsOn and
	// startupPolicy: InOrder. Without a gate both replicated jobs are created
	// together, both sub-groups are populated, and the wait-moved-into-the-launcher
	// barrier above keeps mpirun from dialing a worker before its sshd answers.
	if !isKAIGangScheduler(cfg) {
		launcherReplicatedJob["dependsOn"] = []any{
			map[string]any{
				keyName:  nodeJobName,
				"status": "Ready",
			},
		}
	}

	jobSetSpec := map[string]any{
		"network": map[string]any{
			"publishNotReadyAddresses": true,
		},
		"replicatedJobs": []any{
			workerReplicatedJob,
			launcherReplicatedJob,
		},
		"successPolicy": map[string]any{
			"operator":             "All",
			"targetReplicatedJobs": []string{launcherJobName},
		},
	}

	rt := map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "TrainingRuntime",
		keyMetadata: map[string]any{
			keyName: fmt.Sprintf("%s-runtime", cfg.EntryName),
			keyLabels: map[string]any{
				"trainer.kubeflow.org/framework": "mpi",
				labelKeyApp:                      cfg.EntryName,
			},
		},
		keySpec: map[string]any{
			"mlPolicy": map[string]any{
				"numNodes": 1, // MPI launcher runs on 1 node; workers scale via replicatedJobs
				"mpi": map[string]any{
					"mpiImplementation": "OpenMPI",
					"numProcPerNode":    cfg.GpusPerNode,
					"sshAuthMountPath":  mpiSSHMountPath,
				},
			},
			keyTemplate: map[string]any{
				keySpec: jobSetSpec,
			},
		},
	}

	data, _ := json.Marshal(rt)
	return nvcrev1alpha1.DependencySpec{
		Raw: data,
	}
}

// BuildExecRuntime creates a TrainingRuntime dependency for arbitrary command execution.
// Uses a simple single-replicatedJob layout with torch mlPolicy.
func BuildExecRuntime(cfg RuntimeConfig) nvcrev1alpha1.DependencySpec {
	// Exec uses the same runtime shape as torch (simple single-replicatedJob)
	// since the command is injected via the JobTemplate trainer field.
	return BuildTorchRuntime(cfg)
}
