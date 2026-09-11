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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

var tokenExchangeBreakerStates = [...]string{"closed", "open", "half-open"}

// TokenExchangeBreakerMetrics holds the process-wide token-exchange breaker
// state gauge.
type TokenExchangeBreakerMetrics struct{}

// NewTokenExchangeBreakerMetrics registers the one-hot token-exchange breaker
// state gauge. snapshot must be passive: collection must never call into
// gobreaker because its State method can materialize a state transition.
func NewTokenExchangeBreakerMetrics(
	meter metric.Meter,
	snapshot func() (tokenexchange.BreakerSnapshot, bool),
) (*TokenExchangeBreakerMetrics, error) {
	stateGauge, err := meter.Float64ObservableGauge(
		"mcp.token_exchange.circuit_breaker.state",
		metric.WithDescription("1 for the token-exchange circuit breaker's current materialized state; 0 for the other states."),
	)
	if err != nil {
		return nil, fmt.Errorf("register mcp_token_exchange_circuit_breaker_state: %w", err)
	}

	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		current, ok := snapshot()
		if !ok {
			return nil
		}

		breakerAttr := attribute.String("breaker", current.Name)
		for _, state := range tokenExchangeBreakerStates {
			value := 0.0
			if state == current.State {
				value = 1.0
			}
			observer.ObserveFloat64(stateGauge, value, metric.WithAttributes(
				breakerAttr,
				attribute.String("state", state),
			))
		}
		return nil
	}, stateGauge)
	if err != nil {
		return nil, fmt.Errorf("register token-exchange circuit breaker metrics callback: %w", err)
	}

	return &TokenExchangeBreakerMetrics{}, nil
}
