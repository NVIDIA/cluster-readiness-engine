// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type readAllInput struct {
	PageBytes int64  `yaml:"pageBytes"`
	MaxPages  int    `yaml:"maxPages"`
	Unpaged   bool   `yaml:"unpaged"`
	Log       string `yaml:"log"`
	// LongLineBytes appends a timestamped line of that many bytes, too long
	// for a fixture file.
	LongLineBytes int `yaml:"longLineBytes"`
}

type readAllRead struct {
	Since       string `json:"since,omitempty"`
	LimitBytes  int64  `json:"limitBytes"`
	Lines       int    `json:"lines"`
	Truncated   bool   `json:"truncated,omitempty"`
	PartialTail bool   `json:"partialTail,omitempty"`
}

type readAllResult struct {
	Visited     []string      `json:"visited"`
	Reads       []readAllRead `json:"reads,omitempty"`
	Unpageable  bool          `json:"unpageable,omitempty"`
	ErrorString string        `json:"error,omitempty"`
}

// TestReadAll pins ReadAll against a fake that serves pages the way the
// kubelet does: SinceTime rounded down to the second on the wire, every line at
// or after it, cut at LimitBytes keeping the head.
func TestReadAll(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "read-all",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in readAllInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		kubelet := &kubeletLogs{lines: strings.Split(strings.TrimSuffix(in.Log, "\n"), "\n")}
		if in.LongLineBytes > 0 {
			kubelet.lines = append(kubelet.lines, "2026-09-30T10:00:02.100000000Z "+strings.Repeat("x", in.LongLineBytes))
		}
		var fetcher PodLogFetcher = kubelet
		if in.Unpaged {
			fetcher = unpagedFetcher{kubelet}
		}

		got := readAllResult{Visited: []string{}}
		err := ReadAll(context.Background(), fetcher, "default", "launcher-0",
			ReadAllOptions{PageBytes: in.PageBytes, MaxPages: in.MaxPages},
			func(line string) { got.Visited = append(got.Visited, line) })
		got.Reads = kubelet.reads
		if err != nil {
			got.Unpageable = errors.Is(err, ErrLogUnpageable)
			got.ErrorString = err.Error()
		}

		b, marshalErr := json.MarshalIndent(got, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// kubeletLogs serves a fixed, timestamped log the way the kubelet does.
type kubeletLogs struct {
	lines []string
	reads []readAllRead
}

func (k *kubeletLogs) FetchLogs(ctx context.Context, namespace, podName string, opts LogOptions) ([]string, error) {
	page, err := k.FetchLogsPage(ctx, namespace, podName, opts)
	return page.Lines, err
}

func (k *kubeletLogs) FetchLogsPage(_ context.Context, _, _ string, opts LogOptions) (Page, error) {
	var since time.Time
	read := readAllRead{LimitBytes: opts.LimitBytes}
	if opts.SinceTime != nil {
		// metav1.Time serializes at second precision.
		since = opts.SinceTime.Truncate(time.Second)
		read.Since = since.UTC().Format(time.RFC3339)
	}

	var buf bytes.Buffer
	for _, line := range k.lines {
		if ts, ok := LineTimestamp(line); ok && ts.Before(since) {
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	body := buf.Bytes()
	if int64(len(body)) > opts.LimitBytes {
		body = body[:opts.LimitBytes]
	}

	page, err := scanPage(bytes.NewReader(body), opts.LimitBytes)
	read.Lines = len(page.Lines)
	read.Truncated = page.Truncated
	read.PartialTail = page.PartialTail
	k.reads = append(k.reads, read)
	return page, err
}

// unpagedFetcher hides FetchLogsPage, as a plain PodLogFetcher would.
type unpagedFetcher struct{ k *kubeletLogs }

func (u unpagedFetcher) FetchLogs(ctx context.Context, namespace, podName string, opts LogOptions) ([]string, error) {
	return u.k.FetchLogs(ctx, namespace, podName, opts)
}
