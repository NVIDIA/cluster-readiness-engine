// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/numstr"
)

const (
	statusSucceeded  = "Succeeded"
	statusFailed     = "Failed"
	statusRunning    = "Running"
	statusInProgress = "InProgress"
)

// ---------------------------------------------------------------------------
// Report data model
// ---------------------------------------------------------------------------

// CertReport holds all data needed to render the certification report.
type CertReport struct {
	Title      string `json:"title,omitempty"` // defaults to "Certification Report"
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	GPU        string `json:"gpu"`
	TotalNodes int    `json:"totalNodes"`
	// ExcludedNodes lists nodes that matched the target but were left
	// untested, with ExclusionReason saying why. A run reports PASSED even
	// when it excludes nodes, so these keep that from being invisible.
	ExcludedNodes   []string           `json:"excludedNodes,omitempty"`
	ExclusionReason string             `json:"exclusionReason,omitempty"`
	Categories      []CategoryReport   `json:"categories"`
	FailedNodes     []string           `json:"failedNodes"`
	Result          string             `json:"result"` // "PASSED", "INCOMPLETE", "FAILED", or "RUNNING"
	NodeResults     []NodeResultReport `json:"nodeResults,omitempty"`
}

// NodeResultReport holds per-node pass/fail status for programmatic consumers.
type NodeResultReport struct {
	Name   string `json:"name"`
	Group  string `json:"group"`
	Rack   string `json:"rack,omitempty"`
	Status string `json:"status"` // "Passed" or "Failed"
}

// CategoryReport holds metrics for a single certification category.
type CategoryReport struct {
	Domain        string `json:"domain"`
	Variant       string `json:"variant"`
	Status        string `json:"status"`
	FailureReason string `json:"failureReason,omitempty"` // populated from Workflow Failed condition message
	Runtime       string `json:"runtime,omitempty"`       // total runtime across all iterations
	TestScale     string `json:"testScale,omitempty"`
	NodesPerJob   int    `json:"nodesPerJob,omitempty"`
	Jobs          int    `json:"jobs,omitempty"`
	MNNVL         string `json:"mnnvl,omitempty"` // "Enabled", "Disabled", or "" (unknown)
	// FailedGroups lists groups that failed with their reason.
	FailedGroups []FailedGroupReport `json:"failedGroups,omitempty"`
	// Cliques lists topology domains with node counts and validation status.
	Cliques []CliqueReport `json:"cliques,omitempty"`
	// Training metrics grouped by topology domain.
	Domains []DomainReport `json:"domains,omitempty"`
	// Communication bandwidth results (single-group: one row per size).
	Bandwidth []BandwidthRow `json:"bandwidth,omitempty"`
	// Per-group bandwidth results (multi-group: one row per group).
	GroupBandwidth []GroupBandwidthRow `json:"groupBandwidth,omitempty"`
	// Diagnose results from adaptive fault isolation.
	Diagnose *DiagnoseReport `json:"diagnose,omitempty"`
	// Iterations shows per-iteration timing and outcome.
	Iterations []IterationReport `json:"iterations,omitempty"`
}

// IterationReport holds per-iteration timing and outcome.
type IterationReport struct {
	Number   int    `json:"number"`
	Status   string `json:"status"`   // Succeeded, Failed, Running
	Duration string `json:"duration"` // e.g., "5m 30s"
}

// DiagnoseReport holds results from the adaptive fault isolation algorithm.
type DiagnoseReport struct {
	Stage                string                                          `json:"stage"`
	Rounds               int                                             `json:"rounds"`
	HealthyCount         int                                             `json:"healthyCount"`
	SuspectCount         int                                             `json:"suspectCount"`
	ConfirmedFaulty      []string                                        `json:"confirmedFaulty,omitempty"`
	InfrastructureFaults []burninv1alpha1.InfrastructureFault            `json:"infrastructureFaults,omitempty"`
	ScreeningResults     map[string]burninv1alpha1.DomainScreeningResult `json:"-"` // not serialized, used for rendering
	MaxBW                string                                          `json:"maxBW,omitempty"`
	MaxBWDomain          string                                          `json:"maxBWDomain,omitempty"`
	MaxBWNodeList        []string                                        `json:"maxBWNodes,omitempty"`
	MinBW                string                                          `json:"minBW,omitempty"`
	MinBWDomain          string                                          `json:"minBWDomain,omitempty"`
	MinBWNodeList        []string                                        `json:"minBWNodes,omitempty"`
	Tests                []DiagnoseTestRow                               `json:"tests,omitempty"`
}

// DiagnoseTestRow holds one test result from the diagnose algorithm.
type DiagnoseTestRow struct {
	Stage  string   `json:"stage"`            // screening, bisection, confirmation
	Name   string   `json:"name"`             // job name
	Nodes  []string `json:"nodes"`            // nodes in the group
	Domain string   `json:"domain,omitempty"` // clique/domain for screening tests
	BusBW  string   `json:"busBW"`            // peak bus bandwidth
	Passed bool     `json:"passed"`           // job succeeded
}

// DomainReport holds averaged training metrics for a topology domain.
type DomainReport struct {
	Name      string `json:"name"` // e.g., "clique-0" or "" for no-topology case
	NodeCount int    `json:"nodeCount"`
	Goodput   string `json:"goodput"`  // runtime goodput ratio
	TFLOPs    string `json:"tflops"`   // avg TFLOPs per GPU
	StepTime  string `json:"stepTime"` // avg step time
}

// BandwidthRow holds bandwidth results for a single message size.
type BandwidthRow struct {
	Size    string `json:"size"` // human-readable size
	AlgBW   string `json:"algBW"`
	BusBW   string `json:"busBW"`
	Samples int    `json:"samples"`
}

// CliqueReport holds per-clique validation status.
type CliqueReport struct {
	Name      string `json:"name"`
	Total     int    `json:"total"`     // total nodes in this clique
	Validated int    `json:"validated"` // nodes that passed
	Passed    bool   `json:"passed"`
}

// FailedGroupReport holds details about a failed orchestration group.
type FailedGroupReport struct {
	Name      string   `json:"name"`
	NodeCount int      `json:"nodeCount"`
	Nodes     []string `json:"nodes,omitempty"`
	Reason    string   `json:"reason"` // e.g., "BackoffLimitExceeded (72 pods failed)"
}

// GroupBandwidthRow holds bandwidth for a single group in multi-group Workflows.
type GroupBandwidthRow struct {
	GroupName string   `json:"groupName"` // e.g., "group-0" or "clique-0 (18 nodes)"
	Nodes     []string `json:"nodes"`     // node names in the group
	BusBW     string   `json:"busBW"`     // peak BusBW at largest message size
	BelowMin  bool     `json:"belowMin"`  // true if below minBusBandwidthGBps threshold
	Failed    bool     `json:"failed"`    // true if the group's Job failed
}

// ---------------------------------------------------------------------------
// Report builder — fetches data from the cluster
// ---------------------------------------------------------------------------

// FailedNodesFromRef resolves a nodeResultsRef to its failed-nodes list by fetching
// the referenced ConfigMap and decoding the failed-nodes entry.
func FailedNodesFromRef(
	ctx context.Context, c client.Client, namespace string, ref *corev1.TypedLocalObjectReference,
) []burninv1alpha1.FailedNode {
	if ref == nil || ref.Name == "" {
		return nil
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: namespace}, cm); err != nil {
		return nil
	}
	nodes, err := noderesults.DecodeFailedNodesFromConfigMap(cm)
	if err != nil {
		return nil
	}
	return nodes
}

