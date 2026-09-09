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

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
)

// scrapeMetrics renders p's /metrics surface directly, without a real HTTP
// round trip.
func scrapeMetrics(t *testing.T, p *metrics.Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/metrics", nil)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// droppedExportTimeoutCount extracts mcp_otel_metrics_dropped_total{reason=
// "export_timeout"}'s current value from a scrape body, or (0, false) if the
// series isn't there yet.
func droppedExportTimeoutCount(t *testing.T, body string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, `mcp_otel_metrics_dropped_total{`) || !strings.Contains(line, `reason="export_timeout"`) {
			continue
		}
		fields := strings.Fields(line)
		var v float64
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%f", &v); err != nil {
			t.Fatalf("parsing dropped-total line %q: %v", line, err)
		}
		return v, true
	}
	return 0, false
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestOTLPMetricsUnreachableCollector_DoesNotDegradeToolCallLatency is
// SOL-152418 (Story 46)'s own AC: "An unreachable collector must not degrade
// service ... never blocks a tool call and never fails a request. Verified
// by an integration test against a black-hole endpoint asserting tool-call
// latency is unaffected."
//
// This composes the real broker pool, composite executor, ToolManager, and
// metrics.Provider with OBS_METRICS_OTLP_ENABLED pointed at an address
// nothing listens on — the property is meaningless at any single
// component's level: internal/observability/metrics's own unit tests already
// prove ForceFlush/Shutdown are bounded, but only a call through
// ToolManager.CallTool proves the recording call on the request path
// (ToolMetrics.Record, an in-memory OTel aggregation write) truly never
// waits on the same network I/O the periodic OTLP push does on its own,
// decoupled background goroutine.
//
// The timed loop only starts once a real stuck export is already
// confirmed in flight (the wait on mcp_otel_metrics_dropped_total below),
// and the same counter must keep advancing across the loop — otherwise the
// SDK's default 60s collection interval would let this test's own
// sub-second run finish before the reader ever attempts its first export,
// proving nothing about the condition the AC actually describes.
func TestOTLPMetricsUnreachableCollector_DoesNotDegradeToolCallLatency(t *testing.T) {
	// 192.0.2.0/24 (TEST-NET-1, RFC 5737) is reserved for documentation and
	// never routes — the same black hole
	// internal/observability/metrics/otlp_test.go uses, so a connection
	// attempt hangs rather than failing fast with connection-refused, the
	// closer analogue of a collector behind a misconfigured egress rule.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://192.0.2.1:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	// Real collectors are typically much slower to notice than this to keep
	// the test itself fast; shortening it does not change what is being
	// proven — the tool-call path must never wait on this timeout at all.
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "300")
	// The SDK's default 60s collection interval means the reader's first
	// export attempt would never happen inside this test's own (sub-second)
	// run — proven by measurement, not assumed: the loop below finishes in
	// ~15ms. Without this, the test passes whether or not a stuck export is
	// actually in flight while it runs, which is the one condition the AC
	// asks it to cover. Shortened so a real stuck export exists to measure
	// against before the timed loop starts.
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "10")

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer broker.Close()

	// request_min_interval: 0 disables the default 100ms per-broker pacing
	// (SOL-153441/153442) — irrelevant to what this test measures, but at the
	// default it would throttle every other one of the 20 rapid-fire calls
	// below to ~100ms regardless of OTLP, swamping the ceiling this test
	// actually cares about.
	cfgYAML := fmt.Sprintf("mcp_client_auth:\n  mode: disabled\nsemp:\n  request_min_interval: 0s\nbrokers:\n  dev:\n    url: %s\n    auth:\n      mode: basic\n      username: admin\n      password: admin\n", broker.URL)
	cfgPath := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	operations := map[string]*sempv2.Operation{
		"monitor/getTestObject": {
			ID:     "getTestObject",
			Method: http.MethodGet,
			Path:   "/SEMP/v2/monitor/__private_test__/testObject",
		},
	}
	tool := composite.CompositeTool{
		Name:        "get-test-object",
		Description: "fixture: a fast, always-succeeding read-only composite tool",
		Steps: []composite.Step{{
			ID:        "get",
			Operation: "monitor/getTestObject",
		}},
		Result: composite.ResultStrategy{Strategy: "collect"},
	}
	executor := composite.NewCompositeExecutor(operations)

	mp, err := metrics.New("v1.2.3-test", sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mp.Shutdown(ctx)
	}()
	tm, err := mp.ToolMetrics()
	if err != nil {
		t.Fatalf("ToolMetrics: %v", err)
	}

	mgr := tools.NewToolManagerFromComposite(pool, []composite.CompositeTool{tool}, executor, tools.WithToolMetrics(tm))

	// Wait for a real stuck export to exist before measuring anything: with
	// the 10ms interval above, the reader should already be failing against
	// the black hole in the background by the time this returns. Without
	// this wait, the timed loop below could still finish before the
	// reader's first attempt, proving nothing about the condition the AC
	// actually asks about.
	if !waitForCondition(t, 5*time.Second, func() bool {
		n, found := droppedExportTimeoutCount(t, scrapeMetrics(t, mp))
		return found && n > 0
	}) {
		t.Fatal("mcp_otel_metrics_dropped_total{reason=\"export_timeout\"} never appeared within 5s; " +
			"the black hole never produced a stuck export to measure against")
	}
	droppedBefore, _ := droppedExportTimeoutCount(t, scrapeMetrics(t, mp))

	// A generous per-call ceiling: this is not a tight performance budget,
	// it is the "did this silently start waiting on OTLP_METRIC_EXPORT
	// network I/O" tripwire — any of the black hole's timeouts above (300ms
	// export, or the multi-second TCP-level hang the raw address itself
	// produces) would blow this by at least 10x.
	const perCallCeiling = 100 * time.Millisecond
	const calls = 20
	for i := range calls {
		start := time.Now()
		result, callErr := mgr.CallTool(context.Background(), "get-test-object", map[string]any{"broker": "dev"}, tools.Identity{})
		elapsed := time.Since(start)
		if callErr != nil {
			t.Fatalf("call %d: CallTool: %v", i, callErr)
		}
		if result.IsError {
			t.Fatalf("call %d: unexpected tool error: %+v", i, result.StructuredContent)
		}
		if elapsed > perCallCeiling {
			t.Errorf("call %d took %s with an unreachable OTLP collector, want under %s — the request path may be waiting on OTLP export", i, elapsed, perCallCeiling)
		}
	}

	// Confirms the black hole was still actively failing exports through and
	// past the loop above, not just once before it started. The per-attempt
	// timeout (300ms) is longer than the whole 20-call loop (~microseconds
	// each), so the counter is not expected to have advanced again the
	// instant the loop ends — waited for, not asserted immediately, exactly
	// so this doesn't overstate what a single fixed-size loop can prove.
	if !waitForCondition(t, 2*time.Second, func() bool {
		n, found := droppedExportTimeoutCount(t, scrapeMetrics(t, mp))
		return found && n > droppedBefore
	}) {
		t.Error("dropped export_timeout count never advanced past its pre-loop value within 2s; " +
			"the collector may have stopped being unreachable partway through")
	}
}
