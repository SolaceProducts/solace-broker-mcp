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
	"time"
)

type ToolMetrics struct {
	invocations    metric.Int64Counter
	duration       metric.Float64Histogram
	activeRequests metric.Int64UpDownCounter
}

func NewToolMetrics(meter metric.Meter) (*ToolMetrics, error) {
	invocations, err := meter.Int64Counter(
		"mcp.tool.invocation",
		metric.WithDescription("Number of tool invocations."),
		metric.WithUnit("1"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_tool_invocation: %w", err)
	}

	duration, err := meter.Float64Histogram(
		"mcp.tool.invocation.duration",
		metric.WithDescription("Duration of tool invocation in seconds"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10),
		metric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_tool_invocation_duration_seconds: %w", err)
	}

	activeRequests, err := meter.Int64UpDownCounter(
		"mcp.http.active_requests",
		metric.WithDescription("Number of in-flight HTTP requests"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_http_active_requests: %w", err)
	}

	return &ToolMetrics{invocations: invocations, duration: duration, activeRequests: activeRequests}, nil
}

func (t *ToolMetrics) Record(ctx context.Context, tool, broker, outcome, errorType string, dur time.Duration) {
	if t == nil {
		return
	}
	opts := metric.WithAttributes(
		attribute.String("tool", tool),
		attribute.String("broker", broker),
		attribute.String("outcome", outcome),
		attribute.String("error_type", errorType),
	)

	t.invocations.Add(ctx, 1, opts)
	t.duration.Record(ctx, dur.Seconds(), opts)
}

func (t *ToolMetrics) IncActive(ctx context.Context) {
	if t == nil {
		return
	}

	t.activeRequests.Add(ctx, 1)
}

func (t *ToolMetrics) DecActive(ctx context.Context) {
	if t == nil {
		return
	}

	t.activeRequests.Add(ctx, -1)
}
