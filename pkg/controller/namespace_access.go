// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/rbac"
)

const (
	// DefaultNamespaceAccessTimeout bounds how long a Workflow waits for the
	// controller's write access in its namespace before it fails (ADR-092).
	// cmd/manager's --namespace-access-timeout overrides it.
	DefaultNamespaceAccessTimeout = 10 * time.Minute

	// accessReviewCacheTTL is how long a review answer is reused, so a
	// Workflow that waits, or a namespace running several Workflows, does not
	// send a burst of SelfSubjectAccessReviews on every reconcile.
	accessReviewCacheTTL = 30 * time.Second
)

// AccessCheck is one permission the controller needs in a namespace.
type AccessCheck struct {
	Group       string `json:"group"`
	Resource    string `json:"resource"`
	Subresource string `json:"subresource,omitempty"`
	Verb        string `json:"verb"`
}

// String formats the check the way kubectl auth can-i takes it: verb, then
// resource[/subresource][.group].
func (c AccessCheck) String() string {
	resource := c.Resource
	if c.Subresource != "" {
		resource += "/" + c.Subresource
	}
	if c.Group != "" {
		resource += "." + c.Group
	}
	return c.Verb + " " + resource
}

func compareAccessChecks(a, b AccessCheck) int {
	return cmp.Or(
		cmp.Compare(a.Group, b.Group),
		cmp.Compare(a.Resource, b.Resource),
		cmp.Compare(a.Subresource, b.Subresource),
		cmp.Compare(a.Verb, b.Verb),
	)
}

// AccessReviewer answers whether the controller holds permissions in a
// namespace. envtest does not enforce RBAC, so tests inject an implementation
// in place of the SelfSubjectAccessReview one.
type AccessReviewer interface {
	// Review returns the checks the controller is denied in namespace. An
	// error means the answer is unknown; callers must never treat it as
	// permission. fresh bypasses any cached answer.
	Review(ctx context.Context, namespace string, checks []AccessCheck, fresh bool) ([]AccessCheck, error)
}

// CachedAccessReviewer is implemented by an AccessReviewer that keeps its
// answers. CachedDenials returns the checks a cached answer denies, without
// asking the API server; a check with no live cached answer is not denied.
type CachedAccessReviewer interface {
	CachedDenials(namespace string, checks []AccessCheck) []AccessCheck
}

// NewAccessReviewer returns an AccessReviewer that asks the API server with
// one SelfSubjectAccessReview per check, through c, and reuses each answer
// for accessReviewCacheTTL. A review is a create that is never stored, so it
// always reaches the API server whatever c caches.
func NewAccessReviewer(c client.Client) AccessReviewer {
	return &ssarReviewer{client: c, ttl: accessReviewCacheTTL, now: time.Now, cache: map[accessKey]accessAnswer{}}
}

type accessKey struct {
	namespace string
	check     AccessCheck
}

type accessAnswer struct {
	allowed bool
	at      time.Time
}

type ssarReviewer struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[accessKey]accessAnswer
}

func (r *ssarReviewer) Review(ctx context.Context, namespace string, checks []AccessCheck, fresh bool) ([]AccessCheck, error) {
	var denied []AccessCheck
	for _, check := range checks {
		allowed, err := r.allowed(ctx, namespace, check, fresh)
		if err != nil {
			return nil, err
		}
		if !allowed {
			denied = append(denied, check)
		}
	}
	return denied, nil
}

func (r *ssarReviewer) CachedDenials(namespace string, checks []AccessCheck) []AccessCheck {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var denied []AccessCheck
	for _, check := range checks {
		if answer, ok := r.cache[accessKey{namespace: namespace, check: check}]; ok &&
			now.Sub(answer.at) < r.ttl && !answer.allowed {
			denied = append(denied, check)
		}
	}
	return denied
}

