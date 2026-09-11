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

// Where the tracing middleware sits in the assembled /mcp chain (SOL-152421).
//
// These drive the real buildMCPEndpoint rather than a hand-rebuilt copy, like
// the correlation and cross-origin wiring tests beside them: the property under
// test IS the composition, and a test that re-assembles the layers itself keeps
// passing after main() stops composing them that way.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// spanRecorder is the innermost handler: it captures what is visible where the
// request reaches the MCP SDK, which is what the layer order determines.
type spanRecorder struct {
	invoked  bool
	spanCtx  trace.SpanContext
	corrID   string
	recorded bool
}

func (s *spanRecorder) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	s.invoked = true
	s.corrID = correlation.From(r.Context())
	span := trace.SpanFromContext(r.Context())
	s.spanCtx = span.SpanContext()
	s.recorded = span.IsRecording()
}

// recordChainSpans installs a fresh always-sampling provider for this test and
// restores the previous one when it ends.
//
// Unlike test/integration/request_path_spans_test.go, this installs per test
// rather than once per binary. It can, because the only spans here come from
// otelhttp, whose handler buildMCPEndpoint constructs inside each test and
// which therefore resolves whichever provider is current at that moment — none
// of the package-init-bound tracer handles that force the shared-forwarder
// pattern elsewhere are in play. It also must, because resource_wiring_test.go
// in this same package calls tracing.New, which replaces the global provider:
// under a one-shot install this test's provider is clobbered on every run
// after the first (`go test -count=2`) and the recorder sees nothing.
func recordChainSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// Both halves of the flag: composed when on, absent when off. The off case is
// AC 6 at the chain level — no span at the boundary even though this binary has
// a recording provider installed.
func TestBuildMCPEndpoint_TracingLayerPresentOnlyWhenEnabled(t *testing.T) {
	for _, tt := range []struct {
		name           string
		tracingEnabled bool
		wantSpan       bool
	}{
		{name: "tracing on composes the layer", tracingEnabled: true, wantSpan: true},
		{name: "tracing off omits the layer", tracingEnabled: false, wantSpan: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sr := recordChainSpans(t)
			rec := &spanRecorder{}
			endpoint := buildMCPEndpoint(rec, true, tt.tracingEnabled, nil)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			endpoint.ServeHTTP(httptest.NewRecorder(), req)

			if !rec.invoked {
				t.Fatal("inner handler was not invoked; the chain short-circuited unexpectedly")
			}
			if got := rec.spanCtx.IsValid(); got != tt.wantSpan {
				t.Errorf("span visible at the inner handler = %v, want %v", got, tt.wantSpan)
			}
			if tt.wantSpan {
				if !rec.recorded {
					t.Error("span at the inner handler is not recording")
				}
				if len(sr.Ended()) == 0 {
					t.Error("no span was ended for the request")
				}
				return
			}
			if len(sr.Ended()) != 0 {
				t.Errorf("spans were ended with tracing off: %d", len(sr.Ended()))
			}
		})
	}
}

// First half of the documented order. The entry span stamps correlation_id —
// the join key across trace, logs and audit — so correlation must have run
// already. Composed the other way round the span carries no ID, silently.
func TestBuildMCPEndpoint_TracingSitsInsideCorrelation(t *testing.T) {
	sr := recordChainSpans(t)
	rec := &spanRecorder{}
	endpoint := buildMCPEndpoint(rec, true, true, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	endpoint.ServeHTTP(httptest.NewRecorder(), req)

	if rec.corrID == "" {
		t.Fatal("no correlation ID at the inner handler; the correlation layer did not run")
	}
	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	var got string
	for _, kv := range ended[0].Attributes() {
		if kv.Key == "correlation_id" {
			got = kv.Value.AsString()
		}
	}
	if got != rec.corrID {
		t.Errorf("span correlation_id = %q, want the request's ID %q — tracing must be composed INSIDE correlation",
			got, rec.corrID)
	}
}

// Second half. A rejected cross-origin POST never reaches the inner handler,
// but it is still a 403 this server answered, and an operator diagnosing "my
// client gets 403s" should find it in the trace. Composed inside cross-origin
// protection it would produce no span at all.
func TestBuildMCPEndpoint_TracingSitsOutsideCrossOrigin(t *testing.T) {
	sr := recordChainSpans(t)
	rec := &spanRecorder{}
	endpoint := buildMCPEndpoint(rec, true, true, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp := httptest.NewRecorder()
	endpoint.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (the request should have been rejected)", resp.Code)
	}
	if rec.invoked {
		t.Fatal("inner handler ran on a cross-origin request")
	}
	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1: a 403 origin rejection must still be traced", len(ended))
	}
	if name := ended[0].Name(); name != "POST /mcp" {
		t.Errorf("span name = %q, want %q", name, "POST /mcp")
	}
}

