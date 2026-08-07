// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package goodput

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/numstr"
)

// k8sTimestampRe matches the RFC3339Nano prefix that kubelet injects when
// Timestamps: true is set on the pod log request. The zone is "Z" on a node
// set to UTC and an offset such as "-07:00" on any other node.
// Example: "2026-02-05T16:03:52.889599000Z " or "2026-02-05T09:03:52.889599000-07:00 "
var k8sTimestampRe = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\s`)

// maxStepsInMemory is the maximum number of training steps kept in the result.
const maxStepsInMemory = 100

// compiledPattern holds a compiled regex and its unit conversion table.
type compiledPattern struct {
	re    *regexp.Regexp
	units map[string]string
}

// ProfileParser compiles the regex patterns from a LogProfile CRD and uses
// them to parse training job log lines into structured results.
type ProfileParser struct {
	timestampLayout string
	warmupSteps     int
	logInterval     int

	trainingStep      *compiledPattern
	checkpointSave    *compiledPattern
	checkpointDone    *compiledPattern
	checkpointRestore *compiledPattern
	checkpointLoaded  *compiledPattern
	applicationStart  *compiledPattern
	warmupStep        *compiledPattern
}

// NewProfileParser compiles all non-nil patterns from the LogProfile spec,
// validates each example against its regex, and returns a ready-to-use parser.
func NewProfileParser(profile *v1alpha1.LogProfile) (*ProfileParser, error) {
	p := &ProfileParser{
		timestampLayout: profile.Spec.Timestamp.Layout,
	}
	if profile.Spec.WarmupSteps != nil {
		p.warmupSteps = *profile.Spec.WarmupSteps
	}
	if profile.Spec.LogInterval != nil {
		p.logInterval = *profile.Spec.LogInterval
	}

	type entry struct {
		name    string
		pattern *v1alpha1.EventPattern
		target  **compiledPattern
	}

	entries := []entry{
		{"trainingStep", profile.Spec.Patterns.TrainingStep, &p.trainingStep},
		{"checkpointSave", profile.Spec.Patterns.CheckpointSave, &p.checkpointSave},
		{"checkpointDone", profile.Spec.Patterns.CheckpointDone, &p.checkpointDone},
		{"checkpointRestore", profile.Spec.Patterns.CheckpointRestore, &p.checkpointRestore},
		{"checkpointLoaded", profile.Spec.Patterns.CheckpointLoaded, &p.checkpointLoaded},
		{"applicationStart", profile.Spec.Patterns.ApplicationStart, &p.applicationStart},
		{"warmupStep", profile.Spec.Patterns.WarmupStep, &p.warmupStep},
	}

	for _, e := range entries {
		if e.pattern == nil {
			continue
		}
		cp, err := compilePattern(e.name, e.pattern)
		if err != nil {
			return nil, err
		}
		*e.target = cp
	}

	return p, nil
}

// compilePattern compiles an EventPattern's regex, then validates the example
// (if provided) matches. Returns an error if compilation or validation fails.
func compilePattern(name string, ep *v1alpha1.EventPattern) (*compiledPattern, error) {
	re, err := regexp.Compile(ep.Regex)
	if err != nil {
		return nil, fmt.Errorf("pattern %s: invalid regex: %w", name, err)
	}

	if ep.Example != "" {
		if !re.MatchString(ep.Example) {
			return nil, fmt.Errorf("pattern %s: example does not match regex\n  regex:   %s\n  example: %s", name, ep.Regex, ep.Example)
		}
	}

	return &compiledPattern{
		re:    re,
		units: ep.Units,
	}, nil
}

// ParseLogs processes log lines and returns aggregated parse results.
func (p *ProfileParser) ParseLogs(lines []string) *ParseResult { //nolint:gocyclo
	result := &ParseResult{
		Steps:       make([]*TrainingStepInfo, 0),
		Checkpoints: make([]*CheckpointInfo, 0),
		LogInterval: p.logInterval,
	}

	// warmupBaseStep is the GlobalStep of the first training step after the most
	// recent run boundary (applicationStart or checkpointRestore). Steps within
	// warmupSteps of this base are marked as warmup. This counts by actual step
	// number, not log lines, so it handles log-interval > 1 correctly.
	warmupBaseStep := -1
	warmupPending := false

	// pendingSaveStart tracks the start time of a checkpoint save that has not
	// yet been completed (used by N6-style two-line checkpoint patterns).
	var pendingSaveStart time.Time
	var pendingSaveStep int
	var pendingSavePath string
	// pendingSaveCompleted is true when the save line already created a full
	// checkpoint (N4-style with saveDuration). The subsequent done line should
	// update the existing checkpoint instead of creating a new one.
	var pendingSaveCompleted bool

	for _, line := range lines {
		// Always try to extract the K8s RFC3339 prefix for last-log tracking.
		k8sTS := extractK8sTimestamp(line)
		if !k8sTS.IsZero() {
			result.LastLogTimestamp = k8sTS
		}

		// Try each pattern in priority order.

		// --- Application Start ---
		if p.applicationStart != nil {
			if fields := matchLine(p.applicationStart, line); fields != nil {
				ts := p.resolveTimestamp(fields, k8sTS)
				// Only capture on the first applicationStart match.
				// No continue — broad regexes (e.g. NeMo's "[NeMo ... nemo_logging:...]")
				// match many log lines including checkpoints, so we must fall through
				// to let other patterns match.
				if !ts.IsZero() && result.ApplicationStartTime.IsZero() {
					result.ApplicationStartTime = ts
					warmupPending = true
					warmupBaseStep = -1
				}
			}
		}

		// --- Training Step ---
		if p.trainingStep != nil {
			if fields := matchLine(p.trainingStep, line); fields != nil {
				step := p.buildTrainingStep(fields, k8sTS)
				if step != nil {
					// Filter out steps that are not multiples of logInterval.
					// These are "first after boundary" log lines (e.g., iter 1
					// after start, iter 348 after restart with --log-interval 10)
					// whose StepTiming includes startup/restart overhead.
					if p.logInterval > 1 && step.GlobalStep%p.logInterval != 0 {
						continue
					}
					if p.warmupStep != nil && p.warmupStep.re.MatchString(line) {
						step.IsWarmup = true
					}
					if p.warmupSteps > 0 {
						if warmupPending {
							warmupBaseStep = step.GlobalStep
							warmupPending = false
						}
						if warmupBaseStep >= 0 && step.GlobalStep-warmupBaseStep < p.warmupSteps {
							step.IsWarmup = true
						}
					}
					result.Steps = append(result.Steps, step)
					if len(result.Steps) > maxStepsInMemory {
						result.Steps = result.Steps[len(result.Steps)-maxStepsInMemory:]
					}
					if result.FirstStep == nil {
						result.FirstStep = step
					}
					result.LastStep = step
				}
				continue
			}
		}

		// --- Checkpoint Done (must check before Save to avoid re-matching) ---
		if p.checkpointDone != nil {
			if fields := matchLine(p.checkpointDone, line); fields != nil {
				ts := p.resolveTimestamp(fields, k8sTS)
				step := getIntField(fields, "step")
				path := getStringField(fields, "path")

				// If the save line already created a full checkpoint (N4-style),
				// update that checkpoint with any extra info from the done line
				// instead of creating a duplicate.
				if pendingSaveCompleted && result.LastCheckpoint != nil {
					if path != "" && result.LastCheckpoint.Path == "" {
						result.LastCheckpoint.Path = path
					}
					// Reset pending state.
					pendingSaveStart = time.Time{}
					pendingSaveStep = 0
					pendingSavePath = ""
					pendingSaveCompleted = false
					continue
				}

				var saveDuration float64
				// If we have a saveDuration capture, use it directly.
				if dur, ok := fields["saveDuration"]; ok && dur != "" {
					saveDuration = p.normalizeUnit(p.checkpointDone, "saveDuration", parseFloat(dur))
				} else if !pendingSaveStart.IsZero() && !ts.IsZero() {
					// Compute duration from pending save start time.
					saveDuration = ts.Sub(pendingSaveStart).Seconds()
				}

				// Use pending values if the done line doesn't have them.
				if step == 0 && pendingSaveStep > 0 {
					step = pendingSaveStep
				}
				if path == "" && pendingSavePath != "" {
					path = pendingSavePath
				}

				ckpt := &CheckpointInfo{
					Step:         step,
					Timestamp:    ts,
					Path:         path,
					SaveDuration: saveDuration,
				}
				result.Checkpoints = append(result.Checkpoints, ckpt)
				result.LastCheckpoint = ckpt

				// Reset pending state.
				pendingSaveStart = time.Time{}
				pendingSaveStep = 0
				pendingSavePath = ""
				pendingSaveCompleted = false
				continue
			}
		}

		// --- Checkpoint Save (start) ---
		if p.checkpointSave != nil {
			if fields := matchLine(p.checkpointSave, line); fields != nil {
				ts := p.resolveTimestamp(fields, k8sTS)
				step := getIntField(fields, "step")
				path := getStringField(fields, "path")

				// Check if saveDuration is directly captured (N4-style single line).
				if dur, ok := fields["saveDuration"]; ok && dur != "" {
					saveDuration := p.normalizeUnit(p.checkpointSave, "saveDuration", parseFloat(dur))
					ckpt := &CheckpointInfo{
						Step:         step,
						Timestamp:    ts,
						Path:         path,
						SaveDuration: saveDuration,
					}
					result.Checkpoints = append(result.Checkpoints, ckpt)
					result.LastCheckpoint = ckpt
					// Mark as completed so a subsequent checkpointDone for the same
					// step updates this checkpoint instead of creating a duplicate.
					pendingSaveStep = step
					pendingSavePath = path
					pendingSaveCompleted = true
				} else {
					// N6-style: save start line without duration; wait for done line.
					pendingSaveStart = ts
					pendingSaveStep = step
					pendingSavePath = path
				}
				continue
			}
		}

		// --- Checkpoint Restore ---
		if p.checkpointRestore != nil {
			if fields := matchLine(p.checkpointRestore, line); fields != nil {
				// Only capture the first restore event.
				if result.CheckpointRestore == nil {
					ts := p.resolveTimestamp(fields, k8sTS)
					result.CheckpointRestore = &CheckpointRestoreInfo{
						Path:      getStringField(fields, "path"),
						Timestamp: ts,
						Step:      getIntField(fields, "step"),
					}
				}
				warmupPending = true
				warmupBaseStep = -1
				continue
			}
		}

		// --- Checkpoint Loaded ---
		if p.checkpointLoaded != nil {
			if fields := matchLine(p.checkpointLoaded, line); fields != nil {
				// Update the restore info with loaded time if we have one.
				if result.CheckpointRestore != nil {
					ts := p.resolveTimestamp(fields, k8sTS)
					if !ts.IsZero() {
						// Optionally update step/path from the loaded line.
						if s := getIntField(fields, "step"); s > 0 {
							result.CheckpointRestore.Step = s
						}
						if pathVal := getStringField(fields, "path"); pathVal != "" {
							result.CheckpointRestore.Path = pathVal
						}
					}
				}
				continue
			}
		}
	}

	// Expose pending checkpoint save so the controller can persist it across samples.
	if !pendingSaveStart.IsZero() && !pendingSaveCompleted {
		result.PendingSave = &PendingCheckpointSave{
			Step:      pendingSaveStep,
			Timestamp: pendingSaveStart,
			Path:      pendingSavePath,
		}
	}

	return result
}

// matchLine tries to match a line against a compiled pattern and returns a map
// of named capture groups to their values, or nil if no match.
func matchLine(cp *compiledPattern, line string) map[string]string {
	match := cp.re.FindStringSubmatch(line)
	if match == nil {
		return nil
	}

	fields := make(map[string]string)
	for i, name := range cp.re.SubexpNames() {
		if i == 0 || name == "" {
			continue
		}
		fields[name] = match[i]
	}
	return fields
}

// resolveTimestamp uses the captured timestamp field if present, otherwise
// falls back to the K8s RFC3339 prefix.
func (p *ProfileParser) resolveTimestamp(fields map[string]string, k8sFallback time.Time) time.Time {
	if tsStr, ok := fields["timestamp"]; ok && tsStr != "" {
		if ts, err := time.Parse(p.timestampLayout, tsStr); err == nil {
			return ts
		}
	}
	return k8sFallback
}

// buildTrainingStep constructs a TrainingStepInfo from captured fields.
func (p *ProfileParser) buildTrainingStep(fields map[string]string, k8sFallback time.Time) *TrainingStepInfo {
	ts := p.resolveTimestamp(fields, k8sFallback)

	stepTiming := p.normalizeUnit(p.trainingStep, "stepTiming", getFloatField(fields, "stepTiming"))
	elapsedTime := p.normalizeUnit(p.trainingStep, "elapsedTime", getFloatField(fields, "elapsedTime"))

	// Fall back StepTiming to ElapsedTime when not captured explicitly.
	// NeMo 4 logs use "stepTiming" while NeMo 6 uses "elapsedTime"; both
	// represent per-iteration time in seconds after unit normalization.
	if stepTiming == 0 && elapsedTime > 0 {
		stepTiming = elapsedTime
	}

	globalStep := getIntField(fields, "globalStep")
	iteration := getIntField(fields, "iteration")

	// Fall back GlobalStep to Iteration when not captured explicitly.
	// NeMo 4 logs use "globalStep" while NeMo 6 uses "iteration"; both
	// represent the same training progress counter.
	if globalStep == 0 && iteration > 0 {
		globalStep = iteration
	}

	step := &TrainingStepInfo{
		GlobalStep:  globalStep,
		Epoch:       getIntField(fields, "epoch"),
		Iteration:   iteration,
		Timestamp:   ts,
		StepTiming:  stepTiming,
		Loss:        getFloatField(fields, "loss"),
		TFLOPS:      getFloatField(fields, "tflops"),
		ElapsedTime: elapsedTime,
	}

	return step
}

// normalizeUnit converts a raw value to seconds based on the pattern's units map.
func (p *ProfileParser) normalizeUnit(cp *compiledPattern, field string, value float64) float64 {
	if value == 0 || cp == nil || cp.units == nil {
		return value
	}
	unit, ok := cp.units[field]
	if !ok {
		return value // default is seconds
	}
	switch unit {
	case "ms":
		return value / 1000.0
	case "us":
		return value / 1000000.0
	case "s":
		return value
	default:
		return value
	}
}

// extractK8sTimestamp parses the RFC3339Nano prefix from a Kubernetes log line.
func extractK8sTimestamp(line string) time.Time {
	m := k8sTimestampRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		return time.Time{}
	}
	return ts
}

// getIntField extracts an integer from the named capture fields.
// It handles leading/trailing whitespace and returns 0 if not found or invalid.
func getIntField(fields map[string]string, name string) int {
	v, ok := fields[name]
	if !ok || v == "" {
		return 0
	}
	v = strings.TrimSpace(v)
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// getFloatField extracts a float64 from the named capture fields.
// It handles scientific notation (e.g., "1.218472E+01") and returns 0 if not
// found or invalid.
func getFloatField(fields map[string]string, name string) float64 {
	v, ok := fields[name]
	if !ok || v == "" {
		return 0
	}
	v = strings.TrimSpace(v)
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
}

// getStringField extracts a string from the named capture fields.
func getStringField(fields map[string]string, name string) string { //nolint:unparam
	v, ok := fields[name]
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// parseFloat is a helper that parses a string to float64, returning 0 on error.
func parseFloat(s string) float64 {
	return numstr.Parse(s)
}
