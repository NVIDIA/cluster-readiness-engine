// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const jobSetCRDName = "jobsets." + jobsetAPIGroup

// The JobSet chart hardcodes both webhook configuration names in
// `templates/webhook/_helpers.tpl`, and the upstream kustomize build renders
// the same two, so no release name or override changes them.
const (
	jobSetMutatingWebhookConfigurationName   = "jobset-mutating-webhook-configuration"
	jobSetValidatingWebhookConfigurationName = "jobset-validating-webhook-configuration"
)

// helmManagedByLabel is the label Helm validates when deciding whether it may
// adopt an existing resource, alongside the two release annotations.
const helmManagedByLabel = "app.kubernetes.io/managed-by"

type jobSetMode string

const (
	jobSetModeAbsent   jobSetMode = "absent"
	jobSetModeBundled  jobSetMode = "bundled"
	jobSetModeExternal jobSetMode = "external"
	jobSetModeCRDOnly  jobSetMode = "crd-only"
	jobSetModeUnknown  jobSetMode = "unknown"
)

type jobSetObservation struct {
	mode            jobSetMode
	evidence        []string
	manifestObjects map[string]struct{}
	ownershipToken  string
}

type helmOwner struct {
	release   string
	namespace string
}

func (o helmOwner) valid() bool { return o.release != "" && o.namespace != "" }
func (o helmOwner) bundled() bool {
	return o.release == trainerReleaseName && o.namespace == trainerNamespace
}

type jobSetFingerprint struct {
	kind        string
	name        string
	object      unstructured.Unstructured
	serviceRefs []client.ObjectKey
}