// Adding the tracing layer did not move the 413 boundary: limitRequestBody is
// outermost and rejects before correlation (documented as deliberate), and
// tracing sits inside correlation, so an oversized request gets no span
// either. Asserted so a later reshuffle has to acknowledge it.
func TestBuildMCPEndpoint_BodyLimitStillShortCircuitsOutsideTracing(t *testing.T) {
	sr := recordChainSpans(t)
	rec := &spanRecorder{}
	endpoint := buildMCPEndpoint(rec, true, true, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.ContentLength = 1 << 30
	resp := httptest.NewRecorder()
	endpoint.ServeHTTP(resp, req)

	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.Code)
	}
	if len(sr.Ended()) != 0 {
		t.Errorf("ended spans = %d, want 0: the body-limit short-circuit sits outside tracing", len(sr.Ended()))
	}
}

// activeRequestsSeries scrapes p and reports the value of the unlabelled
// mcp_http_active_requests gauge and whether the series exists at all. The
// presence flag is what makes the assertion below possible: the gauge is an
// UpDownCounter that nets back to zero once a request finishes, so the value
// alone cannot distinguish "counted, then decremented" from "never counted" —
// but the series is only published after a first write.
func activeRequestsSeries(t *testing.T, p *metrics.Provider) (float64, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
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

// newChainToolMetrics builds a real metrics provider so the assembled chain can
// be scraped, rather than passing the nil (disabled) recorder the other tests
// here use.
func newChainToolMetrics(t *testing.T) (*metrics.ToolMetrics, *metrics.Provider) {
	t.Helper()
	p, err := metrics.New("v-test", sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return tm, p
}

// The last of the documented order (SOL-152421 AC 7): the tool-RED
// active-requests gauge is composed OUTSIDE limitRequestBody, so it counts
// every /mcp request the server answered — including the ones rejected before
// any handler runs. That is the whole point of an in-flight gauge: a client
// hammering the server with oversized bodies is load, and a gauge that only
// counted requests which got past the body limit would read zero through
// exactly the incident an operator is trying to see.
//
// Asserted through series presence rather than value, since the gauge is back
// to zero by the time the request returns — see activeRequestsSeries.
func TestBuildMCPEndpoint_ToolREDCountsRequestsRejectedByBodyLimit(t *testing.T) {
	tm, p := newChainToolMetrics(t)
	rec := &spanRecorder{}
	endpoint := buildMCPEndpoint(rec, true, false, tm)

	if _, present := activeRequestsSeries(t, p); present {
		t.Fatal("gauge series exists before any request; the presence check below proves nothing")
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.ContentLength = 1 << 30
	resp := httptest.NewRecorder()
	endpoint.ServeHTTP(resp, req)

	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.Code)
	}
	if rec.invoked {
		t.Fatal("inner handler ran on an oversized request")
	}
	value, present := activeRequestsSeries(t, p)
	if !present {
		t.Fatal("no mcp_http_active_requests series after a 413: the active-requests gauge is composed INSIDE limitRequestBody, so it never saw the request")
	}
	if value != 0 {
		t.Errorf("gauge = %v after the request completed, want 0 (the deferred decrement must always run)", value)
	}
}

// The same layer, on the other side of the chain: a cross-origin 403 is
// rejected inside the chain rather than at its edge, and must also be counted.
// Together with the test above this brackets the gauge's position from both
// directions, so moving it anywhere other than outermost fails one of them.
func TestBuildMCPEndpoint_ToolREDCountsCrossOriginRejections(t *testing.T) {
	tm, p := newChainToolMetrics(t)
	rec := &spanRecorder{}
	endpoint := buildMCPEndpoint(rec, true, false, tm)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp := httptest.NewRecorder()
	endpoint.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.Code)
	}
	if _, present := activeRequestsSeries(t, p); !present {
		t.Fatal("no mcp_http_active_requests series after a 403; the gauge must sit outside cross-origin protection")
	}
}
