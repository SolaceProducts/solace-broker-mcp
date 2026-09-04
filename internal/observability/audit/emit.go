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
// so a server running at WARN would otherwise erase its audit trail the moment
// an operator enabled the audit log. A drop record is WARN, so it survives
// exactly the level filter that suppressed the record it reports.
//
// The Logger's own level check is replicated rather than skipped: without it,
// this would write records the operator's configured level says to suppress.
func Emit(ctx context.Context, e Event) {
	if write(ctx, e) {
		return
	}
	// A drop notice that itself dropped has nothing left to report to, and
	// re-entering here would recurse.
	if e.Type() == EventAuditDrop {
		return
	}
	EmitDrop(ctx)
}

// EmitDrop writes the notice that an audit record could not be produced or
// written. Callers that cannot build a valid record — a failed
// canonicalization, a record the constructor rejected — use this so the gap is
// visible in the audit stream rather than inferred from its absence.
//
// The drop record carries only the common fields: it reports that a record is
// missing, and there is no surviving record for it to describe. It keeps
// event="audit" deliberately, since a notice that fell outside the customer's
// audit filter would go unseen in precisely the situation it exists to report.
func EmitDrop(ctx context.Context) {
	drop, err := NewEvent(ctx, Fields{Type: EventAuditDrop})
	if err != nil {
		// Unreachable: a drop record has no fields that can fail validation.
		// Swallowed rather than panicking, because audit emission must never
		// take down the operation it is describing.
		return
	}
	_ = write(ctx, drop)
}

// write delivers one record and reports whether it reached the handler.
// Separate from Emit so the drop path can reuse it without recursing.
func write(ctx context.Context, e Event) bool {
	h := slog.Default().Handler()
	if !h.Enabled(ctx, e.Level()) {
		return false
	}
	// A zero PC omits source location, matching every other log site here.
	rec := slog.NewRecord(time.Now(), e.Level(), Message, 0)
	rec.AddAttrs(e.Attrs()...)
	return h.Handle(ctx, rec) == nil
}
