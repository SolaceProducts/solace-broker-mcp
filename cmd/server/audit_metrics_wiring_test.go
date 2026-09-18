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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// AuditMetrics is what main hands to audit.SetDropRecorder. The structural
// contract is asserted here, in the one package that imports both sides, so
// that neither internal/observability/audit nor internal/observability/metrics
// has to import the other even in its tests (see audit.DropRecorder's doc for
// why that graph is kept apart).
var _ audit.DropRecorder = (*metrics.AuditMetrics)(nil)

// buildAuditMetrics returns a recorder iff a provider exists (SOL-154569).
// There is no second flag to pin here, unlike buildSecurityMetrics: the
// provider's own existence IS the OBS_METRICS_ENABLED gate.

func TestBuildAuditMetrics_NoProvider_Nil(t *testing.T) {
	if am := buildAuditMetrics(nil); am != nil {
		t.Errorf("buildAuditMetrics(nil) = %v, want nil", am)
	}
}

// With a provider the counter is registered and already on /metrics at zero,
// before any drop — the "visible before the first drop" acceptance criterion.
func TestBuildAuditMetrics_WithProvider_SeededOnScrape(t *testing.T) {
	p := newTestMetricsProvider(t)
	if am := buildAuditMetrics(p); am == nil {
		t.Fatal("buildAuditMetrics with a provider = nil, want a recorder")
	}
	if body := scrapeMetrics(t, p); !strings.Contains(body, "mcp_audit_events_dropped_total 0") {
		t.Errorf("scrape missing the zero-seeded series:\n%s", body)
	}
}

// The whole composition as main wires it: buildAuditMetrics feeds
// audit.SetDropRecorder, and a drop reported through the audit package's own
// entry point lands on /metrics — alongside, not instead of, the audit_drop
// record on the log stream. Restores the package-level recorder afterwards so
// no other test in this binary inherits a recorder pointed at a shut-down
// provider.
func TestAuditDropRecorder_WiredThroughEmitDrop(t *testing.T) {
	buf, restore := captureStartupLog(t)
	defer restore()

	p := newTestMetricsProvider(t)
	audit.SetDropRecorder(buildAuditMetrics(p))
	t.Cleanup(func() { audit.SetDropRecorder(nil) })

	audit.EmitDrop(context.Background(), audit.DropContext{DroppedEventType: audit.EventOperation, Tool: "delete-queue", Broker: "dev"})
	audit.EmitDrop(context.Background(), audit.DropContext{DroppedEventType: audit.EventAuthSuccess})

	if body := scrapeMetrics(t, p); !strings.Contains(body, "mcp_audit_events_dropped_total 2") {
		t.Errorf("scrape does not show the two drops:\n%s", body)
	}
	if got := strings.Count(buf.String(), `"audit_event_type":"audit_drop"`); got != 2 {
		t.Errorf("log stream carries %d audit_drop record(s), want 2 — the counter must accompany the record, not replace it:\n%s", got, buf.String())
	}
}

// With no recorder installed (metrics off), EmitDrop still writes the record
// and nothing else — the same no-op the panic counter has in that mode.
func TestAuditDropRecorder_Unset_RecordOnly(t *testing.T) {
	buf, restore := captureStartupLog(t)
	defer restore()

	audit.SetDropRecorder(nil)
	audit.EmitDrop(context.Background(), audit.DropContext{DroppedEventType: audit.EventOperation})

	if !strings.Contains(buf.String(), `"audit_event_type":"audit_drop"`) {
		t.Errorf("no audit_drop record written with the recorder unset:\n%s", buf.String())
	}
}

func scrapeMetrics(t *testing.T, p *metrics.Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}