func (r *ssarReviewer) allowed(ctx context.Context, namespace string, check AccessCheck, fresh bool) (bool, error) {
	key := accessKey{namespace: namespace, check: check}
	if !fresh {
		r.mu.Lock()
		answer, ok := r.cache[key]
		r.mu.Unlock()
		if ok && r.now().Sub(answer.at) < r.ttl {
			return answer.allowed, nil
		}
	}

	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        check.Verb,
				Group:       check.Group,
				Resource:    check.Resource,
				Subresource: check.Subresource,
			},
		},
	}
	if err := r.client.Create(ctx, review); err != nil {
		return false, fmt.Errorf("SelfSubjectAccessReview for %q in namespace %q: %w", check.String(), namespace, err)
	}
	allowed := review.Status.Allowed && !review.Status.Denied
	if !allowed && review.Status.EvaluationError != "" {
		logf.FromContext(ctx).V(1).Info("SelfSubjectAccessReview denied with an evaluation error",
			"namespace", namespace, "check", check.String(), "evaluationError", review.Status.EvaluationError)
	}

	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Answers for namespaces nothing runs in any more would otherwise
	// accumulate for the life of the process.
	maps.DeleteFunc(r.cache, func(_ accessKey, a accessAnswer) bool { return now.Sub(a.at) >= r.ttl })
	r.cache[key] = accessAnswer{allowed: allowed, at: now}
	return allowed, nil
}

// resourceConfigMaps is the RBAC resource of node-result records.
const resourceConfigMaps = "configmaps"

// RBAC verbs the access check asks about.
const (
	verbGet    = "get"
	verbCreate = "create"
	verbUpdate = "update"
	verbDelete = "delete"
)

// Verbs the controller uses on the namespaced objects it writes. Each list is
// what the code calls, not what nvcre-run-role grants: a narrower custom Role
// that covers these is enough.
var (
	// recordNodeResults writes node-result ConfigMaps with CreateOrUpdate.
	nodeResultsConfigMapVerbs = []string{verbGet, verbCreate, verbUpdate}
	// The Job controller creates and deletes the TrainJob. Reads stay
	// cluster-wide in nvcre-manager-role for the Owns() watch.
	trainJobVerbs = []string{verbCreate, verbDelete}
	// createDependencyResource creates, getDependencyObject reads,
	// setDependencyOwners updates, and cleanup deletes every dependency.
	dependencyVerbs = []string{verbGet, verbCreate, verbUpdate, verbDelete}
)

// requiredNamespaceAccess returns, by namespace, the permissions the Workflow
// will use there. The Workflow's own namespace always needs the node-result
// ConfigMaps, the TrainJob and pod logs for measurements; each namespaced
// dependency kind adds its lifecycle verbs in the namespace it is created in.
//
// The set follows the spec as the caller holds it. Before node discovery that
// spec has not had platform or GPU overrides applied, so a kind an override
// adds is not checked until a write to it is forbidden, which
// awaitAccessOnForbidden then classifies against the full post-override spec.
//
// A dependency whose kind the API server does not serve is skipped: creating
// it fails with its own no-match error, which says more than an access check
// could.
func requiredNamespaceAccess(workflow *nvcrev1alpha1.Workflow, mapper meta.RESTMapper) map[string][]AccessCheck {
	sets := map[string]map[AccessCheck]struct{}{}
	add := func(namespace, group, resource, subresource string, verbs ...string) {
		if sets[namespace] == nil {
			sets[namespace] = map[AccessCheck]struct{}{}
		}
		for _, verb := range verbs {
			sets[namespace][AccessCheck{Group: group, Resource: resource, Subresource: subresource, Verb: verb}] = struct{}{}
		}
	}

	add(workflow.Namespace, "", resourceConfigMaps, "", nodeResultsConfigMapVerbs...)
	add(workflow.Namespace, "trainer.kubeflow.org", "trainjobs", "", trainJobVerbs...)
	add(workflow.Namespace, "", "pods", "log", verbGet)

	for _, dep := range workflow.Spec.Dependencies {
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(dep.Raw, &obj.Object); err != nil {
			continue
		}
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" || mapper == nil {
			continue
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil || mapping.Scope.Name() != meta.RESTScopeNameNamespace {
			continue
		}
		namespace := obj.GetNamespace()
		if namespace == "" {
			namespace = workflow.Namespace
		}
		add(namespace, mapping.Resource.Group, mapping.Resource.Resource, "", dependencyVerbs...)
	}

	out := make(map[string][]AccessCheck, len(sets))
	for namespace, set := range sets {
		out[namespace] = slices.SortedFunc(maps.Keys(set), compareAccessChecks)
	}
	return out
}