func jobSetCRDExists(ctx context.Context, c client.Client) (bool, error) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: kindCustomResourceDefinition,
	})
	if err := c.Get(ctx, client.ObjectKey{Name: jobSetCRDName}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

//nolint:gocyclo // Ownership classification deliberately keeps every fail-closed branch visible in one decision tree.
func observeJobSetOwnership(
	ctx context.Context,
	c client.Client,
	crdPresent bool,
	releaseState string,
	manifest func() (string, error),
) jobSetObservation {
	findings, err := scanJobSetFingerprints(ctx, c)
	if err != nil {
		return unknownJobSet("fingerprint scan failed: " + err.Error())
	}
	if !crdPresent {
		if len(findings) == 0 {
			return jobSetObservation{mode: jobSetModeAbsent, evidence: []string{
				"JobSet CRD is absent and the supported fingerprint scan is clean; this does not prove arbitrary controller absence",
			}}
		}
		return unknownJobSet("JobSet-related resources remain: " + fingerprintNames(findings))
	}

	// The pinned healthy skip never calls this function. Existing releases on
	// install/upgrade paths must contribute their stored manifest as evidence.
	manifestObjects := map[string]struct{}{}
	manifestConsumers := map[string]string{}
	if releaseState != helmStateNotInstalled && releaseState != helmStateUninstalled {
		if manifest == nil {
			return unknownJobSet("Trainer release manifest reader is unavailable")
		}
		raw, manifestErr := manifest()
		if manifestErr != nil {
			return unknownJobSet("read Trainer release manifest: " + manifestErr.Error())
		}
		manifestObjects, manifestErr = releaseManifestIdentities(raw)
		if manifestErr != nil {
			return unknownJobSet("parse Trainer release manifest: " + manifestErr.Error())
		}
		manifestConsumers, manifestErr = releaseManifestJobSetConsumerIdentities(raw)
		if manifestErr != nil {
			return unknownJobSet("parse Trainer release manifest consumer evidence: " + manifestErr.Error())
		}
	}

	items, err := listJobSets(ctx, c)
	if err != nil {
		return unknownJobSet("list JobSets cluster-wide: " + err.Error())
	}
	if len(findings) == 0 {
		leftovers, leftoversErr := knownJobSetNamespacedLeftovers(ctx, c)
		if leftoversErr != nil {
			return unknownJobSet("inspect known JobSet controller resources: " + leftoversErr.Error())
		}
		if len(leftovers) != 0 {
			allBundled := manifestContainsJobSet(manifestObjects)
			var names []string
			for _, leftover := range leftovers {
				names = append(names, fmt.Sprintf("%s/%s/%s", leftover.GetKind(), leftover.GetNamespace(), leftover.GetName()))
				_, inManifest := manifestObjects[manifestIdentity(leftover.GetAPIVersion(), leftover.GetKind(), leftover.GetNamespace(), leftover.GetName())]
				allBundled = allBundled && completeHelmOwner(&leftover).bundled() && inManifest
			}
			if allBundled {
				return jobSetObservation{mode: jobSetModeBundled,
					evidence:        []string{"exact Trainer release manifest and partial live JobSet resources agree"},
					manifestObjects: manifestObjects}
			}
			return unknownJobSet("JobSet supporting resources remain: " + strings.Join(names, ", "))
		}
		if len(items) != 0 {
			return unknownJobSet(fmt.Sprintf("%d JobSet instance(s) exist without an identifiable controller", len(items)))
		}
		if manifestContainsJobSet(manifestObjects) {
			return unknownJobSet("Trainer release manifest contains JobSet resources but no live fingerprint resources were found")
		}
		// The exact release manifest is still the recovery inventory's
		// permission source even when no JobSet resource survives, so it
		// must travel with the crd-only observation.
		return jobSetObservation{mode: jobSetModeCRDOnly, evidence: []string{
			"retained JobSet CRD has no instances and the supported fingerprint scan is clean",
		}, manifestObjects: manifestObjects}
	}

	owners := map[helmOwner]int{}
	unowned := make([]string, 0)
	controllerFindings := make([]jobSetFingerprint, 0, len(findings))
	for _, finding := range findings {
		owner := completeHelmOwner(&finding.object)
		if verifiedTrainerJobSetConsumer(finding, owner, manifestConsumers) {
			continue
		}
		controllerFindings = append(controllerFindings, finding)
		if !owner.valid() {
			unowned = append(unowned, finding.kind+"/"+finding.name)
			continue
		}
		owners[owner]++
	}
	if len(unowned) != 0 {
		if len(unowned) == len(controllerFindings) && !manifestContainsJobSet(manifestObjects) {
			token, verifyErr := verifySupportedNonHelmJobSetController(ctx, c, controllerFindings)
			if verifyErr == nil {
				return jobSetObservation{mode: jobSetModeExternal, evidence: []string{
					"verified supported non-Helm JobSet controller pattern",
				}, manifestObjects: manifestObjects, ownershipToken: token}
			}
			// Report why verification refused. The object list alone does not
			// tell an operator which piece of the supported pattern is absent.
			// Name the observed objects too, unless the refusal already did.
			evidence := "unverified non-Helm JobSet controller: " + verifyErr.Error()
			if observed := strings.Join(unowned, ", "); !strings.Contains(evidence, observed) {
				evidence += " (observed: " + observed + ")"
			}
			return unknownJobSet(evidence)
		}
		return unknownJobSet("unverified non-Helm or incomplete ownership evidence: " + strings.Join(unowned, ", "))
	}
	if len(owners) == 0 {
		return unknownJobSet("only the Trainer consumer role was found; no JobSet controller ownership was established")
	}
	if len(owners) != 1 {
		return unknownJobSet("conflicting JobSet Helm ownership: " + fingerprintNames(findings))
	}
	var owner helmOwner
	for candidate := range owners {
		owner = candidate
	}
	if owner.bundled() {
		for _, finding := range findings {
			if _, ok := manifestObjects[manifestIdentity(finding.object.GetAPIVersion(), finding.object.GetKind(), finding.object.GetNamespace(), finding.object.GetName())]; !ok {
				return unknownJobSet("bundled-looking live resource is absent from the exact Trainer release manifest: " + finding.kind + "/" + finding.name)
			}
		}
		return jobSetObservation{mode: jobSetModeBundled,
			evidence:        []string{"exact Trainer release manifest and live JobSet resources agree"},
			manifestObjects: manifestObjects}
	}
	if manifestContainsJobSet(manifestObjects) {
		return unknownJobSet("external live ownership conflicts with bundled JobSet resources in the exact Trainer release manifest")
	}
	supportToken, err := verifyExternalJobSetController(ctx, c, findings, owner)
	if err != nil {
		return unknownJobSet("external JobSet evidence is incomplete: " + err.Error())
	}
	return jobSetObservation{mode: jobSetModeExternal, evidence: []string{
		fmt.Sprintf("verified external JobSet controller owned by Helm release %s/%s", owner.namespace, owner.release),
	}, manifestObjects: manifestObjects, ownershipToken: fingerprintOwnershipToken(owner, findings) + supportToken}
}

// jobSetNonHelmClusterRoleNames are the controller ClusterRole identities the
// two supported non-Helm channels render: `jobset-manager-role` from the
// upstream kustomize build, and the chart's `jobset-controller` for a cluster
// where those names were applied without a release record.
var jobSetNonHelmClusterRoleNames = []string{"jobset-manager-role", jobSetControllerName}

// jobSetNonHelmDeploymentNames are the matching controller Deployment
// identities: `jobset-controller-manager` from kustomize, `jobset-controller`
// from the chart.
var jobSetNonHelmDeploymentNames = []string{"jobset-controller-manager", jobSetControllerName}

// verifySupportedNonHelmJobSetController verifies a JobSet controller that
// left no Helm release record, which is the only way a fingerprint scan
// produces a complete set of unowned matches.
//
// It targets two channels, whose rendered identities differ:
//
//   - `kubectl apply -f .../jobset/releases/download/<tag>/manifests.yaml`,
//     the kustomize build upstream publishes. It renders ClusterRole
//     `jobset-manager-role` and Deployment `jobset-controller-manager` in
//     `jobset-system`, and labels every object
//     `app.kubernetes.io/managed-by: kustomize`.
//   - the chart's own identities, `jobset-controller` for both, applied
//     without a release record.
//
// Both hardcode the two webhook configuration names and render two webhooks
// per configuration — one for JobSets, one for their Pods — pointing at a
// single `jobset-webhook-service`. Counting references would therefore reject
// either channel, so the requirement is that every reference resolves to the
// same verified Service.
//
// `helm template | kubectl apply` is deliberately out of scope. `jobset.labels`
// emits `app.kubernetes.io/managed-by: Helm` while the apply leaves no
// `meta.helm.sh/*` annotations, which is partial ownership evidence rather
// than a non-Helm install; ADR-078 resolves mixed evidence to `unknown`.
func verifySupportedNonHelmJobSetController(
	ctx context.Context, c client.Client, findings []jobSetFingerprint,
) (string, error) {
	var sawClusterRole, sawMutating, sawValidating bool
	var refs []client.ObjectKey
	var tokenParts []string
	seen := map[client.ObjectKey]bool{}
	for _, finding := range findings {
		key := finding.kind + "/" + finding.name
		if hasAnyHelmOwnershipMetadata(&finding.object) {
			return "", fmt.Errorf("fingerprint %s carries partial Helm ownership metadata", key)
		}
		switch {
		case finding.kind == kindClusterRole && slices.Contains(jobSetNonHelmClusterRoleNames, finding.name):
			sawClusterRole = true
		case finding.kind == kindMutatingWebhook && finding.name == jobSetMutatingWebhookConfigurationName:
			sawMutating = true
		case finding.kind == kindValidatingWebhook && finding.name == jobSetValidatingWebhookConfigurationName:
			sawValidating = true
		default:
			return "", fmt.Errorf("unsupported fingerprint %s", key)
		}
		for _, ref := range finding.serviceRefs {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			refs = append(refs, ref)
		}
		tokenParts = append(tokenParts, key+"/"+string(finding.object.GetUID()))
	}
	switch {
	case !sawClusterRole:
		return "", fmt.Errorf("missing a JobSet controller ClusterRole (%s)",
			strings.Join(jobSetNonHelmClusterRoleNames, " or "))
	case !sawMutating:
		return "", fmt.Errorf("missing MutatingWebhookConfiguration/%s", jobSetMutatingWebhookConfigurationName)
	case !sawValidating:
		return "", fmt.Errorf("missing ValidatingWebhookConfiguration/%s", jobSetValidatingWebhookConfigurationName)
	}
	if len(refs) != 1 {
		return "", fmt.Errorf("expected every JobSet webhook to reference one webhook Service, got %d: %s",
			len(refs), objectKeyNames(refs))
	}
	ref := refs[0]
	if ref.Name != jobSetWebhookServiceName || ref.Namespace == "" {
		return "", fmt.Errorf("unexpected webhook Service %s/%s", ref.Namespace, ref.Name)
	}
	service := &unstructured.Unstructured{}
	service.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: kindService})
	if err := c.Get(ctx, ref, service); err != nil || hasAnyHelmOwnershipMetadata(service) {
		return "", fmt.Errorf("unverified webhook Service %s/%s", ref.Namespace, ref.Name)
	}
	selector, _, _ := unstructured.NestedStringMap(service.Object, "spec", "selector")
	deployment, err := nonHelmJobSetControllerDeployment(ctx, c, ref.Namespace)
	if err != nil {
		return "", err
	}
	labels, _, _ := unstructured.NestedStringMap(deployment.Object, "spec", "template", "metadata", "labels")
	if len(selector) == 0 || !selectorMatches(selector, labels) {
		return "", fmt.Errorf("webhook Service does not select the controller Deployment")
	}
	tokenParts = append(tokenParts, "Service/"+ref.Namespace+"/"+ref.Name+"/"+string(service.GetUID()),
		"Deployment/"+ref.Namespace+"/"+deployment.GetName()+"/"+string(deployment.GetUID()))
	sort.Strings(tokenParts)
	return "nonhelm|" + strings.Join(tokenParts, "|"), nil
}

