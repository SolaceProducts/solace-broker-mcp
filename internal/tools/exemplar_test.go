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

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// The scrape-and-find helpers below are deliberately duplicated in the two
// packages that own a histogram observation call site (here and
// internal/semp/sempv2) rather than lifted into a shared test-support package.
// A non-test package of test helpers reports 0% coverage against the 85% gate
// (see internal/observability/panics/panicstest), and this is a dozen lines.

// scrapeExemplarBearing returns the sample lines of one metric family that
// carry an exemplar. The OpenMetrics Accept header is the load-bearing part:
// exemplars appear in no other representation, so without it this returns
// nothing and every assertion built on it passes vacuously (D4). A scrape that
// comes back as plain text therefore fails loudly here.
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
	if len(all) == 0 {
		t.Fatal("no mcp_tool_invocation_duration_seconds_bucket lines in the scrape — " +
			"the histogram recorded nothing, so an absent exemplar proves nothing")
	}
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
