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
	"log/slog"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// buildSecurityMetrics decides whether the security counters (SOL-152099) are
// recorded: a recorder iff OBS_AUTH_FAILURE_COUNTER_ENABLED resolves true and a
// metrics provider exists, otherwise nil, which every consumer treats as
// inert. The flag follows OBS_METRICS_ENABLED unless set explicitly. Forced on
// while metrics are off has nothing to register against, so it warns; a nil
// provider with metrics on means the provider build failed, which main has
// already logged as an ERROR.
func buildSecurityMetrics(cfg *config.ServerConfig, p *metrics.Provider) *metrics.SecurityMetrics {
	if !metrics.AuthFailureCounterEnabled(cfg.Observability) {
		return nil
	}
	if p == nil {
		if !metrics.Enabled(cfg.Observability) {
			slog.Warn("OBS_AUTH_FAILURE_COUNTER_ENABLED is true but OBS_METRICS_ENABLED is false; mcp_auth_failure_total and mcp_authz_denied_total have no exporter and will not be emitted")
		}
		return nil
	}
	sm, err := p.SecurityMetrics()
	if err != nil {
		slog.Error("security counters unavailable: registration failed", slog.String("error", err.Error()))
		return nil
	}
	return sm
}
