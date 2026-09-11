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

// SOL-154238 (Story 14 gap): TestObservabilityDocMatchesRegistry is the CI
// check docs/observability.md's own Conventions table promised ("A CI check
// that fails the build on any undocumented name or label key is planned for
// GA") and Story 14 never shipped. It lives in cmd/server, not
// internal/observability/metrics, because the live surface it checks against
// spans both packages: the metrics.Provider instruments AND the OTLP
// span-export self-observation counters, which internal/observability/tracing
// registers — internal/observability/metrics/provider_test.go's own
// TestGoldenSchema harness only ever builds a metrics.Provider, so it cannot
// see the tracing package's contribution to the same registry. This test
// assembles both, the same way cmd/server/main.go does.
//
// Maintenance note, stated because it is a real, easy-to-miss obligation: if
// a future story adds a *sixth* instrument-emitting subsystem to
// cmd/server/main.go alongside metrics and tracing, this test's harness must
// be extended to register it too, or that subsystem's metrics will read as
// "documented but missing from the registry" here — a false positive caused
// by an incomplete test harness, not real drift. Fails loud either way, but
// the fix in that case is this file, not the doc.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/panics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/tracing"
)

// readObservabilityDoc reads the real docs/observability.md from the repo
// root. cmd/server is two directories below it (cmd/server -> cmd -> root),
// hence the two ".." segments below.
func readObservabilityDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "observability.md"))
	if err != nil {
		t.Fatalf("read docs/observability.md: %v", err)
	}
	return string(raw)
}

// buildLiveRegistry assembles the same combination cmd/server/main.go does —
// a metrics.Provider plus a tracing.Provider sharing its meter provider and
// resource — and drives one sample through every instrument-emitting
// subsystem currently wired into a real server, so every family that would
// appear on a running instance's /metrics appears here too. Mirrors
// internal/observability/metrics/provider_test.go's TestGoldenSchema, plus
// the tracing self-observation counters TestGoldenSchema cannot see.
func buildLiveRegistry(t *testing.T) http.Handler {
	t.Helper()
	res := sdkresource.Default()

	mp, err := metrics.New("test-version", res)
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("metrics Provider.Shutdown: %v", err)
		}
	})

	tm, err := mp.ToolMetrics()
	if err != nil {
		t.Fatalf("ToolMetrics: %v", err)
	}
	tm.Record(context.Background(), "test-tool", "test-broker", "success", "", 5*time.Millisecond)
	tm.IncActive(context.Background())
	tm.DecActive(context.Background())

	sm, err := mp.SEMPMetrics()
	if err != nil {
		t.Fatalf("SEMPMetrics: %v", err)
	}
	sm.Record(context.Background(), metrics.SEMPRequest{
		API:       "v2",
		Broker:    "test-broker",
		Operation: "getMsgVpnQueue",
		Method:    "GET",
		Status:    "200",
		Address:   "broker.example.com",
		Attempt:   1,
	}, 5*time.Millisecond)

	sec, err := mp.SecurityMetrics()
	if err != nil {
		t.Fatalf("SecurityMetrics: %v", err)
	}
	sec.RecordAuthzDenied(context.Background(), "test-tool", "not_permitted")

	if err := panics.Register(mp.MeterProvider()); err != nil {
		t.Fatalf("panics.Register: %v", err)
	}

	if _, err := mp.BrokerMetrics(func() map[string]health.BrokerSnapshot {
		return map[string]health.BrokerSnapshot{
			"broker-a": {Current: health.StateReachable, LastResult: time.Unix(1700000000, 0)},
			"broker-b": {
				Current:     health.StateUnreachable,
				SeenReasons: []health.BrokerState{health.StateCredentialInvalid, health.StateUnreachable},
				LastResult:  time.Unix(1700000001, 0),
			},
		}
	}); err != nil {
		t.Fatalf("BrokerMetrics: %v", err)
	}

	// Tracing must be enabled for tracing.New to register anything at all —
	// with the flag off it returns (nil, nil), which is correct production
	// behavior but would make this harness silently blind to the span pair.
	tp, err := tracing.New(config.ObservabilityConfig{TracingEnabled: true}, mp.MeterProvider(), res)
	if err != nil {
		t.Fatalf("tracing.New: %v", err)
	}
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("tracing Provider.Shutdown: %v", err)
		}
	})

	return mp.Handler()
}