// namespaceAccessResult is the outcome of reviewing every namespace a
// Workflow writes to. A denial outranks a review error: it names the fix.
type namespaceAccessResult struct {
	denied         map[string][]AccessCheck
	reviewErr      error
	reviewErrInNS  string
	serviceAccount rbac.ServiceAccount
}

// forWrite narrows a fresh review to the write that was forbidden. A Forbidden
// error names the resource it refused but not the verb, so the whole required
// set is reviewed and only denials of that resource count: a denial elsewhere
// in the set did not cause this rejection, and attributing it there would
// turn an admission or quota rejection into a wait, hiding the policy's
// message until the timeout. A review that could not run is kept, since it
// cannot tell either way. An error that names no resource keeps every denial.
func (a namespaceAccessResult) forWrite(writeErr error) namespaceAccessResult {
	refused, ok := forbiddenResource(writeErr)
	if !ok || len(a.denied) == 0 {
		return a
	}
	narrowed := a
	narrowed.denied = nil
	for ns, checks := range a.denied {
		for _, c := range checks {
			if c.Group == refused.Group && c.Resource == refused.Resource {
				if narrowed.denied == nil {
					narrowed.denied = map[string][]AccessCheck{}
				}
				narrowed.denied[ns] = append(narrowed.denied[ns], c)
			}
		}
	}
	return narrowed
}

// forbiddenResource returns the group and resource a Forbidden API error
// names. The API server puts the resource in the status details' Kind field.
func forbiddenResource(err error) (schema.GroupResource, bool) {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return schema.GroupResource{}, false
	}
	details := status.Status().Details
	if details == nil || details.Kind == "" {
		return schema.GroupResource{}, false
	}
	return schema.GroupResource{Group: details.Group, Resource: details.Kind}, true
}

func (a namespaceAccessResult) granted() bool {
	return len(a.denied) == 0 && a.reviewErr == nil
}

// reason is the InProgress (and, on timeout, Failed) reason for the wait.
func (a namespaceAccessResult) reason() string {
	if len(a.denied) > 0 {
		return ReasonNamespacePermissionsMissing
	}
	return ReasonNamespaceAccessCheckFailed
}

// eventReason is the Warning emitted when the wait begins.
func (a namespaceAccessResult) eventReason() string {
	if len(a.denied) > 0 {
		return ReasonNamespaceAccessDenied
	}
	return ReasonNamespaceAccessReviewError
}

func (a namespaceAccessResult) deniedNamespaces() []string {
	return slices.Sorted(maps.Keys(a.denied))
}

// problem says what the controller cannot do, or why it could not find out.
func (a namespaceAccessResult) problem() string {
	if len(a.denied) == 0 {
		return fmt.Sprintf("Could not check the controller's access in namespace %q: %v",
			a.reviewErrInNS, a.reviewErr)
	}
	var parts []string
	for _, ns := range a.deniedNamespaces() {
		checks := make([]string, 0, len(a.denied[ns]))
		for _, c := range a.denied[ns] {
			checks = append(checks, c.String())
		}
		parts = append(parts, fmt.Sprintf("%s in namespace %q", strings.Join(checks, ", "), ns))
	}
	return fmt.Sprintf("ServiceAccount %s cannot %s", a.serviceAccount, strings.Join(parts, ", and cannot "))
}

