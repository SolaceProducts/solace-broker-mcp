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

	"go.opentelemetry.io/otel/metric"
)

// AuditMetrics holds mcp_audit_events_dropped_total (SOL-154569), the one
// audit-pipeline instrument. It is the metrics-side half of the audit drop
// signal: the audit_drop record rides the slog stream that just failed, this
// counter rides the scrape surface, which fails independently — the same
// reasoning behind the mcp_otel_*_dropped_total pairs. Every method is
// nil-safe, so a disabled server (nil) records nothing.
//
// It has no labels. A drop is attributed inside the audit stream by the
// audit_drop record's dropped_audit_event_type/tool/broker fields; putting
// those on the counter would multiply a series that exists to answer one
// question — "did anything go missing?" — by |event types| x |tools| x
// |brokers|, for no alerting benefit. A closed `reason` label (sink refused,
// level filtered, constructor rejected) was weighed and not added either: the
// schema row docs/observability.md carries is `none`, and a level-filtered
// drop is a real loss the operator must fix — the runbook says so — not a
// state to route around. A label added later changes the series identity, a
// major metrics_schema bump, so this is recorded as a decision, not left as
// an omission.
//
// Gated by the metrics provider existing (cmd/server/audit_metrics.go), like
// mcp_broker_authz_denied_total and unlike SecurityMetrics, whose nil gate
// additionally follows OBS_AUTH_FAILURE_COUNTER_ENABLED. It is deliberately
// not also gated by OBS_AUDIT_LOG_ENABLED: with the audit log off nothing is
// emitted, so nothing drops, and a seeded zero is the truthful reading. A
// series that appeared only when both flags were on would make "counter
// absent" ambiguous between "metrics off" and "audit off", which is exactly
// the flag-gated-versus-unimplemented ambiguity SOL-154509 found in the docs.
//
// It satisfies audit.DropRecorder structurally; cmd/server installs it with
// audit.SetDropRecorder, so internal/observability/audit never imports this
// package.
type AuditMetrics struct {
	dropped metric.Int64Counter
}

// NewAuditMetrics registers the counter and seeds its single series at zero.
// The seed is not cosmetic (see panics.Register for the full argument): an
// OTel counter renders no series until its first data point, and increase()
// needs two samples, so without it the alert docs/observability.md prescribes
// would not fire on a process's first drop, and a flat zero would read as
// "No data" instead of "nothing lost". Unlabelled, so one Add(0) creates the
// family's only series.
func NewAuditMetrics(meter metric.Meter) (*AuditMetrics, error) {
	dropped, err := meter.Int64Counter(
		"mcp.audit.events.dropped",
		metric.WithDescription("Number of audit records that could not be produced or written to the log stream."))
	if err != nil {
		return nil, fmt.Errorf("register mcp_audit_events_dropped_total: %w", err)
	}
	a := &AuditMetrics{dropped: dropped}
	a.dropped.Add(context.Background(), 0)
	return a, nil
}

// RecordAuditDrop counts one audit record that could not be produced or
// written. Implements audit.DropRecorder; audit.EmitDrop is the only caller in
// production. No-op on a nil receiver.
func (a *AuditMetrics) RecordAuditDrop(ctx context.Context) {
	if a == nil {
		return
	}
	a.dropped.Add(ctx, 1)
}
