// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// capSYSChroot is required by every container that runs sshd. OpenSSH has made
// privilege separation mandatory since 7.5, so sshd chroots to its compiled-in
// PRIVSEP_PATH before authenticating any session. Docker and containerd grant
// the capability in their default set; CRI-O does not, so on OpenShift the
// chroot returns EPERM and sshd kills every session preauth while the listener
// stays up: a worker that passes a TCP readiness probe and refuses all work
// (issue #461).
const capSYSChroot = "SYS_CHROOT"

// sshdLaunchMarker is how a rendered container declares it runs sshd. Matching
// the exec rather than the string "sshd" avoids counting the guarded
// openssh-server install or an unrelated mention in a comment.
const sshdLaunchMarker = "/usr/sbin/sshd -De"

// Values each case's input.yaml selects between, and the probe handler the
// invariant requires.
const (
	invSourceBuilder   = "builder"
	invSourceOverrides = "overrides"
	invSourceCatalog   = "catalog"
	invFrameworkMPI    = "mpi"
	invFrameworkTorch  = "torch"
	invExecHandler     = "exec"
	// invMinProbeTimeout is the smallest timeoutSeconds that leaves room for
	// ssh's ConnectTimeout=5 inside `timeout 8`; the field defaults to 1.
	invMinProbeTimeout = 10
)

// sshdInvariantsInput is each case's input.yaml. source picks the renderer;
// the remaining fields are what that renderer needs.
type sshdInvariantsInput struct {
	Source        string   `json:"source"`
	FrameworkType string   `json:"frameworkType,omitempty"`
	Image         string   `json:"image,omitempty"`
	NodesPerJob   int32    `json:"nodesPerJob"`
	GpusPerNode   int32    `json:"gpusPerNode,omitempty"`
	GPUProducts   []string `json:"gpuProducts,omitempty"`
}

// probeProjection is the golden-file view of a readinessProbe: which handlers
// it declares and whether the exec, if any, is the loopback SSH session the
// worker needs. handlers is sorted so the golden is stable.
type probeProjection struct {
	Handlers       []string `json:"handlers"`
	InvokesSSH     bool     `json:"invokesSSH"`
	DialsLoopback  bool     `json:"dialsLoopback"`
	BatchMode      bool     `json:"batchMode"`
	TimeoutSeconds float64  `json:"timeoutSeconds"`
}

// containerProjection records one rendered container. fragment marks a
// container that comes from an override rather than a base runtime: it is
// merged onto a base container, so an empty capability list or a missing probe
// means "inherited", not "absent". Only containers that bear on the invariant
// are recorded (they run sshd, request a capability, declare a probe, or are a
// base runtime's worker slot); the rest are counted in containersScanned.
type containerProjection struct {
	Source            string           `json:"source,omitempty"`
	Fragment          bool             `json:"fragment,omitempty"`
	ReplicatedJob     string           `json:"replicatedJob,omitempty"`
	InitContainer     bool             `json:"initContainer,omitempty"`
	Container         string           `json:"container"`
	RunsSSHD          bool             `json:"runsSSHD"`
	CapabilitiesAdded []string         `json:"capabilitiesAdded"`
	ReadinessProbe    *probeProjection `json:"readinessProbe,omitempty"`
}

// sshdInvariantsProjection is the golden. violations is always empty in a
// recorded golden: a non-empty list fails the case before the golden is
// compared, so regeneration cannot record a broken value the way the goldens
// that pinned `add: [IPC_LOCK]` and `tcpSocket: 22` did.
type sshdInvariantsProjection struct {
	ContainersScanned int                   `json:"containersScanned"`
	SSHDWorkers       int                   `json:"sshdWorkers"`
	Containers        []containerProjection `json:"containers"`
	Violations        []string              `json:"violations"`
}

// foundContainer is a container located in a rendered dependency, with the
// context the predicates need.
type foundContainer struct {
	source string
	// fragment: the dependency is an override, merged onto a base runtime.
	fragment bool
	// workerHasSSHD: the runtime this container belongs to runs sshd on its
	// worker, so a fragment restating the worker slot is restating an sshd
	// container even though the fragment itself carries no args.
	workerHasSSHD bool
	job           string
	init          bool
	c             map[string]any
}

