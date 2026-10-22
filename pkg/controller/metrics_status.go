// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// statusMetricsListTimeout bounds a scrape-time List so an unsynced informer
// cannot hang Collect. A timed-out or failed List omits that kind for the scrape.
const statusMetricsListTimeout = 2 * time.Second

// lifecyclePhaseTypes is the exclusive InProgress/Succeeded/Failed set shared
// by Certification, Workflow and Job. Terminal types are listed first so a dual-true
// leftover reports the verdict (ADR-086 decision A4).
var lifecyclePhaseTypes = []string{
	nvcrev1alpha1.CertificationFailed,
	nvcrev1alpha1.CertificationSucceeded,
	nvcrev1alpha1.CertificationInProgress,
}

// lifecycleStatusSource is the scrape-time view of Certifications, Workflows and Jobs.
// reader is the manager cache; elected is mgr.Elected().
type lifecycleStatusSource struct {
	reader  client.Reader
	elected <-chan struct{}
}

// lifecycleStatusCollector builds nvcre_certification_status,
// nvcre_workflow_status and nvcre_job_status from the informer cache on each
// scrape, so every writer, a restart and any deletion path show on the next
// scrape without reconciler code (ADR-086 decision A).
type lifecycleStatusCollector struct {
	source   atomic.Pointer[lifecycleStatusSource]
	certDesc *prometheus.Desc
	wfDesc   *prometheus.Desc
	jobDesc  *prometheus.Desc
}

func newLifecycleStatusCollector() *lifecycleStatusCollector {
	return &lifecycleStatusCollector{
		certDesc: prometheus.NewDesc(
			"nvcre_certification_status",
			"Current status of NVCRE Certifications (1 = current status, 0 = not current status)",
			[]string{labelNamespace, labelCertificationName, labelStatus},
			nil,
		),
		wfDesc: prometheus.NewDesc(
			"nvcre_workflow_status",
			"Current status of NVCRE Workflows (1 = current status, 0 = not current status)",
			[]string{labelNamespace, labelWorkflow, labelCertificationName, labelStatus},
			nil,
		),
		jobDesc: prometheus.NewDesc(
			"nvcre_job_status",
			"Current status of NVCRE jobs (1 = current status, 0 = not current status)",
			[]string{labelNamespace, labelJob, labelWorkflow, labelStatus},
			nil,
		),
	}
}

// statusMetrics is the process-wide collector. It is registered once in init()
// so a second manager (the integration harness builds one per case) cannot
// panic on MustRegister. SetupStatusMetrics installs the live source.
var statusMetrics = newLifecycleStatusCollector()

// SetupStatusMetrics points the Certification, Workflow and Job status collector at
// the manager's cache and elected channel. Collect emits nothing until
// mgr.Elected() is closed, so a standby replica does not start informers or
// double-report series.
func SetupStatusMetrics(mgr manager.Manager) {
	statusMetrics.setSource(mgr.GetCache(), mgr.Elected())
}

func (c *lifecycleStatusCollector) setSource(reader client.Reader, elected <-chan struct{}) {
	c.source.Store(&lifecycleStatusSource{reader: reader, elected: elected})
}

// Describe implements prometheus.Collector.
func (c *lifecycleStatusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.certDesc
	ch <- c.wfDesc
	ch <- c.jobDesc
}

// Collect implements prometheus.Collector. It lists from the cache with
// UnsafeDisableDeepCopy (read-only) and emits a 0/1 const metric per status.
func (c *lifecycleStatusCollector) Collect(ch chan<- prometheus.Metric) {
	src := c.source.Load()
	if src == nil || src.reader == nil {
		return
	}
	if src.elected == nil {
		return
	}
	select {
	case <-src.elected:
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), statusMetricsListTimeout)
	defer cancel()

	c.collectCertifications(ctx, src.reader, ch)
	c.collectWorkflows(ctx, src.reader, ch)
	c.collectJobs(ctx, src.reader, ch)
}

func (c *lifecycleStatusCollector) collectCertifications(ctx context.Context, reader client.Reader, ch chan<- prometheus.Metric) {
	var list nvcrev1alpha1.CertificationList
	if err := reader.List(ctx, &list, client.UnsafeDisableDeepCopy); err != nil {
		return
	}
	for i := range list.Items {
		cert := &list.Items[i]
		emitExclusiveStatus(ch, c.certDesc, cert.Status.Conditions, cert.Namespace, cert.Name)
	}
}

func (c *lifecycleStatusCollector) collectWorkflows(ctx context.Context, reader client.Reader, ch chan<- prometheus.Metric) {
	var list nvcrev1alpha1.WorkflowList
	if err := reader.List(ctx, &list, client.UnsafeDisableDeepCopy); err != nil {
		return
	}
	for i := range list.Items {
		wf := &list.Items[i]
		emitExclusiveStatus(ch, c.wfDesc, wf.Status.Conditions, wf.Namespace, wf.Name, wf.Labels[labelCertification])
	}
}

// collectJobs reports each Job under its nvcre.nvidia.com/workflow label, the
// same value every other job-scoped metric uses.
func (c *lifecycleStatusCollector) collectJobs(ctx context.Context, reader client.Reader, ch chan<- prometheus.Metric) {
	var list nvcrev1alpha1.JobList
	if err := reader.List(ctx, &list, client.UnsafeDisableDeepCopy); err != nil {
		return
	}
	for i := range list.Items {
		job := &list.Items[i]
		emitExclusiveStatus(ch, c.jobDesc, job.Status.Conditions, job.Namespace, job.Name, job.Labels[labelWorkflowTracking])
	}
}

// emitExclusiveStatus writes one 0/1 series per exclusive status. An object
// with no InProgress/Succeeded/Failed condition True is omitted.
func emitExclusiveStatus(ch chan<- prometheus.Metric, desc *prometheus.Desc, conditions []metav1.Condition, labelValues ...string) {
	status := metricStatusFromCondition(trueConditionType(conditions, lifecyclePhaseTypes))
	if status == "" {
		return
	}
	for _, s := range exclusiveMetricStatuses {
		value := float64(0)
		if s == status {
			value = 1
		}
		labels := make([]string, 0, len(labelValues)+1)
		labels = append(labels, labelValues...)
		labels = append(labels, s)
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
	}
}
