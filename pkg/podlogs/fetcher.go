// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package podlogs provides generic pod log fetching utilities.
package podlogs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// DefaultStreamTimeout bounds the complete lifecycle of a pod log request,
// including opening the stream and consuming its response body.
const DefaultStreamTimeout = 2 * time.Minute

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

	// Always bound the response server-side. A training job that logs heavily can
	// otherwise stream hundreds of megabytes into the controller's heap in a
	// single reconcile. Callers reading incrementally (SinceTime anchored) simply
	// pick up the remainder on the next sample.
	limit := opts.LimitBytes
	if limit <= 0 {
		limit = DefaultMaxLogBytes
	}
	podLogOpts.LimitBytes = &limit

	req := f.clientset.CoreV1().Pods(namespace).GetLogs(podName, podLogOpts)
	stream, err := OpenStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("opening log stream: %w", err)
	}
	defer stream.Close() //nolint:errcheck

	// LimitBytes is enforced by the API server, but a misbehaving or proxied
	// endpoint could ignore it — bound the client side too.
	return ScanLines(io.LimitReader(stream, limit))
}

// OpenStream opens a pod log request bounded by DefaultStreamTimeout or the
// parent context's deadline, whichever is earlier. The returned stream keeps
// that deadline active until it is closed, so stalled response-body reads are
// bounded as well as the initial request.
func OpenStream(ctx context.Context, req *rest.Request) (io.ReadCloser, error) {
	return openStreamWithTimeout(ctx, req, DefaultStreamTimeout)
}

func openStreamWithTimeout(ctx context.Context, req *rest.Request, timeout time.Duration) (io.ReadCloser, error) {
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	stream, err := req.Stream(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}

	return &cancelReadCloser{
		ReadCloser: stream,
		cancel:     cancel,
	}, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelReadCloser) Close() error {
	r.cancel()
	return r.ReadCloser.Close()
}

// DefaultMaxLogBytes caps a single log fetch at 8 MB. Sized well above a normal
// sampling window (a chatty trainer emits a few hundred KB per minute) while
// staying far below the controller's memory limit.
const DefaultMaxLogBytes int64 = 8 * 1024 * 1024

// LogOptions configures log reading.
type LogOptions struct {
	Container string
	SinceTime *metav1.Time

	// LimitBytes caps how many bytes the API server returns for this fetch.
	// Zero means DefaultMaxLogBytes; reads are never unbounded.
	LimitBytes int64
}

// maxScannerBuffer is the maximum buffer size for the bufio.Scanner (1 MB).
const maxScannerBuffer = 1024 * 1024

// ScanLines reads all lines from a reader using a buffered scanner with a 1 MB buffer.
//
// Callers are responsible for bounding the reader (see LogOptions.LimitBytes);
// this returns one string per line and so allocates proportionally to input size.
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