// TestSSHDWorkerInvariants pins one property across every renderer: whatever
// container runs sshd must be able to chroot and must be probed with a real
// session, and nothing else may carry the capability.
//
// It is a projection golden, not a verbatim one, because the verbatim goldens
// recorded the defect faithfully. Each case renders one source, projects every
// container that bears on the invariant, and computes violations from the same
// predicates the projection records; the golden then documents the shape a
// reviewer should expect to see, and the violation check keeps a regeneration
// from recording a regression.
//
// The overrides and catalog cases exist because the fix sites do not share
// code. A platform override that restates securityContext.capabilities.add
// REPLACES the builder's list (the dependency merge unions slices only when
// every element is a map with a name key, and a capability list is a plain
// string slice), so a fix in BuildMPIRuntime alone leaves exactly those
// platforms broken. The catalog entries declare their own worker container and
// never call the builder, and their override fragments are where a future
// platform-specific restatement would live, so those are walked too.
func TestSSHDWorkerInvariants(t *testing.T) {
	p := &testutil.TestCaseParser{Subdir: "sshd-worker-invariants"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in sshdInvariantsInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		found, err := renderSSHDCase(tc.T, in)
		if err != nil {
			return err
		}
		proj, restatedWorkerCaps := projectSSHDCase(found)
		requireSSHDCaseNotVacuous(tc.T, in, proj, restatedWorkerCaps)
		require.Empty(tc.T, proj.Violations, "sshd worker invariants violated")

		out, err := json.MarshalIndent(proj, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(out) + "\n"
		return nil
	})
}

// renderSSHDCase runs the renderer the case names and returns every container
// it produced.
func renderSSHDCase(t testing.TB, in sshdInvariantsInput) ([]foundContainer, error) {
	switch in.Source {
	case invSourceBuilder:
		return renderBuilderCase(in)
	case invSourceOverrides:
		return renderOverridesCase(t, in), nil
	case invSourceCatalog:
		return renderCatalogCase(t, in), nil
	default:
		return nil, fmt.Errorf("unknown source %q", in.Source)
	}
}

func renderBuilderCase(in sshdInvariantsInput) ([]foundContainer, error) {
	cfg := RuntimeConfig{
		EntryName:   "test",
		Image:       in.Image,
		NodesPerJob: in.NodesPerJob,
		GpusPerNode: in.GpusPerNode,
	}
	var dep nvcrev1alpha1.DependencySpec
	switch in.FrameworkType {
	case invFrameworkMPI:
		dep = BuildMPIRuntime(cfg)
	case invFrameworkTorch:
		dep = BuildTorchRuntime(cfg)
	default:
		return nil, fmt.Errorf("unknown frameworkType %q", in.FrameworkType)
	}
	return collectContainers(dep, "", false, in.FrameworkType == invFrameworkMPI), nil
}

func renderOverridesCase(t testing.TB, in sshdInvariantsInput) []foundContainer {
	overrides := BuildOverrides(OverrideConfig{
		EntryName:     "test",
		NodesPerJob:   in.NodesPerJob,
		GpusPerNode:   in.GpusPerNode,
		FrameworkType: in.FrameworkType,
	})
	require.NotEmpty(t, overrides, "BuildOverrides rendered nothing")
	var found []foundContainer
	for _, o := range overrides {
		source := "override " + compactJSON(o.When)
		for _, dep := range o.Dependencies {
			found = append(found, collectContainers(dep, source, true, in.FrameworkType == invFrameworkMPI)...)
		}
		found = append(found, overridePatchContainers(o.JobTemplate, o.JobTemplatePatch, source, in.FrameworkType == invFrameworkMPI)...)
	}
	return found
}

