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

package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// panicHandler returns a handler that always panics with the given value.
func panicHandler(v any) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(v)
	})
}

// captureLogs swaps slog's default to a JSON handler writing into buf for the
// duration of the test, then restores the prior default. It returns buf so the
// caller can decode the emitted records.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// drive sends a GET / request through handler and returns the recorder.
func drive(handler http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestHTTPMiddleware_RecoversAndReturns500 pins the core contract: a panicking
// downstream handler is caught and converted to a clean HTTP 500 with the
// documented JSON error body, and the process keeps running (ServeHTTP returns
// normally rather than re-panicking).
func TestHTTPMiddleware_RecoversAndReturns500(t *testing.T) {
	t.Parallel()
	handler := HTTPMiddleware(panicHandler("boom"))
	rec := drive(handler)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v; body=%q", err, rec.Body.String())
	}
	if body["error"] != "internal_error" {
		t.Errorf(`body["error"] = %v, want "internal_error"`, body["error"])
	}
	if body["error_description"] != "the server encountered an unexpected condition" {
		t.Errorf(`body["error_description"] = %v, want the documented description`, body["error_description"])
	}
	// The recovery layer sits OUTSIDE correlation, so it must NOT invent a
	// correlation_id field it cannot see.
	if _, present := body["correlation_id"]; present {
		t.Errorf("body unexpectedly contains correlation_id: %v", body)
	}
}

// TestHTTPMiddleware_PassesThroughWhenNoPanic pins that a well-behaved handler
// is untouched: the middleware is transparent for normal traffic. This is what
// keeps the standalone probe routes (/livez, /health, /ready) working through
// the wrapper.
func TestHTTPMiddleware_PassesThroughWhenNoPanic(t *testing.T) {
	t.Parallel()
	const want = `{"status":"alive"}`
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, want)
	}))
	rec := drive(handler)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q (middleware must be transparent for normal traffic)", got, want)
	}
}

// TestHTTPMiddleware_LogShape pins the structured ERROR log: event marker,
// panic_type carrying only the Go TYPE (never the panic value's text), and a
// non-empty stack trace. The secure-logging rule (mirrors withRecovery in
// internal/tools) forbids logging the panic value's text.
func TestHTTPMiddleware_LogShape(t *testing.T) {
	// Not parallel: mutates the process-global slog default.
	buf := captureLogs(t)
	const secret = "super-secret-panic-text-do-not-log"
	handler := HTTPMiddleware(panicHandler(secret))
	_ = drive(handler)

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("log leaked the panic value's text; secure-logging rule forbids it. log=%s", buf.String())
	}

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v; line=%q", err, buf.String())
	}
	if rec["level"] != "ERROR" {
		t.Errorf("log level = %v, want ERROR", rec["level"])
	}
	if rec["event"] != "panic_recovered" {
		t.Errorf(`log event = %v, want "panic_recovered"`, rec["event"])
	}
	// panic("string") => panic_type "string", proving we log the type not the value.
	if rec["panic_type"] != "string" {
		t.Errorf(`log panic_type = %v, want "string"`, rec["panic_type"])
	}
	if s, ok := rec["stack"].(string); !ok || s == "" {
		t.Errorf("log stack = %v, want a non-empty stack trace string", rec["stack"])
	}
}

// TestHTTPMiddleware_NonStringPanicValues pins the full invalid-input domain:
// the middleware recovers regardless of the panic value's type, and panic_type
// reflects the concrete Go type each time.
func TestHTTPMiddleware_NonStringPanicValues(t *testing.T) {
	// Not parallel: mutates the process-global slog default.
	type custom struct{ Field int }
	cases := []struct {
		name      string
		value     any
		wantType  string
		wantPanic bool // whether next actually panics (nil panic does not)
	}{
		{"int", 42, "int", true},
		{"struct", custom{Field: 7}, "recovery.custom", true},
		{"pointer", &custom{Field: 7}, "*recovery.custom", true},
		{"error", io.EOF, "*errors.errorString", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			handler := HTTPMiddleware(panicHandler(tc.value))
			rec := drive(handler)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
			}
			var logRec map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &logRec); err != nil {
				t.Fatalf("log line is not valid JSON: %v; line=%q", err, buf.String())
			}
			if logRec["panic_type"] != tc.wantType {
				t.Errorf("panic_type = %v, want %q", logRec["panic_type"], tc.wantType)
			}
		})
	}
}

// TestHTTPMiddleware_PanicNil pins behaviour for panic(nil). Under Go 1.21+ a
// panic(nil) is promoted to a *runtime.PanicNilError, so recover() returns a
// non-nil value and the middleware still produces a 500.
func TestHTTPMiddleware_PanicNil(t *testing.T) {
	// Not parallel: mutates the process-global slog default.
	buf := captureLogs(t)
	handler := HTTPMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(nil) //nolint:govet // intentionally testing the panic(nil) edge
	}))
	rec := drive(handler)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d for panic(nil)", rec.Code, http.StatusInternalServerError)
	}
	if buf.Len() == 0 {
		t.Error("expected a panic_recovered log for panic(nil), got none")
	}
}

// TestHTTPMiddleware_PartialWriteThenPanic PINS the partial-write behaviour,
// including its known imperfection. Once a handler has committed a status (e.g.
// 200) and written body bytes, the recovery layer cannot un-send them: the
// status stays at what the handler committed, and the middleware's own
// Write(internalErrorBody) APPENDS onto the partial bytes, yielding a
// concatenated (malformed) body. We assert that EXACT body rather than just a
// prefix so the imperfection is visible and pinned, not hidden — see the
// middleware comment for why this best-effort behaviour is accepted for v1
// (atomic in-tree handlers; wrapping w would break SSE on /mcp). The middleware
// must NOT crash trying to rewrite the committed response.
func TestHTTPMiddleware_PartialWriteThenPanic(t *testing.T) {
	// Not parallel: mutates the process-global slog default.
	buf := captureLogs(t)
	const partial = "partial-body-"
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, partial)
		panic("after partial write")
	}))
	rec := drive(handler)

	// The committed status wins; we cannot rewrite an already-sent header.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (committed status cannot be rewritten)", rec.Code, http.StatusOK)
	}
	// Exact body: the handler's partial bytes followed by the appended error
	// JSON. This documents the real (imperfect) wire output on this rare path.
	if want := partial + internalErrorBody; rec.Body.String() != want {
		t.Errorf("body = %q, want %q (partial bytes with the error body appended)", rec.Body.String(), want)
	}
	// The panic is still recovered (process keeps running) and logged.
	if buf.Len() == 0 {
		t.Error("expected a panic_recovered log even after a partial write, got none")
	}
}
