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

// Package recovery provides panic recovery for the request path. It exposes
// HTTPMiddleware, the whole-mux recover() wrapper that traps a panicking HTTP
// handler and converts it to a clean 500 (SOL-151286). Recovery is always on:
// it is a safety net with no production reason to disable, so there is no flag
// to gate it. The tool-handler goroutine is recovered separately by
// withRecovery in internal/tools (SOL-151287).
//
// It lives under internal/middleware because it changes request behaviour
// (panic -> 500); it does not merely observe.
//
// The package is named recovery (not recover) so it does not shadow the Go
// builtin recover(), which HTTPMiddleware uses.
package recovery

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// internalErrorBody is the JSON 500 response written when a panic is recovered.
// It mirrors the repo's error-body convention used by the catch-all 404 handler
// in cmd/server/main.go ({"error":"...","error_description":"..."}).
//
// It deliberately carries NO correlation_id field. The recovery middleware wraps
// the ENTIRE mux as the outermost layer, OUTSIDE correlation.Middleware.
// correlation.Middleware attaches the ID to a CHILD context via
// next.ServeHTTP(w, r.WithContext(ctx)), so the ID lives only on the downstream
// request's context — it is never visible at this outer frame. Emitting a
// correlation_id here would mean inventing one or reading "", so we omit the
// field entirely (the filed acceptance criteria do not require it in the body).
const internalErrorBody = `{"error":"internal_error","error_description":"the server encountered an unexpected condition"}`

// HTTPMiddleware wraps next with a recover() that traps a panic in ANY
// downstream handler and converts it to a clean HTTP 500, keeping the process
// running. It is installed as the OUTERMOST wrapper around the whole HTTP mux
// (outside correlation.Middleware), so it covers the standalone probe routes
// (/livez, /health, /ready and future /readyz, /metrics), the catch-all 404,
// and the authenticated /mcp chain alike (ADR-001 chain ordering).
//
// On a recovered panic it logs at ERROR with event="panic_recovered", the panic
// value's Go TYPE (panic_type) and a structured stack trace. It logs only the
// type, never the panic value's text: panic values are unaudited and can carry
// arbitrary strings, the same secure-logging rule withRecovery applies in
// internal/tools (see docs/internal/secure-logging-rules.md).
//
// Recovery is unconditional: buildRootHandler always wraps the mux with this
// middleware. There is no flag to disable it.
//
// Scope: recover() catches only panics on the request's own goroutine. A panic
// on a goroutine that a handler SPAWNS (e.g. the per-broker probe goroutines in
// the /ready handler) is NOT caught here — that class must be recovered at the
// point of goroutine creation. The SDK tool-handler goroutine is covered
// separately by withRecovery in internal/tools/register.go.
//
// http.ErrAbortHandler is exempt: net/http documents it as a sentinel meaning
// "abort this request, do not log, do not send a response". We re-raise it
// unchanged so net/http's serving goroutine applies that built-in suppression.
// The MCP streamable/SSE path on /mcp may panic with it on client-disconnect or
// shutdown.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// rw tracks whether the downstream handler committed the response (wrote
		// a header or body bytes). It implements Unwrap() so http.ResponseController
		// — which the MCP SDK's SSE path uses to flush (go-sdk mcp/streamable.go
		// and mcp/event.go call http.NewResponseController(w).Flush()) — still
		// reaches the underlying Flusher/Hijacker. Streaming on /mcp is preserved.
		rw := &recoveryWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is net/http's sentinel: re-raise it before
				// any logging or writing so net/http's serving goroutine handles
				// it (abort the request, no log, no response). Logging or writing
				// a 500 here would defeat the sentinel's contract.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				// Log the type and stack, never the value's text (secure-logging
				// rule). The stack pinpoints the panic site without echoing the
				// value.
				slog.Error("recovered panic in HTTP handler",
					slog.String("event", "panic_recovered"),
					slog.String("panic_type", fmt.Sprintf("%T", rec)),
					slog.String("stack", string(debug.Stack())))

				// TODO: increment a recovered-panic counter once the metrics
				// registry lands (the metrics story). There is no counter
				// infrastructure yet (internal/observability/metrics is a
				// flag-only skeleton), so this story implements the LOG only.

				// Best-effort 500, but only when the response is NOT yet committed.
				// Once a handler has committed a status and written body bytes the
				// response is on the wire and CANNOT be un-sent: WriteHeader would
				// be a no-op and writing internalErrorBody would APPEND it onto the
				// partial bytes, corrupting the body into malformed JSON. So when
				// rw.wroteHeader is set we deliberately write NOTHING and leave the
				// partial response as-is — the process still survives. On the common
				// path (panic before any write) we send the clean 500 below.
				if !rw.wroteHeader {
					rw.Header().Set("Content-Type", "application/json")
					rw.WriteHeader(http.StatusInternalServerError)
					_, _ = rw.Write([]byte(internalErrorBody))
				}
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

// recoveryWriter wraps an http.ResponseWriter to track whether the downstream
// handler committed the response (wrote a status header or body bytes). The
// recovery middleware uses wroteHeader to decide whether it can safely write a
// 500 (uncommitted) or must leave a partial response untouched (committed).
//
// Unwrap() exposes the underlying writer so http.ResponseController can walk to
// the real Flusher/Hijacker. The MCP SDK's SSE path flushes via
// http.NewResponseController(w).Flush(), so wrapping does NOT break streaming on
// /mcp. We intentionally do not implement Flush/Hijack explicitly — Unwrap is
// the idiomatic, sufficient mechanism for ResponseController-based consumers.
type recoveryWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *recoveryWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recoveryWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *recoveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
