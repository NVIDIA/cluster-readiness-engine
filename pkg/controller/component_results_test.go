// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podread"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/shard"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// reconcileComponentResults is the write half of ADR-090: it turns a terminal
// Job's pod log into per-component rows, writes them as gzipped NDJSON shards,
// and seals the counts on the Job's status.
//
// This pins the bytes, which the integration harness structurally cannot. A
// shard's name embeds the owning Workflow's UID, and envtest assigns that
// afresh on every run, so a golden there could only ever assert the seal. Here
// the UID is fixed, which makes the object names, their labels and their
// decoded contents all comparable — and decoding the rows back out is the only
// assertion that actually proves the emission order, since the ordering is
// invisible in a row count.
//
// The cases deliberately include the ways a harness gets its own output wrong.
// A log line that merely mentions the marker in prose must not be read as a
// malformed row, because failing a Job's whole result set on a log message
// would make the mechanism less trustworthy than no mechanism; while a line
// that opens a JSON object behind the marker has declared itself a result, and
// a broken one is reported rather than dropped.
// testLauncherJob names the replicatedJob whose pod carries no job label, so
// the launcher read has to go live. A constant because goconst counts literals
// across every file in the package, and a third copy of a string here trips
// the linter in files this change does not touch.
const (
	testLauncherJob = "launcher"
	// testKindWorkflow is the owner kind getOwnerWorkflow matches on. Also a
	// constant for the goconst reason above.
	testKindWorkflow = "Workflow"
)

