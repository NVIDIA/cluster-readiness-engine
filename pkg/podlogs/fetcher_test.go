// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podlogs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenStreamTimesOutBeforeResponseHeaders(t *testing.T) {
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	req := newPodLogRequest(t, transport)
	stream, err := openStreamWithTimeout(context.Background(), req, 50*time.Millisecond)

	require.Nil(t, stream)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestOpenStreamTimesOutWhileReadingResponseBody(t *testing.T) {
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: &contextBlockingBody{
				ctx:    req.Context(),
				reader: strings.NewReader("complete line\n"),
			},
			Request: req,
		}, nil
	})

	req := newPodLogRequest(t, transport)
	stream, err := openStreamWithTimeout(context.Background(), req, 50*time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	lines, err := ScanLines(stream)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, []string{"complete line"}, lines)
}

func TestOpenStreamHonorsEarlierParentDeadline(t *testing.T) {
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	req := newPodLogRequest(t, transport)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	stream, err := openStreamWithTimeout(ctx, req, time.Minute)

	require.Nil(t, stream)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestOpenStreamUsesDefaultTimeoutAndCancelsOnClose(t *testing.T) {
	requestContexts := make(chan context.Context, 1)
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requestContexts <- req.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})
	clientset, err := kubernetes.NewForConfig(&rest.Config{
		Host:      "http://pod-logs.test",
		Transport: transport,
	})
	require.NoError(t, err)

	started := time.Now()
	stream, err := OpenStream(context.Background(), clientset.CoreV1().Pods("default").GetLogs("worker-0", &corev1.PodLogOptions{}))
	require.NoError(t, err)

	requestCtx := <-requestContexts
	deadline, ok := requestCtx.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, started.Add(DefaultStreamTimeout), deadline, time.Second)

	require.NoError(t, stream.Close())
	select {
	case <-requestCtx.Done():
		assert.ErrorIs(t, requestCtx.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("request context was not cancelled when the stream closed")
	}
}

func newPodLogRequest(t *testing.T, transport http.RoundTripper) *rest.Request {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(&rest.Config{
		Host:      "http://pod-logs.test",
		Transport: transport,
	})
	require.NoError(t, err)
	return clientset.CoreV1().Pods("default").GetLogs("worker-0", &corev1.PodLogOptions{})
}

type contextBlockingBody struct {
	ctx    context.Context
	reader *strings.Reader
}

func (b *contextBlockingBody) Read(buffer []byte) (int, error) {
	n, err := b.reader.Read(buffer)
	if n > 0 || err != io.EOF {
		return n, err
	}

	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*contextBlockingBody) Close() error {
	return nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
