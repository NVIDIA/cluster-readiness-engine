// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nccl provides parsing for NCCL bandwidth test output.
package nccl

// BandwidthDataPoint represents a single bandwidth measurement from NCCL all_reduce_perf output.
type BandwidthDataPoint struct {
	SizeBytes int64   // message size in bytes
	AlgBW     float64 // algorithmic bandwidth in GB/s
	BusBW     float64 // bus bandwidth in GB/s
}
