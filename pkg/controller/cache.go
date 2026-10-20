// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/podread"
)

// GPUNodeLabel marks a node as GPU-equipped. The controllers only ever operate
// on GPU nodes — discoverTargetNodes filters on this label before a node can be
// partitioned into a group — so it also defines the Node cache boundary.
const GPUNodeLabel = "nvidia.com/gpu.present"

// present is the label value indicating a node has the GPUNodeLabel set.
const present = "true"

// cacheScope is a kind the manager's cache holds under a label selector, and
// the condition a read of it must meet to be answered completely from the
// cache (ADR-102). requiredLabel is the label every cached object carries; a
// List must require it (=, ==, in or exists) or use one of scopedIndexes,
// field indexes that only ever hold cached objects. A Get and an informer are
// always refused, since a missing object and an out-of-scope one look alike.
type cacheScope struct {
	gvk           schema.GroupVersionKind
	requiredLabel string
	scopedIndexes []string
}

// guardedScopes lists the scoped kinds whose cached reads are checked.
// CacheOptions builds each one's cache selector from its requiredLabel.
var guardedScopes = []cacheScope{
	{
		gvk:           corev1.SchemeGroupVersion.WithKind("Pod"),
		requiredLabel: podread.JobLabel,
		scopedIndexes: []string{podread.JobIndexField},
	},
}

// exemptScopes lists kinds the cache holds under a selector whose reads are
// not checked, with the reason. Moving a kind to guardedScopes means making
// its reads meet the condition first.
var exemptScopes = map[schema.GroupVersionKind]string{
	corev1.SchemeGroupVersion.WithKind("Node"): "scoped to " + GPUNodeLabel +
		" before ADR-102; discoverTargetNodes lists by the target's selector and filters in Go, " +
		"and Node Gets by name predate the guard",
}

// CacheOptions returns the informer cache configuration for the manager.
//
// Without it, controller-runtime caches every object of every watched kind,
// cluster-wide and in full. This controller caches Nodes, Pods and
// PersistentVolumes, and the Pod field indexes (indexes.go) start the Pod
// informer when the manager starts, so on a large GPU cluster the default
// cache would hold every Pod in the cluster in the controller's heap (issue
// #504). Three reductions are applied:
//
//   - Nodes are restricted to GPU nodes. Non-GPU nodes (control plane, CPU
//     workers, infra) are never targets, never run burn-in pods, and never need
//     health evaluation, so caching and watching them is pure overhead.
//
//   - Pods are restricted to those carrying the NVCRE job label (ADR-102), the
//     workload pods NVCRE creates. Controller code reads Pods through
//     pkg/podread, which reads other Pods, such as MPI launchers and
//     kube-system pods, from the API server; NewCache refuses a cached Pod
//     read that could need them.
//
//   - managedFields is stripped from every cached object. It is metadata the
//     controllers never read and is frequently a large fraction of an object's
//     serialized size, particularly for Pods and CRs written by several
//     controllers. No ByObject entry sets its own Transform, which would
//     replace this default rather than add to it.
func CacheOptions() cache.Options {
	byObject := map[client.Object]cache.ByObject{
		&corev1.Node{}: {
			Label: labels.SelectorFromSet(labels.Set{GPUNodeLabel: present}),
		},
	}
	for _, s := range guardedScopes {
		obj, err := clientgoscheme.Scheme.New(s.gvk)
		if err != nil {
			panic(fmt.Sprintf("guarded cache scope %s is not in the client-go scheme: %v", s.gvk, err))
		}
		byObject[obj.(client.Object)] = cache.ByObject{Label: labelExists(s.requiredLabel)}
	}
	return cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject:         byObject,
	}
}

// labelExists selects objects that carry key, whatever its value.
func labelExists(key string) labels.Selector {
	req, err := labels.NewRequirement(key, selection.Exists, nil)
	if err != nil {
		// The keys are constants and valid; this cannot fail.
		panic(fmt.Sprintf("building the %s selector: %v", key, err))
	}
	return labels.NewSelector().Add(*req)
}

// errUnscopedCacheRead is returned for a read the scoped cache cannot answer
// completely.
var errUnscopedCacheRead = errors.New("the cache holds only part of this kind, and this read could need the rest " +
	"(ADR-102); read Pods through pkg/podread")

// Labels of unscopedCacheReadsTotal, also used as log keys.
const (
	labelKind = "kind"
	labelOp   = "op"
)

// unscopedCacheReadsTotal counts refused cache reads by kind and operation.
var unscopedCacheReadsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "nvcre_unscoped_cache_reads_total",
		Help: "Cache reads the NVCRE controller refused because the scoped cache could not answer them completely (ADR-102).",
	},
	[]string{labelKind, labelOp},
)

func init() {
	metrics.Registry.MustRegister(unscopedCacheReadsTotal)
}

// NewCache builds the manager's cache: cache.New with the guardedScopes rule
// on top (ADR-102). The manager's client reads through this cache, so the rule
// covers reads through the client, through mgr.GetCache(), metadata-only and
// unstructured reads, and informers, which Watches and Owns start. A refused
// read returns errUnscopedCacheRead, is logged at error level with the calling
// code's location, and is counted; the integration harness fails a case that
// causes one. Field indexes are unaffected: the inner cache starts an indexed
// informer through its own GetInformer.
func NewCache(config *rest.Config, opts cache.Options) (cache.Cache, error) {
	inner, err := cache.New(config, opts)
	if err != nil {
		return nil, err
	}
	scheme := opts.Scheme
	if scheme == nil {
		scheme = clientgoscheme.Scheme
	}
	return &scopedCache{Cache: inner, scheme: scheme}, nil
}

