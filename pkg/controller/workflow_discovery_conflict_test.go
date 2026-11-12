// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestDiscoverAndPartitionSurvivesConflict covers the write that ends
// discovery. discoverAndPartition fills status.orchestration (groups, detected
// platform, resolved fabric) and status.dependencyRefs on the cached object
// and then sets InProgress/GroupsPartitioned. When that write loses the
// optimistic-concurrency race, updateStatusWithRetry re-fetches the Workflow
// in place and re-applies only closure mutations.
//
// Without applyOrchestration and applyDependencyRefs on that write, the
// condition was persisted with an empty orchestration status. The next
// reconcile re-ran discovery, found the condition already in place, skipped
// the write as a no-op, and the Workflow looped in discovery without ever
// creating a Job. The cases run discovery `reconciles` times and read the
// result back from the API server.
func TestDiscoverAndPartitionSurvivesConflict(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "discovery-conflict",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Conflicts  int `json:"conflicts"`
			Reconciles int `json:"reconciles"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme, err := newConflictTestScheme()
		if err != nil {
			return err
		}
		// Fabric detection lists ResourceSlices.
		if err := resourcev1.AddToScheme(scheme); err != nil {
			return err
		}

		wf := &nvcrev1alpha1.Workflow{}
		if err := yaml.Unmarshal([]byte(discoveryConflictWorkflowYAML), wf); err != nil {
			return err
		}
		objs := []client.Object{wf}
		for i := 1; i <= 2; i++ {
			node := &corev1.Node{}
			if err := yaml.Unmarshal(fmt.Appendf(nil, discoveryConflictNodeYAML, i), node); err != nil {
				return err
			}
			objs = append(objs, node)
		}

		writes := 0
		c := discoveryConflictClient(scheme, objs, wf, in.Conflicts, &writes)
		r := &WorkflowReconciler{Client: c, Scheme: scheme, JobRequeueInterval: time.Second}

		for range in.Reconciles {
			// Each reconcile starts from the API server's copy, as the real
			// one does, so only persisted state carries over.
			got := &nvcrev1alpha1.Workflow{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: "wf", Namespace: "ns"}, got); err != nil {
				return err
			}
			if got.Status.Orchestration != nil && got.Status.Orchestration.TotalGroups > 0 {
				// Discovery is done; the real reconcile would move on to Jobs.
				break
			}
			if _, err := r.discoverAndPartition(context.Background(), got, r.ensureOrchestrationStatus(got)); err != nil {
				return err
			}
		}

		out, err := persistedOrchestrationOutput(c, writes)
		if err != nil {
			return err
		}
		tc.Actual = out
		return nil
	})
}

// discoveryConflictWorkflowYAML is a hand-written Workflow whose only override
// matches on when.fabric and adds a dependency, so discovery records a
// resolved fabric and a dependency ref as well as the groups.
const discoveryConflictWorkflowYAML = `
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Workflow
metadata:
  name: wf
  namespace: ns
  generation: 1
  uid: 01234567-89ab
spec:
  jobTemplate:
    spec:
      workload:
        trainJob:
          runtimeRef:
            kind: TrainingRuntime
            name: test-runtime
          trainer:
            image: test-image:latest
            numNodes: 2
  orchestration:
    target:
      nodeSelector:
        discovery-conflict: "true"
  overrides:
    - when:
        fabric:
          equals: efa
      dependencies:
        - apiVersion: v1
          kind: ConfigMap
          metadata:
            name: efa-config
            namespace: ns
          data:
            fabric: efa
`

// discoveryConflictNodeYAML is an AWS GB300 node that advertises EFA, so the
// fabric is detected as efa. %[1]d is the node's index.
const discoveryConflictNodeYAML = `
apiVersion: v1
kind: Node
metadata:
  name: node-%[1]d
  labels:
    discovery-conflict: "true"
    nvidia.com/gpu.present: "true"
    nvidia.com/gpu.product: NVIDIA-GB300-NVL72
    kubernetes.io/hostname: node-%[1]d
spec:
  providerID: aws://us-east-1a/i-%017[1]d
status:
  allocatable:
    nvidia.com/gpu: "4"
    vpc.amazonaws.com/efa: "1"
`

// discoveryConflictClient is newConflictInjectingClient with the nodes seeded
// too: the Workflow's status subresource rejects its first `conflicts` updates
// with a 409.
func discoveryConflictClient(
	scheme *runtime.Scheme, objs []client.Object, wf *nvcrev1alpha1.Workflow, conflicts int, writes *int,
) client.Client {
	remaining := conflicts
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(wf).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context, cl client.Client, subResourceName string,
				obj client.Object, opts ...client.SubResourceUpdateOption,
			) error {
				*writes++
				if remaining > 0 {
					remaining--
					return apierrors.NewConflict(
						schema.GroupResource{Group: nvcrev1alpha1.GroupVersion.Group, Resource: workflowResourceName},
						obj.GetName(), errors.New("simulated stale write"))
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
}
