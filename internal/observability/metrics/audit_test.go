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
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

func newAuditMetrics(t *testing.T) (*AuditMetrics, *Provider) {
	t.Helper()
	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	am, err := p.AuditMetrics()
	if err != nil {
		t.Fatal(err)
	}
	return am, p
}

// auditDroppedWant renders the whole one-series family at n.
func auditDroppedWant(n int) string {
	return fmt.Sprintf("# HELP mcp_audit_events_dropped_total Number of audit records that could not be produced or written to the log stream.\n"+
		"# TYPE mcp_audit_events_dropped_total counter\n"+
		"mcp_audit_events_dropped_total %d\n", n)
}

// The series exists at zero from registration, before any drop: a flat zero
// is "nothing lost", and increase() can fire on the first one.
func TestAuditMetrics_SeedsAtZero(t *testing.T) {
	_, p := newAuditMetrics(t)
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(auditDroppedWant(0)), "mcp_audit_events_dropped_total"); err != nil {
		t.Error(err)
	}
}

func TestAuditMetrics_RecordAuditDrop_IncrementsByOne(t *testing.T) {
	am, p := newAuditMetrics(t)
	for range 3 {
		am.RecordAuditDrop(context.Background())
	}
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(auditDroppedWant(3)), "mcp_audit_events_dropped_total"); err != nil {
		t.Error(err)
	}
}

func TestAuditMetrics_NilReceiver_NoOp(t *testing.T) {
	var am *AuditMetrics
	am.RecordAuditDrop(context.Background())
}

func TestProvider_AuditMetrics_RegistersOnce(t *testing.T) {
	am, p := newAuditMetrics(t)
	again, err := p.AuditMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if again != am {
		t.Error("second AuditMetrics() call returned a different recorder")
	}
}
