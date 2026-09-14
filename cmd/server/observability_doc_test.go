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
// a future story adds a third instrument-emitting subsystem to
// cmd/server/main.go alongside metrics and tracing, buildLiveRegistry below
// must be extended to assemble it too.
//
// This does NOT always fail loud on its own, and it matters which direction
// gets it wrong. If the new subsystem's metrics are already documented as
// live (a table row plus a "wired and emitted today" mention) but this
// harness isn't updated to produce them, TestObservabilityDocMatchesRegistry
// correctly fails — "claims live, absent from scrape". But if the new
// metrics are genuinely live in a real server and genuinely undocumented,
// this harness's incompleteness makes them invisible to *both* directions of
// that diff at once: they are missing from the doc AND missing from this
// harness's own scrape, so neither side ever sees a name the other doesn't
// have, and the exact class of bug this ticket exists to catch passes
// silently. TestBuildLiveRegistry_TracksMainGoWiring below closes that gap
// mechanically rather than leaving it to whoever reads this comment: it
// parses cmd/server/main.go's AST and fails if the set of
// internal/observability/*.New(...) provider calls there ever diverges from
// what this file assumes.
package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
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
// effect), the OTLP span-export counters only get their first data point
// when a span export actually succeeds or fails. A fresh, quiescent harness
// — no real OTLP collector, no traffic — legitimately never produces one, by
// design (docs/observability.md, OTLP Export Health): "Diagnosing a broken
// push must not depend on the push working." So their absence here is not
// evidence the doc overstates reality, and is exempted from that one
// direction of the check. If they DO appear (a future harness change forces
// an export attempt through), the other three directions still apply to
// them normally.
//
// Deliberately just the span pair, not the metrics pair too. The two are not
// equivalent: the span pair has a real instrument today that simply has no
// data point yet; the metrics pair (mcp_otel_metrics_exported_total /
// _dropped_total) has no instrument anywhere in this build at all — nothing
// outside docs/ and this file's own strings reference those two names.
// Exempting an unimplemented metric from the "claimed live but absent"
// direction would make it invisible to the one check that could catch a doc
// edit that falsely claims Story 46 (SOL-152418) landed before it has.
// Add the metrics pair here in the same change that actually registers
// those two instruments (Story 46) — at that point they become genuinely
// activity-gated in the same sense the span pair is now, not before.
//
// direction 4 (label-key agreement) is consequently also skipped for the
// span pair while it's absent — but that's not an unverified gap: the
// `reason` label and its values are independently pinned by
// internal/observability/tracing/stats_test.go's TestExportStats_*
// (asserting on the OTel SDK metricdata directly), just not by a live scrape
// the way every other family here is.
var activityGated = map[string]bool{
	"mcp_otel_spans_exported_total": true,
	"mcp_otel_spans_dropped_total":  true,
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

// observabilityProviderCallers is the set of internal/observability/*
// package names buildLiveRegistry assembles today, matching the package
// comment's stated assumption. TestBuildLiveRegistry_TracksMainGoWiring
// checks this against reality instead of leaving it to a comment someone
// has to remember to read.
var observabilityProviderCallers = map[string]bool{
	"metrics": true,
	"tracing": true,
}

// observabilityProviderExclusions names internal/observability/* packages
// that do get a ".New(...)" call in cmd/server/main.go but are not
// instrument-emitting subsystems in the sense this file cares about, along
// with why each is excluded. Anything under internal/observability/ that
// gets a New(...) call in main.go and is NOT in this list must be in
// observabilityProviderCallers instead, or the AST guard below fails.
var observabilityProviderExclusions = map[string]string{
	"resource": "builds the shared identity resource.Resource both metrics.New and tracing.New are passed — it registers no instruments of its own",
}

// TestBuildLiveRegistry_TracksMainGoWiring parses cmd/server/main.go's own
// AST for "<package>.New(...)" calls into packages under
// internal/observability/, and fails if that set doesn't match
// observabilityProviderCallers exactly. Same technique as
// internal/tools/audit_error_type_drift_test.go and
// internal/observability/metrics/error_type_vocabulary_test.go — a
// hand-maintained list (here, buildLiveRegistry's own assembly) has no
// runtime way to prove it stays complete, so this proves it statically
// instead. Closes the gap the package comment above describes: without this,
// a new subsystem wired into main.go but not into buildLiveRegistry can make
// its own metrics invisible to TestObservabilityDocMatchesRegistry entirely,
// rather than failing loud.
func TestBuildLiveRegistry_TracksMainGoWiring(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "main.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Map each import alias in this file to the last path segment of its
	// import path — e.g. `"github.com/.../internal/observability/tracing"`
	// (no explicit alias) maps "tracing" -> "tracing". Only packages under
	// internal/observability/ are tracked; nothing else is relevant here.
	const obsPrefix = "github.com/SolaceProducts/solace-broker-mcp/internal/observability/"
	aliasToPkg := map[string]string{}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasPrefix(path, obsPrefix) {
			continue
		}
		pkg := path[len(obsPrefix):]
		if strings.Contains(pkg, "/") {
			continue // a sub-package of a sub-package; none exist today, and none of this file's direct callers are one
		}
		alias := pkg
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		aliasToPkg[alias] = pkg
	}

	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "New" {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg, tracked := aliasToPkg[ident.Name]; tracked {
			found[pkg] = true
		}
		return true
	})

	for pkg := range found {
		if observabilityProviderExclusions[pkg] != "" {
			continue
		}
		if !observabilityProviderCallers[pkg] {
			t.Errorf("main.go calls %s.New(...), which observabilityProviderCallers does not know about — "+
				"if %s registers Prometheus instruments, add it to buildLiveRegistry and to observabilityProviderCallers; "+
				"if it doesn't, add it (and why) to observabilityProviderExclusions instead", pkg, pkg)
		}
	}
	for pkg := range observabilityProviderCallers {
		if !found[pkg] {
			t.Errorf("observabilityProviderCallers lists %s, but main.go no longer calls %s.New(...) — "+
				"if %s was removed from main.go's wiring, remove it here and from buildLiveRegistry too", pkg, pkg, pkg)
		}
	}
}
