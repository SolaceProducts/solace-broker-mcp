// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// readAllHandler mimics the MCP SDK's StreamableHTTPHandler, which buffers
// the entire request body with io.ReadAll before processing. It records the
// read outcome so tests can assert what the SDK would observe.
type readAllHandler struct {
	readErr error
	read    int
}

func (h *readAllHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	h.read = len(body)
	h.readErr = err
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func TestLimitRequestBody_UnderLimitPassesThrough(t *testing.T) {
	inner := &readAllHandler{}
	handler := limitRequestBody(inner)

	body := bytes.Repeat([]byte("a"), 1024) // typical KB-scale MCP payload
	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if inner.readErr != nil {
		t.Fatalf("downstream read failed for under-limit body: %v", inner.readErr)
	}
	if inner.read != len(body) {
		t.Errorf("downstream read %d bytes, want %d", inner.read, len(body))
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLimitRequestBody_OversizedBodyFailsDownstreamRead(t *testing.T) {
	inner := &readAllHandler{}
	handler := limitRequestBody(inner)

	body := bytes.Repeat([]byte("a"), defaults.MaxMCPRequestBytes+1)
	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var maxBytesErr *http.MaxBytesError
	if !errors.As(inner.readErr, &maxBytesErr) {
		t.Fatalf("downstream read error = %v, want *http.MaxBytesError", inner.readErr)
	}
	if maxBytesErr.Limit != defaults.MaxMCPRequestBytes {
		t.Errorf("MaxBytesError.Limit = %d, want %d", maxBytesErr.Limit, defaults.MaxMCPRequestBytes)
	}
	// The oversized body must never be fully buffered downstream.
	if inner.read > defaults.MaxMCPRequestBytes {
		t.Errorf("downstream buffered %d bytes, want <= %d", inner.read, defaults.MaxMCPRequestBytes)
	}
}
