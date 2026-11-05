// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

// Network fabric selection (ADR-110).
//
// The fabric is resolved in one order everywhere it is needed: an explicit
// spec.fabric wins after it is validated against the target nodes; when it is
// unset, AWS GB300 (the one target the catalog renders for more than one
// fabric) is detected from the nodes; otherwise, and whenever detection is
// inconclusive, the platform default applies, which is the fabric NVCRE
// rendered there before the field existed. Detection only overrides the
// default on a positive, unambiguous signal, so a cluster carrying neither
// signal renders exactly as before.
const (
	// efaResourceName is the extended resource the AWS EFA device plugin
	// advertises. Its allocatable quantity is also the device count requested
	// per worker container.
	efaResourceName corev1.ResourceName = "vpc.amazonaws.com/efa"

	// awsNetworkingDRADriver is the spec.driver of the ResourceSlices the AWS
	// networking DRA plugin (aws-net-k8s-dra-plugin) publishes. Note that
	// roce.networking.k8s.aws, which the catalog requests, is the DeviceClass
	// selecting on this driver, not a driver name: a filter on it never
	// matches a real slice.
	awsNetworkingDRADriver = "dra.networking.k8s.aws"

	// awsNetworkingDeviceTypeAttribute is the fully qualified device attribute
	// carrying the NIC type on those slices; RoCE devices carry "roce".
	awsNetworkingDeviceTypeAttribute resourcev1.QualifiedName = "dra.vpc.amazonaws.com/deviceType"

	// awsRoCEDeviceType is the deviceType attribute value of a RoCE device.
	awsRoCEDeviceType = "roce"

	// defaultEFAPerNode is the EFA device count rendered when no node was
	// read (offline render): the AWS single-EFA layout, which every EFA node
	// can satisfy.
	defaultEFAPerNode int32 = 1
)

// GPU architectures the fabric defaults distinguish.
const (
	gpuArchH100  = "h100"
	gpuArchGB200 = "gb200"
	gpuArchGB300 = "gb300"
)

// Fabric sources recorded in FabricResolution.Source.
const (
	FabricSourceSpec     = "spec"
	FabricSourceDetected = "detected"
	FabricSourceDefault  = "default"
)

// FabricResolution is the outcome of one fabric resolution (ADR-110). The
// controllers and the CLI render paths share it, so the two cannot drift.
type FabricResolution struct {
	// Fabric is the resolved fabric, or "" when the target has no default
	// and the field is unset.
	Fabric string
	// Source is FabricSourceSpec, FabricSourceDetected or FabricSourceDefault.
	Source string
	// EFAPerNode is the EFA device count each worker requests: the minimum
	// vpc.amazonaws.com/efa allocatable across the target nodes when the
	// fabric is efa and nodes were read, otherwise defaultEFAPerNode.
	EFAPerNode int32
	// Notice is non-empty when detection ran and could not conclude: the
	// signals conflicted, covered only some nodes, or the ResourceSlice list
	// was refused, so the default was used. It is the text of the Normal
	// FabricDetection event, and the CLI prints it to stderr.
	Notice string
	// Surface reports whether the fabric chose the render (the field is set,
	// or the target is a detection target), which gates the detected-fabric
	// render annotation and the resolvedFabric status field.
	Surface bool
}

// fabricValidationError is an explicit spec.fabric the target cannot carry.
// It is terminal: the spec is immutable, so a retry would fail the same way.
type fabricValidationError struct {
	Message string
	// ListRefused is true when the check could not run because the
	// ResourceSlice list was refused. The CLI dry-run lists with the user's
	// kubeconfig, so it warns on this one instead of failing, as it does for
	// the OCI network list (ADR-103).
	ListRefused bool
}

func (e *fabricValidationError) Error() string { return e.Message }

