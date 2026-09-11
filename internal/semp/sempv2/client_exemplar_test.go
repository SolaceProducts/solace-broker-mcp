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

package sempv2_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// sempDurationFamily is the published SEMP latency histogram Story 47 attaches
// exemplars to.
const sempDurationFamily = "mcp_semp_request_duration_seconds"

// openMetricsAccept is the header that selects the only exposition carrying
// exemplars (D4). Sending it is load-bearing rather than incidental: against a
// plain-text response every exemplar assertion here would pass vacuously.
const openMetricsAccept = `application/openmetrics-text; version=1.0.0; charset=utf-8`

// scrapeExemplarBearing returns the bucket lines of one histogram family that
// carry an exemplar, and fails the test rather than returning an empty slice
// when a premise those lines rest on does not hold — a scrape that came back
// as plain text, or a family with no bucket lines at all (an absence proves
// nothing about exemplars when nothing was recorded).
//
// Its counterpart in internal/tools/exemplar_test.go is a deliberate
// duplicate: the two packages own the two observation call sites and cannot
// share a test helper without a non-test package, which would report 0%
// coverage against the 85% gate (see internal/observability/panics/panicstest).
// The scrape itself is NOT duplicated — that comes from this package's own
// scrapeMetricsAccepting.
func scrapeExemplarBearing(t *testing.T, p *metrics.Provider, family string) (withExemplar, all []string) {
	t.Helper()

	rec := scrapeMetricsAccepting(t, p, openMetricsAccept)
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

// TestExecute_LatencyBucketCarriesTheRequestSpansTraceID is Story 47
// (SOL-152419) for the SEMP RED histogram, asserted end-to-end from
// Execute down to the transport that observes the histogram.
//
// The chain under test is longer than it looks, and every hop can drop the
// span context without failing anything: Execute starts `semp.request` and
// reassigns ctx, buildRequest attaches that ctx to the request, Sender.Do
// derives the retry-budget context from it and re-attaches, retryablehttp
// copies the request per attempt, and only then does metricsTransport observe
// the histogram on req.Context(). A bare context introduced at any of those
// hops still yields working spans and a correct histogram — it yields silently
// zero exemplars.
//
// The retry is the point of the 503-then-200 handler rather than test padding.
// retryablehttp clones the request between attempts as a shallow struct copy
// (client.go: `httpreq := *req.Request`), which carries the unexported ctx
// field along with everything else. A future dependency bump that rebuilt the
// request instead would drop the span context on every attempt after the
// first, and only a case that actually retries would notice. Both attempts
// land on distinct series (status 503 and 200), and both must carry the same
// trace.
func TestExecute_LatencyBucketCarriesTheRequestSpansTraceID(t *testing.T) {
	sr := recordSpans(t)

	prov, err := metrics.New("vtest", sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sm, err := prov.SEMPMetrics()
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	client, server := newMetricsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 on the first try
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}, 1, sm)
	defer server.Close()

	if _, err := client.Execute(context.Background(), testQueueOp(t), testQueueArgs); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("server saw %d attempts, want 2 — the retry this test relies on did not happen", got)
	}

	var request sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "semp.request" {
			request = s
		}
	}
	if request == nil {
		t.Fatal("no semp.request span recorded, so there is no trace for an exemplar to point at")
	}
	if !request.SpanContext().IsSampled() {
		t.Fatal("semp.request span not sampled: an exemplar can only reference a sampled trace, " +
			"so this test would assert an absence and pass for the wrong reason")
	}
	wantTraceID := request.SpanContext().TraceID().String()

	withExemplar, all := scrapeExemplarBearing(t, prov, sempDurationFamily)

	// One exemplar per bucket, and each attempt landed in one bucket of its own
	// series, so both attempts must be represented.
	if len(withExemplar) < 2 {
		t.Fatalf("%d bucket(s) carry an exemplar, want one per attempt (2) with trace_id %q — "+
			"a retried attempt is not seeing the request span's context:\n%s",
			len(withExemplar), wantTraceID, strings.Join(all, "\n"))
	}
	for _, line := range withExemplar {
		if !strings.Contains(line, `trace_id="`+wantTraceID+`"`) {
			t.Errorf("exemplar does not carry the semp.request span's trace_id %q:\n%s", wantTraceID, line)
		}
	}

	// Both attempts, not the same one twice: the two SEMP histogram series
	// differ by http_response_status_code, and an assertion that only ever saw
	// the successful attempt would miss a context dropped on retry.
	for _, status := range []string{`http_response_status_code="503"`, `http_response_status_code="200"`} {
		found := false
		for _, line := range withExemplar {
			if strings.Contains(line, status) {
				found = true
			}
		}
		if !found {
			t.Errorf("no exemplar on the %s series:\n%s", status, strings.Join(withExemplar, "\n"))
		}
	}
}