// nonHelmJobSetControllerDeployment resolves the controller Deployment behind
// the webhook Service, accepting either channel's identity.
func nonHelmJobSetControllerDeployment(
	ctx context.Context, c client.Client, namespace string,
) (*unstructured.Unstructured, error) {
	for _, name := range jobSetNonHelmDeploymentNames {
		deployment := &unstructured.Unstructured{}
		deployment.SetGroupVersionKind(schema.GroupVersionKind{Group: appsAPIGroup, Version: "v1", Kind: kindDeployment})
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, deployment); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get controller Deployment %s/%s: %w", namespace, name, err)
		}
		if hasAnyHelmOwnershipMetadata(deployment) {
			return nil, fmt.Errorf("controller Deployment %s/%s carries partial Helm ownership metadata", namespace, name)
		}
		return deployment, nil
	}
	return nil, fmt.Errorf("no JobSet controller Deployment (%s) in namespace %s",
		strings.Join(jobSetNonHelmDeploymentNames, " or "), namespace)
}

func objectKeyNames(refs []client.ObjectKey) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Namespace+"/"+ref.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func hasAnyHelmOwnershipMetadata(object client.Object) bool {
	annotations := object.GetAnnotations()
	return object.GetLabels()[helmManagedByLabel] == "Helm" ||
		annotations["meta.helm.sh/release-name"] != "" || annotations["meta.helm.sh/release-namespace"] != ""
}

