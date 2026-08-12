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
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testBodyLimit keeps oversized test bodies small; the middleware behavior
// is independent of the cap value (production wires
// defaults.MaxMCPRequestBytes in main).
const testBodyLimit = 1024

// readAllHandler mimics the MCP SDK's StreamableHTTPHandler, which buffers
// the entire request body with io.ReadAll before processing. It records the
// read outcome so tests can assert what the SDK would observe. Client-visible
// status and body are asserted against the real SDK handler in the
// TestLimitRequestBody_RealSDK tests below, not against this mimic.
type readAllHandler struct {
	invoked bool
	readErr error
	read    int
}

func (h *readAllHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.invoked = true
	body, err := io.ReadAll(r.Body)
	h.read = len(body)
	h.readErr = err
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// newRealSDKHandler builds a genuine StreamableHTTPHandler (no tools needed)
// wrapped by limitRequestBody, so tests observe the exact status and body a
// real MCP client gets when the cap trips inside the SDK's io.ReadAll.
func newRealSDKHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "body-limit-test",
		Version: "0.0.1",
	}, nil)
	sdkHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	return limitRequestBody(sdkHandler, testBodyLimit)
}

// newMCPRequest builds a POST /mcp request with the Accept and Content-Type
// headers the SDK validates before reading the body.
func newMCPRequest(body io.Reader) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return req
}

func TestLimitRequestBody_UnderLimitPassesThrough(t *testing.T) {
	inner := &readAllHandler{}
	handler := limitRequestBody(inner, testBodyLimit)

	body := bytes.Repeat([]byte("a"), testBodyLimit/2)
	req := newMCPRequest(bytes.NewReader(body))
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
	handler := limitRequestBody(inner, testBodyLimit)

	// Wrap the reader so httptest does not set Content-Length; the request
	// must reach the downstream read for MaxBytesReader to trip.
	body := bytes.Repeat([]byte("a"), testBodyLimit+1)
	req := newMCPRequest(struct{ io.Reader }{bytes.NewReader(body)})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var maxBytesErr *http.MaxBytesError
	if !errors.As(inner.readErr, &maxBytesErr) {
		t.Fatalf("downstream read error = %v, want *http.MaxBytesError", inner.readErr)
	}
	if maxBytesErr.Limit != testBodyLimit {
		t.Errorf("MaxBytesError.Limit = %d, want %d", maxBytesErr.Limit, testBodyLimit)
	}
	// The oversized body must never be fully buffered downstream.
	if inner.read > testBodyLimit {
		t.Errorf("downstream buffered %d bytes, want <= %d", inner.read, testBodyLimit)
	}
}

func TestLimitRequestBody_DeclaredOversizeRejectedBeforeHandler(t *testing.T) {
	inner := &readAllHandler{}
	handler := limitRequestBody(inner, testBodyLimit)

	// bytes.Reader gives httptest a known size, so Content-Length declares
	// the overage and the middleware rejects without invoking the handler.
	body := bytes.Repeat([]byte("a"), testBodyLimit+1)
	req := newMCPRequest(bytes.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if inner.invoked {
		t.Error("downstream handler was invoked for a declared-oversize request")
	}
}

// TestLimitRequestBody_RealSDK_UnderLimit proves the middleware does not
// interfere with a normal MCP exchange through the real SDK handler.
func TestLimitRequestBody_RealSDK_UnderLimit(t *testing.T) {
	handler := newRealSDKHandler()

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"body-limit-test","version":"0.0.1"}}}`
	req := newMCPRequest(strings.NewReader(initialize))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestLimitRequestBody_RealSDK_StreamedOversize pins the client-observable
// contract when MaxBytesReader trips inside the SDK's io.ReadAll. As of
// go-sdk v1.7.0 the SDK maps the read failure to 413 "request body exceeds
// <n> bytes" (previously 400 "failed to read body" in <= v1.6.1). If an
// SDK upgrade changes how it buffers or reports the body, this test surfaces
// it so the mitigation can be re-verified.
func TestLimitRequestBody_RealSDK_StreamedOversize(t *testing.T) {
	handler := newRealSDKHandler()

	body := bytes.Repeat([]byte("a"), testBodyLimit+1)
	req := newMCPRequest(struct{ io.Reader }{bytes.NewReader(body)})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "request body exceeds") {
		t.Errorf("body = %q, want it to contain %q", got, "request body exceeds")
	}
}

// TestLimitRequestBody_RealSDK_DeclaredOversize proves a client that declares
// its oversized Content-Length gets the middleware's 413 "request body too
// large" short-circuit, distinct from the SDK's own 413 ("request body
// exceeds N bytes") for streamed bodies.
func TestLimitRequestBody_RealSDK_DeclaredOversize(t *testing.T) {
	handler := newRealSDKHandler()

	body := bytes.Repeat([]byte("a"), testBodyLimit+1)
	req := newMCPRequest(bytes.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "request body too large") {
		t.Errorf("body = %q, want it to contain %q", got, "request body too large")
	}
}
