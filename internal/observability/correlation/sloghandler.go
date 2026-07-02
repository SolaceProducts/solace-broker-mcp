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
// step in order. When it must inject correlation_id under groups, Handle adds
// correlation_id to the UNGROUPED root handler first, then replays the recorded
// goa steps onto next, so the user's own attrs land in their groups while
// correlation_id stays at the root. With no goa steps (the common case) this
// collapses to a single delegated Add.
//
// Caching. Two things are fixed for a given handler (they depend only on next
// and goas, which never change after construction) and are computed once when
// the handler is derived, not on every Handle:
//   - built is next with all goas applied — the static delivery chain WITHOUT
//     correlation_id. For the root handler (no goas) built is just next. It
//     serves every log line that does NOT need a per-record ID injected,
//     avoiding a per-call rebuild of the WithGroup/WithAttrs chain.
//   - rootHasID is whether a correlation_id is already bound at the root via
//     .With(...) before any group (the hasRootCorrelationID walk over goas),
//     used to suppress a duplicate root injection (AC #3).
//
// Only the inject-under-groups path (per-record ID present, groups open) still
// builds per record: the ID comes from THIS record's context and must be bound
// at the root before the caller's groups are reopened, and slog handlers are
// immutable, so that root attr cannot be pre-applied onto the cached chain
// without nesting under a group. See Handle for that path.
type slogHandler struct {
	next slog.Handler
	// goas records WithGroup/WithAttrs calls in the order they were made, so
	// Handle can replay them after injecting correlation_id at the root. It is
	// empty in the common case (no .With/.WithGroup on the default logger), which
	// makes Handle take the cheap delegated path.
	goas []goa
	// built is next with all goas already applied: the static delivery chain
	// WITHOUT correlation_id, computed once at construction. It is exactly next
	// for the root handler (no goas). Handle uses it to deliver any record that
	// does not need a per-record correlation_id injected, so the group/attr chain
	// is not rebuilt on every call.
	built slog.Handler
	// rootHasID caches hasRootCorrelationID(goas): true when a correlation_id is
	// bound at the root before any group. Computed once at construction; used to
	// suppress a duplicate root injection.
	rootHasID bool
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
	return newSlogHandler(next, nil)
}

// newSlogHandler constructs a slogHandler and computes the cached fields (built,
// rootHasID) once from next and goas. Every derived handler (WithAttrs,
// WithGroup) routes through here so the cache is always consistent with goas and
// no cache computation happens in Handle.
func newSlogHandler(next slog.Handler, goas []goa) *slogHandler {
	// built is the static delivery chain WITHOUT correlation_id: next with every
	// goa applied. For the root handler (no goas) it is exactly next.
	built := next
	for _, g := range goas {
		if g.group != "" {
			built = built.WithGroup(g.group)
		} else {
			built = built.WithAttrs(g.attrs)
		}
	}
	return &slogHandler{
		next:      next,
		goas:      goas,
		built:     built,
		rootHasID: hasRootCorrelationID(goas),
	}
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
// to add, the record is delivered through the cached built chain untouched; with
// no recorded goa steps built is exactly next, so absent-ID output matches the
// unwrapped handler exactly.
//
// When correlation_id must be injected under recorded goa steps, the delivery
// handler is rebuilt per record from the ungrouped root: correlation_id is bound
// at the root first, then the goa steps are replayed (groups reopened, attrs
// rebound) so the record's own attributes keep their grouping while
// correlation_id stays at the top level. Every other case is served by the
// cached delivery chain, which is not rebuilt per call.
func (h *slogHandler) Handle(ctx context.Context, rec slog.Record) error {
	id := From(ctx)
	// rootHasID is cached (fixed for this handler); only hasCorrelationID depends
	// on the record and is checked per call.
	addID := id != "" && !hasCorrelationID(rec) && !h.rootHasID

	if !addID {
		// No per-record ID to inject: deliver through the cached chain. For the
		// root handler (no goas) built is exactly next, preserving the byte-for-byte
		// unwrapped output for absent-ID / no-goas logs. For a grouped/attrs handler
		// built already has all goas applied, so no chain is rebuilt here.
		return h.built.Handle(ctx, rec)
	}

	if len(h.goas) == 0 {
		// Fast path: no groups, so correlation_id is naturally at the root. Clone
		// before mutating: slog may pass the same Record to more than one handler,
		// and rec.Add appends to the Record's own attribute slice, so cloning keeps
		// our added correlation_id local to this delivery.
		rec = rec.Clone()
		rec.Add(slog.String(attrKey, id))
		return h.next.Handle(ctx, rec)
	}

	// Inject-under-groups path: this is the ONLY path that must build per record,
	// and it cannot be precomputed. The ID comes from THIS record's context, and
	// it must be bound at the ROOT before the caller's groups are reopened. slog
	// handlers are immutable, so we cannot pre-apply a per-record root attr onto
	// the cached (already-grouped) built chain without the attr nesting under a
	// group. So bind correlation_id on the ungrouped next first, then replay the
	// goa steps.
	delivery := h.next.WithAttrs([]slog.Attr{slog.String(attrKey, id)})
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
// caller explicitly chose to set for that line. It takes no handler state, so it
// is a package function called per record.
func hasCorrelationID(rec slog.Record) bool {
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
//
// It depends only on goas (fixed for a handler), so it is computed once at
// construction and cached in slogHandler.rootHasID rather than run per Handle.
func hasRootCorrelationID(goas []goa) bool {
	for _, g := range goas {
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
	return newSlogHandler(h.next, appendGoa(h.goas, goa{attrs: attrs}))
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
	return newSlogHandler(h.next, appendGoa(h.goas, goa{group: name}))
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
