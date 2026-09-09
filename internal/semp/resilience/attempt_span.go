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

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/attemptspan"
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

// newAttemptTransport wraps base in the shared attemptspan.Transport, seeded
// from this request's retryState. Installed unconditionally (unlike
// metricsTransport): the cost when tracing is off is one interface call plus
// one integer increment per attempt, and a non-recording span from the no-op
// tracer, which is orders of magnitude below the HTTP round trip it wraps.
// Gating it on a tracing flag would add a knob whose off-state is never the
// operator's intent.
//
// The attempt counter it bumps (State.Attempt) is the SAME counter
// metricsTransport reads for its `attempt` metric label — see attemptNumber.
// ONE owner is what keeps the label and the span attribute in step, as
// SOL-152422 requires. Before this wrapper the counter was bumped by
// metricsTransport itself, so it only advanced when metrics were enabled —
// tracing alone would have reported every attempt as attempt 1.
func newAttemptTransport(base http.RoundTripper) *attemptspan.Transport {
	return &attemptspan.Transport{
		Base:     base,
		Tracer:   tracer,
		SpanName: attemptSpanName,
		GetState: func(ctx context.Context) *attemptspan.State {
			s := getRetryStateOrNil(ctx)
			if s == nil {
				return nil
			}
			return &s.spanState
		},
	}
}

// endAttemptSpan closes the attempt span opened by the attempt-span transport (via
// attemptspan.Finish), tagging it with the retry decision checkRetry is
// about to return and computed just now — not re-derived at some later
// point, since the decision retryablehttp actually acts on is the one and
// only thing this attribute may report.
//
// Called from checkRetry's own defer, so it fires on every one of that
// function's exits without any of them having to remember to. Reads
// retry/allowanceSpent through named returns for the same reason: named
// returns are the only way to see checkRetry's actual answer at the point
// the defer runs, since a return statement's expression is evaluated before
// named returns are assigned.
//
// The ordering this relies on — CheckRetry running once per attempt, right
// after RoundTrip returns — is not an assumption about "today's
// retryablehttp"; it is the library's documented contract on the exported
// CheckRetry type ("It is called following each request with the response
// and error values returned by the http.Client."), so a version that
// skipped or reordered the call would be a major-version break, not a quiet
// drift. The nine retry.decision value assertions in attempt_span_test.go
// exercise the real retryablehttp client end to end and would fail — an
// absent attribute is an untyped nil that matches neither true nor false —
// so the ordering is pinned by those tests, even though no test can pin
// Sender.Do's own backstop defer (closeDanglingAttemptSpan) directly: that
// path is unreachable on today's retryablehttp, where CheckRetry always
// runs before RoundTrip can return again.
func endAttemptSpan(ctx context.Context, retryMax int, resp *http.Response, retry, allowanceSpent bool) {
	state := getRetryStateOrNil(ctx)
	if state == nil {
		return
	}
	exhausted := retriesExhausted(retryMax, state.spanState.Attempt, retry, allowanceSpent)
	attemptspan.Finish(ctx, &state.spanState, resp, retry, exhausted)
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
// why, and the two answers have different remedies. This derivation is kept
// local to SEMP rather than folded into the shared attemptspan package: the
// IdP client (internal/idpclient) has only its own configured retry count and
// none of the sub-caps below, so the two packages' exhaustion rules are
// genuinely different policies, not one rule with two callers.
func retriesExhausted(retryMax, attempt int, retry, allowanceSpent bool) bool {
	if retry {
		return attempt > retryMax
	}
	return allowanceSpent
}