// liveInventory maps an mcp_* family name to its sorted, deduplicated label
// keys (the union across every series in the family).
type liveInventory map[string][]string

// scrapeLiveMCPFamilies scrapes handler's /metrics and parses it with the
// real Prometheus text-format parser rather than regexing the wire text —
// expfmt.TextParser already resolves a histogram's _bucket/_sum/_count
// series and a counter's _total suffix back to the single family name
// docs/observability.md tables use, because that name comes from the
// # TYPE line, not from any one sample.
func scrapeLiveMCPFamilies(t *testing.T, handler http.Handler) liveInventory {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse scrape body: %v", err)
	}

	out := liveInventory{}
	for name, fam := range families {
		if !strings.HasPrefix(name, "mcp_") {
			continue // go_*, process_*, target_info: not first-party schema, doc doesn't itemize them
		}
		labelSet := map[string]bool{}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				labelSet[lp.GetName()] = true
			}
		}
		var labels []string
		for l := range labelSet {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		out[name] = labels
	}
	return out
}

// activityGated names families the doc itself documents as "absent, not
// zero" until real activity occurs — unlike the Tool/SEMP/security/broker
// counters, which registerInstruments seeds with an explicit zero value the
// moment the provider is built (see TestGoldenSchema's own comments to that
// effect), the OTLP export-health counters only get their first data point
// when a span or metric export actually succeeds or fails. A fresh,
// quiescent harness — no real OTLP collector, no traffic — legitimately
// never produces one, by design (docs/observability.md, OTLP Export
// Health): "Diagnosing a broken push must not depend on the push working."
// So their absence here is not evidence the doc overstates reality, and is
// exempted from that one direction of the check. If they DO appear (a
// future harness change forces an export attempt through), the other three
// directions still apply to them normally.
var activityGated = map[string]bool{
	"mcp_otel_spans_exported_total":   true,
	"mcp_otel_spans_dropped_total":    true,
	"mcp_otel_metrics_exported_total": true,
	"mcp_otel_metrics_dropped_total":  true,
}

// TestObservabilityDocMatchesRegistry is the four-way check: every mcp_*
// family the live registry actually exposes must be documented somewhere in
// docs/observability.md's Metrics tables, and vice versa restricted to what
// the doc itself claims is currently live (its own stated default is
// "assume any other metric below is not yet emitted" — see
// observability_doc_helpers_test.go) — plus label keys must agree on names
// both sides recognize as live.
func TestObservabilityDocMatchesRegistry(t *testing.T) {
	inv, err := parseObservabilityDoc(readObservabilityDoc(t))
	if err != nil {
		t.Fatalf("parseObservabilityDoc: %v", err)
	}

	live := scrapeLiveMCPFamilies(t, buildLiveRegistry(t))

	// Doc-internal sanity check: a name cannot be claimed both live and not-yet-emitted.
	for name := range inv.liveClaimed {
		if inv.notYetClaimed[name] {
			t.Errorf("%s is listed in both the \"wired and emitted today\" and \"documented but not emitted\" paragraphs — the doc contradicts itself", name)
		}
	}

	for name, labels := range live {
		doc, documented := inv.metrics[name]
		if !documented {
			t.Errorf("%s is emitted but not documented in any docs/observability.md metric table — add a row", name)
			continue
		}
		if !inv.liveClaimed[name] {
			t.Errorf("%s is emitted, but docs/observability.md does not list it as \"wired and emitted today\" — flip its status (see the OTLP Export Health / Broker-Side Authorization Denials sections for the pattern)", name)
		}
		if !reflect.DeepEqual(labels, doc.labels) {
			t.Errorf("%s label keys = %v, but docs/observability.md documents %v", name, labels, doc.labels)
		}
	}

	for name := range inv.liveClaimed {
		if activityGated[name] {
			continue
		}
		if _, ok := live[name]; !ok {
			t.Errorf("docs/observability.md claims %s is \"wired and emitted today\", but it is absent from a live scrape — the doc overstates what the server emits (or this test's harness needs updating, see the file's package comment)", name)
		}
	}
}
