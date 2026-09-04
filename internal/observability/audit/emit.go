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
	"log/slog"
	"time"
)

// Emit writes e to the process logger, and reports a drop if it cannot.
//
// It goes through slog.Default().Handler() rather than slog.Log because
// slog.Logger discards the handler's error return, and that error is the only
// signal that an audit record did not reach the log. Delivery is best-effort
// by design (see docs/observability.md, "Audit Delivery") — an audit write
// never stalls or fails the broker operation — but a silent best effort is
// worthless to a compliance reviewer, so a failure becomes an audit_drop
// record on the same routed stream (ISSUE-020).
//
// Two failure modes are covered. The handler can reject the record. And the
// handler can be filtering out the record's level: an operation record is INFO,
// so a server running above INFO would otherwise erase its audit trail the
// moment an operator enabled the audit log. A drop record is ERROR, the highest
// level an operator may configure, so it survives every supported log level —
// including the one that suppressed the record it reports.
//
// The Logger's own level check is replicated rather than skipped: without it,
// this would write records the operator's configured level says to suppress.
func Emit(ctx context.Context, e Event) {
	if write(ctx, e) {
		return
	}
	// A drop notice that itself dropped has nothing left to report to, and
	// re-entering here would recurse. e.Type() is the only attribution
	// available here — Event exposes rendered attrs, not the original
	// Fields — so this path carries no Tool/Broker even when the record that
	// failed to write had them.
	if e.Type() == EventAuditDrop {
		return
	}
	EmitDrop(ctx, DropContext{DroppedEventType: e.Type()})
}

// DropContext is best-effort attribution for an EmitDrop call. Every field is
// optional — carry only what the call site actually knows; the zero value
// reports a plain, unattributed gap.
type DropContext struct {
	// DroppedEventType names the audit_event_type of the record that could
	// not be built or written.
	DroppedEventType EventType
	// Tool and Broker are the same values the missing record would have
	// carried. Admitting exactly these two fields on a drop record — never a
	// general-purpose payload — is what EventAuditDrop's row in
	// applicabilityByType enforces; a drop record must never become a second
	// place raw arguments could leak.
	Tool   string
	Broker string
}

// EmitDrop writes the notice that an audit record could not be produced or
// written, with whatever attribution dctx carries. Callers that cannot build a
// valid record — a failed canonicalization, a record the constructor rejected
// — use this so the gap is visible in the audit stream rather than inferred
// from its absence.
//
// The drop record carries only common fields plus DroppedEventType/Tool/Broker
// from dctx: it reports that a record is missing, and admits just enough
// context to be actionable inside the audit stream itself
// (docs/observability.md, "event=audit" is the routing predicate customers are
// told captures everything) without becoming a second place raw arguments
// could leak. It keeps event="audit" deliberately, since a notice that fell
// outside the customer's audit filter would go unseen in precisely the
// situation it exists to report.
func EmitDrop(ctx context.Context, dctx DropContext) {
	drop, err := NewEvent(ctx, Fields{
		Type:             EventAuditDrop,
		DroppedEventType: dctx.DroppedEventType,
		Tool:             dctx.Tool,
		Broker:           dctx.Broker,
	})
	if err != nil {
		// Unreachable in production: DroppedEventType is always either empty
		// or one of this package's own EventType constants, its only two
		// legitimate values. Swallowed rather than panicking, because audit
		// emission must never take down the operation it is describing.
		return
	}
	_ = write(ctx, drop)
}

// write delivers one record and reports whether it reached the handler.
// Separate from Emit so the drop path can reuse it without recursing.
//
// A panicking handler counts as a drop, not as a failed operation. This
// package promises that writing an audit record never fails the broker
// operation it describes, and without this recover that promise is only true
// for handlers that return errors politely: a panic out of Enabled or Handle
// would unwind through CallTool's defer to withRecovery, which reports the
// call to the agent as failed. A destructive tool that had already mutated the
// broker would then be told it had not. The recursion this could feed is
// already bounded — Emit refuses to report a drop about a drop — so a handler
// that panics on every record degrades to two swallowed panics.
func write(ctx context.Context, e Event) (delivered bool) {
	defer func() {
		if r := recover(); r != nil {
			delivered = false
		}
	}()
	h := slog.Default().Handler()
	if !h.Enabled(ctx, e.Level()) {
		return false
	}
	// A zero PC omits source location, matching every other log site here.
	rec := slog.NewRecord(time.Now(), e.Level(), Message, 0)
	rec.AddAttrs(e.Attrs()...)
	return h.Handle(ctx, rec) == nil
}
