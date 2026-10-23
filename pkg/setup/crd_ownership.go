// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// crdsChartName is the chart that manages the NVCRE CRDs outside the main
// release (ADR-107).
const crdsChartName = "cluster-readiness-engine-crds"

// crdAction is what the helm phase does with the NVCRE CRDs (ADR-107
// decision 7).
type crdAction int

const (
	// crdActionApply fetches the chart's CRDs and server-side-applies them,
	// as every release before ADR-107 did.
	crdActionApply crdAction = iota
	// crdActionReleaseOwned leaves the CRDs to the deployed Helm release
	// that owns them, and passes --skip-crds so the main release does not
	// re-apply its subchart's CRDs over that release's ownership.
	crdActionReleaseOwned
	// crdActionLeave leaves the CRDs alone because crds.enabled is false:
	// the operator manages them outside nvcrectl.
	crdActionLeave
)

// crdPlan is the helm phase's CRD decision. planCRDReconcile takes it before
// the deps phase; confirmReleaseOwned settles a release-owned plan against
// the chart's CRDs once they are fetched.
type crdPlan struct {
	action crdAction
	// owner is the deployed release that owns every NVCRE CRD in the
	// cluster, when one does.
	owner helmOwner
	// owners maps each NVCRE CRD in the cluster to its Helm owner, the zero
	// helmOwner when it has none.
	owners map[string]helmOwner
	// crdsDisabled is true when the release's values set crds.enabled=false.
	crdsDisabled bool
}

// skipCRDs reports whether helm upgrade --install gets --skip-crds.
func (p crdPlan) skipCRDs() bool { return p.action == crdActionReleaseOwned }

// releaseStatusFunc returns a Helm release's state as `helm status` reports
// it, helmStateNotInstalled for a release Helm has no record of, and an error
// when the state cannot be read. Tests substitute a stub.
type releaseStatusFunc func(release, namespace string) (string, error)

// newReleaseStatusQuery returns a releaseStatusFunc backed by the helm CLI.
func newReleaseStatusQuery(helmPath, kubeconfig, kubeContext string) releaseStatusFunc {
	return func(release, namespace string) (string, error) {
		state, _, err := helmReleaseStateAndVersion(helmPath, release, namespace, kubeconfig, kubeContext)
		return state, err
	}
}

// planCRDReconcile decides whether setup init applies the NVCRE CRDs
// (ADR-107 decision 7). It reads the state of every release that owns any of
// them first: a failed or pending owner stops init before anything changes,
// even when it owns only some CRDs, because a partial upgrade or adoption may
// have moved those and not the rest. Applying the CRDs could then downgrade
// the moved ones, and upgrading the controller would run it over a partly
// upgraded schema. Only then does ownership decide: a deployed release that
// owns every CRD in the cluster leaves them to it, pending
// confirmReleaseOwned's check against the chart. In every other case the
// CRDs are applied as before, unless crdsDisabled says the operator manages
// them.
func planCRDReconcile(
	ctx context.Context, c client.Client, crdsDisabled bool, status releaseStatusFunc, out io.Writer,
) (crdPlan, error) {
	crds, err := listGroupCRDs(ctx, c, nvcreAPIGroup)
	if err != nil {
		return crdPlan{}, fmt.Errorf("read NVCRE CRDs: %w", err)
	}
	plan := crdPlan{action: crdActionApply, owners: map[string]helmOwner{}, crdsDisabled: crdsDisabled}
	for i := range crds {
		plan.owners[crds[i].GetName()] = completeHelmOwner(&crds[i])
	}

	states := map[helmOwner]string{}
	for _, owner := range distinctOwners(plan.owners) {
		state, err := status(owner.release, owner.namespace)
		if err != nil {
			return crdPlan{}, fmt.Errorf("read the state of Helm release %s/%s, which owns NVCRE CRDs: %w",
				owner.namespace, owner.release, err)
		}
		switch state {
		case helmStateDeployed, helmStateUninstalled, helmStateNotInstalled:
			states[owner] = state
		default:
			printCRDReleaseRecovery(out, owner, state)
			return crdPlan{}, fmt.Errorf(
				"helm release %s/%s, which owns NVCRE CRDs, is %s; resolve it and rerun setup init",
				owner.namespace, owner.release, state)
		}
	}

	if owner, ok := soleHelmOwner(crds); ok {
		state := states[owner]
		if state == helmStateDeployed {
			plan.action, plan.owner = crdActionReleaseOwned, owner
			return plan, nil
		}
		// crds.keep leaves the annotations on the CRDs after helm
		// uninstall, so they can name a release that is gone.
		_, _ = fmt.Fprintf(out,
			"[helm] The NVCRE CRDs name Helm release %s/%s, which is %s; treating them as unmanaged.\n",
			owner.namespace, owner.release, state)
	} else if owners := crdOwnerSummary(crds); owners != "" {
		_, _ = fmt.Fprintf(out,
			"[helm] Warning: the NVCRE CRDs do not share one Helm owner (%s); nvcrectl will apply them.\n", owners)
	}

	if crdsDisabled {
		printCRDsLeft(out)
		plan.action = crdActionLeave
	}
	return plan, nil
}

