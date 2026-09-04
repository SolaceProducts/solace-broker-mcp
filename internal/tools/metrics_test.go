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

package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// newMetricsManager builds a ToolManager wired to a real metrics provider and
// returns both so a test can drive invocations and scrape the result.
func newMetricsManager(t *testing.T) (*ToolManager, *metrics.Provider) {
	t.Helper()
	p, err := metrics.New("v-test", sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewToolManager(newTestPool(t))
	mgr.metrics = tm
	return mgr, p
}

// activeRequests scrapes the provider and returns the current value of the
// unlabelled mcp_http_active_requests gauge and whether the series was present.
func activeRequests(t *testing.T, p *metrics.Provider) (float64, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "mcp_http_active_requests "); ok {
			var v float64
			if _, err := fmt.Sscanf(rest, "%g", &v); err != nil {
				t.Fatalf("parse gauge value from %q: %v", line, err)
			}
			return v, true
		}
	}
	return 0, false
}

// TestCallTool_PanicRecordsErrorMetric verifies the recovered-panic path records
// the invocation counter with outcome="error" and error_type="panic" — a panic
// is an error distinguished by its cause, never its own outcome value.
func TestCallTool_PanicRecordsErrorMetric(t *testing.T) {
	mgr, p := newMetricsManager(t)
	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		panic("boom")
	}
	mgr.Register(handler)

	// CallTool re-panics after its deferred recording runs (the recover lives in
	// the registration wrapper), so swallow it. The metric is recorded during
	// the deferred unwind, before the re-panic.
	func() {
		defer func() { _ = recover() }()
		_, _ = mgr.CallTool(context.Background(), "test-tool", map[string]any{
			"broker":     "dev",
			"msgVpnName": "default",
		}, Identity{})
	}()

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))

	const want = `mcp_tool_invocation_total{broker="dev",error_type="panic",outcome="error",tool="test-tool"} 1`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("scrape missing panic series.\nwant line: %s\n--- got ---\n%s", want, rec.Body.String())
	}
}

// TestCallTool_UnknownBrokerMetricLabelCanonicalized proves the broker metric
// label is bounded on the error path: an unconfigured alias records the fixed
// "unknown" sentinel, not the raw caller-supplied string, while the raw alias
// still reaches the user-facing error result.
func TestCallTool_UnknownBrokerMetricLabelCanonicalized(t *testing.T) {
	mgr, p := newMetricsManager(t)
	mgr.Register(newStubHandler("test-tool"))

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "Nope",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "Nope") {
		t.Errorf("user-facing error dropped the raw alias; got: %s", text)
	}

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	const want = `mcp_tool_invocation_total{broker="unknown",error_type="unknown_broker",outcome="error",tool="test-tool"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("metric missing canonical broker label.\nwant line: %s\n--- got ---\n%s", want, body)
	}
	if strings.Contains(body, `broker="Nope"`) {
		t.Errorf("raw caller alias leaked into the metric label:\n%s", body)
	}
}

// TestActiveRequestsMiddleware_IncDecAroundRequest verifies the gauge reads 1
// while the wrapped handler runs and returns to 0 once it completes.
func TestActiveRequestsMiddleware_IncDecAroundRequest(t *testing.T) {
	_, p := newMetricsManager(t)
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	var during float64
	var seen bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		during, seen = activeRequests(t, p) // read while "in flight"
	})
	ActiveRequestsMiddleware(tm, next).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil))

	if !seen || during != 1 {
		t.Errorf("gauge during request = %v (seen=%v), want 1", during, seen)
	}
	if after, _ := activeRequests(t, p); after != 0 {
		t.Errorf("gauge after request = %v, want 0", after)
	}
}

// TestActiveRequestsMiddleware_PanicStillDecrements verifies the deferred
// decrement runs even when the wrapped handler panics. The gauge is asserted to
// read 1 inside the handler before it panics, so a middleware that incremented
// nothing would fail here rather than pass vacuously.
func TestActiveRequestsMiddleware_PanicStillDecrements(t *testing.T) {
	_, p := newMetricsManager(t)
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	var during float64
	var seen bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		during, seen = activeRequests(t, p)
		panic("boom")
	})

	func() {
		defer func() { _ = recover() }()
		ActiveRequestsMiddleware(tm, next).ServeHTTP(
			httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil))
	}()

	if !seen || during != 1 {
		t.Errorf("gauge inside panicking handler = %v (seen=%v), want 1", during, seen)
	}
	if after, _ := activeRequests(t, p); after != 0 {
		t.Errorf("gauge after panicking request = %v, want 0", after)
	}
}

// TestActiveRequestsMiddleware_DisabledNoOp verifies the default disabled path:
// a nil recorder makes the middleware a transparent pass-through with no panic
// and no gauge writes.
func TestActiveRequestsMiddleware_DisabledNoOp(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	ActiveRequestsMiddleware(nil, next).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil))

	if !called {
		t.Error("wrapped handler was not called through the disabled middleware")
	}
}
