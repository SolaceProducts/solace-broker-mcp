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

package resilience

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// tracer is this layer's named tracer (SOL-152422), so a backend attributes
// each attempt span to the source that produced it rather than to one
// server-wide scope. Resolved lazily by the OTel global delegation, so it works
// whether or not main() has installed a provider yet, and is a cheap no-op when
// tracing is off.
var tracer = otel.Tracer("solace-broker-mcp/semp/resilience")

// attemptSpanName is the span for ONE SEMP request attempt (SOL-152422,
// Story 27), nested under the `semp.request` span the protocol clients start
// around the whole retry chain. Two spans rather than one is the point: an
// operator reading a retry storm needs to see how many attempts ran and which
// one finally succeeded, which a single chain-wide span cannot show.
const attemptSpanName = "semp.attempt"

// attemptTransport wraps an http.RoundTripper to own the per-attempt window.
// retryablehttp calls the transport once per try, so this is the one place
// below its internal retry loop that sees every attempt — the same reason
// metricsTransport lives here (Story 16).
//
// It owns two things:
//
//   - The 1-based attempt counter on the per-request retryState, read by
//     metricsTransport for its `attempt` metric label and by the attempt span
//     for its `attempt` attribute. ONE owner is what keeps the label and the
//     attribute in step, as SOL-152422 requires. Before this wrapper the
//     counter was bumped by metricsTransport itself, so it only advanced when
//     metrics were enabled — tracing alone would have reported every attempt
//     as attempt 1.
//   - Starting the attempt span. It is deliberately NOT ended here: the
//     `retry.decision` attribute has to come from checkRetry's actual returned
//     decision, and retryablehttp calls CheckRetry only after the transport has
//     returned. So the span handle is parked on the shared retryState and
//     closed by endAttemptSpan from inside checkRetry. See Sender.Do for the
//     backstop that guarantees the handle is never left open.
//
// Installed unconditionally (unlike metricsTransport): the cost when tracing is
// off is one interface call plus one integer increment per attempt, and a
// non-recording span from the no-op tracer, which is orders of magnitude below
// the HTTP round trip it wraps. Gating it on a tracing flag would add a knob
// whose off-state is never the operator's intent.
type attemptTransport struct {
	base http.RoundTripper
}

// RoundTrip bumps the attempt counter, opens the attempt span, and runs the
// try inside it.
//
// Redirect hops (req.Response != nil) are passed straight through, for the same
// reason metricsTransport skips them: a redirect is not a new SEMP attempt.
// Skipping them also keeps the span accounting honest — a second Start on the
// same attempt would overwrite the parked handle and leak the first span.
//
// A request with no retryState on its context reached retryablehttp without
// going through Sender.Do. The attempt is real but unattributable to a chain,
// so it is passed through untraced rather than given a span that claims an
// attempt number and a retry decision nothing will ever fill in.
func (t *attemptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response != nil {
		return t.base.RoundTrip(req)
	}
	state := getRetryStateOrNil(req.Context())
	if state == nil {
		return t.base.RoundTrip(req)
	}

	state.attempt++

	// The span goes into the context the base transport sees, not just into
	// retryState, so the attempt is the active span for the duration of the
	// try. Nothing nests under it today; doing it this way means anything added
	// below this layer later (an otelhttp-instrumented transport, say) parents
	// to the attempt rather than to the whole chain.
	ctx, span := tracer.Start(req.Context(), attemptSpanName, trace.WithSpanKind(trace.SpanKindClient))
	state.attemptSpan = span

	return t.base.RoundTrip(req.WithContext(ctx))
}

// CloseIdleConnections forwards to the wrapped transport.
//
// Required, not politeness. http.Client.CloseIdleConnections reaches the
// transport by type assertion on an unexported `closeIdler` interface, so a
// wrapper without this method turns the call into a silent no-op — and
// retryablehttp calls it at three points that all mean "this chain went wrong,
// do not reuse these connections": a request-body rewind failure, the context
// ending during backoff, and a deferred call on every failure path before
// ErrorHandler. Because this wrapper is installed unconditionally, omitting the
// method would disable that hygiene on the default configuration for every
// broker, invisibly.
func (t *attemptTransport) CloseIdleConnections() {
	forwardCloseIdleConnections(t.base)
}

// forwardCloseIdleConnections calls CloseIdleConnections on rt when it has one,
// so a chain of wrappers relays it down to the real *http.Transport. Every
// wrapper in the chain has to relay, or the first one that does not breaks it.
func forwardCloseIdleConnections(rt http.RoundTripper) {
	type closeIdler interface{ CloseIdleConnections() }
	if c, ok := rt.(closeIdler); ok {
		c.CloseIdleConnections()
	}
}