// message is the waiting message: the problem and how to fix it.
func (a namespaceAccessResult) message() string {
	if len(a.denied) == 0 {
		return a.problem()
	}
	// The print command defaults to the ServiceAccount `nvcrectl setup init`
	// installs, so another release's account has to be named, or the
	// binding applied would grant the wrong subject.
	saFlags := ""
	if a.serviceAccount != rbac.DefaultServiceAccount() {
		saFlags = fmt.Sprintf(" --controller-namespace %s --service-account %s",
			a.serviceAccount.Namespace, a.serviceAccount.Name)
	}
	var commands []string
	for _, ns := range a.deniedNamespaces() {
		commands = append(commands, fmt.Sprintf("nvcrectl rbac print --namespace %s%s | kubectl apply -f -", ns, saFlags))
	}
	where := "there"
	if len(commands) > 1 {
		where = "in each"
	}
	return fmt.Sprintf("%s. Bind ClusterRole %s to it %s with RoleBinding %s: %s",
		a.problem(), rbac.RunRoleName, where, rbac.RunBindingName, strings.Join(commands, "; "))
}

// timeoutMessage is the Failed message once the wait has run out. For a
// denial it carries the RoleBinding to apply, so the failure names its fix.
// It names the configured timeout rather than the measured wait, which
// depends on requeue timing and second-truncated transition times.
func (a namespaceAccessResult) timeoutMessage(timeout time.Duration) string {
	head := fmt.Sprintf("Waited longer than the namespace access timeout (%s) for write access. %s",
		timeout, a.problem())
	if len(a.denied) == 0 {
		return head
	}
	var manifests []string
	for _, ns := range a.deniedNamespaces() {
		manifest, err := rbac.RunBindingYAML(ns, a.serviceAccount)
		if err != nil {
			continue
		}
		manifests = append(manifests, manifest)
	}
	return head + ". Apply this RoleBinding, then run again:\n" + strings.Join(manifests, "---\n")
}

// controllerServiceAccount returns the ServiceAccount the controller runs as,
// defaulting to the one `nvcrectl setup init` installs.
func (r *WorkflowReconciler) controllerServiceAccount() rbac.ServiceAccount {
	sa := r.ServiceAccount
	def := rbac.DefaultServiceAccount()
	if sa.Namespace == "" {
		sa.Namespace = def.Namespace
	}
	if sa.Name == "" {
		sa.Name = def.Name
	}
	return sa
}

func (r *WorkflowReconciler) namespaceAccessTimeout() time.Duration {
	if r.NamespaceAccessTimeout > 0 {
		return r.NamespaceAccessTimeout
	}
	return DefaultNamespaceAccessTimeout
}

