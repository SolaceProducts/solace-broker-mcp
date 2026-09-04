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
// both request-path panic nets increment when they trap a panic:
// recovery.HTTPMiddleware (internal/middleware/recovery, boundary "http") and
// withRecovery (internal/tools, boundary "tool").
//
// Before this counter existed a recovered panic was visible only as an ERROR
// log line an operator had to know to grep for; nothing outside the process
// could alert on it (SOL-154037, closing the outstanding acceptance criteria of
// SOL-151286 and SOL-151287).
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
// Until then — and permanently when OBS_METRICS_ENABLED is false — Recovered is
// a no-op. Recovery itself is unconditional and never depends on this package.
package panics

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Boundary names the recovery net that trapped the panic. Its label value is an
// unexported field, so no package outside this one can construct a third value:
// mcp_panic_recovered_total's two-series cardinality bound is a compiler
// guarantee, not a convention a reviewer has to police. Adding a boundary means
// adding a var here and a row to docs/observability.md, which is the point.
type Boundary struct{ name string }

var (
	// BoundaryHTTP is recovery.HTTPMiddleware: a panic on an HTTP handler's own
	// goroutine, converted to a clean 500.
	BoundaryHTTP = Boundary{name: "http"}
	// BoundaryTool is withRecovery in internal/tools: a panic on the SDK's
	// tool-handler goroutine, converted to an IsError tool result.
	BoundaryTool = Boundary{name: "tool"}
)

// instrumentScope names the meter that owns this instrument, matching the
// convention in internal/observability/metrics and internal/observability/tracing.
const instrumentScope = "github.com/SolaceProducts/solace-broker-mcp"

// counter holds the registered instrument, or nil before Register runs. It is
// an atomic pointer because Register happens on the startup goroutine while
// Recovered is called from request goroutines; a plain field would be a data
// race even though the write happens once.
var counter atomic.Pointer[metric.Int64Counter]

// Register creates mcp_panic_recovered_total{boundary} against meterProvider
// and installs it as the counter Recovered increments. Call it once at startup,
// only when metrics are enabled; cmd/server/main.go owns that gate.
//
// A nil meterProvider is a caller error rather than a silent no-op: the only
// way to get one here is to call Register when metrics are off, which the
// caller is supposed to decide, not this package.
func Register(meterProvider *sdkmetric.MeterProvider) error {
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
	return nil
}

// Recovered records one recovered panic at boundary. It is a no-op when
// Register has not run (metrics disabled), so a recovery site can call it
// unconditionally without knowing whether metrics are on.
//
// Boundary's unexported field means the only value a caller can pass that this
// package did not define is the zero Boundary, which is dropped rather than
// recorded as an empty label. Every real call site passes a package var.
func Recovered(ctx context.Context, boundary Boundary) {
	if boundary.name == "" {
		return
	}
	c := counter.Load()
	if c == nil {
		return
	}
	(*c).Add(ctx, 1, metric.WithAttributes(attribute.String("boundary", boundary.name)))
}
