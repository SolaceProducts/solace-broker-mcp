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
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

func breakerStateExposition(active string) string {
	value := func(state string) float64 {
		if state == active {
			return 1
		}
		return 0
	}
	return fmt.Sprintf(`
# HELP mcp_token_exchange_circuit_breaker_state 1 for the token-exchange circuit breaker's current materialized state; 0 for the other states.
# TYPE mcp_token_exchange_circuit_breaker_state gauge
mcp_token_exchange_circuit_breaker_state{breaker="idp-token-exchange",state="closed"} %v
mcp_token_exchange_circuit_breaker_state{breaker="idp-token-exchange",state="half-open"} %v
mcp_token_exchange_circuit_breaker_state{breaker="idp-token-exchange",state="open"} %v
`, value("closed"), value("half-open"), value("open"))
}

func TestTokenExchangeBreakerMetrics_OneHot(t *testing.T) {
	for _, active := range tokenExchangeBreakerStates {
		t.Run(active, func(t *testing.T) {
			t.Parallel()

			p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.TokenExchangeBreakerMetrics(func() (tokenexchange.BreakerSnapshot, bool) {
				return tokenexchange.BreakerSnapshot{
					Name:  "idp-token-exchange",
					State: active,
				}, true
			}); err != nil {
				t.Fatal(err)
			}

			if err := testutil.GatherAndCompare(
				p.registry,
				strings.NewReader(breakerStateExposition(active)),
				"mcp_token_exchange_circuit_breaker_state",
			); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestTokenExchangeBreakerMetrics_TransitionUpdatesGauge(t *testing.T) {
	t.Parallel()

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	current := "closed"
	if _, err := p.TokenExchangeBreakerMetrics(func() (tokenexchange.BreakerSnapshot, bool) {
		return tokenexchange.BreakerSnapshot{Name: "idp-token-exchange", State: current}, true
	}); err != nil {
		t.Fatal(err)
	}

	for _, state := range []string{"closed", "open", "half-open", "closed"} {
		current = state
		if err := testutil.GatherAndCompare(
			p.registry,
			strings.NewReader(breakerStateExposition(state)),
			"mcp_token_exchange_circuit_breaker_state",
		); err != nil {
			t.Errorf("state %q: %v", state, err)
		}
	}
}

func TestTokenExchangeBreakerMetrics_AbsentWhenDisabled(t *testing.T) {
	t.Parallel()

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.TokenExchangeBreakerMetrics(func() (tokenexchange.BreakerSnapshot, bool) {
		return tokenexchange.BreakerSnapshot{}, false
	}); err != nil {
		t.Fatal(err)
	}

	if err := testutil.GatherAndCompare(
		p.registry,
		strings.NewReader(""),
		"mcp_token_exchange_circuit_breaker_state",
	); err != nil {
		t.Error(err)
	}
}

func TestTokenExchangeBreakerMetrics_SnapshotOncePerCollection(t *testing.T) {
	t.Parallel()

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Uint32
	snapshot := func() (tokenexchange.BreakerSnapshot, bool) {
		calls.Add(1)
		return tokenexchange.BreakerSnapshot{Name: "idp-token-exchange", State: "closed"}, true
	}

	// Comparing the two returned pointers would prove nothing:
	// TokenExchangeBreakerMetrics is zero-sized, so every allocation of it
	// shares runtime.zerobase and compares equal. The sync.Once gate shows up
	// instead in the snapshot call count below, since a second registration
	// would add a second callback and double it.
	if _, err := p.TokenExchangeBreakerMetrics(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := p.TokenExchangeBreakerMetrics(snapshot); err != nil {
		t.Fatal(err)
	}

	if _, err := p.registry.Gather(); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("snapshot calls after one collection = %d, want 1", got)
	}
}