// CertFailedNodes returns the deduped union of failed node names across all
// categories, resolved from each category's nodeResultsRef ConfigMap.
func CertFailedNodes(ctx context.Context, c client.Client, cert *burninv1alpha1.Certification) []string {
	seen := make(map[string]struct{})
	var union []string
	for _, cat := range cert.Status.CategoryStatuses {
		for _, n := range FailedNodesFromRef(ctx, c, cert.Namespace, cat.FailedNodesRef) {
			if n.Name == "" {
				continue
			}
			if _, ok := seen[n.Name]; ok {
				continue
			}
			seen[n.Name] = struct{}{}
			union = append(union, n.Name)
		}
	}
	sort.Strings(union)
	return union
}

// Build fetches Workflow and measurement data for a Certification (completed or running).
func Build(ctx context.Context, c client.Client, cert *burninv1alpha1.Certification) *CertReport {
	result := "RUNNING"
	if controller.CondIsTrue(cert.Status.Conditions, burninv1alpha1.CertificationFailed) {
		result = "FAILED"
	} else if controller.CondIsTrue(cert.Status.Conditions, burninv1alpha1.CertificationSucceeded) {
		result = "PASSED"
	}
	report := &CertReport{
		Name:        cert.Name,
		FailedNodes: CertFailedNodes(ctx, c, cert),
		Result:      result,
	}

	// Fetch metrics per category from Workflows.
	for i, cs := range cert.Status.CategoryStatuses {
		status := cs.Status
		if status == statusInProgress {
			status = statusRunning
		}
		cat := CategoryReport{
			Domain:  cs.Domain,
			Variant: cs.Variant,
			Status:  status,
		}

		// Populate MNNVL: check per-category options first, fall back to global.
		if i < len(cert.Spec.Categories) {
			var mnnvl *bool
			if specOpts := cert.Spec.Categories[i].Options; specOpts != nil && specOpts.EnableMNNVL != nil {
				mnnvl = specOpts.EnableMNNVL
			} else if cert.Spec.EnableMNNVL != nil {
				mnnvl = cert.Spec.EnableMNNVL
			}
			if mnnvl != nil {
				if *mnnvl {
					cat.MNNVL = "Enabled"
				} else {
					cat.MNNVL = "Disabled"
				}
			}
		}

		if cs.WorkflowRef != nil {
			wf := &burninv1alpha1.Workflow{}
			ns := cs.WorkflowRef.Namespace
			if ns == "" {
				ns = cert.Namespace
			}
			if err := c.Get(ctx, client.ObjectKey{Name: cs.WorkflowRef.Name, Namespace: ns}, wf); err == nil {
				PopulateCategoryFromWorkflow(ctx, c, &cat, wf)
				if cat.Status == statusFailed {
					cat.FailureReason = failureReasonFromConditions(wf.Status.Conditions)
				}
			}
		}

		report.Categories = append(report.Categories, cat)
	}

	// Set platform/GPU/nodes from the first Workflow's orchestration status.
	for _, cs := range cert.Status.CategoryStatuses {
		if cs.WorkflowRef == nil {
			continue
		}
		wf := &burninv1alpha1.Workflow{}
		ns := cs.WorkflowRef.Namespace
		if ns == "" {
			ns = cert.Namespace
		}
		if err := c.Get(ctx, client.ObjectKey{Name: cs.WorkflowRef.Name, Namespace: ns}, wf); err != nil {
			continue
		}
		if wf.Status.Orchestration != nil {
			report.Platform = wf.Status.Orchestration.DetectedPlatform
			report.GPU = wf.Status.Orchestration.DetectedGPUArchitecture
			report.TotalNodes = wf.Status.Orchestration.TotalNodes
			report.ExcludedNodes = wf.Status.Orchestration.ExcludedNodes
			report.ExclusionReason = wf.Status.Orchestration.ExclusionReason
			break
		}
	}

	// A run that certified fewer nodes than it targeted did not do what was
	// asked. It is not a failure either: the skipped nodes were never tested, so
	// nothing is known about them, and calling that FAILED would assert a fault
	// nobody observed. The usual causes are a mixed fleet or a leftover cordon,
	// both configuration rather than hardware. So it warns — and keeps exit 0,
	// because a warning that fails the build is not a warning.
	if report.Result == "PASSED" && len(report.ExcludedNodes) > 0 {
		report.Result = "INCOMPLETE"
	}

	return report
}

// batchJobFailureReason extracts the batch/v1 Job name from the CRE
// Job's failure message and returns its failure reason.
func batchJobFailureReason(ctx context.Context, c client.Client, excalMsg, namespace string) string {
	// Parse "first failed job: <name>" from the message.
	const prefix = "first failed job: "
	_, after, ok := strings.Cut(excalMsg, prefix)
	if !ok {
		return ""
	}
	batchJobName := after
	// Trim trailing parenthesis or garbage.
	if i := strings.IndexByte(batchJobName, ')'); i >= 0 {
		batchJobName = batchJobName[:i]
	}
	batchJobName = strings.TrimSpace(batchJobName)
	if batchJobName == "" {
		return ""
	}

	batchJob := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Name: batchJobName, Namespace: namespace}, batchJob); err != nil {
		return ""
	}
	for _, cond := range batchJob.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			if batchJob.Status.Failed > 0 {
				return fmt.Sprintf("%s — %d of %d pods failed",
					cond.Reason, batchJob.Status.Failed, batchJob.Status.Failed+batchJob.Status.Succeeded)
			}
			return cond.Reason
		}
	}
	return ""
}

func failureReasonFromConditions(conditions []metav1.Condition) string {
	for _, cond := range conditions {
		if cond.Type == burninv1alpha1.WorkflowFailed && cond.Status == metav1.ConditionTrue {
			return cond.Message
		}
	}
	return ""
}

// buildFailedGroups collects failed orchestration groups with their root cause.
// buildIterationReports creates per-iteration timing from iteration history
// and the current iteration. Returns reports and total runtime string.
func buildIterationReports(orch *burninv1alpha1.OrchestrationStatus) ([]IterationReport, string) {
	if orch == nil {
		return nil, ""
	}

	now := time.Now().Unix()
	var totalSecs int64
	var reports []IterationReport

	// Process completed iterations from history.
	for _, iter := range orch.IterationHistory {
		status, secs := iterGroupDuration(iter.Groups, now)
		totalSecs += secs
		reports = append(reports, IterationReport{
			Number:   iter.Iteration,
			Status:   status,
			Duration: fmtSecs(secs),
		})
	}

	// Add current iteration from Groups.
	if orch.CurrentIteration > 0 && len(orch.Groups) > 0 {
		status, secs := currentGroupDuration(orch.Groups, now)
		totalSecs += secs
		reports = append(reports, IterationReport{
			Number:   orch.CurrentIteration,
			Status:   status,
			Duration: fmtSecs(secs),
		})
	}

	runtime := fmtSecs(totalSecs)
	if len(reports) <= 1 {
		return nil, runtime // Single iteration: show runtime but not iteration list
	}
	return reports, runtime
}

// iterGroupDuration computes duration and status from completed iteration groups.
func iterGroupDuration(groups []burninv1alpha1.GroupIterationResult, now int64) (string, int64) {
	status := statusSucceeded
	var earliest, latest int64
	for _, g := range groups {
		if g.Phase == burninv1alpha1.GroupFailed {
			status = statusFailed
		}
		if g.StartTime != nil {
			t := g.StartTime.Unix()
			if earliest == 0 || t < earliest {
				earliest = t
			}
		}
		if g.CompletionTime != nil {
			t := g.CompletionTime.Unix()
			if t > latest {
				latest = t
			}
		}
	}
	if earliest == 0 {
		return status, 0
	}
	if latest == 0 {
		latest = now // still running
	}
	return status, latest - earliest
}