// scopedCache refuses reads of guarded kinds that the scoped cache cannot
// answer completely. Every other call goes to the embedded cache.
type scopedCache struct {
	cache.Cache
	scheme *kruntime.Scheme
}

// Get refuses every Get of a guarded kind.
func (c *scopedCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if s := c.scopeOf(obj); s != nil {
		return refuse(ctx, s, "get", key.String())
	}
	return c.Cache.Get(ctx, key, obj, opts...)
}

// List refuses a List of a guarded kind that does not meet its scope.
func (c *scopedCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if s := c.scopeOf(list); s != nil && !s.allows(opts) {
		return refuse(ctx, s, "list", "")
	}
	return c.Cache.List(ctx, list, opts...)
}

// GetInformer refuses an informer for a guarded kind: a watch on it would see
// only the cached part without any error.
func (c *scopedCache) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	if s := c.scopeOf(obj); s != nil {
		return nil, refuse(ctx, s, "informer", "")
	}
	return c.Cache.GetInformer(ctx, obj, opts...)
}

// GetInformerForKind refuses an informer for a guarded kind.
func (c *scopedCache) GetInformerForKind(
	ctx context.Context, gvk schema.GroupVersionKind, opts ...cache.InformerGetOption,
) (cache.Informer, error) {
	if s := scopeForGVK(gvk); s != nil {
		return nil, refuse(ctx, s, "informer", "")
	}
	return c.Cache.GetInformerForKind(ctx, gvk, opts...)
}

// scopeOf returns the guarded scope of obj's kind, or nil. A list's kind is
// its item kind.
func (c *scopedCache) scopeOf(obj kruntime.Object) *cacheScope {
	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return nil
	}
	gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
	return scopeForGVK(gvk)
}

func scopeForGVK(gvk schema.GroupVersionKind) *cacheScope {
	for i := range guardedScopes {
		if guardedScopes[i].gvk == gvk {
			return &guardedScopes[i]
		}
	}
	return nil
}

// allows reports whether opts guarantee every match carries requiredLabel: a
// label requirement on it with =, ==, in or exists, alongside any others, or
// one of scopedIndexes. A negated requirement (!key, !=, notin) also matches
// objects without the label, so it does not count.
func (s *cacheScope) allows(opts []client.ListOption) bool {
	listOpts := (&client.ListOptions{}).ApplyOptions(opts)
	if listOpts.FieldSelector != nil {
		for _, req := range listOpts.FieldSelector.Requirements() {
			for _, index := range s.scopedIndexes {
				if req.Field == index && (req.Operator == selection.Equals || req.Operator == selection.DoubleEquals) {
					return true
				}
			}
		}
	}
	if listOpts.LabelSelector != nil {
		reqs, selectable := listOpts.LabelSelector.Requirements()
		if !selectable {
			return false
		}
		for _, req := range reqs {
			if req.Key() != s.requiredLabel {
				continue
			}
			switch req.Operator() {
			case selection.Equals, selection.DoubleEquals, selection.In, selection.Exists:
				return true
			}
		}
	}
	return false
}

// refuse counts and logs a refused read and returns the error. The log names
// the first caller outside controller-runtime, pkg/podread and this file, the
// controller code that made the read, since the error itself may reach a
// caller that only requeues.
func refuse(ctx context.Context, s *cacheScope, op, target string) error {
	unscopedCacheReadsTotal.WithLabelValues(s.gvk.Kind, op).Inc()
	unscopedCacheReads.Add(1)
	what := op + " " + s.gvk.Kind
	if target != "" {
		what += " " + target
	}
	err := fmt.Errorf("%s: %w", what, errUnscopedCacheRead)
	logf.FromContext(ctx).Error(err, "Refused a cache read the scoped cache cannot answer",
		labelKind, s.gvk.Kind, labelOp, op, "caller", callerOutsideCache())
	return err
}

// callerOutsideCache returns file:line of the first stack frame outside
// controller-runtime, pkg/podread and this file.
func callerOutsideCache() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !strings.Contains(f.File, "sigs.k8s.io/controller-runtime") &&
			!strings.Contains(f.File, "pkg/podread/") &&
			!strings.HasSuffix(f.File, "pkg/controller/cache.go") {
			return fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if !more {
			return "unknown"
		}
	}
}

// unscopedCacheReads mirrors unscopedCacheReadsTotal across all labels, so
// the integration harness can read it without a metrics client.
var unscopedCacheReads atomic.Int64

// UnscopedCacheReads returns how many cache reads have been refused since the
// process started. The integration harness asserts it stays zero.
func UnscopedCacheReads() int64 {
	return unscopedCacheReads.Load()
}

// ClientOptions returns the manager client configuration.
//
// ConfigMaps and PersistentVolumeClaims are read straight from the API server,
// never through the informer cache (ADR-092). A cached read of either starts a
// cluster-wide informer, which needs list and watch on that type in every
// namespace. The controller's write access is granted one namespace at a time,
// so under rbac.scope: namespaced that informer cannot list, and the manager
// loops on `Failed to watch ... forbidden` (issue #424, where a cached PVC read
// did exactly this). The ConfigMap informer also held every ConfigMap in the
// cluster in the controller's memory. Both types are read a handful of times
// per Workflow, so a live read costs little.
//
// PersistentVolumes stay cached: they are cluster-scoped, the role keeps list
// and watch on them, and findPVByClaimRef needs the cached claimRef index.
func ClientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{
			DisableFor: []client.Object{
				&corev1.ConfigMap{},
				&corev1.PersistentVolumeClaim{},
			},
		},
	}
}
