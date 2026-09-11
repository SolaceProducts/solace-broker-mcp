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

package metrics

import (
	"bufio"
	"context"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/panics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

// update regenerates the golden fixture. Regenerating is a deliberate,
// reviewable act: run `go test ./internal/observability/metrics/ -update`.
var update = flag.Bool("update", false, "regenerate golden files")

const testVersion = "v1.2.3-test"

// scrapePlainText does one plain-text (non-OpenMetrics) scrape and returns the
// body. No OpenMetrics Accept header: exemplars carry non-deterministic trace
// IDs, so the golden file pins the plain-text representation only.
func scrapePlainText(t *testing.T, p *Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/metrics", nil)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// mcpFamilies keeps only the mcp_* HELP/TYPE/series lines, dropping the
// environment-dependent go_*/process_*/target_info output so the fixture is
// deterministic.
func mcpFamilies(body string) string {
	var b strings.Builder
	s := bufio.NewScanner(strings.NewReader(body))
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "mcp_") ||
			strings.HasPrefix(line, "# HELP mcp_") ||
			strings.HasPrefix(line, "# TYPE mcp_") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestGoldenSchema pins the published mcp_* schema. A version bump that changes
// rendering fails here before it can break a customer dashboard.
//
// mcp_panic_recovered_total (SOL-154037) is registered against this provider so
// its published shape is pinned here too. Registering is enough: panics.Register
// seeds both boundary series at zero, so a healthy process exposes them without
// ever having panicked. That is what the fixture pins — the OTel-to-Prometheus
// rendering of mcp.panic.recovered into mcp_panic_recovered_total, the counter
// type, both boundary label values, and the HELP text. The instrument lives in
// internal/observability/panics because its call sites reach it as process state
// rather than through a provider; the golden file is still the contract for how
// it appears on the wire.
//
// mcp_broker_authz_denied_total (SOL-153332, Story 49) is a ToolMetrics
// instrument like the tool-RED counters above, so it is seeded here the same
// way: one fixed sample.
func TestGoldenSchema(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// The tool-RED instruments and the in-flight gauge only render after they
	// are observed, so drive one deterministic sample of each. Fixed labels and
	// a fixed 5ms duration keep the fixture stable. The gauge is incremented and
	// decremented to surface its series at a resting value of 0.
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}
	tm.Record(context.Background(), "test-tool", "test-broker", "success", "", 5*time.Millisecond)
	tm.IncActive(context.Background())
	tm.DecActive(context.Background())
	// mcp_broker_authz_denied_total (SOL-153332, Story 49): same
	// only-renders-after-observed rule as the tool-RED instruments above, so a
	// fixed sample seeds it into the fixture.
	tm.RecordBrokerAuthzDenied(context.Background(), "test-tool", "test-broker", "permission_denied")

	// One fixed SEMP sample so the two mcp_semp_request families render. Fixed
	// labels and a 5ms duration keep the fixture stable.
	sm, err := p.SEMPMetrics()
	if err != nil {
		t.Fatal(err)
	}
	sm.Record(context.Background(), SEMPRequest{
		API:       "v2",
		Broker:    "test-broker",
		Operation: "getMsgVpnQueue",
		Method:    "GET",
		Status:    "200",
		Address:   "broker.example.com",
		Attempt:   1,
	}, 5*time.Millisecond)

	// Security counters (SOL-152099): registration seeds the auth-failure
	// series; one denial surfaces the unseeded authz family.
	sec, err := p.SecurityMetrics()
	if err != nil {
		t.Fatal(err)
	}
	sec.RecordAuthzDenied(context.Background(), "test-tool", "not_permitted")

	if err := panics.Register(p.MeterProvider()); err != nil {
		t.Fatalf("panics.Register() error = %v", err)
	}

	// Broker reachability gauges (SOL-152088): seed one reachable and one failed
	// broker with fixed timestamps so the one-hot 0 series is captured in the golden.
	if _, err := p.BrokerMetrics(func() map[string]health.BrokerSnapshot {
		return map[string]health.BrokerSnapshot{
			"broker-a": {
				Current:    health.StateReachable,
				LastResult: time.Unix(1700000000, 0),
			},
			"broker-b": {
				Current:     health.StateUnreachable,
				SeenReasons: []health.BrokerState{health.StateCredentialInvalid, health.StateUnreachable},
				LastResult:  time.Unix(1700000001, 0),
			},
		}
	}); err != nil {
		t.Fatalf("BrokerMetrics() error = %v", err)
	}

	// Token-exchange circuit-breaker state (SOL-152284): the closed startup
	// snapshot captures the complete three-series one-hot contract.
	if _, err := p.TokenExchangeBreakerMetrics(func() (tokenexchange.BreakerSnapshot, bool) {
		return tokenexchange.BreakerSnapshot{
			Name:  "idp-token-exchange",
			State: "closed",
		}, true
	}); err != nil {
		t.Fatalf("TokenExchangeBreakerMetrics() error = %v", err)
	}

	got := mcpFamilies(scrapePlainText(t, p))

	golden := filepath.Join("testdata", "metrics_golden.txt")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", golden)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("scrape schema drifted from golden file.\n--- got ---\n%s\n--- want ---\n%s\n"+
			"If this change is intended, regenerate: go test ./internal/observability/metrics/ -update", got, want)
	}
}