func fingerprintOwnershipToken(owner helmOwner, findings []jobSetFingerprint) string {
	parts := []string{owner.namespace + "/" + owner.release}
	for _, finding := range findings {
		if completeHelmOwner(&finding.object) == owner {
			parts = append(parts, finding.kind+"/"+finding.name+"/"+string(finding.object.GetUID()))
		}
	}
	sort.Strings(parts[1:])
	return strings.Join(parts, "|")
}

func verifiedTrainerJobSetConsumer(
	finding jobSetFingerprint, owner helmOwner, manifestObjects map[string]string,
) bool {
	if finding.kind != kindClusterRole || finding.name != "kubeflow-trainer-controller-manager" || !owner.bundled() {
		return false
	}
	wantChart, ok := manifestObjects[manifestIdentity(
		finding.object.GetAPIVersion(), finding.object.GetKind(), finding.object.GetNamespace(), finding.object.GetName())]
	labels := finding.object.GetLabels()
	return ok && labels["helm.sh/chart"] == wantChart &&
		labels["app.kubernetes.io/name"] == "kubeflow-trainer" &&
		labels["app.kubernetes.io/component"] == "manager"
}

func knownJobSetNamespacedLeftovers(ctx context.Context, c client.Client) ([]unstructured.Unstructured, error) {
	types := []schema.GroupVersionKind{
		{Version: "v1", Kind: kindServiceAccount},
		{Version: "v1", Kind: kindService},
		{Version: "v1", Kind: kindConfigMap},
		{Version: "v1", Kind: "Secret"},
		{Group: appsAPIGroup, Version: "v1", Kind: kindDeployment},
		{Group: rbacAPIGroup, Version: "v1", Kind: "Role"},
		{Group: rbacAPIGroup, Version: "v1", Kind: "RoleBinding"},
	}
	knownNames := map[string]bool{
		jobSetControllerName: true, jobSetWebhookServiceName: true,
		"jobset-metrics-service": true, "jobset-controller-config": true,
		"jobset-webhook-server-cert": true,
	}
	var leftovers []unstructured.Unstructured
	for _, gvk := range types {
		objects, err := listAllUnstructured(ctx, c, gvk, "")
		if err != nil {
			return nil, fmt.Errorf("list %s across namespaces: %w", gvk.Kind, err)
		}
		for _, object := range objects {
			if knownNames[object.GetName()] {
				leftovers = append(leftovers, object)
			}
		}
	}
	sort.Slice(leftovers, func(i, j int) bool {
		return recoveryIdentity(leftovers[i]) < recoveryIdentity(leftovers[j])
	})
	return leftovers, nil
}