// defaultFabric is the fabric the catalog has always rendered for a platform
// and GPU architecture, read from the fabric its fragments are named for. ""
// means NVCRE renders no RDMA fabric of its own there: TCP (Akamai, GCP RTX PRO
// 6000), a site-specific fabric (on-prem, which nicResourceName handles), or
// an unknown platform.
func defaultFabric(platformName, gpuArch string) string {
	switch platformName {
	case platform.AWS:
		if gpuArch == gpuArchGB300 {
			return string(nvcrev1alpha1.FabricRoCE)
		}
		return string(nvcrev1alpha1.FabricEFA)
	case platform.Azure, platform.Forge, platform.TogetherAI, platform.Mistral, platform.NScale:
		return string(nvcrev1alpha1.FabricIB)
	case platform.OCI:
		return string(nvcrev1alpha1.FabricRoCE)
	case platform.GCP:
		switch gpuArch {
		case gpuArchH100:
			return string(nvcrev1alpha1.FabricTCPXO)
		case gpuArchGB200, gpuArchGB300:
			return string(nvcrev1alpha1.FabricRoCE)
		}
	}
	return ""
}

// isFabricDetectionTarget reports whether the fabric is detected from the
// nodes for this platform and architecture. Only AWS GB300 qualifies: it is
// the one target the catalog renders for more than one fabric, so no other
// target ever lists ResourceSlices or sees a FabricDetection event.
func isFabricDetectionTarget(platformName, gpuArch string) bool {
	return platformName == platform.AWS && gpuArch == gpuArchGB300
}

// renderableFabrics is the set of fabrics the catalog renders on a target:
// both of AWS GB300's, otherwise the default alone. nil means unconstrained:
// the target has no default, and nothing in the catalog keys on the fabric.
func renderableFabrics(platformName, gpuArch string) []string {
	if isFabricDetectionTarget(platformName, gpuArch) {
		return []string{string(nvcrev1alpha1.FabricRoCE), string(nvcrev1alpha1.FabricEFA)}
	}
	if d := defaultFabric(platformName, gpuArch); d != "" {
		return []string{d}
	}
	return nil
}

// efaSignal reads vpc.amazonaws.com/efa from node allocatable. A node whose
// quantity is below one carries no EFA signal.
func efaSignal(nodes []corev1.Node) map[string]int64 {
	out := map[string]int64{}
	for i := range nodes {
		qty, ok := nodes[i].Status.Allocatable[efaResourceName]
		if !ok {
			continue
		}
		if n := qty.Value(); n >= 1 {
			out[nodes[i].Name] = n
		}
	}
	return out
}

// roceSignal lists ResourceSlices and returns the target nodes with at least
// one RoCE device from the AWS networking DRA driver, in the current
// generation of its pool. An API the cluster does not serve reads as no RoCE
// devices, since a cluster without the DRA API cannot allocate the RoCE claim
// either. A refused list (403) says nothing about the devices, so it comes
// back as refused, non-nil, and the caller decides without the RoCE signal.
// Any other list error is returned so the caller retries instead of deciding
// on a partial view.
func roceSignal(
	ctx context.Context, reader client.Reader, nodes []corev1.Node,
) (roce map[string]bool, refused, err error) {
	listCtx, cancel := context.WithTimeout(ctx, resourceSliceListTimeout)
	defer cancel()

	var rsl resourcev1.ResourceSliceList
	if err := reader.List(listCtx, &rsl); err != nil {
		switch {
		case apierrors.IsForbidden(err):
			return map[string]bool{}, err, nil
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			logf.FromContext(ctx).Info("ResourceSlices are not served; fabric detection sees no RoCE devices",
				"error", err)
			return map[string]bool{}, nil, nil
		}
		return nil, nil, fmt.Errorf("listing ResourceSlices for fabric detection: %w", err)
	}

	target := make(map[string]bool, len(nodes))
	for i := range nodes {
		target[nodes[i].Name] = true
	}
	out := map[string]bool{}
	for _, rs := range currentPoolSlices(rsl.Items, awsNetworkingDRADriver) {
		if rs.Spec.NodeName == nil || !target[*rs.Spec.NodeName] {
			continue
		}
		for _, d := range rs.Spec.Devices {
			if attr, ok := d.Attributes[awsNetworkingDeviceTypeAttribute]; ok &&
				attr.StringValue != nil && *attr.StringValue == awsRoCEDeviceType {
				out[*rs.Spec.NodeName] = true
				break
			}
		}
	}
	return out, nil, nil
}

