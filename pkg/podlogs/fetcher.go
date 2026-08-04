// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package podlogs provides generic pod log fetching utilities.
package podlogs

import (
	"bufio"
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodLogFetcher abstracts fetching raw log lines from a pod.
type PodLogFetcher interface {
	FetchLogs(ctx context.Context, namespace, podName string, opts LogOptions) ([]string, error)
}

// kubernetesLogFetcher reads logs from the Kubernetes API.
type kubernetesLogFetcher struct {
	clientset *kubernetes.Clientset
}

// NewKubernetesLogFetcher creates a PodLogFetcher backed by a Kubernetes clientset.
func NewKubernetesLogFetcher(clientset *kubernetes.Clientset) PodLogFetcher {
	return &kubernetesLogFetcher{clientset: clientset}
}

func (f *kubernetesLogFetcher) FetchLogs(ctx context.Context, namespace, podName string, opts LogOptions) ([]string, error) {
	podLogOpts := &corev1.PodLogOptions{
		Timestamps: true,
	}
	if opts.Container != "" {
		podLogOpts.Container = opts.Container
	}
	if opts.SinceTime != nil {
		podLogOpts.SinceTime = opts.SinceTime
	}

	req := f.clientset.CoreV1().Pods(namespace).GetLogs(podName, podLogOpts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening log stream: %w", err)
	}
	defer stream.Close() //nolint:errcheck

	return ScanLines(stream)
}

// LogOptions configures log reading.
type LogOptions struct {
	Container string
	SinceTime *metav1.Time
}

// maxScannerBuffer is the maximum buffer size for the bufio.Scanner (1 MB).
const maxScannerBuffer = 1024 * 1024

// ScanLines reads all lines from a reader using a buffered scanner with a 1 MB buffer.
func ScanLines(reader io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, maxScannerBuffer), maxScannerBuffer)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return lines, fmt.Errorf("scanning log lines: %w", err)
	}
	return lines, nil
}
