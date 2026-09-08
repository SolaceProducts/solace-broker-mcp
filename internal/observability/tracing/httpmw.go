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

package tracing

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// HTTPMiddleware wraps an HTTP handler so every request gets an entry span at
// the server's edge — the outermost span of a request trace, which the
// dispatcher, executor, and SEMP spans below all descend from (SOL-152421).
//
// otelhttp runs the global TextMapPropagator (installed by New) over the
// inbound headers, so a request carrying a W3C traceparent continues the
// caller's trace and one without starts a fresh root. Both directions are
// pinned by test: a missing propagator just yields roots forever, silently.
//
// Composed only when OBS_TRACING_ENABLED is on (cmd/server/main.go), so with
// tracing off no span is created at all.
//
// route is a compile-time constant from the caller, never request-derived, so
// it cannot be a cardinality or injection vector.
func HTTPMiddleware(route string, next http.Handler) http.Handler {
	stamped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// otelhttp has started the entry span by now, and correlation.Middleware
		// sits outside this one, so the ID is available here. This attribute is
		// the join key from the trace to the logs and audit records for the same
		// request. No-op when correlation is off.
		if id := correlation.From(r.Context()); id != "" {
			trace.SpanFromContext(r.Context()).SetAttributes(
				attribute.String("correlation_id", id))
		}
		next.ServeHTTP(w, r)
	})

	return otelhttp.NewHandler(stamped, route,
		// Skip the SSE notification stream. On /mcp a GET opens the
		// server-to-client channel and stays open for the whole session, so
		// otelhttp would hold one span open for hours: unexported until the
		// session ends, lost entirely if the process dies, and a duration that
		// swamps any latency view built on entry-span duration. MCP requests
		// travel over POST, which is what this traces; a DELETE teardown is
		// short-lived and still traced.
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.Method != http.MethodGet
		}),
		otelhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
			// OTel convention is "{method} {route}"; otelhttp's own default
			// names every span after `operation` alone, collapsing GET and
			// POST on one route into a single name.
			return r.Method + " " + operation
		}),
	)
}