// resolveFabric resolves the fabric the way every consumer (both controllers
// and the CLI dry-run paths) must agree on (ADR-110). field is the explicit
// spec.fabric; nodes are the target nodes the job is sized against. With no
// nodes (offline render, or a WorkloadRun whose target matched nothing yet)
// an explicit value is checked against the renderable set only.
//
// It returns a *fabricValidationError when the explicit value cannot be
// carried, and a plain error when a ResourceSlice list failed transiently,
// which the caller retries. The resolution is filled in either way, so a
// caller that tolerates a refused list can still use it.
func resolveFabric(
	ctx context.Context, reader client.Reader, field nvcrev1alpha1.Fabric,
	platformName, gpuArch string, nodes []corev1.Node,
) (FabricResolution, error) {
	detectionTarget := isFabricDetectionTarget(platformName, gpuArch)
	res := FabricResolution{EFAPerNode: defaultEFAPerNode}

	if field != "" {
		res.Fabric, res.Source, res.Surface = string(field), FabricSourceSpec, true
		if allowed := renderableFabrics(platformName, gpuArch); allowed != nil && !slices.Contains(allowed, res.Fabric) {
			return res, &fabricValidationError{Message: fmt.Sprintf(
				"fabric %q is not rendered for platform %q and GPU architecture %q; NVCRE renders %s there",
				res.Fabric, platformName, gpuArch, strings.Join(allowed, " or "))}
		}
		if len(nodes) == 0 {
			return res, nil
		}
		switch field {
		case nvcrev1alpha1.FabricEFA:
			efa := efaSignal(nodes)
			if missing := nodesMissing(nodes, func(n string) bool { _, ok := efa[n]; return ok }); len(missing) > 0 {
				return res, &fabricValidationError{Message: fmt.Sprintf(
					"fabric \"efa\" is set, but %d of %d target nodes advertise no %s allocatable: %s",
					len(missing), len(nodes), efaResourceName, strings.Join(missing, ", "))}
			}
			res.EFAPerNode = minEFA(efa)
		case nvcrev1alpha1.FabricRoCE:
			if platformName != platform.AWS {
				return res, nil
			}
			roce, refused, err := roceSignal(ctx, reader, nodes)
			if err != nil {
				return res, err
			}
			if refused != nil {
				return res, &fabricValidationError{ListRefused: true, Message: fmt.Sprintf(
					"fabric \"roce\" is set, but the ResourceSlice list that checks it for RoCE devices was refused (%v);"+
						" grant get and list on resourceslices.resource.k8s.io", refused)}
			}
			if missing := nodesMissing(nodes, func(n string) bool { return roce[n] }); len(missing) > 0 {
				return res, &fabricValidationError{Message: fmt.Sprintf(
					"fabric \"roce\" is set, but %d of %d target nodes have no RoCE device published by the %s DRA driver: %s",
					len(missing), len(nodes), awsNetworkingDRADriver, strings.Join(missing, ", "))}
			}
		}
		return res, nil
	}

	res.Fabric, res.Source, res.Surface = defaultFabric(platformName, gpuArch), FabricSourceDefault, detectionTarget
	if !detectionTarget || len(nodes) == 0 {
		return res, nil
	}

	efa := efaSignal(nodes)
	roce, refused, err := roceSignal(ctx, reader, nodes)
	if err != nil {
		return res, err
	}
	if refused != nil {
		// Without the RoCE signal an EFA-only reading cannot be told apart
		// from both fabrics present, so detection does not conclude.
		res.Notice = fmt.Sprintf(
			"fabric detection could not list ResourceSlices (%v), so RoCE devices cannot be seen; rendering the"+
				" default fabric %q. Set spec.fabric to choose one.", refused, res.Fabric)
		return res, nil
	}
	switch {
	case len(efa) == 0 && len(roce) == 0:
		// Neither signal: today's behavior, silently.
	case len(efa) == len(nodes) && len(roce) == 0:
		res.Fabric, res.Source, res.EFAPerNode = string(nvcrev1alpha1.FabricEFA), FabricSourceDetected, minEFA(efa)
	case len(roce) == len(nodes) && len(efa) == 0:
		res.Fabric, res.Source = string(nvcrev1alpha1.FabricRoCE), FabricSourceDetected
	default:
		res.Notice = fmt.Sprintf(
			"fabric detection was inconclusive: %d of %d target nodes advertise %s, and %d of %d have RoCE devices"+
				" from the %s DRA driver; rendering the default fabric %q. Set spec.fabric to choose one.",
			len(efa), len(nodes), efaResourceName, len(roce), len(nodes), awsNetworkingDRADriver, res.Fabric)
	}
	return res, nil
}

