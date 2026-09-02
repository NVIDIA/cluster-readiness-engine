// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type openStreamInput struct {
	Stall         string `yaml:"stall"`
	StreamTimeout string `yaml:"streamTimeout"`
	ParentTimeout any    `yaml:"parentTimeout"`
	Body          string `yaml:"body"`
}

type openStreamResult struct {
	Lines               *[]string `json:"lines,omitempty"`
	ErrorKind           string    `json:"errorKind,omitempty"`
	StreamOpened        *bool     `json:"streamOpened,omitempty"`
	ElapsedUnder        string    `json:"elapsedUnder,omitempty"`
	DeadlineFromDefault *bool     `json:"deadlineFromDefault,omitempty"`
	CancelledOnClose    *bool     `json:"cancelledOnClose,omitempty"`
}

func TestOpenStream(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "open-stream",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in openStreamInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		var got openStreamResult
		var err error
		switch in.Stall {
		case "none":
			got, err = runDefaultTimeoutCase(in)
		case "headers", "body":
			got, err = runStallCase(tc.T, in)
		default:
			return fmt.Errorf("unknown stall %q", in.Stall)
		}
		if err != nil {
			return err
		}

		b, marshalErr := json.MarshalIndent(got, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func runStallCase(t testing.TB, in openStreamInput) (openStreamResult, error) {
	streamTimeout, err := time.ParseDuration(in.StreamTimeout)
	if err != nil {
		return openStreamResult{}, fmt.Errorf("streamTimeout: %w", err)
	}

	ctx := context.Background()
	parentTimeout, hasParent, err := parseParentTimeout(in.ParentTimeout)
	if err != nil {
		return openStreamResult{}, err
	}
	if hasParent {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, parentTimeout)
		defer cancel()
	}

	body := in.Body
	if body == "" {
		body = "complete line\n"
	}

	var transport http.RoundTripper
	switch in.Stall {
	case "headers":
		transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
	case "body":
		transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &contextBlockingBody{
					ctx:    req.Context(),
					reader: strings.NewReader(body),
				},
				Request: req,
			}, nil
		})
	}

	started := time.Now()
	stream, err := openStreamWithTimeout(ctx, newPodLogRequest(t, transport), streamTimeout)
	elapsed := time.Since(started)

	lines := []string{}
	opened := false
	got := openStreamResult{
		Lines:        &lines,
		StreamOpened: &opened,
	}
	if err != nil {
		got.ErrorKind = errorKind(err)
	} else {
		opened = true
		defer stream.Close() //nolint:errcheck
		scanned, scanErr := ScanLines(stream)
		if scanned != nil {
			lines = scanned
			got.Lines = &lines
		}
		got.ErrorKind = errorKind(scanErr)
	}
	if hasParent && elapsed < time.Second {
		got.ElapsedUnder = "1s"
	}
	return got, nil
}

func runDefaultTimeoutCase(in openStreamInput) (openStreamResult, error) {
	if in.StreamTimeout != "default" {
		return openStreamResult{}, fmt.Errorf("streamTimeout: want default, got %q", in.StreamTimeout)
	}

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
	if err != nil {
		return openStreamResult{}, err
	}

	started := time.Now()
	stream, err := OpenStream(context.Background(), clientset.CoreV1().Pods("default").GetLogs("worker-0", &corev1.PodLogOptions{}))
	if err != nil {
		return openStreamResult{}, err
	}

	requestCtx := <-requestContexts
	deadline, ok := requestCtx.Deadline()
	fromDefault := ok && deadline.Sub(started).Abs() <= DefaultStreamTimeout+time.Second &&
		started.Add(DefaultStreamTimeout).Sub(deadline).Abs() <= time.Second

	closeErr := stream.Close()
	if closeErr != nil {
		return openStreamResult{}, closeErr
	}

	cancelled := false
	select {
	case <-requestCtx.Done():
		cancelled = errors.Is(requestCtx.Err(), context.Canceled)
	case <-time.After(time.Second):
	}

	return openStreamResult{
		DeadlineFromDefault: &fromDefault,
		CancelledOnClose:    &cancelled,
	}, nil
}

func parseParentTimeout(v any) (time.Duration, bool, error) {
	if v == nil {
		return 0, false, nil
	}
	switch x := v.(type) {
	case string:
		if x == "" || x == "0" {
			return 0, false, nil
		}
		d, err := time.ParseDuration(x)
		if err != nil {
			return 0, false, fmt.Errorf("parentTimeout: %w", err)
		}
		if d == 0 {
			return 0, false, nil
		}
		return d, true, nil
	case int:
		if x == 0 {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("parentTimeout: unsupported int %d", x)
	case int64:
		if x == 0 {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("parentTimeout: unsupported int %d", x)
	case float64:
		if x == 0 {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("parentTimeout: unsupported number %v", x)
	default:
		return 0, false, fmt.Errorf("parentTimeout: unsupported type %T", v)
	}
}

func errorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "Canceled"
	default:
		return err.Error()
	}
}

func newPodLogRequest(t testing.TB, transport http.RoundTripper) *rest.Request {
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