// endAttemptSpan closes the attempt span opened by attemptTransport, tagging it
// with the retry decision checkRetry is about to return. Called from
// checkRetry's own defer, so it fires on every one of that function's exits
// without any of them having to remember to.
//
// retry and checkErr are checkRetry's actual return values, read through named
// returns. That is the whole point of closing the span here rather than in the
// transport: SOL-152422 requires `retry.decision` to BE the decision the code
// acted on, not a second opinion re-derived from the status code or the error.
// Re-deriving would let the span disagree with the retry that actually
// happened, which is the one thing this span exists to show — a POST that a
// broker answered with 503 is NOT retried (the non-idempotent method guard), so
// a status-derived attribute would report a retry that never occurred.
//
// A nil parked span means either that this request never went through
// attemptTransport, or that checkRetry ran twice for one attempt (which
// retryablehttp does when a Request carries a responseHandler — nothing in this
// codebase sets one). Both are no-ops rather than errors.
func endAttemptSpan(ctx context.Context, retryMax int, resp *http.Response, retry, allowanceSpent bool) {
	state := getRetryStateOrNil(ctx)
	if state == nil || state.attemptSpan == nil {
		return
	}
	span := state.attemptSpan
	// Cleared before End so a second call for the same attempt cannot end the
	// same span twice, and so Sender.Do's backstop sees nothing left to close.
	state.attemptSpan = nil

	// IsRecording guard: a non-recording span (tracing off, or this trace
	// unsampled) still needs End(), but building attribute values for a span
	// nothing will read is pure waste on a path every broker call goes through.
	if span.IsRecording() {
		attrs := []attribute.KeyValue{
			// Same value, same source, as metricsTransport's `attempt` metric
			// label — see attemptTransport for why the counter has one owner.
			attribute.Int("attempt", state.attempt),
			attribute.Bool("retry.decision", retry),
		}
		// Omitted rather than written empty when the request carries no ID,
		// matching how correlation_id is handled on every other span and audit
		// record (internal/tools/spans.go, docs/observability.md).
		//
		// Every attempt of one request reads the SAME context value here, so
		// all of them carry the byte-identical ID the correlation middleware
		// resolved once for the request — never a per-attempt regeneration.
		// That identity is what lets an operator carry one ID from a retry
		// storm in the trace into the broker-side command log.
		if id := correlation.From(ctx); id != "" {
			attrs = append(attrs, attribute.String("correlation_id", id))
		}
		// The status this attempt actually got. Absent on a connection error,
		// where there was no response at all. This is what answers "which
		// attempt succeeded".
		if resp != nil {
			attrs = append(attrs, attribute.Int("http.response.status_code", resp.StatusCode))
		}
		if retriesExhausted(retryMax, state.attempt, retry, allowanceSpent) {
			attrs = append(attrs, attribute.Bool("retry.exhausted", true))
		}
		span.SetAttributes(attrs...)
	}

	// No `outcome` attribute and no error span status, deliberately, unlike
	// every other span in docs/observability.md's table. An attempt is not a
	// call: the 503 that got retried and then succeeded is a normal step of a
	// healthy call, and reporting it as outcome=error would put an error span
	// under a successful `semp.request` on every retried call, inflating the
	// error views a trace backend builds off exactly that filter. The retry
	// decision and the status code describe an attempt; the call's outcome
	// lives on the parent span.
	span.End()
}

// closeDanglingAttemptSpan ends an attempt span that was opened but never
// closed by checkRetry, so it is exported (undecided) rather than dropped.
//
// A backstop, not a normal path, and unreachable on today's retryablehttp,
// where CheckRetry runs immediately after every dispatch. No behavioural test
// can therefore pin the deferred call site in Sender.Do — deleting that defer
// leaves the suite green, and that is inherent to guarding an unreachable
// state, not an oversight. What it bounds is the failure mode if a future
// library version grows an exit between the dispatch and CheckRetry: a span
// with no decision, rather than a span the SDK drops entirely because it was
// never ended. This method itself is tested directly.
//
// No attributes are written. A span reaching here has no decision to report,
// and inventing one would be exactly the re-derivation SOL-152422 forbids; its
// bare presence, with an `attempt` number and no `retry.decision`, is the
// honest signal.
func (s *retryState) closeDanglingAttemptSpan() {
	if s.attemptSpan == nil {
		return
	}
	span := s.attemptSpan
	s.attemptSpan = nil
	span.End()
}

// retriesExhausted reports whether a retry allowance ran out on this attempt,
// which is the one thing `retry.exhausted` means: the policy stopped because
// something it was counting was already spent, not because the outcome was
// terminal on its own merits.
//
// Two ways that happens, both read off checkRetry's own decision plus real
// per-request state, never re-derived from the response:
//
//   - retry == true with no attempts left. retryablehttp breaks out of its loop
//     when `remain := RetryMax - i` is non-positive, with i the 0-based loop
//     index, so the last attempt it will make is attempt RetryMax+1. Past that
//     the decision to retry stands but is never acted on, and this attribute is
//     what tells the two apart in a trace.
//   - allowanceSpent — one of checkRetry's own sub-caps refused the replay
//     because its budget was already used: the maxTransientRetries cap on
//     429/503, the once-only replay for a non-429/503 5xx, or the once-only
//     401 re-auth. All three matter to an operator for the same reason
//     RetryMax does, and the 429/503 cap fires far below RetryMax, so at
//     default settings it — not RetryMax — is how a real broker-overload retry
//     storm ends. Covering only RetryMax would leave the storm this story
//     exists to make legible without the attribute.
//
// Deliberately NOT exhaustion, and the distinction is the point:
//
//   - A refused replay of a caller-declared non-idempotent request. Nothing ran
//     out; the policy declined to repeat a request the broker may already have
//     carried out. An operator seeing "exhausted" here would go looking for a
//     budget to raise when the answer is that the request is not replayable.
//   - A context that ended, and any status the policy never retries (2xx, 4xx).
//   - An authenticator that declined to retry the FIRST 401. That is the auth
//     mode saying it cannot recover, not a budget running out; the once-only
//     allowance is only spent on a 401 that persists after an attempt.
//
// `retry.decision = false` already says no retry happened. This attribute says
// why, and the two answers have different remedies.
func retriesExhausted(retryMax, attempt int, retry, allowanceSpent bool) bool {
	if retry {
		return attempt > retryMax
	}
	return allowanceSpent
}
