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
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// TestCallTool_PanicRecordsErrorMetric verifies the recovered-panic path records
// the invocation counter with outcome="error" and error_type="panic" — a panic
// is an error distinguished by its cause, never its own outcome value.
func TestCallTool_PanicRecordsErrorMetric(t *testing.T) {
	p, err := metrics.New("v-test", sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	SetToolMetrics(tm)
	t.Cleanup(func() { SetToolMetrics(nil) })

	mgr := NewToolManager(newTestPool(t))
	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		panic("boom")
	}
	mgr.Register(handler)

	// CallTool re-panics after its deferred logToolResult runs (the recover
	// lives in the registration wrapper, not here), so swallow it. The metric
	// is recorded during the deferred unwind, before the re-panic.
	func() {
		defer func() { _ = recover() }()
		_, _ = mgr.CallTool(context.Background(), "test-tool", map[string]any{
			"broker":     "dev",
			"msgVpnName": "default",
		}, Identity{})
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	p.Handler().ServeHTTP(rec, req)

	const want = `mcp_tool_invocation_total{broker="dev",error_type="panic",outcome="error",tool="test-tool"} 1`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("scrape missing panic series.\nwant line: %s\n--- got ---\n%s", want, rec.Body.String())
	}
}

// activeRequests scrapes the provider and returns the current value of the
// unlabelled mcp_http_active_requests gauge, or 0 if the series is absent.
func activeRequests(t *testing.T, p *metrics.Provider) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "mcp_http_active_requests "); ok {
			var v float64
			if _, err := fmt.Sscanf(rest, "%g", &v); err != nil {
				t.Fatalf("parse gauge value from %q: %v", line, err)
			}
			return v
		}
	}
	return 0
}

// TestActiveRequestsMiddleware_IncDecAroundRequest verifies the gauge reads 1
// while the wrapped handler runs and returns to 0 once it completes.
func TestActiveRequestsMiddleware_IncDecAroundRequest(t *testing.T) {
	p, err := metrics.New("v-test", sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	SetToolMetrics(tm)
	t.Cleanup(func() { SetToolMetrics(nil) })

	var during float64
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		during = activeRequests(t, p) // read while "in flight"
	})
	ActiveRequestsMiddleware(next).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil))

	if during != 1 {
		t.Errorf("gauge during request = %v, want 1", during)
	}
	if after := activeRequests(t, p); after != 0 {
		t.Errorf("gauge after request = %v, want 0", after)
	}
}

// TestActiveRequestsMiddleware_PanicStillDecrements verifies the deferred
// decrement runs even when the wrapped handler panics, so a crashing request
// cannot leak an in-flight count.
func TestActiveRequestsMiddleware_PanicStillDecrements(t *testing.T) {
	p, err := metrics.New("v-test", sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	SetToolMetrics(tm)
	t.Cleanup(func() { SetToolMetrics(nil) })

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	func() {
		defer func() { _ = recover() }()
		ActiveRequestsMiddleware(next).ServeHTTP(
			httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil))
	}()

	if after := activeRequests(t, p); after != 0 {
		t.Errorf("gauge after panicking request = %v, want 0", after)
	}
}