// confirmReleaseOwned settles a release-owned plan against the CRDs of the
// chart being installed. The release owns the CRDs only when every one the
// chart carries exists and names it. A CRD that is missing, such as one a
// newer chart adds before the CRDs chart is upgraded, or one with another
// owner, would otherwise never be created or updated, so the plan falls back
// to applying the CRDs, or to leaving them when crds.enabled is false. Any
// other plan is returned unchanged.
func confirmReleaseOwned(plan crdPlan, chartCRDs []string, out io.Writer) crdPlan {
	if plan.action != crdActionReleaseOwned {
		return plan
	}
	var gaps []string
	for _, name := range chartCRDs {
		owner, found := plan.owners[name]
		switch {
		case !found:
			gaps = append(gaps, name+": missing")
		case owner != plan.owner:
			gaps = append(gaps, name+": not owned by it")
		}
	}
	if len(gaps) == 0 {
		_, _ = fmt.Fprintf(out,
			"[helm] The NVCRE CRDs are managed by Helm release %s/%s; not applying them.\n",
			plan.owner.namespace, plan.owner.release)
		return plan
	}
	_, _ = fmt.Fprintf(out,
		"[helm] Warning: Helm release %s/%s does not own every CRD the chart carries (%s). "+
			"Upgrade the %s chart before the main chart.\n",
		plan.owner.namespace, plan.owner.release, strings.Join(gaps, ", "), crdsChartName)
	plan.owner = helmOwner{}
	if plan.crdsDisabled {
		printCRDsLeft(out)
		plan.action = crdActionLeave
		return plan
	}
	_, _ = fmt.Fprintln(out, "[helm] nvcrectl will apply the chart's CRDs.")
	plan.action = crdActionApply
	return plan
}

// listGroupCRDs returns the CRDs of one API group, sorted by name.
func listGroupCRDs(ctx context.Context, c client.Client, apiGroup string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	var crds []unstructured.Unstructured
	for _, item := range list.Items {
		group, _, _ := unstructured.NestedString(item.Object, "spec", "group")
		if group == apiGroup {
			crds = append(crds, item)
		}
	}
	sort.Slice(crds, func(i, j int) bool { return crds[i].GetName() < crds[j].GetName() })
	return crds, nil
}

// distinctOwners returns each complete Helm owner once, sorted, so status
// reads and their output are deterministic.
func distinctOwners(owners map[string]helmOwner) []helmOwner {
	seen := map[helmOwner]bool{}
	var list []helmOwner
	for _, owner := range owners {
		if owner.valid() && !seen[owner] {
			seen[owner] = true
			list = append(list, owner)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].namespace != list[j].namespace {
			return list[i].namespace < list[j].namespace
		}
		return list[i].release < list[j].release
	})
	return list
}

// soleHelmOwner returns the release that owns every CRD, when all of them
// carry the same complete Helm ownership metadata.
func soleHelmOwner(crds []unstructured.Unstructured) (helmOwner, bool) {
	if len(crds) == 0 {
		return helmOwner{}, false
	}
	owner := completeHelmOwner(&crds[0])
	if !owner.valid() {
		return helmOwner{}, false
	}
	for i := 1; i < len(crds); i++ {
		if completeHelmOwner(&crds[i]) != owner {
			return helmOwner{}, false
		}
	}
	return owner, true
}

// crdOwnerSummary names each CRD's Helm owner when any CRD carries Helm
// ownership metadata, and is empty when none does.
func crdOwnerSummary(crds []unstructured.Unstructured) string {
	anyOwned := false
	parts := make([]string, 0, len(crds))
	for i := range crds {
		owner := completeHelmOwner(&crds[i])
		desc := "none"
		switch {
		case owner.valid():
			desc = owner.namespace + "/" + owner.release
			anyOwned = true
		case hasAnyHelmOwnershipMetadata(&crds[i]):
			desc = "incomplete metadata"
			anyOwned = true
		}
		parts = append(parts, crds[i].GetName()+": "+desc)
	}
	if !anyOwned {
		return ""
	}
	return strings.Join(parts, ", ")
}

// printCRDsLeft says why nvcrectl leaves the CRDs alone. It names the value
// without printing it: the summary rules keep values out of logs (ADR-098).
func printCRDsLeft(out io.Writer) {
	_, _ = fmt.Fprintln(out,
		"[helm] The release's values turn the chart's CRDs off (crds.enabled); "+
			"leaving the NVCRE CRDs to whatever manages them.")
}

// printCRDReleaseRecovery prints how to recover a CRDs release that is
// failed or mid-operation (ADR-107 decision 6). A failed upgrade or adoption
// may have moved some CRDs and not others, which is safe while the controller
// has not moved, so the CRDs release is finished first and the controller
// after.
func printCRDReleaseRecovery(out io.Writer, owner helmOwner, state string) {
	_, _ = fmt.Fprintf(out,
		"[helm] Helm release %s/%s owns NVCRE CRDs and is %s. setup init changed nothing.\n",
		owner.namespace, owner.release, state)
	_, _ = fmt.Fprintf(out,
		"[helm] Inspect it with: helm status %s --namespace %s\n"+
			"[helm] When no other operation is running, rerun its upgrade (the %s chart).\n"+
			"[helm] On Helm 4, a conflict on .spec.versions means another writer co-owns the CRDs. Check with\n"+
			"[helm]   kubectl get crd <name> --show-managed-fields\n"+
			"[helm] stop that writer, and rerun the upgrade with --force-conflicts. Then rerun setup init.\n",
		owner.release, owner.namespace, crdsChartName)
}
