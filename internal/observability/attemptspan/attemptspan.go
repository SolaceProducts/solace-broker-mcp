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

// Package attemptspan holds the half of the per-HTTP-attempt span lifecycle
// that internal/semp/resilience and internal/idpclient share (SOL-152422,
// Story 27). Both packages open one span per retryablehttp attempt, tag it
// with the retry decision checkRetry actually returned — never one
// re-derived from the status code, which could disagree with the retry
// that really happened — and close it from checkRetry's own defer.
//
// What is NOT shared, deliberately: how a caller seeds its per-request
// state onto context (SEMP already has a retryState to hang it on;
// idpclient's *http.Client has no such seam and has to plant one via an
// outer RoundTripper), and how exhaustion is derived (SEMP has several
// allowances — a transient-retry cap, a once-only non-429/503 5xx replay, a
// once-only 401 re-auth — the IdP client has only its configured retry
// count). Each caller keeps its own retriesExhausted and passes the result
// in to Finish.
package attemptspan

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// State is the per-request bookkeeping an attempt span needs: the 1-based
// try counter and the span parked for the attempt now in flight. nil Span
// between attempts. Callers embed or wrap this in whatever per-request
// state they already carry (SEMP's retryState; idpclient's own small
// attemptSpanState, which adds the retry allowance Finish needs from its
// caller).
//
// No lock: retryablehttp drives one request's whole retry loop on a single
// goroutine, and Transport.RoundTrip, Finish and CloseDangling all run
// there in sequence.
type State struct {
	Attempt int
	Span    trace.Span
}

// Transport wraps an http.RoundTripper to open one span per HTTP attempt.
// retryablehttp calls the wrapped transport once per try, below its own
// retry loop, so this is the one place that sees every attempt.
//
// GetState extracts this request's *State from its context — the one thing
// the two production callers do differently, since SEMP's retryState
// already carries far more than attempt-span bookkeeping while idpclient
// seeds a bare state directly.
//
// The span is deliberately NOT ended here: `retry.decision` has to come
// from checkRetry's actual decision, and retryablehttp calls CheckRetry
// only after the transport has returned, so the span handle is parked on
// State and closed later by Finish.
type Transport struct {
	Base     http.RoundTripper
	Tracer   trace.Tracer
	SpanName string
	GetState func(context.Context) *State
}

// RoundTrip bumps the attempt counter, opens the attempt span, and runs the
// try inside it.
//
// Redirect hops (req.Response != nil) are passed straight through: a
// redirect is not a new attempt, and a second Start for the same attempt
// would overwrite the parked handle and leak the first span.
//
// A request whose context carries no State did not go through the caller's
// own seeding path — a test wiring retryablehttp by hand, say. The attempt
// is real but unattributable, so it is passed through untraced rather than
// given a span nothing will ever close.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response != nil {
		return t.Base.RoundTrip(req)
	}
	state := t.GetState(req.Context())
	if state == nil {
		return t.Base.RoundTrip(req)
	}

	state.Attempt++
	// The span goes into the context the base transport sees, so the
	// attempt is the active span for the duration of the try; anything
	// instrumented below this layer later nests under the attempt rather
	// than under the whole chain.
	ctx, span := t.Tracer.Start(req.Context(), t.SpanName, trace.WithSpanKind(trace.SpanKindClient))
	state.Span = span

	return t.Base.RoundTrip(req.WithContext(ctx))
}

// CloseIdleConnections forwards to the wrapped transport. See
// ForwardCloseIdleConnections for why every wrapper in a chain has to.
func (t *Transport) CloseIdleConnections() {
	ForwardCloseIdleConnections(t.Base)
}

// ForwardCloseIdleConnections calls CloseIdleConnections on rt when it has
// one, so a chain of wrappers relays the call down to the real
// *http.Transport.
//
// Required, not politeness. http.Client.CloseIdleConnections reaches the
// transport by type assertion on an unexported closeIdler interface, so any
// wrapper in the chain that omits this method turns the call into a silent
// no-op — and retryablehttp calls it at points that all mean "this chain
// went wrong, do not reuse these connections".
func ForwardCloseIdleConnections(rt http.RoundTripper) {
	type closeIdler interface{ CloseIdleConnections() }
	if c, ok := rt.(closeIdler); ok {
		c.CloseIdleConnections()
	}
}

// Finish closes the span parked on state (if any), tagging it with the
// attribute set both `semp.attempt` and `tokenexchange.attempt` share:
// attempt, retry.decision, correlation_id (when present), the response
// status (when there was a response), and retry.exhausted (when exhausted
// is true).
//
// Call this from the caller's own checkRetry defer, passing checkRetry's
// actual returned decision — never one re-derived from the status code or
// error, which could disagree with the retry that really happened. A POST
// a broker answered with 503 is NOT retried (the non-idempotent method
// guard), so a status-derived attribute would report a retry that never
// occurred.
//
// exhausted is the one thing Finish does not compute — see the package doc
// for why each caller derives its own.
//
// A nil state or a nil state.Span means either this request never went
// through Transport, or checkRetry ran twice for one attempt (which
// retryablehttp does when a Request carries a responseHandler — neither
// caller in this codebase sets one). Both are no-ops.
func Finish(ctx context.Context, state *State, resp *http.Response, retry, exhausted bool) {
	if state == nil || state.Span == nil {
		return
	}
	span := state.Span
	// Cleared before End so a second call for the same attempt cannot end
	// the same span twice, and so CloseDangling sees nothing left to close.
	state.Span = nil

	// IsRecording guard: a non-recording span (tracing off, or this trace
	// unsampled) still needs End(), but building attribute values for a
	// span nothing will read is pure waste on a path every call goes
	// through.
	if span.IsRecording() {
		attrs := []attribute.KeyValue{
			attribute.Int("attempt", state.Attempt),
			attribute.Bool("retry.decision", retry),
		}
		// Omitted rather than written empty when the request carries no
		// ID, matching how correlation_id is handled on every other span
		// and audit record.
		if id := correlation.From(ctx); id != "" {
			attrs = append(attrs, attribute.String("correlation_id", id))
		}
		// Absent on a connection error, where there was no response at
		// all. This is what answers "which attempt succeeded".
		if resp != nil {
			attrs = append(attrs, attribute.Int("http.response.status_code", resp.StatusCode))
		}
		if exhausted {
			attrs = append(attrs, attribute.Bool("retry.exhausted", true))
		}
		span.SetAttributes(attrs...)
	}

	// No `outcome` attribute and no error span status, deliberately, unlike
	// every other span in docs/observability.md's table. An attempt is not
	// a call: the 503 that got retried and then succeeded is a normal step
	// of a healthy call, and reporting it as outcome=error would put an
	// error span under a successful parent on every retried call. The
	// retry decision and the status code describe an attempt; the call's
	// outcome lives on the parent span.
	span.End()
}

// CloseDangling ends state's span if one is still parked, so it is exported
// (undecided) rather than dropped by the SDK entirely — an unended span is
// never exported at all.
//
// A backstop, not a normal path: on today's retryablehttp, CheckRetry runs
// immediately after every dispatch, so nothing should reach here. What it
// bounds is the failure mode if a future library version grows an exit
// between the dispatch and CheckRetry — a span with no decision, rather
// than a span the SDK drops entirely because it was never ended.
//
// No attributes are written. A span reaching here has no decision to
// report, and inventing one would be exactly the re-derivation this design
// forbids; its bare presence, with an `attempt` number and no
// `retry.decision`, is the honest signal.
func (s *State) CloseDangling() {
	if s.Span == nil {
		return
	}
	span := s.Span
	s.Span = nil
	span.End()
}