func (r *WorkflowReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// reviewNamespaceAccess reviews every namespace the Workflow writes to.
func (r *WorkflowReconciler) reviewNamespaceAccess(ctx context.Context, workflow *nvcrev1alpha1.Workflow, fresh bool) namespaceAccessResult {
	result := namespaceAccessResult{serviceAccount: r.controllerServiceAccount()}
	var mapper meta.RESTMapper
	if r.Client != nil {
		mapper = r.RESTMapper()
	}
	required := requiredNamespaceAccess(workflow, mapper)
	for _, ns := range slices.Sorted(maps.Keys(required)) {
		denied, err := r.AccessReviewer.Review(ctx, ns, required[ns], fresh)
		if err != nil {
			if result.reviewErr == nil {
				result.reviewErr, result.reviewErrInNS = err, ns
			}
			continue
		}
		if len(denied) > 0 {
			if result.denied == nil {
				result.denied = map[string][]AccessCheck{}
			}
			result.denied[ns] = denied
		}
	}
	return result
}

// gateNamespaceAccess checks the controller's access in every namespace the
// Workflow writes to, before it writes anything there. It reports whether the
// caller must stop and return the result as is: the Workflow is waiting, or
// this pass ended a wait.
//
// Ending a wait writes status, and a status write refreshes workflow in place,
// which leaves any pointer the caller holds into its status (the
// orchestration status above all) pointing at the old copy. So that pass ends
// there and requeues at once, like ensureFinalizer; the next starts from what
// was written and passes the gate without writing.
func (r *WorkflowReconciler) gateNamespaceAccess(ctx context.Context, workflow *nvcrev1alpha1.Workflow) (ctrl.Result, bool, error) {
	if r.AccessReviewer == nil {
		return ctrl.Result{}, false, nil
	}
	access := r.reviewNamespaceAccess(ctx, workflow, false)
	if access.granted() {
		ended, err := r.markNamespaceAccessGranted(ctx, workflow)
		if err != nil || ended {
			return ctrl.Result{RequeueAfter: requeueImmediate}, true, err
		}
		return ctrl.Result{}, false, nil
	}
	result, err := r.waitForNamespaceAccess(ctx, workflow, access)
	return result, true, err
}

// gateKnownDenial is the check before observing running groups or completing
// an iteration. Those passes run on every requeue of a running Workflow, so
// it asks the API server nothing: it waits only on a denial already cached,
// which requeueAfterForbiddenWrite puts there when a node-result write is
// forbidden. A reviewer that keeps no answers gets a full check instead.
func (r *WorkflowReconciler) gateKnownDenial(ctx context.Context, workflow *nvcrev1alpha1.Workflow) (ctrl.Result, bool, error) {
	if r.AccessReviewer == nil {
		return ctrl.Result{}, false, nil
	}
	cached, ok := r.AccessReviewer.(CachedAccessReviewer)
	if !ok {
		return r.gateNamespaceAccess(ctx, workflow)
	}
	access := namespaceAccessResult{serviceAccount: r.controllerServiceAccount()}
	var mapper meta.RESTMapper
	if r.Client != nil {
		mapper = r.RESTMapper()
	}
	for ns, checks := range requiredNamespaceAccess(workflow, mapper) {
		if denied := cached.CachedDenials(ns, checks); len(denied) > 0 {
			if access.denied == nil {
				access.denied = map[string][]AccessCheck{}
			}
			access.denied[ns] = denied
		}
	}
	if access.granted() {
		return ctrl.Result{}, false, nil
	}
	result, err := r.waitForNamespaceAccess(ctx, workflow, access)
	return result, true, err
}

// awaitAccessOnForbidden classifies a Forbidden write error, and reports
// whether the caller must stop and return the result as is. HTTP 403 does not
// mean only RBAC: a ValidatingAdmissionPolicy, an admission webhook and an
// exhausted ResourceQuota all reject with it too. Rather than match on message
// text, which differs between admission plugins and Kubernetes versions, it
// asks the API server again with fresh reviews. A denial means the binding was
// removed, or never covered a kind an override added, and the Workflow waits.
// A grant means the rejection came from admission or quota, and the caller's
// existing error path runs so the policy's message reaches the status at once.
func (r *WorkflowReconciler) awaitAccessOnForbidden(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow, writeErr error, extra ...func(*nvcrev1alpha1.Workflow) bool,
) (ctrl.Result, bool, error) {
	if r.AccessReviewer == nil || !apierrors.IsForbidden(writeErr) {
		return ctrl.Result{}, false, nil
	}
	access := r.reviewNamespaceAccess(ctx, workflow, true).forWrite(writeErr)
	if access.granted() {
		logf.FromContext(ctx).Info("Write was forbidden although the access review allows it; "+
			"treating it as an admission or quota rejection", "error", writeErr.Error())
		return ctrl.Result{}, false, nil
	}
	logf.FromContext(ctx).Info("Write was forbidden and an access review confirms the denial",
		"error", writeErr.Error())
	result, err := r.waitForNamespaceAccess(ctx, workflow, access, extra...)
	return result, true, err
}

// requeueAfterForbiddenWrite handles a pass that ended on a Forbidden write it
// could not leave half done, such as node results written before a terminal
// status. Such a pass returns before writing status, and the in-memory object
// carries transitions that must not be persisted (a counted iteration, an
// advanced diagnose stage), so the wait is not written from it. It is written
// from a copy of the Workflow loaded fresh from the API server instead, which
// holds only the persisted state, so the wait starts its clock, and its
// timeout and backoff, in this pass. That holds whether the fresh review was
// denied or could not run: failed reviews are not cached, so leaving the wait
// to a later pass's cached-denial check would retry the write forever with no
// wait recorded. A node-result write arrives already classified by
// nodeResultsWriteError, which handles an admission or quota rejection itself;
// any other Forbidden the fresh review allows is returned as it is.
func (r *WorkflowReconciler) requeueAfterForbiddenWrite(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
) func(ctrl.Result, error) (ctrl.Result, error) {
	return func(result ctrl.Result, err error) (ctrl.Result, error) {
		if err == nil || r.AccessReviewer == nil || !apierrors.IsForbidden(err) {
			return result, err
		}
		var access namespaceAccessResult
		if denied, ok := errors.AsType[*accessDeniedWriteError](err); ok {
			access = denied.access
		} else {
			access = r.reviewNamespaceAccess(ctx, workflow, true).forWrite(err)
		}
		if access.granted() {
			return result, err
		}
		persisted := &nvcrev1alpha1.Workflow{}
		if getErr := r.jobReader().Get(ctx, client.ObjectKeyFromObject(workflow), persisted); getErr != nil {
			return ctrl.Result{}, errors.Join(err, fmt.Errorf("reload Workflow to record the access wait: %w", getErr))
		}
		logf.FromContext(ctx).Info("Write forbidden and the access review does not allow it; "+
			"waiting for access", "error", err.Error())
		return r.waitForNamespaceAccess(ctx, persisted, access)
	}
}

// waitForNamespaceAccess keeps the Workflow InProgress while it waits for
// access, and fails it once the wait has outlasted the timeout. The wait is
// clocked on the NamespaceAccess condition, not on InProgress, whose
// lastTransitionTime dates from when the Workflow started.
func (r *WorkflowReconciler) waitForNamespaceAccess(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow, access namespaceAccessResult,
	extra ...func(*nvcrev1alpha1.Workflow) bool,
) (ctrl.Result, error) {
	now := r.clock()
	since := now
	if cond := meta.FindStatusCondition(workflow.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess); cond != nil &&
		cond.Status == metav1.ConditionFalse {
		since = cond.LastTransitionTime.Time
	}
	waited := now.Sub(since)
	timeout := r.namespaceAccessTimeout()
	reason, message := access.reason(), access.message()

	// started reports whether this write begins the wait, or changes its
	// cause. It is decided inside the status closure, on the object the
	// write is computed from, so a conflict retry cannot emit twice.
	started := false
	setAccess := func(w *nvcrev1alpha1.Workflow) bool {
		prev := meta.FindStatusCondition(w.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess)
		started = prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason
		// The transition time is the wait's clock, so it is stamped from the
		// same clock the timeout is measured with. SetStatusCondition keeps
		// it while the condition stays False.
		return meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.WorkflowNamespaceAccess,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: w.GetGeneration(),
			LastTransitionTime: metav1.NewTime(now),
		})
	}
	writers := append(slices.Clone(extra), setAccess)

	if waited >= timeout {
		logf.FromContext(ctx).Info("Namespace access wait timed out", "reason", reason, "waited", waited)
		if err := r.setWorkflowFailed(ctx, workflow, reason, access.timeoutMessage(timeout), writers...); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.setWorkflowInProgress(ctx, workflow, reason, message, writers...); err != nil {
		return ctrl.Result{}, err
	}
	if started {
		r.eventf(workflow, corev1.EventTypeWarning, access.eventReason(), "%s", message)
	}
	logf.FromContext(ctx).Info("Waiting for namespace access", "reason", reason,
		"waited", waited.Round(time.Second), "timeout", timeout)
	return ctrl.Result{RequeueAfter: min(r.namespaceAccessRequeue(reason, waited), timeout-waited)}, nil
}

