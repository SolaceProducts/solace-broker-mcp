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

// Package panics owns mcp_panic_recovered_total{boundary}, the single counter
// incremented by the two recovery nets that guard a request's OWN goroutine:
// recovery.HTTPMiddleware (internal/middleware/recovery, boundary "http") and
// withRecovery (internal/tools, boundary "tool").
//
// Before this counter existed a recovered panic was visible only as an ERROR
// log line an operator had to know to grep for; nothing outside the process
// could alert on it (SOL-154037, closing the outstanding acceptance criteria of
// SOL-151286 and SOL-151287).
//
// # What this counter does NOT cover
//
// Only panics on the request's own goroutine. A panic on a goroutine a handler
// SPAWNS is recovered elsewhere and is not counted here, even though it happens
// during a request: internal/safego (the fan-out workers in the brokerstatus,
// queuemetrics and discardstats handlers and in the composite executor) and
// internal/tokenexchange (the singleflight goroutine) both recover and convert
// the panic into an error, which then returns through the handler normally, so
// withRecovery's recover never fires. Those sites log event="panic_recovered"
// but do not increment this counter. An alert on the log attribute therefore has
// wider reach than an alert on this metric; docs/observability.md says so too.
// Widening the counter to cover them means adding a boundary here and a row
// there — deliberately, not by accident.
//
// # Why the counter is package state and not a parameter
//
// Injecting it is orderable — cmd/server/main.go could build the meter provider
// before it composes the two recovery nets — so ordering is not the reason. The
// reason is what injection would cost at those two particular seams.
// withRecovery is installed by tools.RegisterWithServer, whose signature is
// already security-relevant (it carries the authorization policy and the
// write-tools gate) and which has 20+ call sites, nearly all in tests;
// recovery.HTTPMiddleware is a one-argument http.Handler decorator. Both would
// gain a telemetry parameter, and both would still need a nil-counter branch for
// the metrics-disabled case — which is exactly the no-op this package already
// provides. That is a poor trade for a process-wide, fire-and-forget counter
// with no per-instance state. Both call sites already reach for process-global
// telemetry (slog.Default()) for the same reason.
//
// Register is called once, from cmd/server/main.go, when metrics are enabled.
// Until then — and permanently when OBS_METRICS_ENABLED is false — the record
// functions are no-ops. Recovery itself is unconditional and never depends on
// this package.
package panics

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The two boundary label values. They are unexported, and the only way to record
// against either is RecoveredHTTP or RecoveredTool, so no caller can name a
// third: mcp_panic_recovered_total's two-series cardinality bound is a property
// of this package's API, not a convention a reviewer has to police. Adding a
// boundary means adding a function here and a row in docs/observability.md.
const (
	boundaryHTTP = "http"
	boundaryTool = "tool"
)

// instrumentScope names the meter that owns this instrument, matching the
// convention in internal/observability/metrics and internal/observability/tracing.
const instrumentScope = "github.com/SolaceProducts/solace-broker-mcp"

// counter holds the registered instrument, or nil before Register runs. It is
// an atomic pointer because Register happens on the startup goroutine while the
// record functions are called from request goroutines; a plain field would be a
// data race even though the write happens once.
var counter atomic.Pointer[metric.Int64Counter]

// Register creates mcp_panic_recovered_total{boundary} against meterProvider and
// installs it as the counter the record functions increment. Call it once at
// startup, only when metrics are enabled; cmd/server/main.go owns that gate.
//
// It seeds both series at zero before returning. That is not cosmetic: an OTel
// counter renders no series at all until it has a data point, and PromQL's
// rate/increase need two samples in the window, so the sample that CREATES a
// series is only a baseline. Without the seed, `increase(...) > 0` — the alert
// docs/observability.md prescribes — would not fire on a process's first panic,
// which for a bug that reached production is the case that matters most. Seeding
// also makes a flat zero mean "nothing panicked" instead of "No data", and makes
// absent() a usable "the counter was never wired" alert.
//
// A nil meterProvider is a caller error rather than a silent no-op: the only way
// to get one here is to call Register when metrics are off, which the caller is
// supposed to decide, not this package.
func Register(meterProvider metric.MeterProvider) error {
	if meterProvider == nil {
		return fmt.Errorf("panics: meterProvider must not be nil (do not call Register when metrics are disabled)")
	}
	c, err := meterProvider.Meter(instrumentScope).Int64Counter(
		"mcp.panic.recovered",
		metric.WithDescription("Panics trapped by a request-path recovery net, by boundary."),
	)
	if err != nil {
		return fmt.Errorf("register mcp_panic_recovered_total: %w", err)
	}
	counter.Store(&c)

	ctx := context.Background()
	record(ctx, boundaryHTTP, 0)
	record(ctx, boundaryTool, 0)
	return nil
}

// RecoveredHTTP records one panic trapped by recovery.HTTPMiddleware. It is a
// no-op when Register has not run (metrics disabled), so the recovery site can
// call it unconditionally without knowing whether metrics are on.
func RecoveredHTTP(ctx context.Context) { record(ctx, boundaryHTTP, 1) }

// RecoveredTool records one panic trapped by withRecovery in internal/tools.
// It is a no-op when Register has not run (metrics disabled).
func RecoveredTool(ctx context.Context) { record(ctx, boundaryTool, 1) }

// record adds n to the counter for boundary, doing nothing until Register has
// installed an instrument.
func record(ctx context.Context, boundary string, n int64) {
	c := counter.Load()
	if c == nil {
		return
	}
	(*c).Add(ctx, n, metric.WithAttributes(attribute.String("boundary", boundary)))
}
