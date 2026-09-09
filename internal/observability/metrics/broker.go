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
)

// BrokerMetrics holds the two broker reachability gauges.
type BrokerMetrics struct{}

// NewBrokerMetrics registers mcp_broker_reachable and mcp_broker_unreachable_reason.
// brokerStates is called on every scrape and returns the current map[broker]reason.
func NewBrokerMetrics(meter metric.Meter, brokerStates func() map[string]string) (*BrokerMetrics, error) {
	_, err := meter.Float64ObservableGauge(
		"mcp.broker.reachable",
		metric.WithDescription("1 when the broker's last SEMP call succeeded, 0 otherwise."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for broker, state := range brokerStates() {
				val := 0.0
				if state == "reachable" {
					val = 1.0
				}
				o.Observe(val, metric.WithAttributes(attribute.String("broker", broker)))
			}
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("register mcp_broker_reachable: %w", err)
	}

	_, err = meter.Float64ObservableGauge(
		"mcp.broker.unreachable.reason",
		metric.WithDescription("1 when the broker is not reachable, labelled with the failure reason."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for broker, state := range brokerStates() {
				if state == "reachable" {
					continue
				}
				o.Observe(1.0, metric.WithAttributes(
					attribute.String("broker", broker),
					attribute.String("reason", state),
				))
			}
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("register mcp_broker_unreachable_reason: %w", err)
	}

	return &BrokerMetrics{}, nil
}
