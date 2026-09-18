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

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// buildAuditMetrics decides whether mcp_audit_events_dropped_total (SOL-154569)
// is recorded: a recorder iff a metrics provider exists, otherwise nil, which
// main treats as "leave audit's drop recorder unset" so every audit.EmitDrop
// stays a metrics no-op and the audit_drop record remains the only drop
// signal. Gated by OBS_METRICS_ENABLED alone — the provider is nil exactly
// when that flag is off or the provider build failed, and main has already
// logged the latter as an ERROR — and deliberately not by
// OBS_AUDIT_LOG_ENABLED; see metrics.AuditMetrics for why a seeded zero with
// the audit log off is the truthful reading. Mirrors buildSecurityMetrics
// minus that builder's second, narrower flag.
func buildAuditMetrics(p *metrics.Provider) *metrics.AuditMetrics {
	if p == nil {
		return nil
	}
	am, err := p.AuditMetrics()
	if err != nil {
		slog.Error("audit drop counter unavailable: registration failed", slog.String("error", err.Error()))
		return nil
	}
	return am
}
