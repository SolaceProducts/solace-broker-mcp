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

package correlation

import (
	"context"
	"log/slog"
)

// attrKey is the slog attribute key under which the correlation ID is stamped on
// every request-scoped log line. It matches the field name an operator greps for
// to follow a single request across all of its log output.
const attrKey = "correlation_id"

// slogHandler is a slog.Handler decorator that stamps the request's correlation
// ID onto every emitted record. It reads the ID from the record's context (where
// the correlation middleware seeded it via With) rather than from a fixed value,
// so a single handler instance serves all requests: the ID is derived per record
// in Handle, not captured at construction.
//
// Redaction is preserved automatically: the wrapper does no formatting and owns
// no ReplaceAttr. It delegates Handle to next (the JSON handler that owns the
// credential-redaction ReplaceAttr filter), so every attribute — including the
// correlation_id this wrapper adds — still flows through ReplaceAttr. The added
// correlation_id key is not a credential pattern, so it passes through intact
// while passwords/tokens/etc. are still redacted by next.
//
// Top-level placement under groups. slog's built-in handlers nest every record
// attribute (and WithAttrs-bound attribute) under any group opened via
// WithGroup. If the wrapper just added correlation_id after delegating groups to
// next, the ID would nest inside the group (e.g. {"g":{"correlation_id":...}})
// instead of at the root, defeating a top-level grep. To keep correlation_id at
// the root regardless of grouping, the wrapper does NOT delegate WithAttrs and
// WithGroup to next immediately. It records each call as a "group or attrs" (goa)
// step in order. In Handle it adds correlation_id to the UNGROUPED root handler
// first, then replays the recorded goa steps onto next, so the user's own attrs
// land in their groups while correlation_id stays at the root. With no goa steps
// (the common case) this collapses to a single delegated Add.
type slogHandler struct {
	next slog.Handler
	// goas records WithGroup/WithAttrs calls in the order they were made, so
	// Handle can replay them after injecting correlation_id at the root. It is
	// empty in the common case (no .With/.WithGroup on the default logger), which
	// makes Handle take the cheap delegated path.
	goas []goa
}

// goa is one recorded "group or attrs" operation: exactly one of group (a
// WithGroup call) or attrs (a WithAttrs call) is set. This is the standard slog
// decorator technique for deferring grouping so a decorator can inject an
// attribute at the root before reopening the caller's groups.
type goa struct {
	group string
	attrs []slog.Attr
}

// NewSlogHandler wraps next so that any log emitted within a request context
// automatically includes a correlation_id attribute. Install it once over the
// base handler; gating is transitive: From returns "" when the correlation
// middleware is not wired (capability off) or for startup/non-request logs, so
// in those cases no correlation_id attribute is emitted and output is unchanged.
func NewSlogHandler(next slog.Handler) slog.Handler {
	return &slogHandler{next: next}
}

// Enabled delegates to next; the wrapper does not change level filtering. Groups
// and attrs do not affect a level-only decision, so delegating to the (possibly
// ungrouped) next is correct.
func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle stamps correlation_id onto the record when the context carries a
// non-empty correlation ID AND the record does not already carry a
// correlation_id attribute, then delegates to next.
//
// The no-overwrite rule (AC #3) is enforced by checking for an existing
// root-level correlation_id before adding one: either on the record itself
// (hasCorrelationID) or bound on the logger at root via .With(...) before any
// group was opened (hasRootCorrelationID). In both cases the caller's explicit
// correlation_id wins and is never duplicated at the root. When there is no ID
// to add and no recorded goa steps, the record is passed through untouched, so
// absent-ID output matches the unwrapped handler exactly.
//
// With recorded goa steps, the delivery handler is rebuilt from the ungrouped
// root: correlation_id is bound at the root first, then the goa steps are
// replayed (groups reopened, attrs rebound) so the record's own attributes keep
// their grouping while correlation_id stays at the top level.
func (h *slogHandler) Handle(ctx context.Context, rec slog.Record) error {
	id := From(ctx)
	addID := id != "" && !h.hasCorrelationID(rec) && !h.hasRootCorrelationID()

	if len(h.goas) == 0 {
		if !addID {
			return h.next.Handle(ctx, rec)
		}
		// Clone before mutating: slog may pass the same Record to more than one
		// handler, and rec.Add appends to the Record's own attribute slice, so
		// cloning keeps our added correlation_id local to this delivery.
		rec = rec.Clone()
		rec.Add(slog.String(attrKey, id))
		return h.next.Handle(ctx, rec)
	}

	delivery := h.next
	if addID {
		delivery = delivery.WithAttrs([]slog.Attr{slog.String(attrKey, id)})
	}
	for _, g := range h.goas {
		if g.group != "" {
			delivery = delivery.WithGroup(g.group)
		} else {
			delivery = delivery.WithAttrs(g.attrs)
		}
	}
	return delivery.Handle(ctx, rec)
}

