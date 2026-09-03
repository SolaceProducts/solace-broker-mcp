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

// Package tracing wires OpenTelemetry distributed tracing: a tracer provider
// with OTLP export, self-observation counters, and (when metrics are off) a
// periodic INFO fallback (SOL-152420, Story 25). Span instrumentation at the
// MCP entry, executor, and SEMP layers lands in later stories (26, 27, 40, 47,
// 50), which create spans via the global tracer this package installs. The v1
// default is OFF (door-closing policy) — operators opt in.
//
// Resource attributes: New's caller (cmd/server/main.go) builds one shared
// identity resource.Resource via internal/observability/resource and passes
// it to both this provider and the metrics meter provider (Story 14,
// SOL-152091) — one construction site, so metrics and traces cannot disagree
// about which instance emitted them (SOL-152425, Story 34).
package tracing

import "github.com/SolaceProducts/solace-broker-mcp/internal/config"

// Enabled reports whether tracing is turned on, reading the OBS_TRACING_ENABLED
// flag off the observability config. Later wiring consults this before
// configuring the tracer provider.
func Enabled(cfg config.ObservabilityConfig) bool {
	return cfg.TracingEnabled
}