func unknownJobSet(reason string) jobSetObservation {
	return jobSetObservation{mode: jobSetModeUnknown, evidence: []string{reason}}
}

func completeHelmOwner(obj client.Object) helmOwner {
	if obj.GetLabels()[helmManagedByLabel] != "Helm" {
		return helmOwner{}
	}
	annotations := obj.GetAnnotations()
	return helmOwner{
		release:   annotations["meta.helm.sh/release-name"],
		namespace: annotations["meta.helm.sh/release-namespace"],
	}
}

func scanJobSetFingerprints(ctx context.Context, c client.Client) ([]jobSetFingerprint, error) {
	types := []schema.GroupVersionKind{
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: kindValidatingWebhook},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: kindMutatingWebhook},
		{Group: rbacAPIGroup, Version: "v1", Kind: kindClusterRole},
	}
	var findings []jobSetFingerprint
	for _, gvk := range types {
		objects, err := listAllUnstructured(ctx, c, gvk, "")
		if err != nil {
			return nil, fmt.Errorf("list %s cluster-wide: %w", gvk.Kind, err)
		}
		for _, obj := range objects {
			if !objectHasJobSetRule(obj.Object, gvk.Kind) {
				continue
			}
			findings = append(findings, jobSetFingerprint{
				kind: gvk.Kind, name: obj.GetName(), object: obj,
				serviceRefs: webhookServiceRefs(obj.Object),
			})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].kind != findings[j].kind {
			return findings[i].kind < findings[j].kind
		}
		return findings[i].name < findings[j].name
	})
	return findings, nil
}

func listAllUnstructured(
	ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace string,
) ([]unstructured.Unstructured, error) {
	var result []unstructured.Unstructured
	continuation := ""
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		opts := []client.ListOption{client.Limit(200)}
		if namespace != "" {
			opts = append(opts, client.InNamespace(namespace))
		}
		if continuation != "" {
			opts = append(opts, client.Continue(continuation))
		}
		if err := c.List(ctx, list, opts...); err != nil {
			return nil, err
		}
		result = append(result, list.Items...)
		if list.GetContinue() == "" {
			return result, nil
		}
		continuation = list.GetContinue()
	}
}

func objectHasJobSetRule(object map[string]any, kind string) bool {
	var rules []any
	if kind == "ClusterRole" {
		rules, _, _ = unstructured.NestedSlice(object, "rules")
	} else {
		webhooks, _, _ := unstructured.NestedSlice(object, "webhooks")
		for _, webhook := range webhooks {
			wm, ok := webhook.(map[string]any)
			if !ok {
				continue
			}
			webhookRules, _, _ := unstructured.NestedSlice(wm, "rules")
			rules = append(rules, webhookRules...)
		}
	}
	for _, rule := range rules {
		rm, ok := rule.(map[string]any)
		if !ok {
			continue
		}
		groups, _, _ := unstructured.NestedStringSlice(rm, "apiGroups")
		if slices.Contains(groups, jobsetAPIGroup) {
			return true
		}
	}
	return false
}

func webhookServiceRefs(object map[string]any) []client.ObjectKey {
	webhooks, _, _ := unstructured.NestedSlice(object, "webhooks")
	var refs []client.ObjectKey
	for _, webhook := range webhooks {
		wm, ok := webhook.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(wm, "clientConfig", "service", "name")
		namespace, _, _ := unstructured.NestedString(wm, "clientConfig", "service", "namespace")
		if name != "" && namespace != "" {
			refs = append(refs, client.ObjectKey{Name: name, Namespace: namespace})
		}
	}
	return refs
}

// jobSetChartName is the JobSet chart's own name. `jobset.chart` renders
// `helm.sh/chart: <chart name>-<chart version>` from chart metadata alone,
// and the bundled subchart renders the same label, so it identifies the
// JobSet project whatever release name, `nameOverride`, or
// `fullnameOverride` an operator chose.
const jobSetChartName = "jobset"