// renderCatalogCase builds every registered entry for every listed product.
// Entry.Build leaves overrides unapplied in spec.overrides (the controller
// merges them at reconcile), so the fragments are walked from there: a
// fragment restating the worker's list or probe is the defect class this test
// exists for. The worker can also be patched from the jobTemplate, through
// workload.trainJob.runtimePatches in the base entry or in an override's
// jobTemplate or jobTemplatePatch (the communication entries mount /dev/shm
// that way), so those are walked as fragments too. Trainer's ContainerPatch
// admits only name, env, volumeMounts and securityContext, so that route can
// restate the capability list but cannot carry a probe; a probe found there
// is still reported, because whoever wrote it meant it to apply.
func renderCatalogCase(t testing.TB, in sshdInvariantsInput) []foundContainer {
	var found []foundContainer
	for _, category := range catalog.List() {
		entry := catalog.Lookup(category.Domain, category.Variant)
		require.NotNil(t, entry, "catalog.List returned %s/%s but Lookup does not know it",
			category.Domain, category.Variant)
		for _, product := range in.GPUProducts {
			target := nvcrev1alpha1.TargetSpec{
				NodeSelector: map[string]string{"nvidia.com/gpu.product": product},
			}
			spec, err := entry.Build(target, catalog.BuildConfig{
				NodesPerJob:     in.NodesPerJob,
				GPUArchitecture: catalog.GPUArchFromNodeSelector(target.NodeSelector),
			})
			if err != nil {
				// Not every variant supports every architecture or node
				// count; those combinations render no worker.
				continue
			}
			source := fmt.Sprintf("%s/%s@%s", category.Domain, category.Variant, product)

			var base []foundContainer
			for _, dep := range spec.Dependencies {
				base = append(base, collectContainers(dep, source, false, false)...)
			}
			workerHasSSHD := slices.ContainsFunc(base, func(fc foundContainer) bool { return runsSSHD(fc.c) })
			for i := range base {
				base[i].workerHasSSHD = workerHasSSHD
			}
			found = append(found, base...)

			jobTemplate, err := json.Marshal(spec.JobTemplate)
			require.NoError(t, err)
			found = append(found, collectContainersFromJSON(jobTemplate, source+" jobTemplate", true, workerHasSSHD)...)

			for i, o := range spec.Overrides {
				fragSource := fmt.Sprintf("%s override[%d] %s", source, i, compactJSON(o.When))
				for _, dep := range o.Dependencies {
					found = append(found, collectContainers(dep, fragSource, true, workerHasSSHD)...)
				}
				found = append(found, overridePatchContainers(o.JobTemplate, o.JobTemplatePatch, fragSource, workerHasSSHD)...)
			}
		}
	}
	return found
}