// nodesMissing returns the sorted names of the nodes for which has is false.
func nodesMissing(nodes []corev1.Node, has func(string) bool) []string {
	var missing []string
	for i := range nodes {
		if !has(nodes[i].Name) {
			missing = append(missing, nodes[i].Name)
		}
	}
	slices.Sort(missing)
	return missing
}

// minEFA is the smallest EFA count across the nodes, so every node can
// satisfy the per-worker request: a count one node cannot satisfy leaves its
// pods permanently Pending.
func minEFA(efa map[string]int64) int32 {
	lowest := int64(0)
	for _, n := range efa {
		if lowest == 0 || n < lowest {
			lowest = n
		}
	}
	if lowest < 1 {
		return defaultEFAPerNode
	}
	return int32(min(lowest, int64(1<<30)))
}

// resolveFabricOverrides settles every when.fabric matcher against the
// resolved fabric before overrides are handed to a Workflow: overrides whose
// fabric matcher does not match are dropped, and the matcher is cleared on the
// ones that do. The Workflow controller re-applies overrides with its own
// context and has no spec.fabric, so without this an explicit fabric could be
// re-decided there. The input slice is not modified.
func resolveFabricOverrides(overrides []nvcrev1alpha1.OverrideSpec, fabric string) []nvcrev1alpha1.OverrideSpec {
	if overrides == nil {
		return nil
	}
	out := make([]nvcrev1alpha1.OverrideSpec, 0, len(overrides))
	for _, o := range overrides {
		if o.When.Fabric == nil {
			out = append(out, o)
			continue
		}
		if !matchesStringSpec(*o.When.Fabric, fabric) {
			continue
		}
		o.When.Fabric = nil
		out = append(out, o)
	}
	return out
}

// Surfaced is the fabric where it chose the render, otherwise "": what the
// WorkloadRun platform override template selects its AWS GB300 blocks by,
// and what the detected-fabric annotation and resolvedFabric status record.
func (r FabricResolution) Surfaced() string {
	if r.Surface {
		return r.Fabric
	}
	return ""
}

// fabricNodes returns the nodes the fabric is resolved on (ADR-110): those of
// the primary GPU architecture that can supply gpusPerNode GPUs, the nodes the
// job can actually run on. A node of another architecture, or one the
// capacity filter drops, never runs the job, so it must neither fail an
// explicit spec.fabric nor make detection inconclusive. A gpusPerNode of zero
// applies no capacity filter.
func fabricNodes(nodes []corev1.Node, gpusPerNode int32) []corev1.Node {
	_, archNodes := detectGPUArchConsistent(nodes)
	capable, _ := filterNodesByGPUCapacity(archNodes, gpusPerNode)
	return capable
}

// overridesUseFabric reports whether any override matches on when.fabric.
func overridesUseFabric(overrides []nvcrev1alpha1.OverrideSpec) bool {
	return slices.ContainsFunc(overrides, func(o nvcrev1alpha1.OverrideSpec) bool { return o.When.Fabric != nil })
}
