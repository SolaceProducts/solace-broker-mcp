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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
)

func snap(states map[string]health.BrokerSnapshot) func() map[string]health.BrokerSnapshot {
	return func() map[string]health.BrokerSnapshot { return states }
}

func TestBrokerMetrics_Reachable(t *testing.T) {
	t.Parallel()

	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.BrokerMetrics(snap(map[string]health.BrokerSnapshot{
		"prod": {Current: health.StateReachable},
	})); err != nil {
		t.Fatal(err)
	}

	const want = `
# HELP mcp_broker_reachable 1 when the broker's last SEMP call succeeded, 0 otherwise.
# TYPE mcp_broker_reachable gauge
mcp_broker_reachable{broker="prod"} 1
`
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(want), "mcp_broker_reachable"); err != nil {
		t.Error(err)
	}
}

func TestBrokerMetrics_Unreachable(t *testing.T) {
	t.Parallel()

	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.BrokerMetrics(snap(map[string]health.BrokerSnapshot{
		"prod": {
			Current:     health.StateUnreachable,
			SeenReasons: []health.BrokerState{health.StateUnreachable},
		},
	})); err != nil {
		t.Fatal(err)
	}

	const wantReachable = `
# HELP mcp_broker_reachable 1 when the broker's last SEMP call succeeded, 0 otherwise.
# TYPE mcp_broker_reachable gauge
mcp_broker_reachable{broker="prod"} 0
`
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(wantReachable), "mcp_broker_reachable"); err != nil {
		t.Error(err)
	}

	const wantReason = `
# HELP mcp_broker_unreachable_reason 1 for the active failure reason; 0 for all other reasons seen for this broker.
# TYPE mcp_broker_unreachable_reason gauge
mcp_broker_unreachable_reason{broker="prod",reason="unreachable"} 1
`
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(wantReason), "mcp_broker_unreachable_reason"); err != nil {
		t.Error(err)
	}
}

// TestBrokerMetrics_OneHot asserts that after a broker transitions from unreachable
// to credential_invalid, both reasons appear in the scrape — unreachable at 0,
// credential_invalid at 1.
func TestBrokerMetrics_OneHot(t *testing.T) {
	t.Parallel()

	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.BrokerMetrics(snap(map[string]health.BrokerSnapshot{
		"prod": {
			Current: health.StateCredentialInvalid,
			SeenReasons: []health.BrokerState{
				health.StateCredentialInvalid,
				health.StateUnreachable,
			},
		},
	})); err != nil {
		t.Fatal(err)
	}

	const want = `
# HELP mcp_broker_unreachable_reason 1 for the active failure reason; 0 for all other reasons seen for this broker.
# TYPE mcp_broker_unreachable_reason gauge
mcp_broker_unreachable_reason{broker="prod",reason="credential_invalid"} 1
mcp_broker_unreachable_reason{broker="prod",reason="unreachable"} 0
`
	if err := testutil.GatherAndCompare(p.registry, strings.NewReader(want), "mcp_broker_unreachable_reason"); err != nil {
		t.Error(err)
	}
}

func TestBrokerMetrics_AbsentBeforeFirstCall(t *testing.T) {
	t.Parallel()

	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.BrokerMetrics(snap(nil)); err != nil {
		t.Fatal(err)
	}

	mfs, err := p.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "mcp_broker_reachable" || mf.GetName() == "mcp_broker_unreachable_reason" {
			if len(mf.GetMetric()) > 0 {
				t.Errorf("expected no broker metrics before first call, got %v", mf)
			}
		}
	}
}