// projectSSHDCase projects every container, evaluates the predicates, and
// returns the golden body plus the number of fragments that restated the
// worker's capability list (the overrides-mpi vacuity guard needs it).
func projectSSHDCase(found []foundContainer) (sshdInvariantsProjection, int) {
	proj := sshdInvariantsProjection{Containers: []containerProjection{}, Violations: []string{}}
	restatedWorkerCaps := 0
	for _, fc := range found {
		proj.ContainersScanned++
		cp, violations, restated := projectContainer(fc)
		if restated {
			restatedWorkerCaps++
		}
		proj.Violations = append(proj.Violations, violations...)
		if cp == nil {
			continue
		}
		proj.Containers = append(proj.Containers, *cp)
		if cp.RunsSSHD && !cp.Fragment {
			proj.SSHDWorkers++
		}
	}
	sort.SliceStable(proj.Containers, func(i, j int) bool {
		a, b := proj.Containers[i], proj.Containers[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.ReplicatedJob != b.ReplicatedJob {
			return a.ReplicatedJob < b.ReplicatedJob
		}
		if a.InitContainer != b.InitContainer {
			return !a.InitContainer
		}
		return a.Container < b.Container
	})
	sort.Strings(proj.Violations)
	return proj, restatedWorkerCaps
}

// requireSSHDCaseNotVacuous fails a case that had nothing to check, since an
// empty violation list proves nothing on its own.
func requireSSHDCaseNotVacuous(t testing.TB, in sshdInvariantsInput, proj sshdInvariantsProjection, restatedWorkerCaps int) {
	require.NotZero(t, proj.ContainersScanned, "no containers rendered")
	switch {
	case in.Source == invSourceBuilder && in.FrameworkType == invFrameworkMPI:
		require.Equal(t, 1, proj.SSHDWorkers,
			"expected exactly one sshd worker from BuildMPIRuntime; if that changed, the predicates need to cover the new container too")
	case in.Source == invSourceOverrides && in.FrameworkType == invFrameworkMPI:
		require.NotZero(t, restatedWorkerCaps,
			"no override restated the MPI worker capability list; if the overrides stopped doing that, this case is passing vacuously")
	case in.Source == invSourceCatalog:
		require.NotZero(t, proj.SSHDWorkers,
			"no catalog entry rendered an sshd worker; if the catalog stopped declaring them, this case is passing vacuously")
	}
}

// projectContainer records a container and evaluates the predicates against
// it. It returns nil when the container has no bearing on the invariant, the
// violations it found, and whether it is a fragment restating the worker's
// capability list.
func projectContainer(fc foundContainer) (*containerProjection, []string, bool) {
	name, _ := fc.c[keyName].(string)
	sshd := runsSSHD(fc.c)
	caps := capabilitiesAdded(fc.c)
	probe := projectProbe(fc.c)
	workerSlot := fc.job == nodeJobName && name == nodeJobName && !fc.init

	// A base worker is always recorded, even a torch one with nothing on it,
	// so the golden shows the slot. A fragment is recorded only when it says
	// something about the invariant; most fragments patch env or resources
	// on the worker and inherit everything this test cares about.
	relevant := sshd || len(caps) > 0 || probe != nil || (workerSlot && !fc.fragment)
	if !relevant {
		return nil, nil, false
	}

	where := describeContainer(fc, name)
	var violations []string
	restated := false
	switch {
	case !fc.fragment:
		violations = baseContainerViolations(where, fc.workerHasSSHD, sshd, caps, probe)
	case sshd || (workerSlot && fc.workerHasSSHD):
		// This fragment is, or restates, the container that runs sshd. A
		// fragment in the worker slot carries no args, so sshd alone cannot
		// identify it.
		violations, restated = fragmentWorkerViolations(where, caps, probe)
	case slices.Contains(caps, capSYSChroot):
		violations = []string{where + ": does not run sshd but requests " + capSYSChroot}
	}

	return &containerProjection{
		Source:            fc.source,
		Fragment:          fc.fragment,
		ReplicatedJob:     fc.job,
		InitContainer:     fc.init,
		Container:         name,
		RunsSSHD:          sshd,
		CapabilitiesAdded: caps,
		ReadinessProbe:    probe,
	}, violations, restated
}

// baseContainerViolations evaluates a container declared by a base runtime:
// one that execs sshd must carry the capability and the session probe, and one
// that does not must not carry the capability.
func baseContainerViolations(where string, workerHasSSHD, sshd bool, caps []string, probe *probeProjection) []string {
	var violations []string
	if !sshd {
		if slices.Contains(caps, capSYSChroot) {
			violations = append(violations, where+": does not run sshd but requests "+capSYSChroot)
		}
		return violations
	}
	if !workerHasSSHD {
		violations = append(violations, where+": runs sshd in a runtime that is not expected to run it")
	}
	if !slices.Contains(caps, capSYSChroot) {
		violations = append(violations, where+": execs sshd without "+capSYSChroot+
			"; every session dies preauth wherever the runtime does not grant it by default")
	}
	if !isSessionProbe(probe) {
		violations = append(violations, where+": execs sshd but its readiness probe is not a loopback "+
			"ssh session (exec, ssh client, 127.0.0.1, BatchMode=yes, timeoutSeconds >= 10); "+
			"a weaker probe reports a worker Ready while sshd refuses every session")
	}
	return violations
}

// fragmentWorkerViolations evaluates an override fragment that restates the
// sshd worker. A restated capability list replaces the base list, so it must
// carry the capability itself; a restated probe is map-merged onto the exec
// probe, so any other handler leaves two handlers, which the API rejects, and
// could not see a refused session anyway.
func fragmentWorkerViolations(where string, caps []string, probe *probeProjection) ([]string, bool) {
	var violations []string
	restated := len(caps) > 0
	if restated && !slices.Contains(caps, capSYSChroot) {
		violations = append(violations, where+": restates the sshd worker's capability list without "+
			capSYSChroot+"; the merge replaces the list, so this silently drops the capability on exactly this platform")
	}
	if probe == nil {
		return violations, restated
	}
	for _, h := range probe.Handlers {
		if h != invExecHandler {
			violations = append(violations, where+": restates the sshd worker's readiness probe with "+h)
		}
	}
	execIsSession := probe.InvokesSSH && probe.DialsLoopback && probe.BatchMode
	if slices.Contains(probe.Handlers, invExecHandler) && !execIsSession {
		violations = append(violations, where+": restates the sshd worker's exec probe with something other than a loopback ssh session")
	}
	if probe.TimeoutSeconds != 0 && probe.TimeoutSeconds < invMinProbeTimeout {
		violations = append(violations, where+": restates the sshd worker's probe timeoutSeconds below 10")
	}
	return violations, restated
}

// isSessionProbe is the positive shape a base sshd container must carry.
func isSessionProbe(p *probeProjection) bool {
	return p != nil &&
		len(p.Handlers) == 1 && p.Handlers[0] == invExecHandler &&
		p.InvokesSSH && p.DialsLoopback && p.BatchMode &&
		p.TimeoutSeconds >= invMinProbeTimeout
}

func describeContainer(fc foundContainer, name string) string {
	where := fc.job + "/" + name
	if fc.source != "" {
		where = fc.source + " " + where
	}
	if fc.init {
		where += " (init)"
	}
	return where
}

// collectContainers returns every container and initContainer in a rendered
// dependency, tagged with the replicatedJob it sits under. It walks the object
// graph rather than matching text so a capability on a sibling container
// cannot be mistaken for one on the worker, and it carries the job name
// because the launcher's container is also named "node".
func collectContainers(dep nvcrev1alpha1.DependencySpec, source string, fragment, workerHasSSHD bool) []foundContainer {
	return collectContainersFromJSON(dep.Raw, source, fragment, workerHasSSHD)
}

// overridePatchContainers walks the two ways an override patches the
// jobTemplate: the strategic-merge jobTemplate and the RFC 6902
// jobTemplatePatch. Both are opaque JSON on the API type, and either can carry
// workload.trainJob.runtimePatches that reach the worker container. The JSON
// patch is a list of operations whose values are walked as they are, so an
// operation that adds a container or a runtime patch is found the same way.
func overridePatchContainers(jobTemplate, jobTemplatePatch *apiextensionsv1.JSON, source string, workerHasSSHD bool) []foundContainer {
	var out []foundContainer
	if jobTemplate != nil {
		out = append(out, collectContainersFromJSON(jobTemplate.Raw, source+" jobTemplate", true, workerHasSSHD)...)
	}
	if jobTemplatePatch != nil {
		out = append(out, collectContainersFromJSON(jobTemplatePatch.Raw, source+" jobTemplatePatch", true, workerHasSSHD)...)
	}
	return out
}

func collectContainersFromJSON(raw []byte, source string, fragment, workerHasSSHD bool) []foundContainer {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		// A document that is not JSON has no containers to check; the
		// renderer's own tests cover its validity.
		return nil
	}
	var out []foundContainer
	walkContainers(root, "", func(job string, init bool, c map[string]any) {
		out = append(out, foundContainer{
			source:        source,
			fragment:      fragment,
			workerHasSSHD: workerHasSSHD,
			job:           job,
			init:          init,
			c:             c,
		})
	})
	return out
}

