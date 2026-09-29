// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"bytes"
	"compress/gzip"
	"errors"
	"math/rand"
	"net/http"
	"testing"
	"time"
)

func TestSnapshotSizeLimits(t *testing.T) {
	t.Run("raw", func(t *testing.T) {
		if _, err := encodeSnapshot(bytes.Repeat([]byte("x"), MaxPayloadBytes+1)); !errors.Is(err, errPayloadTooLarge) {
			t.Fatalf("raw limit: %v", err)
		}
	})
	t.Run("compressed", func(t *testing.T) {
		body := make([]byte, MaxCompressedBytes*2)
		if _, err := rand.New(rand.NewSource(42)).Read(body); err != nil {
			t.Fatal(err)
		}
		if _, err := encodeSnapshot(body); !errors.Is(err, errPayloadTooLarge) {
			t.Fatalf("compressed limit: %v", err)
		}
	})
	t.Run("decompression", func(t *testing.T) {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(bytes.Repeat([]byte("x"), MaxPayloadBytes+1)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := decodeSnapshot(compressed.Bytes()); !errors.Is(err, errPayloadTooLarge) {
			t.Fatalf("decompression limit: %v", err)
		}
	})
	t.Run("exact-raw-limit", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), MaxPayloadBytes)
		compressed, err := encodeSnapshot(body)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeSnapshot(compressed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, decoded) {
			t.Fatal("snapshot round trip changed payload")
		}
	})
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, value string
		want        time.Duration
	}{
		{"seconds", "600", 10 * time.Minute},
		{"date", now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
		{"past", now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"negative", "-1", 0},
		{"invalid", "later", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value, now); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRetryBackoffBounds(t *testing.T) {
	spec := testExport().Spec
	for attempts := int32(1); attempts <= 100; attempts++ {
		delay := retryDelay(spec, attempts)
		if delay <= 0 || delay > spec.Retry.MaxBackoff.Duration {
			t.Fatalf("attempt %d delay %s is outside bounds", attempts, delay)
		}
	}
}