// maxAccessCheckBackoff caps the requeue while access reviews cannot run.
const maxAccessCheckBackoff = 2 * time.Minute

// namespaceAccessRequeue is how long a waiting Workflow waits before checking
// again. A missing binding is rechecked at the ordinary requeue interval: it
// usually lands within seconds, and answers are cached per namespace, so the
// API server sees a few reviews per namespace per half minute. A review that
// could not be created is never cached, and the usual cause is an API server
// or webhook under strain, so that wait backs off: the interval grows with
// the time already waited, doubling each pass, up to maxAccessCheckBackoff.
func (r *WorkflowReconciler) namespaceAccessRequeue(reason string, waited time.Duration) time.Duration {
	base := r.getJobRequeueInterval()
	if reason != ReasonNamespaceAccessCheckFailed {
		return base
	}
	return max(base, min(waited, maxAccessCheckBackoff))
}

// markNamespaceAccessGranted ends a wait: it flips NamespaceAccess to True
// when it is False, so a later wait starts its own clock, and reports whether
// it wrote. A Workflow that never waited carries no NamespaceAccess
// condition, and gets none here.
func (r *WorkflowReconciler) markNamespaceAccessGranted(ctx context.Context, workflow *nvcrev1alpha1.Workflow) (bool, error) {
	cond := meta.FindStatusCondition(workflow.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		return false, nil
	}
	message := fmt.Sprintf("ServiceAccount %s has the write access it needs", r.controllerServiceAccount())
	flipped := false
	if err := updateStatusWithRetry(ctx, r.Client, workflow, func(w *nvcrev1alpha1.Workflow) bool {
		prev := meta.FindStatusCondition(w.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess)
		if prev == nil || prev.Status != metav1.ConditionFalse {
			flipped = false
			return false
		}
		flipped = meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
			Type:               nvcrev1alpha1.WorkflowNamespaceAccess,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonNamespaceAccessGranted,
			Message:            message,
			ObservedGeneration: w.GetGeneration(),
			LastTransitionTime: metav1.NewTime(r.clock()),
		})
		return flipped
	}); err != nil {
		return false, fmt.Errorf("failed to record namespace access: %w", err)
	}
	if flipped {
		r.eventf(workflow, corev1.EventTypeNormal, ReasonNamespaceAccessGranted, "%s", message)
	}
	return flipped, nil
}

// namespaceTerminating reports whether namespace is being deleted. A
// namespace that is already gone counts: nothing in it is left to clean up.
// It reads through reader, which must be uncached: a cached Namespace read
// would start a cluster-wide informer the role cannot list or watch, the
// failure mode of issue #424.
func namespaceTerminating(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("failed to read namespace %s: %w", namespace, err)
	}
	return !ns.DeletionTimestamp.IsZero(), nil
}

// forbiddenInTerminatingNamespace reports whether err is a Forbidden child
// read or delete that teardown can skip because namespace is terminating
// (ADR-092 decision 5). The namespace controller deletes what is left, and the
// RoleBinding that granted access may already be gone, so retrying would hold
// the finalizer, and with it the namespace, forever. A failed namespace read
// is returned rather than guessed at, so the caller keeps its finalizer.
func forbiddenInTerminatingNamespace(ctx context.Context, reader client.Reader, namespace string, err error) (bool, error) {
	if namespace == "" || !apierrors.IsForbidden(err) {
		return false, nil
	}
	return namespaceTerminating(ctx, reader, namespace)
}