func TestReconcileComponentResults(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "component-results",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// Producer names the row producer; empty means the Job declares no
			// component results at all.
			Producer          string `yaml:"producer"`
			ContainerName     string `yaml:"containerName"`
			ReplicatedJobName string `yaml:"replicatedJobName"`
			// NoWorkflow drops the owner reference, so no Workflow can own the
			// shards.
			NoWorkflow bool `yaml:"noWorkflow"`
			// NoWorkloadRef clears the Job's workload reference, which the
			// launcher read needs to name the JobSet.
			NoWorkloadRef bool `yaml:"noWorkloadRef"`
			// Truncated makes every log read report that it could not be paged
			// to the end.
			Truncated bool `yaml:"truncated"`
			// ShardWriteFails makes every shard ConfigMap write fail, so a case
			// can pin that the pass stays retryable instead of sealing a
			// terminal phase over a shard set that was never written.
			ShardWriteFails bool `yaml:"shardWriteFails"`
			// PriorSealShards puts an existing Writing seal on the Job,
			// carrying this many shards, as a re-drive would find. A Job whose
			// shard writes kept failing is left in Writing; the workload is
			// then deleted and the next pass finds no logs, so the branch that
			// seals a read failure has to preserve the count the group already
			// copied rather than overwrite it with zero.
			PriorSealShards int32 `yaml:"priorSealShards"`
			// TerminalAge backdates the Job's terminal condition. A read that
			// fails is retried until componentSealGracePeriod has passed since
			// the Job went terminal, so a case that wants the sealing path has
			// to look older than that. Parsed as a Go duration; empty means
			// the Job just finished.
			TerminalAge string `yaml:"terminalAge"`
			Log         string `yaml:"log"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const (
			ns          = "ns"
			jobName     = "wf-group-0-iter-1"
			workflowUID = "0123456789abcdef-fixed-for-the-golden"
		)

		workflow := &nvcrev1alpha1.Workflow{
			Name: "wf", Namespace: ns,
			Status: nvcrev1alpha1.WorkflowStatus{Namespace: ns},
		}
		workflow.UID = types.UID(workflowUID)

		job := &nvcrev1alpha1.Job{Name: jobName, Namespace: ns}
		terminalAt := metav1.Now()
		if input.TerminalAge != "" {
			age, err := time.ParseDuration(input.TerminalAge)
			if err != nil {
				return fmt.Errorf("parse terminalAge %q: %w", input.TerminalAge, err)
			}
			terminalAt = metav1.NewTime(time.Now().Add(-age))
		}
		job.Status.Conditions = []metav1.Condition{{
			Type:               nvcrev1alpha1.JobSucceeded,
			Status:             metav1.ConditionTrue,
			Reason:             "WorkloadSucceeded",
			LastTransitionTime: terminalAt,
		}}
		if !input.NoWorkflow {
			job.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: nvcrev1alpha1.GroupVersion.String(),
				Kind:       testKindWorkflow,
				Name:       workflow.Name,
				UID:        workflow.UID,
				Controller: new(true),
			}}
		}
		if !input.NoWorkloadRef {
			job.Status.WorkloadRef = &nvcrev1alpha1.WorkloadReference{
				APIVersion: "v1", Kind: kindConfigMap,
				Name: jobName + "-workload", Namespace: ns,
			}
		}
		if input.Producer != "" {
			job.Spec.ComponentResults = &nvcrev1alpha1.ComponentResultSpec{
				Producer:          input.Producer,
				ContainerName:     input.ContainerName,
				ReplicatedJobName: input.ReplicatedJobName,
			}
		}

		// One worker pod, labelled as production labels workers, plus a
		// launcher that carries only the JobSet labels — exactly as the real
		// thing does, so the cached read legitimately cannot see it.
		worker := &corev1.Pod{
			Name: jobName + "-workload-worker-0", Namespace: ns,
			Labels: map[string]string{
				podread.JobLabel:       jobName,
				jobSetNameLabel:        jobName + "-workload",
				replicatedJobNameLabel: "node",
			},
			Spec:   corev1.PodSpec{NodeName: "gpu-node-1"},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		}
		launcher := &corev1.Pod{
			Name: jobName + "-workload-" + testLauncherJob + "-0", Namespace: ns,
			Labels: map[string]string{
				jobSetNameLabel:        jobName + "-workload",
				replicatedJobNameLabel: testLauncherJob,
			},
			Spec:   corev1.PodSpec{NodeName: "gpu-node-0"},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		if input.PriorSealShards > 0 {
			job.Status.ComponentResults = &nvcrev1alpha1.ComponentResultSeal{
				Phase:      nvcrev1alpha1.ComponentResultsWriting,
				ShardCount: input.PriorSealShards,
				RowCount:   int64(input.PriorSealShards) * 10,
				SealedAt:   &metav1.Time{Time: terminalAt.Time},
			}
		}

		objs := []client.Object{job.DeepCopy(), worker, launcher}
		// Seed the shards that seal claims. On a real cluster they are there:
		// the seal was written before the first shard, and the writes that
		// followed are what succeeded before the workload went away. A fixture
		// that omitted them would record the invariant against a state that
		// cannot occur.
		for i := range int(input.PriorSealShards) {
			cm := &corev1.ConfigMap{}
			cm.Name = shardName(shardNameStem(workflowUID, jobName), i)
			cm.Namespace = ns
			cm.Labels = map[string]string{
				labelShardWorkflowUID:          workflowUID,
				labelShardJob:                  jobName,
				labelShardKind:                 shardKindRows,
				"app.kubernetes.io/managed-by": managedByValue,
			}
			objs = append(objs, cm)
		}
		if !input.NoWorkflow {
			objs = append(objs, workflow.DeepCopy())
		}
		builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
			WithStatusSubresource(&nvcrev1alpha1.Job{}).
			WithIndex(&corev1.Pod{}, podread.JobIndexField, func(obj client.Object) []string {
				pod, ok := obj.(*corev1.Pod)
				if !ok {
					return nil
				}
				if jn, found := pod.Labels[podread.JobLabel]; found {
					return []string{jn}
				}
				return nil
			})
		if input.ShardWriteFails {
			builder = builder.WithInterceptorFuncs(interceptor.Funcs{
				Create: func(
					ctx context.Context, c client.WithWatch, obj client.Object,
					opts ...client.CreateOption,
				) error {
					if _, isCM := obj.(*corev1.ConfigMap); isCM {
						return fmt.Errorf("simulated shard write failure")
					}
					return c.Create(ctx, obj, opts...)
				},
			})
		}
		c := builder.Build()

		logText := input.Log
		r := &JobReconciler{
			Client:     c,
			APIReader:  c,
			Scheme:     scheme,
			LogFetcher: staticLogFetcher{log: &logText, truncated: input.Truncated},
		}

		live := &nvcrev1alpha1.Job{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: jobName}, live); err != nil {
			return err
		}
		result, reconcileErr := r.reconcileComponentResults(context.Background(), live)

		// Read the Job back: the seal must be durable, not just in memory.
		sealed := &nvcrev1alpha1.Job{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: jobName}, sealed); err != nil {
			return err
		}

		out := componentResultsOutput(context.Background(), c, ns, workflowUID, jobName, sealed)
		out.Requeued = result.RequeueAfter > 0
		if reconcileErr != nil {
			out.Error = reconcileErr.Error()
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// shardReport is one shard ConfigMap as the golden records it: its identity and
// labels, and the rows decoded back out of its gzipped payload.
type shardReport struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	OwnedBy    string            `json:"ownedBy"`
	DataKeys   []string          `json:"dataKeys"`
	BinaryKeys []string          `json:"binaryKeys"`
	Rows       []string          `json:"rows,omitempty"`
	Dropped    []string          `json:"dropped,omitempty"`
}

type componentResultsReport struct {
	Requeued bool   `json:"requeued"`
	Error    string `json:"error,omitempty"`
	// Seal is the Job's status as persisted, with the wall-clock stamp reduced
	// to whether it was set.
	SealPhase    string        `json:"sealPhase"`
	SealShards   int32         `json:"sealShardCount"`
	SealRows     int64         `json:"sealRowCount"`
	SealedAtSet  bool          `json:"sealedAtSet"`
	Truncations  []string      `json:"truncations,omitempty"`
	ShardObjects []shardReport `json:"shardObjects"`
}

// componentResultsOutput collects everything a case asserts: the persisted seal
// and every shard object the write produced, decoded.
//
// Shards are found by derived name rather than by listing, which is both what
// the controller itself must do (its ConfigMap grant carries no list verb) and
// what makes a missing shard visible: the seal says how many exist, so a name
// that does not resolve is recorded as missing rather than silently skipped.
func componentResultsOutput(
	ctx context.Context, c client.Client, ns, workflowUID, jobName string, job *nvcrev1alpha1.Job,
) componentResultsReport {
	out := componentResultsReport{ShardObjects: []shardReport{}}
	if seal := job.Status.ComponentResults; seal != nil {
		out.SealPhase = string(seal.Phase)
		out.SealShards = seal.ShardCount
		out.SealRows = seal.RowCount
		out.SealedAtSet = seal.SealedAt != nil
		for _, t := range seal.Truncations {
			out.Truncations = append(out.Truncations,
				fmt.Sprintf("%s count=%d message=%q", t.Reason, t.Count, t.Message))
		}
	}

	stem := shardNameStem(workflowUID, jobName)
	names := make([]string, 0, int(out.SealShards)+1)
	for i := range int(out.SealShards) {
		names = append(names, shardName(stem, i))
	}
	names = append(names, stem+"-truncation")

	for _, name := range names {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cm); err != nil {
			if strings.HasSuffix(name, "-truncation") {
				// Absent unless something was dropped; not a finding.
				continue
			}
			out.ShardObjects = append(out.ShardObjects, shardReport{
				Name: name, OwnedBy: "MISSING: " + err.Error(),
			})
			continue
		}
		out.ShardObjects = append(out.ShardObjects, describeShard(cm))
	}
	return out
}

func describeShard(cm *corev1.ConfigMap) shardReport {
	rep := shardReport{Name: cm.Name, Labels: cm.Labels}
	for _, ref := range cm.OwnerReferences {
		rep.OwnedBy = ref.Kind + "/" + ref.Name
	}
	rep.DataKeys = sortedKeys(cm.Data)
	rep.BinaryKeys = sortedKeys(cm.BinaryData)

	if _, ok := cm.BinaryData[shard.RowsKey]; ok {
		rows, err := shard.DecodeRows(cm)
		if err != nil {
			rep.Rows = []string{"DECODE FAILED: " + err.Error()}
			return rep
		}
		for _, row := range rows {
			rep.Rows = append(rep.Rows, formatRowForGolden(row))
		}
	}
	if payload, ok := cm.BinaryData[shard.TruncationKey]; ok {
		dropped, err := shard.DecodeTruncation(payload)
		if err != nil {
			rep.Dropped = []string{"DECODE FAILED: " + err.Error()}
			return rep
		}
		for _, d := range dropped {
			// Message is printed because carrying it into the shard is the
			// point: a bundle reader never sees Job status, so without it a
			// partial read reaches the bundle as a reason with no cause.
			rep.Dropped = append(rep.Dropped,
				fmt.Sprintf("%s node=%s component=%s index=%d count=%d message=%q",
					d.Reason, d.Node, d.ComponentID, d.Index, d.Count, d.Message))
		}
	}
	return rep
}

// formatRowForGolden renders a decoded row in emission-key order, so a golden
// read top to bottom is a direct statement of the sort: node, componentID,
// index, name, then sample.
func formatRowForGolden(r shard.Row) string {
	return fmt.Sprintf("%s %s idx=%d %s sample=%d value=%v unit=%s outcome=%s ts=%s",
		r.Node, r.ComponentID, r.Index, r.Name, r.Sample, r.Value, r.Unit, r.Outcome, r.Timestamp)
}

func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