// hasJobSetChartIdentity reports whether an object carries the JobSet
// chart's identity. A release that only consumes the JobSet API labels its
// resources after itself: Kueue, which NVCRE supports as a gang scheduler,
// renders `helm.sh/chart: kueue-<version>` on the ClusterRole and webhook
// configurations through which it reconciles JobSets.
//
// `app.kubernetes.io/name` is deliberately not accepted as an alternative.
// Both charts derive it from `default .Chart.Name .Values.nameOverride`, so
// a Kueue release installed with `nameOverride=jobset` would carry
// `app.kubernetes.io/name: jobset` while remaining a consumer. A configurable
// application name cannot establish ownership on its own.
func hasJobSetChartIdentity(object client.Object) bool {
	return chartNameFromLabel(object.GetLabels()["helm.sh/chart"]) == jobSetChartName
}

// chartNameFromLabel splits the chart name out of a `helm.sh/chart` label,
// which Helm renders as `<name>-<version>`. Only the final segment is a
// version, so `jobset-0.11.0` yields `jobset` while a hypothetical
// `jobset-operator-1.2.3` yields `jobset-operator` and does not match.
func chartNameFromLabel(chart string) string {
	index := strings.LastIndex(chart, "-")
	if index <= 0 {
		return ""
	}
	return chart[:index]
}

// jobSetAPIRegistrationEvidence returns the admission configuration that
// establishes the candidate release as a JobSet controller rather than a
// JobSet consumer.
//
// A rule naming the JobSet API group is not that evidence: any consumer
// reconciling JobSets holds the same rules and can register its own
// admission webhooks on the same resources and paths. What separates the two
// is that the release ships the JobSet project's own admission
// configuration, carrying the chart identity above. Without it the release
// may only react to JobSets, and disabling the bundled subchart would leave
// the cluster with no JobSet controller at all (ADR-078 decision 1).
func jobSetAPIRegistrationEvidence(findings []jobSetFingerprint) (string, bool) {
	for _, finding := range findings {
		if finding.kind != kindMutatingWebhook && finding.kind != kindValidatingWebhook {
			continue
		}
		if hasJobSetChartIdentity(&finding.object) {
			return finding.kind + "/" + finding.name, true
		}
	}
	return "", false
}

