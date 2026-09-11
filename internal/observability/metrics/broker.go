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

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
)

// BrokerMetrics holds the three broker reachability gauges.
type BrokerMetrics struct{}

// NewBrokerMetrics registers mcp_broker_reachable, mcp_broker_unreachable_reason,
// and mcp_broker_last_result_timestamp_seconds. brokerStates is called once per
// scrape; all three gauges share one callback so they are always consistent and
// honour the one-hot contract.
func NewBrokerMetrics(meter metric.Meter, brokerStates func() map[string]health.BrokerSnapshot) (*BrokerMetrics, error) {
	gReachable, err := meter.Float64ObservableGauge(
		"mcp.broker.reachable",
		metric.WithDescription("1 when the broker's last SEMP call succeeded, 0 otherwise."),
	)
	if err != nil {
		return nil, fmt.Errorf("register mcp_broker_reachable: %w", err)
	}

	gReason, err := meter.Float64ObservableGauge(
		"mcp.broker.unreachable.reason",
		metric.WithDescription("1 for the active failure reason; 0 for all other reasons seen for this broker."),
	)
	if err != nil {
		return nil, fmt.Errorf("register mcp_broker_unreachable_reason: %w", err)
	}

	gTimestamp, err := meter.Float64ObservableGauge(
		"mcp.broker.last.result.timestamp.seconds",
		metric.WithDescription("Unix timestamp of the most recent SEMP call result for this broker."),
	)
	if err != nil {
		return nil, fmt.Errorf("register mcp_broker_last_result_timestamp_seconds: %w", err)
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for broker, snap := range brokerStates() {
			brokerAttr := attribute.String("broker", broker)

			if snap.Current == health.StateReachable {
				o.ObserveFloat64(gReachable, 1.0, metric.WithAttributes(brokerAttr))
			} else {
				o.ObserveFloat64(gReachable, 0.0, metric.WithAttributes(brokerAttr))
			}

			for _, reason := range snap.SeenReasons {
				val := 0.0
				if reason == snap.Current {
					val = 1.0
				}
				o.ObserveFloat64(gReason, val, metric.WithAttributes(
					brokerAttr,
					attribute.String("reason", string(reason)),
				))
			}

			if !snap.LastResult.IsZero() {
				o.ObserveFloat64(gTimestamp, float64(snap.LastResult.Unix()), metric.WithAttributes(brokerAttr))
			}
		}
		return nil
	}, gReachable, gReason, gTimestamp)
	if err != nil {
		return nil, fmt.Errorf("register broker metrics callback: %w", err)
	}

	return &BrokerMetrics{}, nil
}