// TestScrapeCounterIncrements proves mcp_metrics_scrape_total rises by one per
// served scrape, so support can confirm Prometheus is actually scraping.
func TestScrapeCounterIncrements(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	first := scrapeCounterValue(t, scrapePlainText(t, p))
	second := scrapeCounterValue(t, scrapePlainText(t, p))
	if second != first+1 {
		t.Errorf("mcp_metrics_scrape_total = %d then %d, want +1", first, second)
	}
}

// scrapeCounterValue parses the mcp_metrics_scrape_total value from a scrape.
func scrapeCounterValue(t *testing.T, body string) int {
	t.Helper()
	s := bufio.NewScanner(strings.NewReader(body))
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "mcp_metrics_scrape_total ") {
			fields := strings.Fields(line)
			v, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				t.Fatalf("parse counter value from %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("mcp_metrics_scrape_total not found in scrape:\n%s", body)
	return 0
}

// TestGoAndProcessFamiliesPresent asserts the standard Go runtime and process
// metric families are present in a plain-text scrape. Values are not checked —
// they are environment-dependent. This test exercises New()'s MustRegister call;
// TestExporterFidelity_SharedRegistry covers the same families on an isolated
// harness registry and the two are not redundant.
func TestGoAndProcessFamiliesPresent(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	body := scrapePlainText(t, p)
	for _, prefix := range []string{"go_goroutines", "go_gc_", "go_memstats_", "process_"} {
		found := false
		s := bufio.NewScanner(strings.NewReader(body))
		for s.Scan() {
			if strings.HasPrefix(s.Text(), prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scrape has no line matching %q", prefix)
		}
	}
}

// TestProviderAccessors covers the meter-provider accessors and a clean shutdown.
func TestProviderAccessors(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if p.MeterProvider() == nil {
		t.Error("MeterProvider() = nil")
	}
	if p.Meter("test-scope") == nil {
		t.Error("Meter() = nil")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() = %v, want nil", err)
	}
}

// TestNew_NilResourceIsRejected pins the guard in New (SOL-152425): passing
// nil silently overrides the SDK's own resource.Default() and collapses
// target_info to zero labels with no error anywhere — exactly the identity
// loss this parameter exists to prevent. A caller with no opinion on
// identity must pass sdkresource.Default() explicitly, not nil.
func TestNew_NilResourceIsRejected(t *testing.T) {
	if _, err := New(testVersion, nil, config.ObservabilityConfig{}); err == nil {
		t.Fatal("New(_, nil) error = nil, want an error")
	}
}

// TestNoDeprecatedExporterOptions fails if the construction path reaches for the
// deprecated suppression options. Their absence is what keeps the published
// names tied to the exporter's default, non-deprecated rendering (ADR-008).
func TestNoDeprecatedExporterOptions(t *testing.T) {
	src, err := os.ReadFile("provider.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"WithoutUnits", "WithoutCounterSuffixes"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("provider.go uses banned exporter option %s()", banned)
		}
	}
}
