// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

// Shared string literals reused across multiple test files in this package.
// goconst flags literals repeated three or more times package-wide; these
// constants collect the ones that span file boundaries so every call site
// can share a single definition instead of duplicating the literal.
const (
	// testJobsResource is the plural resource name used by injected conflicts.
	testJobsResource = "jobs"
	// testGPUProductLabel is the node label key advertising GPU product.
	testGPUProductLabel = "nvidia.com/gpu.product"
	// testGPUProductH100 is a GPU product label value used by fixtures.
	testGPUProductH100 = "NVIDIA-H100-80GB-HBM3"
	// testDomainCommunication is a CertificateCategory domain used by fixtures.
	testDomainCommunication = "communication"
	// testNodeA is a node name used by fixtures.
	testNodeA = "node-a"
	// testMetricGoodputRatio is the goodput ratio metric/result key.
	testMetricGoodputRatio = "goodputRatio"
	// testVariantNCCLAllReduce is a CertificateCategory variant used by fixtures.
	testVariantNCCLAllReduce = "nccl-all-reduce"
	// testNS is the namespace used by fixtures.
	testNS = "default"
	// testPlatformAWS is the detected-platform value used by fixtures.
	testPlatformAWS = "aws"
	// testProviderIDAWS is an AWS providerID; nodePlatform keys off its prefix.
	testProviderIDAWS = "aws://us-east-1/i-0abc"
	// testRunName is the WorkloadRun name used by fixtures.
	testRunName = "run"
	// testJobName is the Job name used by fixtures.
	testJobName = "job"
	// testNodeFailureDetail is a per-node failure message used by fixtures.
	testNodeFailureDetail = "xid"
)
