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
// correlation_id here would mean inventing one or reading "" , so we omit the
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
// Callers gate installation on Enabled (OBS_PANIC_RECOVERY_ENABLED). When the
// capability is off the middleware is not wired and a panic propagates to
// net/http's per-connection recovery (today's behaviour).
//
// Scope: recover() catches only panics on the request's own goroutine. A panic
// on a goroutine that a handler SPAWNS (e.g. the per-broker probe goroutines in
// the /ready handler) is NOT caught here — that class must be recovered at the
// point of goroutine creation. The SDK tool-handler goroutine is covered
// separately by withRecovery in internal/tools/register.go.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// Log the type and stack, never the value's text (secure-logging
				// rule). The stack pinpoints the panic site without echoing the
				// value.
				slog.Error("recovered panic in HTTP handler",
					slog.String("event", "panic_recovered"),
					slog.String("panic_type", fmt.Sprintf("%T", rec)),
					slog.String("stack", string(debug.Stack())))

				// TODO(SOL-151...metrics, Story 15): increment a recovered-panic
				// counter once the metrics registry lands. There is no counter
				// infrastructure yet (internal/observability/metrics is a
				// flag-only skeleton), so this story implements the LOG only.

				// Best-effort 500. If the downstream handler already committed a
				// status and wrote body bytes before panicking, the response is
				// already on the wire and CANNOT be un-sent: WriteHeader becomes a
				// no-op (net/http logs "superfluous WriteHeader") AND the Write
				// below APPENDS internalErrorBody onto the partial bytes the
				// handler already sent, producing a concatenated (malformed) body.
				// We accept this best-effort behaviour for v1: (a) the in-tree
				// handlers — the probes and the SDK /mcp handler — write their
				// response atomically, so a panic after a partial write is not
				// reached in practice; and (b) the alternative, wrapping w in a
				// ResponseWriter that tracks commitment to suppress the append,
				// would defeat the http.Flusher type-assertion the MCP streamable
				// HTTP (SSE) path on /mcp depends on, breaking streaming. Keeping
				// the process alive is the contract that matters; on the common
				// path (panic before any write) the clean 500 below is correct.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(internalErrorBody))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