func verifyExternalJobSetController(
	ctx context.Context, c client.Client, findings []jobSetFingerprint, owner helmOwner,
) (string, error) {
	var owned []jobSetFingerprint
	var refs []client.ObjectKey
	var tokenParts []string
	// The chart renders two webhooks per configuration, one for JobSets and
	// one for their Pods, both pointing at the same Service. Verify each
	// distinct Service once.
	seen := map[client.ObjectKey]bool{}
	for _, finding := range findings {
		if completeHelmOwner(&finding.object) != owner {
			continue
		}
		owned = append(owned, finding)
		for _, ref := range finding.serviceRefs {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("matching roles exist without a JobSet webhook service reference")
	}
	if _, ok := jobSetAPIRegistrationEvidence(owned); !ok {
		return "", fmt.Errorf(
			"release %s/%s holds JobSet API-group rules but registers no admission configuration carrying the JobSet chart identity"+
				" (helm.sh/chart=%s-<version>), so it may only consume the JobSet API",
			owner.namespace, owner.release, jobSetChartName)
	}
	for _, ref := range refs {
		service := &unstructured.Unstructured{}
		service.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Service"})
		if err := c.Get(ctx, ref, service); err != nil {
			return "", fmt.Errorf("get webhook Service %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		if completeHelmOwner(service) != owner {
			return "", fmt.Errorf("webhook Service %s/%s has different or incomplete Helm ownership", ref.Namespace, ref.Name)
		}
		selector, _, _ := unstructured.NestedStringMap(service.Object, "spec", "selector")
		if len(selector) == 0 {
			return "", fmt.Errorf("webhook Service %s/%s has no selector", ref.Namespace, ref.Name)
		}
		deployments, err := listAllUnstructured(ctx, c,
			schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, ref.Namespace)
		if err != nil {
			return "", fmt.Errorf("list Deployments in %s: %w", ref.Namespace, err)
		}
		matched := false
		lookalike := ""
		for _, deployment := range deployments {
			labels, _, _ := unstructured.NestedStringMap(deployment.Object, "spec", "template", "metadata", "labels")
			if completeHelmOwner(&deployment) != owner || !selectorMatches(selector, labels) {
				continue
			}
			// The workload behind the webhook must be the JobSet controller
			// itself. A consumer's controller-manager backs its own webhook
			// Service on the same selector.
			if !hasJobSetChartIdentity(&deployment) {
				if lookalike == "" {
					lookalike = deployment.GetName()
				}
				continue
			}
			matched = true
			tokenParts = append(tokenParts, "Service/"+ref.Namespace+"/"+ref.Name+"/"+string(service.GetUID()),
				"Deployment/"+ref.Namespace+"/"+deployment.GetName()+"/"+string(deployment.GetUID()))
			break
		}
		if !matched {
			if lookalike != "" {
				return "", fmt.Errorf(
					"webhook Service %s/%s is backed by Deployment %s without the JobSet chart identity, so it may run a JobSet consumer rather than its controller",
					ref.Namespace, ref.Name, lookalike)
			}
			return "", fmt.Errorf("no same-release Deployment backs webhook Service %s/%s", ref.Namespace, ref.Name)
		}
	}
	sort.Strings(tokenParts)
	return "|" + strings.Join(tokenParts, "|"), nil
}

func selectorMatches(selector, labels map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func listJobSets(ctx context.Context, c client.Client) ([]unstructured.Unstructured, error) {
	return listAllUnstructured(ctx, c,
		schema.GroupVersionKind{Group: jobsetAPIGroup, Version: "v1alpha2", Kind: "JobSet"}, "")
}

func fingerprintNames(findings []jobSetFingerprint) string {
	names := make([]string, 0, len(findings))
	for _, finding := range findings {
		names = append(names, finding.kind+"/"+finding.name)
	}
	return strings.Join(names, ", ")
}

func releaseManifestIdentities(raw string) (map[string]struct{}, error) {
	result := map[string]struct{}{}
	reader := utilyaml.NewYAMLToJSONDecoder(bufio.NewReader(strings.NewReader(raw)))
	for {
		var object map[string]any
		if err := reader.Decode(&object); err != nil {
			if err == io.EOF {
				return result, nil
			}
			return nil, err
		}
		if len(object) == 0 {
			continue
		}
		u := unstructured.Unstructured{Object: object}
		namespace := u.GetNamespace()
		if namespace == "" && !clusterScopedManifestKind(u.GetKind()) {
			namespace = trainerNamespace
		}
		result[manifestIdentity(u.GetAPIVersion(), u.GetKind(), namespace, u.GetName())] = struct{}{}
	}
}

func releaseManifestJobSetConsumerIdentities(raw string) (map[string]string, error) {
	result := map[string]string{}
	reader := utilyaml.NewYAMLToJSONDecoder(bufio.NewReader(strings.NewReader(raw)))
	for {
		var object map[string]any
		if err := reader.Decode(&object); err != nil {
			if err == io.EOF {
				return result, nil
			}
			return nil, err
		}
		if len(object) == 0 {
			continue
		}
		u := unstructured.Unstructured{Object: object}
		labels := u.GetLabels()
		if u.GetAPIVersion() == rbacV1APIVersion && u.GetKind() == kindClusterRole &&
			u.GetName() == "kubeflow-trainer-controller-manager" &&
			strings.HasPrefix(labels["helm.sh/chart"], "kubeflow-trainer-") &&
			labels["app.kubernetes.io/name"] == "kubeflow-trainer" &&
			labels["app.kubernetes.io/component"] == "manager" && objectHasJobSetRule(u.Object, u.GetKind()) {
			result[manifestIdentity(u.GetAPIVersion(), u.GetKind(), "", u.GetName())] = labels["helm.sh/chart"]
		}
	}
}

func manifestIdentity(apiVersion, kind, namespace, name string) string {
	return apiVersion + "/" + kind + "/" + namespace + "/" + name
}

func clusterScopedManifestKind(kind string) bool {
	switch kind {
	case kindCustomResourceDefinition, kindNamespace, kindClusterRole, "ClusterRoleBinding",
		kindValidatingWebhook, kindMutatingWebhook, "ClusterTrainingRuntime":
		return true
	default:
		return false
	}
}

func manifestContainsJobSet(objects map[string]struct{}) bool {
	clusterObjects := [][3]string{
		{"apiextensions.k8s.io/v1", kindCustomResourceDefinition, jobSetCRDName},
		{rbacV1APIVersion, kindClusterRole, jobSetControllerName},
		{rbacV1APIVersion, "ClusterRoleBinding", jobSetControllerName},
		{"admissionregistration.k8s.io/v1", kindMutatingWebhook, jobSetMutatingWebhookConfigurationName},
		{"admissionregistration.k8s.io/v1", kindValidatingWebhook, jobSetValidatingWebhookConfigurationName},
	}
	for _, object := range clusterObjects {
		if _, ok := objects[manifestIdentity(object[0], object[1], "", object[2])]; ok {
			return true
		}
	}
	namespacedObjects := [][2]string{
		{"v1", "ServiceAccount"}, {"v1", "Service"}, {"v1", "ConfigMap"}, {"v1", "Secret"},
		{"apps/v1", "Deployment"}, {"rbac.authorization.k8s.io/v1", "Role"},
		{"rbac.authorization.k8s.io/v1", "RoleBinding"},
	}
	knownNames := []string{jobSetControllerName, jobSetWebhookServiceName, "jobset-metrics-service",
		"jobset-controller-config", "jobset-webhook-server-cert"}
	for _, object := range namespacedObjects {
		for _, name := range knownNames {
			if _, ok := objects[manifestIdentity(object[0], object[1], trainerNamespace, name)]; ok {
				return true
			}
		}
	}
	return false
}

func missingJobSetCRDError(out io.Writer, state string) error {
	_, _ = fmt.Fprintf(out,
		"[deps] JobSet CRD %s is missing while the Trainer release is %q. Restore a compatible CRD using the intended JobSet installation procedure, wait for it to become established, and rerun setup init. Restoring the CRD cannot restore deleted JobSets.\n",
		jobSetCRDName, state)
	return fmt.Errorf("[deps] existing Kubeflow Trainer release is missing JobSet CRD %s", jobSetCRDName)
}

func printUnknownTrainerStateFallback(out io.Writer, cause error) {
	_, _ = fmt.Fprintf(out, "[deps] Cannot determine the Kubeflow Trainer release state: %v\n", cause)
	_, _ = fmt.Fprintln(out, "[deps] Resolve the Helm release-state inspection failure, then rerun setup init. No Trainer mutation was attempted.")
	_, _ = fmt.Fprintln(out, "[deps] Alternatively, independently verify the existing Trainer installation and JobSet ownership before managing Trainer manually.")
	printNVCREInstallFallback(out)
}

func printJobSetOwnershipFallback(out io.Writer, observation jobSetObservation) {
	_, _ = fmt.Fprintln(out, "[deps] JobSet ownership inspection is inconclusive:")
	for _, evidence := range observation.evidence {
		_, _ = fmt.Fprintf(out, "  - %s\n", evidence)
	}
	printManualTrainerInstallFallback(out)
}

func printManualTrainerInstallFallback(out io.Writer) {
	_, _ = fmt.Fprintln(out, "[deps] Resolve JobSet ownership, then install Kubeflow Trainer manually with the appropriate jobset.install value.")
	printNVCREInstallFallback(out)
}

func printNVCREInstallFallback(out io.Writer) {
	_, _ = fmt.Fprintln(out, "[deps] Continue with: nvcrectl setup init --skip-phases=deps")
	_, _ = fmt.Fprintln(out, "[deps] This skips Trainer setup only and still installs NVCRE. A dev CLI build requires --version <nvcre-chart-version>.")
	_, _ = fmt.Fprintln(out, "[deps] Dev-build command: nvcrectl setup init --skip-phases=deps --version <nvcre-chart-version>")
	_, _ = fmt.Fprintln(out, "[deps] The NVCRE chart must be reachable; optionally use --chart-ref <nvcre-chart-reference> with registry access.")
}

func printTrainerFailureGuidance(sp setupPhaseParams, output string) {
	if classifyTrainerInstallFailure(output) != failureClassJobSetOwnership {
		return
	}
	_, _ = fmt.Fprintln(sp.out, "[deps] Helm reported invalid ownership metadata for a concrete JobSet resource.")
	_, _ = fmt.Fprintln(sp.out, "[deps] If external ownership is confirmed, install Trainer manually with jobset.install=false.")
	printManualTrainerInstallFallback(sp.out)
	_, _ = fmt.Fprintln(sp.out, "[deps] Older CLIs also require setup reset --skip-phases=deps to preserve external JobSet.")
}
