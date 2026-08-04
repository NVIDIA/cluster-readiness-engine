// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// GzipString compresses s. ModTime is left at its zero value so the output is
// deterministic for the same input and level (stable across reconciles and in
// golden tests).
func GzipString(s string) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip writer: %w", err)
	}
	if _, err := zw.Write([]byte(s)); err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("failed to write gzip data: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}

// GunzipString decompresses gzip-compressed bytes back to a string.
func GunzipString(b []byte) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("failed to read gzip data: %w", err)
	}
	return string(out), nil
}
