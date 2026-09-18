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

package audit

import (
	"context"
	"sync/atomic"
)

// DropRecorder is how a dropped audit record is counted on a surface other
// than the log stream that just failed to carry it: mcp_audit_events_dropped_total
// on /metrics (SOL-154569). The audit_drop record is the in-stream signal; when
// the stream itself is what failed, this is the only signal left.
//
// internal/observability/metrics.AuditMetrics satisfies it. The interface is
// declared here, rather than that type imported, so the dependency runs
// cmd/server → {audit, metrics} and this package stays free of the metrics
// package — the same shape auth.AuthAuditHook gives internal/auth. No import
// cycle exists today; two things keep it this way regardless. metrics drags in
// the OTel SDK, the Prometheus exporter, tokenexchange and health, and this
// package is imported by internal/tools and internal/semp/resilience, which
// should not inherit that graph. And metrics → tokenexchange means the first
// audit record ever emitted from tokenexchange would close a cycle through a
// direct import.
type DropRecorder interface {
	// RecordAuditDrop counts one audit record that could not be produced or
	// written. Called once per EmitDrop, before the drop notice is attempted.
	//
	// Implementations must not panic and must not block. This runs on the
	// request path inside this package's promise that emitting a record never
	// fails the operation it describes (see write), and unlike the handler
	// call it is not wrapped in a recover: the only production implementation
	// is a nil-safe OTel counter Add, which cannot panic, so a recover here
	// would guard nothing real.
	RecordAuditDrop(ctx context.Context)
}

// dropRecorder holds the installed recorder, or nil before SetDropRecorder
// runs. An atomic pointer for the reason panics.counter is one: SetDropRecorder
// runs on the startup goroutine while EmitDrop runs on request goroutines, so a
// plain variable would be a data race even though the write happens once.
var dropRecorder atomic.Pointer[DropRecorder]

// SetDropRecorder installs r as the process-wide recorder EmitDrop increments.
// Call it once at startup, only when metrics are enabled; cmd/server/main.go
// owns that gate, as it does for panics.Register. Until then — and permanently
// when OBS_METRICS_ENABLED is false — the audit_drop record is the only drop
// signal, and EmitDrop's increment is a no-op. nil uninstalls (tests).
//
// Process state rather than a parameter, for the reason internal/observability/
// panics gives for its counter: Emit and EmitDrop are free functions with
// emission sites in internal/tools, internal/semp/resilience and this package,
// every one of which already reaches process-global state (slog.Default()) to
// write the record. Threading a recorder through each would add a telemetry
// parameter to every audit seam and still need the nil branch this provides.
func SetDropRecorder(r DropRecorder) {
	if r == nil {
		dropRecorder.Store(nil)
		return
	}
	// &r, a pointer to the interface value, not the interface itself:
	// atomic.Pointer[T] holds a *T, and T here is the interface type. Do not
	// "simplify" this to storing r or its dynamic value; the pointer is what
	// makes the install/read pair race-free.
	dropRecorder.Store(&r)
}

// recordDrop counts one drop on the installed recorder, doing nothing until
// SetDropRecorder has installed one.
func recordDrop(ctx context.Context) {
	// Load returns the *DropRecorder stored above; dereference once to reach
	// the interface, then call through it.
	if r := dropRecorder.Load(); r != nil {
		(*r).RecordAuditDrop(ctx)
	}
}
