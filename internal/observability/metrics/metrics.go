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

// Package metrics is the home for the server's metrics emission. Skeleton
// (SOL-151278): only the capability gate exists today; the metric instruments
// and their export land in a later story. The v1 default is OFF (door-closing
// policy) — operators opt in. Emitted records will carry
// schema.MetricsSchemaVersion.
package metrics

import "github.com/SolaceProducts/solace-broker-mcp/internal/config"

// Enabled reports whether a metrics meter provider is built at all: true when
// either egress flag — OBS_METRICS_SCRAPE_ENABLED or OBS_METRICS_OTLP_ENABLED —
// is on (SOL-154607). cmd/server consults this before building the provider
// and registering instruments; which egresses the provider then serves is the
// provider's own concern (see New).
func Enabled(cfg config.ObservabilityConfig) bool {
	return cfg.MetricsProviderEnabled()
}

// AuthFailureCounterEnabled reports whether the auth-failure counter is turned
// on. It follows Enabled (either metrics egress) unless
// OBS_AUTH_FAILURE_COUNTER_ENABLED is explicitly set; that resolution happens
// at config load, so this accessor just reads the resolved flag.
func AuthFailureCounterEnabled(cfg config.ObservabilityConfig) bool {
	return cfg.AuthFailureCounterEnabled
}
