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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// scrapeExemplarBearing returns the bucket lines of one histogram family that
// carry an exemplar, and fails the test rather than returning an empty slice
// when the two premises those lines rest on do not hold.
//
// The OpenMetrics Accept header is the load-bearing part: exemplars appear in
// no other representation (D4), so against a plain-text response every
// assertion built on the result would pass vacuously.
//
// A family with no bucket lines at all is likewise fatal, not "no exemplar" —
// an absence proves nothing about exemplars when nothing was recorded.
//
// Its counterpart in internal/semp/sempv2/client_exemplar_test.go is a
// deliberate duplicate: the two packages own the two observation call sites and
// cannot share a test helper without a non-test package, which would report 0%
// coverage against the 85% gate (see internal/observability/panics/panicstest).
func scrapeExemplarBearing(t *testing.T, p *metrics.Provider, family string) (withExemplar, all []string) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", `application/openmetrics-text; version=1.0.0; charset=utf-8`)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "openmetrics-text") {
		t.Fatalf("handler did not serve OpenMetrics: Content-Type = %q — exemplars appear "+
			"in no other representation, so this test would prove nothing", ct)
	}

	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, family+"_bucket{") {
			continue
		}
		all = append(all, line)
		if strings.Contains(line, " # ") {
			withExemplar = append(withExemplar, line)
		}
	}
	if len(all) == 0 {
		t.Fatalf("no %s_bucket lines in the scrape. Most likely the histogram recorded "+
			"nothing, so an absent exemplar would prove nothing — but check the exposition "+
			"too: an exporter that switched to native histograms emits no _bucket lines at "+
			"all, and the first explanation would then be the wrong one.", family)
	}
	return withExemplar, all
}

// TestCallTool_LatencyBucketCarriesTheDispatchSpansTraceID is Story 47
// (SOL-152419) for the tool RED histogram, asserted end-to-end through the
// real dispatch path rather than against the instrument in isolation.
//
// The mechanism it guards is invisible when it breaks. CallTool reassigns ctx
// from tracer.Start and the deferred recordToolInvocation observes on that
// reassigned value; swap in a bare context anywhere between the two and spans
// still export, the histogram still records, and the exemplars silently vanish
// with no error on any surface. Nothing else in the suite notices — which is
// why this asserts the trace_id equals the trace_id of the dispatch span the
// in-memory recorder captured for this same call, not merely that some
// exemplar is present.
func TestCallTool_LatencyBucketCarriesTheDispatchSpansTraceID(t *testing.T) {
	sr := recordSpans(t)
	mgr, p := newMetricsManager(t)
	mgr.Register(newStubHandler("test-tool"))

	if _, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{}); err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}

	var dispatch sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == dispatchSpanName {
			dispatch = s
		}
	}
	if dispatch == nil {
		t.Fatalf("no %q span recorded, so there is no trace for an exemplar to point at", dispatchSpanName)
	}
	if !dispatch.SpanContext().IsSampled() {
		t.Fatal("dispatch span not sampled: an exemplar can only reference a sampled trace, " +
			"so this test would assert an absence and pass for the wrong reason")
	}
	wantTraceID := dispatch.SpanContext().TraceID().String()

	withExemplar, all := scrapeExemplarBearing(t, p, "mcp_tool_invocation_duration_seconds")
	if len(withExemplar) == 0 {
		t.Fatalf("no bucket carries an exemplar, want one with trace_id %q — the observation "+
			"site is not seeing the dispatch span's context:\n%s", wantTraceID, strings.Join(all, "\n"))
	}
	for _, line := range withExemplar {
		if !strings.Contains(line, `trace_id="`+wantTraceID+`"`) {
			t.Errorf("exemplar does not carry the dispatch span's trace_id %q:\n%s", wantTraceID, line)
		}
	}
}

// TestBrokerlessDispatchSites_LatencyBucketCarriesTheDispatchSpansTraceID
// covers the two tool dispatch sites that never reach ToolManager.CallTool.
//
// They are the weakest link in this story rather than an afterthought: each
// duplicates the whole start-span-then-defer-the-emission mechanism instead of
// sharing CallTool's, so the ctx threading is a separate, independently
// breakable copy at each — and both tools appear on every dashboard, so an
// operator carrying a `tool` label into a trace backend is exactly who a
// missing exemplar strands.
//
// The fourth site, the argument-parse failure in register.go's instrumented
// closure, is pinned by TestDispatch_AuditAndMetricSeeTheDispatchSpanInContext:
// it asserts the audit line was emitted with the dispatch span in context, and
// the metric is recorded from that same ctx on the line below it.
func TestBrokerlessDispatchSites_LatencyBucketCarriesTheDispatchSpansTraceID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(t *testing.T, server *mcp.Server, tm *metrics.ToolMetrics)
		params   *mcp.CallToolParams
	}{
		{
			name: "list-brokers",
			register: func(t *testing.T, server *mcp.Server, tm *metrics.ToolMetrics) {
				RegisterListBrokers(server, newTestPool(t), tm)
			},
			params: &mcp.CallToolParams{Name: "list-brokers"},
		},
		{
			name: "describe-semp-schema",
			register: func(t *testing.T, server *mcp.Server, tm *metrics.ToolMetrics) {
				if err := RegisterDescribeSempSchema(server, specs.FS, tm); err != nil {
					t.Fatalf("RegisterDescribeSempSchema: %v", err)
				}
			},
			params: &mcp.CallToolParams{
				Name:      describeSempSchemaToolName,
				Arguments: map[string]any{"operation": "config/createMsgVpnQueue"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sr := recordSpans(t)

			p, err := metrics.New("v-test", sdkresource.Default())
			if err != nil {
				t.Fatal(err)
			}
			tm, err := p.ToolMetrics()
			if err != nil {
				t.Fatal(err)
			}

			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
			tc.register(t, server, tm)

			ctx := context.Background()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			go func() { _ = server.Run(ctx, serverTransport) }()

			client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatalf("client connect: %v", err)
			}
			defer func() { _ = session.Close() }()

			if _, err := session.CallTool(ctx, tc.params); err != nil {
				t.Fatalf("CallTool returned a protocol error: %v", err)
			}

			var dispatch sdktrace.ReadOnlySpan
			for _, s := range sr.Ended() {
				if s.Name() == dispatchSpanName {
					dispatch = s
				}
			}
			if dispatch == nil {
				t.Fatalf("no %q span recorded: this handler bypasses CallTool, so it must "+
					"start its own or there is no trace for an exemplar to point at", dispatchSpanName)
			}
			if !dispatch.SpanContext().IsSampled() {
				t.Fatal("dispatch span not sampled: an exemplar can only reference a sampled " +
					"trace, so this test would assert an absence and pass for the wrong reason")
			}
			wantTraceID := dispatch.SpanContext().TraceID().String()

			withExemplar, all := scrapeExemplarBearing(t, p, "mcp_tool_invocation_duration_seconds")
			if len(withExemplar) == 0 {
				t.Fatalf("no bucket carries an exemplar, want one with trace_id %q — this "+
					"dispatch site is not threading its own span's context into the "+
					"observation call:\n%s", wantTraceID, strings.Join(all, "\n"))
			}
			for _, line := range withExemplar {
				if !strings.Contains(line, `trace_id="`+wantTraceID+`"`) {
					t.Errorf("exemplar does not carry the dispatch span's trace_id %q:\n%s", wantTraceID, line)
				}
			}
		})
	}
}