// hasCorrelationID reports whether rec already carries a correlation_id
// attribute, so an existing one is never overwritten or duplicated (AC #3). Only
// the record's own attributes are scanned; attributes bound earlier via
// WithAttrs are held in goas and are not part of the per-record set, which is
// the correct scope: a correlation_id attached to the record itself is the one a
// caller explicitly chose to set for that line.
func (h *slogHandler) hasCorrelationID(rec slog.Record) bool {
	found := false
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == attrKey {
			found = true
			return false // stop iterating
		}
		return true
	})
	return found
}

// hasRootCorrelationID reports whether a correlation_id was already bound on the
// logger at the ROOT level via .With(...)/WithAttrs, recorded in goas. The
// wrapper always injects its correlation_id at the root (bound on the ungrouped
// next before any goa step is replayed), so a duplicate root key arises only
// when a root-level correlation_id already exists. Returning true here suppresses
// that injection so the caller's explicit value wins with no duplicate (AC #3).
//
// Only attrs bound BEFORE the first WithGroup land at the root; attrs bound after
// a group nest under that group (a different key path) and must NOT suppress the
// root injection. So the scan walks goas in order and stops at the first group
// step: any correlation_id in an attr step seen before that is root-level; a
// correlation_id only appearing in attrs after a group is nested and is ignored
// here (still inject at root). With no goas (the common case) this returns false.
func (h *slogHandler) hasRootCorrelationID() bool {
	for _, g := range h.goas {
		if g.group != "" {
			// First group opened: subsequent attrs nest under it, so no further
			// step can introduce a root-level correlation_id.
			return false
		}
		for _, a := range g.attrs {
			if a.Key == attrKey {
				return true
			}
		}
	}
	return false
}

// WithAttrs returns a new slogHandler that records the attrs as a goa step. It
// MUST re-wrap rather than return the bare next: slog clones the handler on
// every logger.With(...) call, and returning the unwrapped inner handler would
// strip the correlation behavior from the derived logger. Recording (rather than
// delegating to next.WithAttrs) keeps the replay ordering correct relative to
// any interleaved WithGroup calls.
func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &slogHandler{next: h.next, goas: appendGoa(h.goas, goa{attrs: attrs})}
}

// WithGroup returns a new slogHandler that records the opened group as a goa
// step. Like WithAttrs it re-wraps so correlation behavior survives
// logger.WithGroup(...), and it defers the actual grouping to Handle so
// correlation_id can be injected at the root before the group is reopened.
func (h *slogHandler) WithGroup(name string) slog.Handler {
	// An empty group name is a no-op per the slog.Handler contract.
	if name == "" {
		return h
	}
	return &slogHandler{next: h.next, goas: appendGoa(h.goas, goa{group: name})}
}

// appendGoa returns a new slice with g appended, copying the existing steps so
// derived handlers never share (and so cannot mutate) a parent's goa backing
// array — sibling loggers derived from the same parent must stay independent.
func appendGoa(existing []goa, g goa) []goa {
	out := make([]goa, 0, len(existing)+1)
	out = append(out, existing...)
	out = append(out, g)
	return out
}