func walkContainers(node any, job string, visit func(job string, init bool, c map[string]any)) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			switch key {
			case keyContainers, keyInitContainers:
				if list, ok := value.([]any); ok {
					for _, item := range list {
						if c, ok := item.(map[string]any); ok {
							visit(job, key == keyInitContainers, c)
						}
					}
				}
			case keyReplicatedJobs:
				if list, ok := value.([]any); ok {
					for _, item := range list {
						if rj, ok := item.(map[string]any); ok {
							name, _ := rj[keyName].(string)
							walkContainers(rj, name, visit)
						}
					}
					continue
				}
			}
			walkContainers(value, job, visit)
		}
	case []any:
		for _, value := range typed {
			walkContainers(value, job, visit)
		}
	}
}

// runsSSHD reports whether the container execs sshd, checking command and args
// specifically rather than the whole serialized container, so an env var or
// label mentioning sshd cannot make an unrelated container look like a worker.
func runsSSHD(c map[string]any) bool {
	for _, key := range []string{keyCommand, keyArgs} {
		raw, ok := c[key]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), sshdLaunchMarker) {
			return true
		}
	}
	return false
}

func capabilitiesAdded(c map[string]any) []string {
	added := []string{}
	securityContext, ok := c["securityContext"].(map[string]any)
	if !ok {
		return added
	}
	capabilities, ok := securityContext["capabilities"].(map[string]any)
	if !ok {
		return added
	}
	list, ok := capabilities["add"].([]any)
	if !ok {
		return added
	}
	for _, item := range list {
		if name, ok := item.(string); ok {
			added = append(added, name)
		}
	}
	return added
}

// projectProbe summarizes readinessProbe. Numbers arrive as float64 because
// every dependency is JSON by the time this test sees it.
func projectProbe(c map[string]any) *probeProjection {
	probe, ok := c["readinessProbe"].(map[string]any)
	if !ok {
		return nil
	}
	p := &probeProjection{Handlers: []string{}}
	for _, h := range []string{invExecHandler, "tcpSocket", "httpGet", "grpc"} {
		if _, present := probe[h]; present {
			p.Handlers = append(p.Handlers, h)
		}
	}
	sort.Strings(p.Handlers)
	p.TimeoutSeconds, _ = probe["timeoutSeconds"].(float64)
	if exec, ok := probe[invExecHandler].(map[string]any); ok {
		if encoded, err := json.Marshal(exec[keyCommand]); err == nil {
			command := string(encoded)
			// "ssh -" is an ssh client invocation with options; it does not
			// match the `command -v ssh` guard or the "sshd -De" launch.
			p.InvokesSSH = strings.Contains(command, "ssh -")
			p.DialsLoopback = strings.Contains(command, " 127.0.0.1 true")
			p.BatchMode = strings.Contains(command, "BatchMode=yes")
		}
	}
	return p
}

func compactJSON(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(encoded)
}