// currentGroupDuration computes duration and status from the current iteration's groups.
func currentGroupDuration(groups []burninv1alpha1.GroupStatus, now int64) (string, int64) {
	status := statusSucceeded
	allTerminal := true
	var earliest, latest int64
	for _, g := range groups {
		if g.Phase == burninv1alpha1.GroupFailed {
			status = statusFailed
		}
		if g.Phase != burninv1alpha1.GroupSucceeded && g.Phase != burninv1alpha1.GroupFailed {
			allTerminal = false
		}
		if g.StartTime != nil {
			t := g.StartTime.Unix()
			if earliest == 0 || t < earliest {
				earliest = t
			}
		}
		if g.CompletionTime != nil {
			t := g.CompletionTime.Unix()
			if t > latest {
				latest = t
			}
		}
	}
	if !allTerminal {
		status = statusRunning
	}
	if earliest == 0 {
		return status, 0
	}
	if latest == 0 {
		latest = now // still running
	}
	return status, latest - earliest
}

// fmtSecs formats seconds as human-readable duration.
func fmtSecs(secs int64) string {
	if secs <= 0 {
		return ""
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm %ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%dh %dm", secs/3600, (secs%3600)/60)
}

func buildFailedGroups(ctx context.Context, c client.Client, wf *burninv1alpha1.Workflow) []FailedGroupReport {
	orch := wf.Status.Orchestration
	if orch == nil || c == nil {
		return nil
	}
	var result []FailedGroupReport
	for _, g := range orch.Groups {
		if g.Phase != burninv1alpha1.GroupFailed || g.JobRef == nil {
			continue
		}
		fg := FailedGroupReport{
			Name:      g.Name,
			NodeCount: len(g.Nodes),
			Nodes:     g.Nodes,
		}
		excalJob := &burninv1alpha1.Job{}
		if err := c.Get(ctx, client.ObjectKey{Name: g.JobRef.Name, Namespace: wf.Namespace}, excalJob); err == nil {
			for _, cond := range excalJob.Status.Conditions {
				if cond.Type == burninv1alpha1.JobFailed && cond.Status == metav1.ConditionTrue {
					if reason := batchJobFailureReason(ctx, c, cond.Message, wf.Namespace); reason != "" {
						fg.Reason = reason
					} else {
						fg.Reason = cond.Message
					}
					break
				}
			}
		}
		result = append(result, fg)
	}
	return result
}

// PopulateCategoryFromWorkflow fills in category metrics from a Workflow and its children.
func PopulateCategoryFromWorkflow(
	ctx context.Context, c client.Client, cat *CategoryReport, wf *burninv1alpha1.Workflow,
) {
	orch := wf.Status.Orchestration
	if orch != nil {
		cat.NodesPerJob = orch.NodesPerJob
		cat.Jobs = orch.TotalGroups
	}
	cat.TestScale = detectTestScale(wf)

	failedNodes := FailedNodesFromRef(ctx, c, wf.Namespace, wf.Status.FailedNodesRef)
	if wf.Spec.Orchestration.Topology != nil {
		cat.Cliques = buildCliqueReport(wf, failedNodes)
	}
	cat.FailedGroups = buildFailedGroups(ctx, c, wf)
	cat.Iterations, cat.Runtime = buildIterationReports(orch)
	if orch != nil && orch.Diagnose != nil {
		diag := orch.Diagnose
		stage := diag.Stage
		// If the workflow is terminal, show "complete" regardless of the stored stage
		// (older controller versions may not have set it on failure paths).
		if controller.CondIsTrue(wf.Status.Conditions, burninv1alpha1.WorkflowSucceeded) ||
			controller.CondIsTrue(wf.Status.Conditions, burninv1alpha1.WorkflowFailed) {
			stage = burninv1alpha1.DiagnoseStageComplete
		}
		cat.Diagnose = &DiagnoseReport{
			Stage:        stage,
			Rounds:       diag.Round,
			HealthyCount: len(diag.HealthyNodes),
			SuspectCount: len(diag.SuspectNodes),
		}
		if len(failedNodes) > 0 {
			cat.Diagnose.ConfirmedFaulty = noderesults.FailedNodeNames(failedNodes)
		}
		cat.Diagnose.InfrastructureFaults = diag.InfrastructureFaults
		cat.Diagnose.ScreeningResults = diag.ScreeningResults
	}

	// Collect Job names owned by this Workflow to filter measurements.
	workflowJobs := collectWorkflowJobs(ctx, c, wf, orch)

	// Find GoodputMeasurements owned by this Workflow's Jobs.
	var goodputList burninv1alpha1.GoodputMeasurementList
	if err := c.List(ctx, &goodputList, client.InNamespace(wf.Namespace)); err == nil {
		var filtered []burninv1alpha1.GoodputMeasurement
		for _, gm := range goodputList.Items {
			if workflowJobs[gm.Spec.JobRef.Name] {
				filtered = append(filtered, gm)
			}
		}
		if len(filtered) > 0 {
			cat.Domains = buildDomainReports(orch, filtered)
		}
	}

	// Find BandwidthMeasurements owned by this Workflow's Jobs.
	var bwList burninv1alpha1.BandwidthMeasurementList
	if err := c.List(ctx, &bwList, client.InNamespace(wf.Namespace)); err == nil {
		// Collect filtered BandwidthMeasurements.
		var filtered []burninv1alpha1.BandwidthMeasurement
		for _, bm := range bwList.Items {
			if workflowJobs[bm.Spec.JobRef.Name] {
				filtered = append(filtered, bm)
			}
		}

		// Diagnose mode: show per-job results across all stages.
		if cat.Diagnose != nil && len(filtered) > 0 {
			cat.Diagnose.Tests = buildDiagnoseTests(ctx, c, wf, filtered)
			computeDiagnoseMinMax(cat.Diagnose)
		} else if len(filtered) > 1 && orch != nil && orch.TotalGroups > 1 {
			// Multi-group: show per-group peak bandwidth.
			var bwThreshold string
			if v := wf.Spec.Validation; v != nil && v.Performance != nil &&
				v.Performance.Thresholds != nil {
				bwThreshold = v.Performance.Thresholds.Thresholds["busBandwidthGBps"]
			}
			cat.GroupBandwidth = buildGroupBandwidthRows(orch, filtered, bwThreshold)
		}

		// Show aggregate bandwidth for non-diagnose modes.
		// Diagnose shows per-stage bandwidth in the diagnosis section.
		if cat.Diagnose == nil {
			var peak *burninv1alpha1.BandwidthResult
			for i := range filtered {
				r := peakBandwidthResult(filtered[i].Status.Results)
				if r == nil {
					continue
				}
				if peak == nil || r.SizeBytes > peak.SizeBytes {
					peak = r
				}
			}
			if peak != nil {
				cat.Bandwidth = append(cat.Bandwidth, BandwidthRow{
					Size:    humanSize(peak.SizeBytes),
					AlgBW:   peak.AlgBW + " GB/s",
					BusBW:   peak.BusBW + " GB/s",
					Samples: peak.Samples,
				})
			}
		}
	}
}

// buildGroupBandwidthRows maps BandwidthMeasurements to groups and returns
// per-group peak bandwidth rows for multi-group Workflows.
func buildGroupBandwidthRows(
	orch *burninv1alpha1.OrchestrationStatus,
	measurements []burninv1alpha1.BandwidthMeasurement,
	minBusBandwidthGBps string,
) []GroupBandwidthRow {
	// Map job name → group info.
	type groupInfo struct {
		name      string
		nodes     []string
		domains   []string
		nodeCount int
		failed    bool
	}
	jobToGroup := map[string]groupInfo{}
	for _, g := range orch.Groups {
		if g.JobRef != nil {
			jobToGroup[g.JobRef.Name] = groupInfo{
				name:      g.Name,
				nodes:     g.Nodes,
				domains:   g.Domains,
				nodeCount: len(g.Nodes),
				failed:    g.Phase == burninv1alpha1.GroupFailed,
			}
		}
	}

	// Build per-group bandwidth rows.
	var rows []GroupBandwidthRow
	for _, bm := range measurements {
		gi, ok := jobToGroup[bm.Spec.JobRef.Name]
		if !ok {
			continue
		}
		// Get peak BusBW from the largest message size.
		peak := peakBandwidthResult(bm.Status.Results)
		if peak == nil {
			continue
		}

		// Build label: prefer domain name, fall back to group name with nodes.
		var label string
		if len(gi.domains) > 0 {
			label = fmt.Sprintf("%s (%d nodes)", gi.domains[0], gi.nodeCount)
		} else if gi.nodeCount <= 4 {
			label = fmt.Sprintf("%s (%s)", gi.name, strings.Join(gi.nodes, ", "))
		} else {
			label = fmt.Sprintf("%s (%d nodes)", gi.name, gi.nodeCount)
		}

		// Check threshold.
		belowMin := false
		if minBusBandwidthGBps != "" {
			threshold, _ := strconv.ParseFloat(minBusBandwidthGBps, 64)
			measured, _ := strconv.ParseFloat(peak.BusBW, 64)
			if threshold > 0 && measured < threshold {
				belowMin = true
			}
		}

		rows = append(rows, GroupBandwidthRow{
			GroupName: label,
			Nodes:     gi.nodes,
			BusBW:     peak.BusBW + " GB/s",
			BelowMin:  belowMin,
			Failed:    gi.failed,
		})
	}

	// Add failed groups that have no BandwidthMeasurement.
	seen := make(map[string]bool)
	for _, r := range rows {
		seen[r.GroupName] = true
	}
	for _, g := range orch.Groups {
		if g.Phase != burninv1alpha1.GroupFailed {
			continue
		}
		var label string
		if len(g.Domains) > 0 {
			label = fmt.Sprintf("%s (%d nodes)", g.Domains[0], len(g.Nodes))
		} else {
			label = fmt.Sprintf("%s (%d nodes)", g.Name, len(g.Nodes))
		}
		if seen[label] {
			continue
		}
		rows = append(rows, GroupBandwidthRow{
			GroupName: label,
			Nodes:     g.Nodes,
			Failed:    true,
		})
	}

	return rows
}

// buildDomainReports groups GoodputMeasurements by topology domain.
// If no domains exist, returns a single entry with averages across all measurements.
func buildDomainReports(
	orch *burninv1alpha1.OrchestrationStatus, measurements []burninv1alpha1.GoodputMeasurement,
) []DomainReport {
	// Map job names to their group's domain info.
	type groupInfo struct {
		domains   []string
		nodeCount int
	}
	jobToDomain := map[string]groupInfo{}
	if orch != nil {
		for _, g := range orch.Groups {
			if g.JobRef != nil {
				jobToDomain[g.JobRef.Name] = groupInfo{
					domains:   g.Domains,
					nodeCount: len(g.Nodes),
				}
			}
		}
	}

	// Aggregate metrics by domain.
	type domainAgg struct {
		nodeCount int
		goodputs  []float64
		tflops    []float64
		stepTime  []float64
	}
	domains := map[string]*domainAgg{}

	for _, gm := range measurements {
		info := jobToDomain[gm.Spec.JobRef.Name]
		// Use all domains as the key so groups spanning multiple cliques
		// show all clique names in the report.
		domainName := strings.Join(info.domains, ", ")

		agg, exists := domains[domainName]
		if !exists {
			agg = &domainAgg{nodeCount: info.nodeCount}
			domains[domainName] = agg
		}

		if v := parseFloat(gm.Status.Result); v > 0 {
			agg.goodputs = append(agg.goodputs, v)
		}
		if v := parseFloat(gm.Status.AvgTFLOPSPerGPU); v > 0 {
			agg.tflops = append(agg.tflops, v)
		}
		if v := parseFloat(gm.Status.AvgStepTimeSec); v > 0 {
			agg.stepTime = append(agg.stepTime, v)
		}
	}

	reports := make([]DomainReport, 0, len(domains))
	for name, agg := range domains {
		dr := DomainReport{
			Name:      name,
			NodeCount: agg.nodeCount,
			Goodput:   fmtAvg(agg.goodputs, fmtPercent),
			TFLOPs:    fmtAvg(agg.tflops, fmtFloat1),
			StepTime:  fmtAvg(agg.stepTime, fmtFloat2),
		}
		reports = append(reports, dr)
	}

	return reports
}

// buildCliqueReport builds per-clique validation status from orchestration
// groups and failed nodes. For single-domain groups (intra-rack), node counts
// are exact. For multi-domain groups, DomainNodeCounts provides per-domain
// totals recorded at partition time.
func buildCliqueReport(wf *burninv1alpha1.Workflow, failedNodes []burninv1alpha1.FailedNode) []CliqueReport {
	orch := wf.Status.Orchestration
	if orch == nil {
		return nil
	}

	failedSet := make(map[string]bool)
	for _, n := range failedNodes {
		failedSet[n.Name] = true
	}

	// Collect domains from groups with Failed phase.
	failedDomains := make(map[string]bool)
	for _, g := range orch.Groups {
		if g.Phase == burninv1alpha1.GroupFailed {
			for _, d := range g.Domains {
				failedDomains[d] = true
			}
		}
	}

	type agg struct{ total, failed int }
	cliques := map[string]*agg{}

	for _, g := range orch.Groups {
		if len(g.Domains) == 1 {
			// Strict domain: all nodes belong to this one clique.
			d := g.Domains[0]
			if cliques[d] == nil {
				cliques[d] = &agg{}
			}
			for _, n := range g.Nodes {
				cliques[d].total++
				if failedSet[n] {
					cliques[d].failed++
				}
			}
		} else if len(g.DomainNodeCounts) > 0 {
			// Multi-domain group with per-domain counts from partition time.
			for d, count := range g.DomainNodeCounts {
				if cliques[d] == nil {
					cliques[d] = &agg{}
				}
				cliques[d].total += count
			}
			// Failed nodes: count once per node (we don't know exact domain,
			// so attribute to first domain — failures are rare and surfaced
			// at the certification level anyway).
			if len(g.Domains) > 0 {
				d := g.Domains[0]
				if cliques[d] == nil {
					cliques[d] = &agg{}
				}
				for _, n := range g.Nodes {
					if failedSet[n] {
						cliques[d].failed++
					}
				}
			}
		} else {
			// Legacy fallback: no DomainNodeCounts, attribute all to first domain.
			if len(g.Domains) > 0 {
				d := g.Domains[0]
				if cliques[d] == nil {
					cliques[d] = &agg{}
				}
				cliques[d].total += len(g.Nodes)
				for _, n := range g.Nodes {
					if failedSet[n] {
						cliques[d].failed++
					}
				}
			}
		}
	}

	names := make([]string, 0, len(cliques))
	for name := range cliques {
		names = append(names, name)
	}
	sort.Strings(names)

	var reports []CliqueReport
	for _, name := range names {
		a := cliques[name]
		passed := a.failed == 0 && !failedDomains[name]
		reports = append(reports, CliqueReport{
			Name:      name,
			Total:     a.total,
			Validated: a.total - a.failed,
			Passed:    passed,
		})
	}
	return reports
}

// detectTestScale infers the testScale from the workflow orchestration spec.
// Returns "" when no explicit test scale was set (e.g., training workloads).
// annotationRequestedTestScale carries the testScale the operator asked for, set
// by the Certification controller when it creates the Workflow.
const annotationRequestedTestScale = "cre.nvidia.com/requested-test-scale"

func detectTestScale(wf *burninv1alpha1.Workflow) string {
	// What the operator asked for, when the Certification recorded it. The
	// fallback below infers the scale from what was applied, which is not the
	// same thing: an entry whose template ignores testScale still partitions one
	// node per group, so a run that asked for intra-rack was reported as
	// intra-node. Prefer the request; infer only for Workflows created before
	// this annotation existed, or created directly rather than by a Certification.
	if req := wf.GetAnnotations()[annotationRequestedTestScale]; req != "" {
		return req
	}
	o := wf.Spec.Orchestration
	if o.Topology != nil && o.Topology.StrictDomain {
		return burninv1alpha1.TestScaleIntraRack
	}
	if o.Diagnose != nil {
		return burninv1alpha1.TestScaleDiagnose
	}
	if wf.Status.Orchestration != nil && wf.Status.Orchestration.NodesPerJob == 1 {
		return burninv1alpha1.TestScaleIntraNode
	}
	return burninv1alpha1.TestScaleFullScale
}

// ---------------------------------------------------------------------------
// Report printer — box-drawing output
// ---------------------------------------------------------------------------

const (
	boxWidth   = 66
	noneString = "none"
	markPass   = "✓"
	markFail   = "✗"
)

// Print writes the formatted report to the given writer.
func Print(w io.Writer, r *CertReport) {
	PrintMulti(w, []*CertReport{r})
}

// PrintMulti writes one or more certification reports.
// A single cert uses the original layout; multiple certs get section separators.
func PrintMulti(w io.Writer, reports []*CertReport) {
	// Shared banner — use first report's title, default to "Certification Report".
	title := "Certification Report"
	if len(reports) > 0 && reports[0].Title != "" {
		title = reports[0].Title
	}
	_, _ = fmt.Fprintln(w)
	printBoxTop(w)
	printBoxCenter(w, title)
	printBoxBottom(w)
	_, _ = fmt.Fprintln(w)

	multi := len(reports) > 1
	for _, r := range reports {
		if multi {
			// Section separator: ━━ cert-name ━━━━━━━━━━━━━━━━
			label := " " + r.Name + " "
			remaining := max(boxWidth-2-utf8.RuneCountInString(label), 0)
			_, _ = fmt.Fprintf(w, "━━%s%s\n", label, strings.Repeat("━", remaining))
		} else {
			_, _ = fmt.Fprintf(w, "  Name:      %s\n", r.Name)
		}
		if r.Platform != "" {
			_, _ = fmt.Fprintf(w, "  Platform:  %s\n", r.Platform)
		}
		if r.GPU != "" {
			_, _ = fmt.Fprintf(w, "  GPU:       %s\n", r.GPU)
		}
		if r.TotalNodes > 0 {
			_, _ = fmt.Fprintf(w, "  Nodes:     %d\n", r.TotalNodes)
		}
		// A run can pass while leaving nodes untested. Say so next to the
		// node count rather than only in an event on the Workflow.
		if len(r.ExcludedNodes) > 0 {
			_, _ = fmt.Fprintf(w, "  Excluded:  %d (%s)\n",
				len(r.ExcludedNodes), strings.Join(r.ExcludedNodes, ", "))
			if r.ExclusionReason != "" {
				_, _ = fmt.Fprintf(w, "             %s\n", r.ExclusionReason)
			}
		}
		_, _ = fmt.Fprintln(w)

		// Category cards.
		passed := 0
		for _, cat := range r.Categories {
			printCategoryCard(w, &cat)
			_, _ = fmt.Fprintln(w)
			if cat.Status == statusSucceeded {
				passed++
			}
		}

		// Summary.
		printCardTop(w)
		printCardTitle(w, "Summary")
		printCardSep(w)
		_, _ = fmt.Fprintf(w, "│  Categories:   %d/%d passed%s│\n",
			passed, len(r.Categories), pad(boxWidth-26-countDigits(passed)-countDigits(len(r.Categories))))
		if len(r.FailedNodes) == 0 {
			printBoxLine(w, fmt.Sprintf("Failed Nodes: %s", noneString))
		} else {
			printBoxLine(w, fmt.Sprintf("Failed Nodes: %d", len(r.FailedNodes)))
			for _, node := range r.FailedNodes {
				printBoxLine(w, "  - "+node)
			}
		}
		_, _ = fmt.Fprintf(w, "│  Result:       %s%s│\n",
			r.Result, pad(boxWidth-18-len(r.Result)))
		printCardBottom(w)
		_, _ = fmt.Fprintln(w)
	}
}

// printCategoryCard renders a single category with its metrics.
// printFailedGroups renders the failed groups section of a category card.
func printFailedGroups(w io.Writer, groups []FailedGroupReport) {
	if len(groups) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
	printBoxLine(w, "Failed Groups:")
	for _, fg := range groups {
		line := fmt.Sprintf("    ✗  %s (%d nodes)", fg.Name, fg.NodeCount)
		printBoxLine(w, line)
		if fg.Reason != "" {
			reason := fg.Reason
			maxLen := boxWidth - 12 // "       " + padding
			if len(reason) > maxLen {
				reason = reason[:maxLen-3] + "..."
			}
			printBoxLine(w, fmt.Sprintf("       %s", reason))
		}
		for _, node := range fg.Nodes {
			printBoxLine(w, fmt.Sprintf("         - %s", node))
		}
	}
}

func printCategoryCard(w io.Writer, cat *CategoryReport) {
	title := cat.Domain + "/" + cat.Variant
	printCardTop(w)
	printCardTitle(w, title)
	printCardSep(w)

	statusLine := fmt.Sprintf("Status:    %s", cat.Status)
	_, _ = fmt.Fprintf(w, "│  %s%s│\n", statusLine, pad(boxWidth-4-len(statusLine)))
	if cat.FailureReason != "" {
		reasonLine := fmt.Sprintf("Reason:    %s", cat.FailureReason)
		// Truncate if too wide for the box.
		if len(reasonLine) > boxWidth-4 {
			reasonLine = reasonLine[:boxWidth-7] + "..."
		}
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", reasonLine, pad(boxWidth-4-len(reasonLine)))
	}
	if cat.Runtime != "" {
		label := fmt.Sprintf("Runtime:   %s", cat.Runtime)
		if len(cat.Iterations) > 0 {
			label = fmt.Sprintf("Runtime:   %s (across %d iterations)", cat.Runtime, len(cat.Iterations))
		}
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", label, pad(boxWidth-4-len(label)))
	}
	if len(cat.Iterations) > 0 {
		printIterations(w, cat.Iterations)
	}
	if cat.TestScale != "" {
		tsLine := fmt.Sprintf("Scale:     %s", cat.TestScale)
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", tsLine, pad(boxWidth-4-len(tsLine)))
	}
	isDiagnose := cat.Diagnose != nil
	if cat.NodesPerJob > 0 && !isDiagnose {
		npjLine := fmt.Sprintf("Nodes/Job: %d", cat.NodesPerJob)
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", npjLine, pad(boxWidth-4-len(npjLine)))
	}
	if cat.Jobs > 0 && !isDiagnose {
		jobsLine := fmt.Sprintf("Jobs:      %d", cat.Jobs)
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", jobsLine, pad(boxWidth-4-len(jobsLine)))
	}
	if cat.MNNVL != "" && !isDiagnose {
		mnnvlLine := fmt.Sprintf("MNNVL:     %s", cat.MNNVL)
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", mnnvlLine, pad(boxWidth-4-len(mnnvlLine)))
	}

	// Failed groups with reasons.
	printFailedGroups(w, cat.FailedGroups)

	// Per-clique validation status.
	if len(cat.Cliques) > 0 {
		printCliques(w, cat.Cliques, cat.GroupBandwidth)
	}

	// Diagnose results.
	if cat.Diagnose != nil {
		printDiagnoseResults(w, cat.Diagnose)
	}

	// Training metrics by domain.
	if len(cat.Domains) > 0 {
		_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
		hasDomainNames := false
		for _, d := range cat.Domains {
			if d.Name != "" {
				hasDomainNames = true
				break
			}
		}

		if hasDomainNames && len(cat.Cliques) == 0 {
			// Show domain sub-boxes only when cliques aren't already shown above.
			for _, d := range cat.Domains {
				label := d.Name
				if d.NodeCount > 0 {
					label = fmt.Sprintf("%s (%d nodes)", d.Name, d.NodeCount)
				}
				printDomainBox(w, label, &d)
			}
		} else {
			// No topology — print metrics directly.
			for _, d := range cat.Domains {
				printMetricsFlat(w, &d)
			}
		}
	}

	// Per-group bandwidth results (multi-group Workflows).
	// Skip if cliques are already shown (they include bandwidth via merge).
	if len(cat.GroupBandwidth) > 0 && len(cat.Cliques) == 0 {
		printGroupBandwidth(w, cat.GroupBandwidth)
	}

	// Aggregate bandwidth results.
	if len(cat.Bandwidth) > 0 {
		_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
		bwHeader := "Bandwidth:"
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", bwHeader, pad(boxWidth-4-len(bwHeader)))
		colHeader := fmt.Sprintf("    %-10s %-12s %-12s %s", "Size", "AlgBW", "BusBW", "Samples")
		_, _ = fmt.Fprintf(w, "│%s%s│\n", colHeader, pad(boxWidth-2-len(colHeader)))
		for _, bw := range cat.Bandwidth {
			row := fmt.Sprintf("    %-10s %-12s %-12s %d", bw.Size, bw.AlgBW, bw.BusBW, bw.Samples)
			_, _ = fmt.Fprintf(w, "│%s%s│\n", row, pad(boxWidth-2-len(row)))
		}
	}

	printCardBottom(w)
}

// printDomainBox prints a nested domain sub-box within a category card.
// computeDiagnoseMinMax finds the highest and lowest bandwidth tests.
func computeDiagnoseMinMax(d *DiagnoseReport) {
	type entry struct {
		bw   float64
		test *DiagnoseTestRow
	}
	var best, worst *entry
	for i := range d.Tests {
		t := &d.Tests[i]
		bw := parseFloat(strings.TrimSuffix(t.BusBW, " GB/s"))
		if bw <= 0 {
			continue
		}
		e := &entry{bw: bw, test: t}
		if best == nil || bw > best.bw {
			best = e
		}
		if worst == nil || bw < worst.bw {
			worst = e
		}
	}
	if best != nil {
		d.MaxBW = fmt.Sprintf("%.1f GB/s", best.bw)
		d.MaxBWDomain = best.test.Domain
		d.MaxBWNodeList = best.test.Nodes
		d.MinBW = fmt.Sprintf("%.1f GB/s", worst.bw)
		d.MinBWDomain = worst.test.Domain
		d.MinBWNodeList = worst.test.Nodes
	}
}

// printGroupBandwidth renders per-group bandwidth with pass/fail markers.
func printGroupBandwidth(w io.Writer, groups []GroupBandwidthRow) {
	_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
	printBoxLine(w, "Bandwidth by group:")
	for _, gb := range groups {
		mark := markPass
		if gb.Failed || gb.BelowMin {
			mark = markFail
		}
		status := ""
		if gb.BelowMin {
			status = "  LOW"
		}
		printBoxLine(w, fmt.Sprintf("    %s  %s", mark, gb.GroupName))
		if gb.BusBW != "" {
			printBoxLine(w, "       "+gb.BusBW+status)
		} else {
			printBoxLine(w, "       no bandwidth data")
		}
		if gb.Failed || gb.BelowMin {
			for _, node := range gb.Nodes {
				printBoxLine(w, "       - "+node)
			}
		}
	}
}

// printCliques renders per-clique validation status with merged bandwidth.
func printCliques(w io.Writer, cliques []CliqueReport, groupBW []GroupBandwidthRow) {
	_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
	cliqueBW := make(map[string]string)
	for _, gb := range groupBW {
		name := gb.GroupName
		if idx := strings.Index(name, " ("); idx > 0 {
			name = name[:idx]
		}
		cliqueBW[name] = gb.BusBW
	}
	printBoxLine(w, "Cliques:")
	for _, cl := range cliques {
		mark := markPass
		if !cl.Passed {
			mark = markFail
		}
		printBoxLine(w, fmt.Sprintf("    %s  %s  %d/%d nodes", mark, cl.Name, cl.Validated, cl.Total))
		if bw := cliqueBW[cl.Name]; bw != "" {
			printBoxLine(w, "       "+bw)
		}
	}
}

// printDiagnoseTestRow renders one test with nodes and bandwidth on separate lines.
func printDiagnoseTestRow(w io.Writer, mark string, t DiagnoseTestRow) {
	if t.Domain != "" {
		// Screening: show clique ID and node count on separate lines.
		printBoxLine(w, fmt.Sprintf("    %s  %s", mark, t.Domain))
		printBoxLine(w, fmt.Sprintf("       (%d nodes)", len(t.Nodes)))
	} else if t.Passed {
		// Passed non-screening: just show count.
		printBoxLine(w, fmt.Sprintf("    %s  %d nodes", mark, len(t.Nodes)))
	} else {
		// Failed non-screening: list every node for investigation.
		printBoxLine(w, fmt.Sprintf("    %s  %d nodes:", mark, len(t.Nodes)))
		for _, node := range t.Nodes {
			printBoxLine(w, "         - "+node)
		}
	}
	if t.BusBW != "" {
		printBoxLine(w, "       "+t.BusBW)
	}
}

// printBoxLine prints a single line within the box, padded to boxWidth.
// printIterations renders per-iteration timing and outcome.
func printIterations(w io.Writer, iterations []IterationReport) {
	_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
	printBoxLine(w, "Iterations:")
	last := len(iterations) - 1
	for i, iter := range iterations {
		var mark, label string
		switch iter.Status {
		case statusFailed:
			if i < last {
				mark = "↻"
				label = "Restarted"
			} else {
				mark = "✗"
				label = statusFailed
			}
		case statusRunning:
			mark = "⋯"
			label = statusRunning
		default:
			mark = "✓"
			label = iter.Status
		}
		line := fmt.Sprintf("    %s  #%d  %s  %s", mark, iter.Number, label, iter.Duration)
		printBoxLine(w, line)
	}
}

func printBoxLine(w io.Writer, content string) {
	_, _ = fmt.Fprintf(w, "│  %s%s│\n", content, pad(boxWidth-4-displayWidth(content)))
}

// appendNodeLines adds node info lines. For screening (has domain), shows clique ID.
// For other stages, lists each node.
func appendNodeLines(
	lines []struct{ label, value string }, domain string, nodes []string,
) []struct{ label, value string } {
	if domain != "" {
		lines = append(lines, struct{ label, value string }{"", fmt.Sprintf("      %s (%d nodes)", domain, len(nodes))})
	} else {
		for _, node := range nodes {
			lines = append(lines, struct{ label, value string }{"", "      - " + node})
		}
	}
	return lines
}

// collectWorkflowJobs returns the set of Job names for a Workflow.
// For diagnose mode, lists Jobs by label since groups are replaced between stages.
// For other modes, reads JobRefs from the current groups.
func collectWorkflowJobs(
	ctx context.Context, c client.Client,
	wf *burninv1alpha1.Workflow, orch *burninv1alpha1.OrchestrationStatus,
) map[string]bool {
	jobs := map[string]bool{}
	if orch != nil && orch.Diagnose != nil {
		var jobList burninv1alpha1.JobList
		if err := c.List(ctx, &jobList, client.InNamespace(wf.Namespace),
			client.MatchingLabels{"cre.nvidia.com/workflow": wf.Name}); err == nil {
			for _, j := range jobList.Items {
				jobs[j.Name] = true
			}
		}
	} else if orch != nil {
		for _, g := range orch.Groups {
			if g.JobRef != nil {
				jobs[g.JobRef.Name] = true
			}
		}
	}
	return jobs
}

// buildDiagnoseTests builds per-test results by listing all Jobs for the
// Workflow and correlating with BandwidthMeasurements.
func buildDiagnoseTests(
	ctx context.Context, c client.Client,
	wf *burninv1alpha1.Workflow,
	measurements []burninv1alpha1.BandwidthMeasurement,
) []DiagnoseTestRow {
	// Build bandwidth map: job name → peak BusBW (at the largest message size).
	bwByJob := map[string]string{}
	for _, bm := range measurements {
		if peak := peakBandwidthResult(bm.Status.Results); peak != nil {
			bwByJob[bm.Spec.JobRef.Name] = peak.BusBW
		}
	}

	// List all Jobs for this Workflow.
	var jobList burninv1alpha1.JobList
	if err := c.List(ctx, &jobList, client.InNamespace(wf.Namespace),
		client.MatchingLabels{"cre.nvidia.com/workflow": wf.Name}); err != nil {
		return nil
	}

	var rows []DiagnoseTestRow
	for _, j := range jobList.Items {
		groupName := j.GetLabels()["cre.nvidia.com/group"]
		stage := inferDiagnoseStage(groupName)
		passed := controller.CondIsTrue(j.Status.Conditions, burninv1alpha1.JobSucceeded)
		nodes := getJobNodes(&j)

		// Only set domain for screening tests — other stages list individual nodes.
		var domain string
		if stage == "screening" {
			domain = lookupScreeningDomain(wf, nodes)
		}

		row := DiagnoseTestRow{
			Stage:  stage,
			Name:   j.Name,
			Nodes:  nodes,
			Domain: domain,
			Passed: passed,
		}
		if bw, ok := bwByJob[j.Name]; ok {
			row.BusBW = bw + " GB/s"
		}
		rows = append(rows, row)
	}
	return rows
}

// inferDiagnoseStage infers the stage from the group name.
func inferDiagnoseStage(groupName string) string {
	if strings.Contains(groupName, "screen-no-nvl") {
		return "screening-no-nvl"
	}
	if strings.Contains(groupName, "screen") {
		return "screening"
	}
	if strings.Contains(groupName, "bisect") {
		return "bisection"
	}
	if strings.Contains(groupName, "confirm") {
		return "confirmation"
	}
	if strings.Contains(groupName, "inter-domain") {
		return "inter-screening"
	}
	return "unknown"
}

// lookupScreeningDomain finds the topology domain for a screening test's nodes.
func lookupScreeningDomain(wf *burninv1alpha1.Workflow, nodes []string) string {
	if len(nodes) == 0 || wf.Status.Orchestration == nil || wf.Status.Orchestration.Diagnose == nil {
		return ""
	}
	for d, sr := range wf.Status.Orchestration.Diagnose.ScreeningResults {
		if slices.Contains(sr.Nodes, nodes[0]) {
			return d
		}
	}
	return ""
}

// getJobNodes reads the group-nodes annotation set by the workflow controller.
func getJobNodes(job *burninv1alpha1.Job) []string {
	ann := job.GetAnnotations()["cre.nvidia.com/group-nodes"]
	if ann == "" {
		return nil
	}
	return strings.Split(ann, ",")
}

// groupFaultyByDomain groups faulty nodes by their screening domain.
// Returns sorted domain→nodes pairs. Nodes without a domain go under "unknown".
func groupFaultyByDomain(faulty []string, screening map[string]burninv1alpha1.DomainScreeningResult) []struct {
	domain string
	nodes  []string
} {
	// Build node→domain lookup.
	nodeDomain := make(map[string]string)
	for domain, sr := range screening {
		for _, n := range sr.Nodes {
			nodeDomain[n] = domain
		}
	}

	grouped := make(map[string][]string)
	for _, n := range faulty {
		d := nodeDomain[n]
		if d == "" {
			d = "unknown"
		}
		grouped[d] = append(grouped[d], n)
	}

	domains := make([]string, 0, len(grouped))
	for d := range grouped {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	result := make([]struct {
		domain string
		nodes  []string
	}, 0, len(domains))
	for _, d := range domains {
		sort.Strings(grouped[d])
		result = append(result, struct {
			domain string
			nodes  []string
		}{d, grouped[d]})
	}
	return result
}

// printDiagnoseResults renders adaptive fault isolation status and bandwidth.
func printDiagnoseResults(w io.Writer, d *DiagnoseReport) {
	_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
	stageNames := map[string]string{
		"intra-screening":        "intra-rack screening",
		"intra-screening-no-nvl": "intra-rack screening (no NVL)",
		"inter-screening":        "inter-rack screening",
		"bisection":              "bisection",
		"confirmation":           "confirmation",
		"cross-boundary":         "cross-boundary probing",
		"complete":               "complete",
	}
	stage := d.Stage
	if name, ok := stageNames[stage]; ok {
		stage = name
	}
	lines := []struct{ label, value string }{
		{"Diagnosis", ""},
		{"  Stage", stage},
		{"  Rounds", fmt.Sprintf("%d", d.Rounds)},
		{"  Healthy", fmt.Sprintf("%d nodes", d.HealthyCount)},
	}
	if d.SuspectCount > 0 {
		lines = append(lines, struct{ label, value string }{"  Suspect", fmt.Sprintf("%d nodes", d.SuspectCount)})
	}
	if len(d.ConfirmedFaulty) > 0 {
		lines = append(lines, struct{ label, value string }{"  Faulty", fmt.Sprintf("%d nodes", len(d.ConfirmedFaulty))})
		for _, g := range groupFaultyByDomain(d.ConfirmedFaulty, d.ScreeningResults) {
			lines = append(lines, struct{ label, value string }{"", fmt.Sprintf("    %s:", g.domain)})
			for _, node := range g.nodes {
				lines = append(lines, struct{ label, value string }{"", "      - " + node})
			}
		}
	}
	if len(d.InfrastructureFaults) > 0 {
		lines = append(lines, struct{ label, value string }{
			"  Infra", fmt.Sprintf("%d faults", len(d.InfrastructureFaults)),
		})
		for _, f := range d.InfrastructureFaults {
			label := f.Domain
			if label == "" {
				label = "inter-domain"
			}
			lines = append(lines, struct{ label, value string }{
				"", fmt.Sprintf("    %s: %d + %d nodes", label, len(f.GroupA), len(f.GroupB)),
			})
		}
	}
	if d.MaxBW != "" {
		lines = append(lines, struct{ label, value string }{"  Max BW", d.MaxBW})
		lines = appendNodeLines(lines, d.MaxBWDomain, d.MaxBWNodeList)
	}
	if d.MinBW != "" {
		lines = append(lines, struct{ label, value string }{"  Min BW", d.MinBW})
		lines = appendNodeLines(lines, d.MinBWDomain, d.MinBWNodeList)
	}
	for _, l := range lines {
		var line string
		if l.label == "" {
			line = l.value
		} else if l.value == "" {
			line = l.label + ":"
		} else {
			line = fmt.Sprintf("%-12s %s", l.label+":", l.value)
		}
		if len(line) > boxWidth-4 {
			line = line[:boxWidth-7] + "..."
		}
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", line, pad(boxWidth-4-len(line)))
	}

	if len(d.Tests) > 0 {
		_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
		printBoxLine(w, "Test Results:")

		stageDisplay := map[string]string{
			"screening":        "intra-rack screening",
			"screening-no-nvl": "intra-rack screening (no NVL)",
			"inter-screening":  "inter-rack screening",
			"bisection":        "bisection",
			"confirmation":     "confirmation",
		}
		stages := []string{"screening", "screening-no-nvl", "inter-screening", "bisection", "confirmation"}
		for _, stage := range stages {

			var stageTests []DiagnoseTestRow
			for _, t := range d.Tests {
				if t.Stage == stage {
					stageTests = append(stageTests, t)
				}
			}
			if len(stageTests) == 0 {
				continue
			}
			_, _ = fmt.Fprintf(w, "│%s│\n", pad(boxWidth-2))
			header := fmt.Sprintf("  %s:", stageDisplay[stage])
			_, _ = fmt.Fprintf(w, "│  %s%s│\n", header, pad(boxWidth-4-len(header)))
			for _, t := range stageTests {
				mark := markPass
				if !t.Passed {
					mark = markFail
				}
				printDiagnoseTestRow(w, mark, t)
			}
		}
	}
}

func printDomainBox(w io.Writer, label string, d *DomainReport) {
	maxLabel := boxWidth - 10

	// If the label fits, show it inline. Otherwise, list domains vertically.
	if len(label) <= maxLabel {
		_, _ = fmt.Fprintf(w, "│  ┌ %s %s┐ │\n",
			label, strings.Repeat("─", max(0, domainInnerWidth-2-len(label))))
	} else {
		// Open the box, then list each domain on its own line.
		_, _ = fmt.Fprintf(w, "│  ┌%s┐ │\n", strings.Repeat("─", domainInnerWidth))
		for domain := range strings.SplitSeq(d.Name, ", ") {
			line := fmt.Sprintf("  %s", domain)
			_, _ = fmt.Fprintf(w, "│  │%s%s│ │\n", line, pad(domainInnerWidth-len(line)))
		}
		if d.NodeCount > 0 {
			line := fmt.Sprintf("  (%d nodes)", d.NodeCount)
			_, _ = fmt.Fprintf(w, "│  │%s%s│ │\n", line, pad(domainInnerWidth-len(line)))
		}
	}
	printMetricLine(w, "Avg Runtime Goodput", d.Goodput)
	printMetricLine(w, "Avg TFLOPs/GPU", d.TFLOPs)
	printMetricLine(w, "Avg Step Time", d.StepTime)
	_, _ = fmt.Fprintf(w, "│  └%s┘ │\n", strings.Repeat("─", domainInnerWidth))
}

// printMetricsFlat prints metrics directly in the card (no domain sub-box).
func printMetricsFlat(w io.Writer, d *DomainReport) {
	metrics := []struct{ label, value string }{
		{"Avg Runtime Goodput", d.Goodput},
		{"Avg TFLOPs/GPU", d.TFLOPs},
		{"Avg Step Time", d.StepTime},
	}
	for _, m := range metrics {
		if m.value == "" {
			continue
		}
		line := fmt.Sprintf("%s:  %s", m.label, m.value)
		_, _ = fmt.Fprintf(w, "│  %s%s│\n", line, pad(boxWidth-4-len(line)))
	}
}

// domainInnerWidth is the content width between the inner │ borders of a
// domain sub-box. The overhead per line is: outer │ + 2sp + inner │ + content
// + inner │ + 1sp + outer │ = 7 chars, so inner width = boxWidth - 7.
const domainInnerWidth = boxWidth - 7

// printMetricLine prints a single metric line within a domain sub-box.
func printMetricLine(w io.Writer, label, value string) {
	if value == "" {
		return
	}
	line := fmt.Sprintf("  %s:  %s", label, value)
	_, _ = fmt.Fprintf(w, "│  │%s%s│ │\n", line, pad(domainInnerWidth-len(line)))
}

// ---------------------------------------------------------------------------
// Box drawing helpers
// ---------------------------------------------------------------------------

func printBoxTop(w io.Writer) { _, _ = fmt.Fprintf(w, "╔%s╗\n", strings.Repeat("═", boxWidth-2)) }
func printBoxBottom(w io.Writer) {
	_, _ = fmt.Fprintf(w, "╚%s╝\n", strings.Repeat("═", boxWidth-2))
}
func printBoxCenter(w io.Writer, text string) {
	padding := (boxWidth - 2 - len(text)) / 2
	right := boxWidth - 2 - len(text) - padding
	_, _ = fmt.Fprintf(w, "║%s%s%s║\n", pad(padding), text, pad(right))
}

func printCardTop(w io.Writer) {
	_, _ = fmt.Fprintf(w, "┌%s┐\n", strings.Repeat("─", boxWidth-2))
}
func printCardBottom(w io.Writer) {
	_, _ = fmt.Fprintf(w, "└%s┘\n", strings.Repeat("─", boxWidth-2))
}
func printCardSep(w io.Writer) {
	_, _ = fmt.Fprintf(w, "├%s┤\n", strings.Repeat("─", boxWidth-2))
}
func printCardTitle(w io.Writer, title string) {
	_, _ = fmt.Fprintf(w, "│  %s%s│\n", title, pad(boxWidth-4-len(title)))
}

func pad(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}

// displayWidth returns the number of terminal columns a string occupies.
// For ASCII this equals len(s); for multi-byte runes like ✓, ✗, … each
// occupies one column but len() counts 3 bytes.
func displayWidth(s string) int {
	return utf8.RuneCountInString(s)
}

func countDigits(n int) int {
	if n == 0 {
		return 1
	}
	count := 0
	for n > 0 {
		n /= 10
		count++
	}
	return count
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func parseFloat(s string) float64 {
	return numstr.Parse(s)
}

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

type fmtFunc func(float64) string

func fmtPercent(v float64) string {
	return fmt.Sprintf("%.2f (%.0f%%)", v, v*100)
}

func fmtFloat1(v float64) string {
	return fmt.Sprintf("%.1f", v)
}

func fmtFloat2(v float64) string {
	return fmt.Sprintf("%.2fs", v)
}

func fmtAvg(vals []float64, fn fmtFunc) string {
	a := avg(vals)
	if a == 0 {
		return ""
	}
	return fn(a)
}

// peakBandwidthResult returns a pointer to the result with the largest
// SizeBytes in results, or nil if results is empty. Use this instead of
// indexing the last entry: BandwidthMeasurement appends new sizes in the
// order they are first observed in the data points, which is not guaranteed
// to be ascending.
func peakBandwidthResult(results []burninv1alpha1.BandwidthResult) *burninv1alpha1.BandwidthResult {
	var peak *burninv1alpha1.BandwidthResult
	for i := range results {
		if peak == nil || results[i].SizeBytes > peak.SizeBytes {
			peak = &results[i]
		}
	}
	return peak
}

// humanSize converts bytes to a human-readable size string.
func humanSize(bytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%d GB", bytes/gb)
	case bytes >= mb:
		return fmt.Sprintf("%d MB", bytes/mb)
	case bytes >= kb:
		return fmt.Sprintf("%d KB", bytes/kb)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// WriteJSON serializes the report as JSON and writes it to the given path.
// Called when CRE_RESULTS_FILE is set, allowing programmatic consumers
// (e.g. k8s-platform-validator) to read structured results without log parsing.
func WriteJSON(path string, reports []*CertReport) error {
	var v any = reports[0]
	if len(reports) > 1 {
		v = reports
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil { // #nosec G306 -- reports are output files meant to be readable
		return fmt.Errorf("write report file: %w", err)
	}
	return nil
}
