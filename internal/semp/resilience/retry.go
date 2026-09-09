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

// Package resilience provides shared HTTP retry and rate-limiting logic for
// SEMP clients (both v1 and v2). It wraps hashicorp/go-retryablehttp with a
// custom retry policy matching the Solace Terraform Provider approach.
package resilience

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/hashicorp/go-retryablehttp"
)

// maxTransientRetries caps how many times a single request is retried against
// 429/503 ("broker overloaded/unavailable") responses, independently of the
// larger RetryMax (config DefaultRetries, default 10). Retrying an overloaded
// broker up to 10 times — and outside the per-broker rate limiter — amplifies
// load precisely when the broker can least handle it. This bounds a transient
// episode to 1 + maxTransientRetries requests. It is a resilience-policy
// sub-cap, not an operator knob, so it lives here rather than in config.
const maxTransientRetries = 3

// errTransientCapReached is returned from checkRetry when maxTransientRetries is
// hit. Returning an error (rather than a bare "don't retry") routes the request
// through the Sender's errorHandler, so the caller sees the same
// *RetriesExhaustedError it already handles for RetryMax exhaustion.
var errTransientCapReached = errors.New("transient-error (429/503) retry cap reached")

// errNonIdempotentNotRetried is returned from checkRetry when the gate refuses
// to replay a caller-declared non-idempotent request on a status that would
// otherwise have been retried.
//
// Suppressing the replay is only half the job — the agent must not reissue the
// call either. Returning an error routes the request through errorHandler, so
// the caller gets a *RetriesExhaustedError that Do marks NonIdempotent and
// internal/tools reports as non-retryable. Returning a bare "don't retry" hands
// the raw 503 back instead, and a 503 is unconditionally retryable to the tools
// layer: the agent would be told to repeat a purge the broker may already have
// carried out. Same reasoning, and the same shape, as errTransientCapReached.
var errNonIdempotentNotRetried = errors.New("request declared non-idempotent, not retried")

// retryStateKey is the context key for per-request retry state.
type retryStateKey struct{}

// retryState tracks per-request retry decisions to enforce the retry caps: the
// "retry once" limits for 401 re-auth (auth401Retried) and non-429/503 5xx
// (other5xxRetried), plus the maxTransientRetries cap for 429/503
// (transientRetried). It also carries attempt, the 1-based try counter, and the
// in-flight attempt span. Each Do() call creates its own instance via context,
// so concurrent requests to the same Sender are safe.
//
// Every field is written and read on the single goroutine driving
// retryablehttp's Do loop for that request — the transport wrapper, checkRetry
// and prepareRetry all run there, in sequence — so no field needs a lock.
type retryState struct {
	auth401Retried   bool   // true after first 401 re-auth attempt
	authRecovered    bool   // true iff the most recent response was non-401 after a 401 (flips back to false on another 401; see checkRetry)
	other5xxRetried  bool   // true after first non-429/503 5xx retry
	transientRetried int    // count of 429/503 retries taken (capped at maxTransientRetries)
	method           string // HTTP method captured at Do() time for idempotency check
	retrySafe        bool   // caller-declared semantic idempotency (see WithRetrySafe)
	retryUnsafe      bool   // caller-declared semantic NON-idempotency (see WithRetryUnsafe)
	needsReauth      bool   // true when the next retry should re-run AddAuth (set on 401)
	attempt          int    // 1-based try counter, bumped by attemptTransport
	// attemptSpan is the span attemptTransport opened for the attempt now in
	// flight, parked here so checkRetry can tag it with the decision it
	// returns and close it (see attempt_span.go). nil between attempts.
	attemptSpan trace.Span
}

// retrySafeKey is the context key for the caller-declared retry-safe marker.
type retrySafeKey struct{}

// WithRetrySafe marks the request issued under this context as semantically
// idempotent, enabling the full retry policy even over a non-idempotent HTTP
// method. SEMPv1 is an RPC protocol where read-only <show> commands travel
// over POST; without this marker the method-based guard would deny them all
// retries (429/503 backoff, connection errors, 401 re-auth). Callers must
// only set this for requests that are safe to repeat.
func WithRetrySafe(ctx context.Context) context.Context {
	return context.WithValue(ctx, retrySafeKey{}, true)
}

// isRetrySafe reports whether the caller marked the request retry-safe via
// WithRetrySafe.
func isRetrySafe(ctx context.Context) bool {
	v, _ := ctx.Value(retrySafeKey{}).(bool)
	return v
}

// retryUnsafeKey is the context key for the caller-declared non-idempotent marker.
type retryUnsafeKey struct{}

// WithRetryUnsafe marks the request issued under this context as semantically
// NON-idempotent, suppressing every retry path on which the broker may already
// have acted on the request.
//
// The method-based guard in checkRetry cannot infer this. RFC 9110 idempotency
// constrains a request's final state, not the set of side effects produced along
// the way, and SEMPv2's action API routes destructive RPC over PUT: a replayed
// doMsgVpnQueueDeleteMsgs takes no message-ID range, so it purges whatever has
// been spooled since the caller's request. Callers that know an operation is
// non-idempotent — the composite executor, from a tool's `idempotent: false`
// annotation — declare it here.
//
// This is the inverse of WithRetrySafe: that widens the policy for a request a
// method-based check would wrongly deny, this narrows it for one a method-based
// check would wrongly allow.
//
// Granularity: the composite executor sets this once per tool invocation, so
// every step of a non-idempotent tool inherits it — a read-only lookup step
// inside such a tool also loses its retries. That is safe but imprecise, and
// harmless today because both marked tools are single-step. A multi-step write
// tool would want this applied per operation instead.
func WithRetryUnsafe(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryUnsafeKey{}, true)
}

// isRetryUnsafe reports whether the caller declared the request non-idempotent
// via WithRetryUnsafe.
func isRetryUnsafe(ctx context.Context) bool {
	v, _ := ctx.Value(retryUnsafeKey{}).(bool)
	return v
}

// OperationIDKey is the context key callers use to attach an operation
// identifier for logging. The Sender reads it in errorHandler and checkRetry.
type OperationIDKey struct{}

// getRetryState retrieves the per-request retryState from the context.
// The key is attached by Sender.Do() before each request; callers must go
// through Do() so the retry caps (auth401Retried, other5xxRetried, transientRetried)
// are enforced. If the key is missing (direct retryablehttp use bypassing
// Do), the fresh retryState means every cap starts at its zero value,
// effectively allowing full RetryMax retries instead of the intended limits.
func getRetryState(ctx context.Context) *retryState {
	if s, ok := ctx.Value(retryStateKey{}).(*retryState); ok {
		return s
	}
	return &retryState{}
}

// getRetryStateOrNil returns the per-request retryState on ctx, or nil when
// there is none. Unlike getRetryState it does NOT substitute a fresh state:
// callers that only observe a request (the transport wrapper, the attempt span)
// must be able to tell "no chain to attribute this to" apart from "a chain
// whose caps all happen to be at zero".
func getRetryStateOrNil(ctx context.Context) *retryState {
	s, _ := ctx.Value(retryStateKey{}).(*retryState)
	return s
}

// attemptNumber reports the 1-based try counter for the attempt now in flight.
// attemptTransport is the counter's single owner and bumps it once per attempt
// (see attempt_span.go); every other reader — metricsTransport's `attempt`
// label, the attempt span's `attempt` attribute — reads it here so the two can
// never disagree.
//
// Returns 1 when no state is on the context (Sender.Do was bypassed) or the
// counter has not been bumped: the attempt is real but uncounted, and reporting
// it as attempt 1 is closer to the truth than reporting attempt 0.
func attemptNumber(ctx context.Context) int {
	s := getRetryStateOrNil(ctx)
	if s == nil || s.attempt == 0 {
		return 1
	}
	return s.attempt
}

// checkRetry is the custom retry policy for retryablehttp. It implements:
//   - POST and PATCH: never retried (non-idempotent — see guard below)
//   - 401: delegate to Authenticator.HandleAuthFailure — retry once if it recovers
//   - Caller-declared non-idempotent (WithRetryUnsafe): 401 re-auth only, never
//     replayed on a transient status or a connection error
//   - 429, 503: retry with exponential backoff, capped at maxTransientRetries
//   - Other 5xx: retry once only (likely a bug, not transient)
//   - Connection errors: delegate to retryablehttp's default policy
//   - All other status codes (4xx): no retry
//
// The named returns exist for the deferred endAttemptSpan call below, which
// closes the attempt span attemptTransport opened and tags it with the decision
// this function actually returns (SOL-152422). Deferring it once here, rather
// than editing each of the many exits, is what guarantees the span reports the
// real decision on every path — including the two sentinel-error exits, which a
// span site reading only the status code would misreport.
func (d *Sender) checkRetry(ctx context.Context, resp *http.Response, err error) (retry bool, checkErr error) {
	// allowanceSpent records that one of the sub-caps below refused the replay
	// because its own budget was already used, which is what `retry.exhausted`
	// reports (see retriesExhausted). A local rather than a field on
	// retryState: scoped to this one call, so a value set on one attempt can
	// never be read by the span of a later one.
	var allowanceSpent bool
	defer func() {
		endAttemptSpan(ctx, d.retryClient.RetryMax, resp, retry, allowanceSpent)
	}()

	// Context cancellation: never retry.
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	// Non-idempotent methods are never retried unless the caller explicitly
	// marked the request retry-safe (WithRetrySafe — e.g. SEMPv1 read-only
	// <show> commands, which travel over POST). POST and PATCH can produce
	// side effects (resource creation, partial config update) that are not safe
	// to repeat even on transient errors — a double-write is worse than a
	// visible failure. This check covers both HTTP errors and connection errors.
	//
	// PUT and DELETE are deliberately NOT in this guard: RFC 9110 §9.2.2 defines
	// both as idempotent, so a retry yields the same final state as a single call.
	// SEMPv2 routes resource replacement through PUT; treating it as non-idempotent
	// would defeat the retry policy for legitimately transient failures.
	state := getRetryState(ctx)
	if (state.method == http.MethodPost || state.method == http.MethodPatch) && !state.retrySafe {
		// A caller-declared non-idempotent request takes the same exit as it
		// would below, sentinel and all. Both guards refuse the replay, but only
		// the sentinel stops the tools layer reporting a 429/503 as retryable and
		// inviting the agent to reissue by hand. Without this the guarantee would
		// be method-conditional: it holds for the PUT-based action tools shipping
		// today and would vanish, silently, for the first `idempotent: false`
		// tool built on a POST config operation.
		if state.retryUnsafe && resp != nil && err == nil && retryableStatus(resp.StatusCode) {
			slog.Debug("not retrying: caller declared the request non-idempotent",
				slog.String("broker", d.brokerURL),
				slog.Int("status", resp.StatusCode))
			return false, errNonIdempotentNotRetried
		}
		return false, nil
	}

	// Connection errors: delegate to retryablehttp's default policy which
	// handles network errors, DNS failures, TLS handshake errors, etc.
	//
	// A caller-declared non-idempotent request is never replayed here. The
	// broker may have received and executed it before the connection broke —
	// a TCP reset, a broker restart mid-request, or a load-balancer idle drop
	// all look identical to the client — so a retry can duplicate an
	// irreversible side effect. Returning (false, nil), the same shape the
	// method guard above uses, leaves the original transport error as the
	// caller-visible cause; returning a sentinel here would mask it, because
	// retryablehttp prefers CheckRetry's error over the request's own.
	//
	// Logged, and at the same level as the status-code gate below: of the three
	// suppressions this is the ambiguous one. A 503 is very probably a
	// pre-execution rejection, but a mid-flight reset is exactly the case where
	// the broker may already have purged the queue, and nothing downstream
	// records it — errorHandler logs only when resp != nil. Without this line an
	// operator asking "did the purge run?" gets evidence for the safe case and
	// silence for the dangerous one.
	if err != nil {
		if state.retryUnsafe {
			slog.Debug("not retrying: caller declared the request non-idempotent",
				slog.String("broker", d.brokerURL),
				slog.String("cause", "transport error"))
			return false, nil
		}
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}

	if resp == nil {
		return false, nil
	}

	// 401 is handled ahead of the non-idempotency gate below, and deliberately
	// so: the broker issues an authentication rejection before acting on the
	// request, so re-authenticating and retrying cannot duplicate a side effect.
	// Gating it would turn a token expiring mid-request into a hard failure for
	// exactly the write operations that most need to complete.
	//
	// This function only ever SETS state.auth401Retried; it never decides or
	// emits the broker_auth_retry audit record (SOL-152097). checkRetry can run
	// many more times after a 401 is recovered — a 503 working through the
	// transient cap, a second 401, a connection error — and several paths that
	// matter (a transport error, ctx cancellation during backoff, prepareRetry's
	// AddAuth itself failing) never re-enter checkRetry at all. Deciding the
	// outcome here, mid-chain, either double-emits or silently drops the record
	// depending on which of those paths the chain happens to take next. The
	// decision is made exactly once, from the chain's actual final result, by
	// Sender.Do's auditBrokerAuthRetryOutcome after d.retryClient.Do returns.
	if resp.StatusCode == http.StatusUnauthorized { // 401
		// Any 401 — first or persisted — means the credential problem is
		// unresolved as of this attempt. Cleared here rather than only ever
		// set once below, so a chain that recovers and then hits a fresh 401
		// (state.authRecovered would otherwise still read true from the
		// earlier non-401 response) reports the persisted failure it actually
		// ended in, not the transient recovery partway through.
		state.authRecovered = false
		if !state.auth401Retried {
			state.auth401Retried = true
			// The authenticator decides both retry and re-auth; relay ReAuth
			// into needsReauth, which PrepareRetry keys off.
			result := d.authenticator.HandleAuthFailure(ctx, resp.Header)
			if result.Retry {
				state.needsReauth = result.ReAuth
				slog.Warn("retrying: 401 received, auth handler signalled retry",
					slog.String("broker", d.brokerURL))
			} else {
				slog.Warn("auth failure: 401 received, auth handler cannot recover",
					slog.String("broker", d.brokerURL))
			}
			return result.Retry, nil
		}
		// The once-only 401 re-auth allowance is spent: an attempt was made and
		// the 401 came back anyway. Distinct from the authenticator declining
		// the first 401 above, which is "cannot recover", not "budget used".
		allowanceSpent = true
		slog.Warn("auth failure: 401 persisted after recovery attempt",
			slog.String("broker", d.brokerURL))
		return false, nil
	}

	// Reached only for a non-401 response. Once a 401 retry has happened for
	// this request (auth401Retried), any later non-401 response means the
	// credential problem is resolved as of this attempt — independent of
	// whether this particular response goes on to be retried or is itself
	// terminal, which the switch below (and ultimately Do's own return)
	// decides separately. auditBrokerAuthRetryOutcome reads this flag rather
	// than re-deriving the same fact from the chain's final resp/err, because
	// a later retry cap (transient 429/503, other-5xx) exhausting routes
	// through errorHandler, which nils out resp — see that function's doc.
	if state.auth401Retried {
		state.authRecovered = true
	}

	// Past 401, every remaining retry path replays a request the broker may
	// already have acted on: a 5xx can follow partial execution, and a 504 can
	// fire while the broker is still working. A caller-declared non-idempotent
	// request must fail visibly instead. This is the gate that stops an
	// `idempotent: false` action tool — delete-queue-messages, whose purge takes
	// no message-ID range — from destroying data the caller never authorized.
	//
	// Scoped to the statuses the switch below would actually replay. retryablehttp
	// calls CheckRetry on every response, 2xx included, so an unscoped gate would
	// claim "not retrying" on each successful action — suppression that never
	// happened. Behaviour is unchanged: every other status falls through to a
	// switch arm that returns false anyway.
	if state.retryUnsafe && retryableStatus(resp.StatusCode) {
		// Downgraded to DEBUG for the same reason errTransientCapReached is:
		// errorHandler logs an ERROR for this same terminal failure, so a higher
		// level here would double-log every gated action.
		slog.Debug("not retrying: caller declared the request non-idempotent",
			slog.String("broker", d.brokerURL),
			slog.Int("status", resp.StatusCode))
		return false, errNonIdempotentNotRetried
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests, // 429
		resp.StatusCode == http.StatusServiceUnavailable: // 503
		// Transient broker conditions: retry with exponential backoff, but only
		// up to maxTransientRetries. The broker is signalling overload/unavailability;
		// retrying it RetryMax (10) times — and outside the rate limiter — amplifies
		// load when it can least handle it. Cap the transient retries and fail fast
		// once the cap is hit so the caller sees a RetriesExhaustedError.
		if state.transientRetried >= maxTransientRetries {
			// Downgraded to DEBUG: the errorHandler logs an ERROR ("request failed
			// after retries exhausted", carrying the cap-reached error string) for
			// this same terminal failure, so a WARN here would double-log every
			// capped 429/503 — noisy precisely when the broker is overloaded.
			allowanceSpent = true
			slog.Debug("not retrying: transient-error retry cap reached",
				slog.String("broker", d.brokerURL),
				slog.Int("status", resp.StatusCode),
				slog.Int("cap", maxTransientRetries))
			return false, errTransientCapReached
		}
		state.transientRetried++
		slog.Debug("retrying: transient broker error",
			slog.String("broker", d.brokerURL),
			slog.Int("status", resp.StatusCode),
			slog.Int("attempt", state.transientRetried))
		return true, nil

	case resp.StatusCode >= 500:
		// Other 5xx (500, 502, 504, etc.): likely a bug or infrastructure issue.
		// Retry once to catch momentary glitches, but don't hammer a broken broker.
		if !state.other5xxRetried {
			state.other5xxRetried = true
			slog.Debug("retrying: server error, will retry once",
				slog.String("broker", d.brokerURL),
				slog.Int("status", resp.StatusCode))
			return true, nil
		}
		// The once-only replay for a non-429/503 5xx is spent.
		allowanceSpent = true
		return false, nil

	default:
		// 4xx (except 401/429): client errors, no retry.
		return false, nil
	}
}

// retryableStatus reports whether the retry switch above would replay this
// status for an idempotent request: 429 and 503 as transient, any other 5xx
// once. Keep it in step with that switch — the non-idempotency gate uses it to
// fire only on responses that were genuinely retry candidates.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// auditBrokerAuthRetryOutcome decides and emits the broker_auth_retry record
// (SOL-152097) exactly once, at Sender.Do's true terminal point — after the
// whole retry chain (every attempt, including any 429/503/other-5xx retries
// that followed a 401 recovery) has concluded one way or another. Called from
// Do after d.retryClient.Do(retryReq) returns.
//
// This cannot be decided from inside checkRetry: that function can run many
// more times after a 401 is recovered, and several paths that matter —a
// transport error, ctx cancellation during backoff, prepareRetry's AddAuth
// itself failing on the re-auth — never re-enter checkRetry at all, so a
// decision made there either double-emits (a 401 that recovers into a 503
// which then exhausts its own retry cap and finally 401s again) or silently
// drops the record on the paths that skip checkRetry entirely.
//
// state.auth401Retried is the only gate: false means this request never saw a
// 401, so there is nothing to report. Once true, outcome comes from
// state.authRecovered — maintained inside checkRetry as the credential status
// of the most recently observed response (cleared on a 401, set on a non-401
// that follows one — see checkRetry) — rather than from Do's own final
// resp/err. Those two do not carry the fact reliably: a chain that recovers
// the 401 and then exhausts a *later* retry cap (the maxTransientRetries cap
// on 429/503, or the other-5xx one-retry limit) routes through errorHandler,
// which returns a nil *http.Response on every populated path — so resp != nil
// would always be false on exactly the chains where the two most commonly
// diverge, and every one of them would misreport a resolved credential as
// unresolved. outcome is success when the credential problem was resolved as
// of the chain's last real response, whatever this call's own final
// disposition turns out to be — a separate question the caller's returned
// error answers — and error otherwise: a persisted or recurring 401, a
// transport failure, a context cancellation, or the re-auth attempt itself
// failing inside prepareRetry.
func (d *Sender) auditBrokerAuthRetryOutcome(ctx context.Context) {
	state := getRetryState(ctx)
	if !state.auth401Retried {
		return
	}
	outcome := audit.OutcomeError
	if state.authRecovered {
		outcome = audit.OutcomeSuccess
	}
	d.auditBrokerAuthRetry(ctx, outcome)
}

// auditBrokerAuthRetry emits a broker_auth_retry record (SOL-152097), gated
// by d.auditLog (mirrors tools.WithAuditLog's own gate — off is inert, not
// degraded). outcome reports whether the 401 recovery attempt resolved the
// authentication failure for this request: OutcomeSuccess once a later
// response is no longer itself a 401, OutcomeError when the authenticator
// declined to retry at all or the 401 persisted after the attempt.
func (d *Sender) auditBrokerAuthRetry(ctx context.Context, outcome audit.Outcome) {
	if !d.auditLog {
		return
	}
	event, err := audit.NewEvent(ctx, audit.Fields{
		Type:    audit.EventBrokerAuthRetry,
		Outcome: outcome,
		Broker:  d.brokerAlias,
	})
	if err != nil {
		// Same build-or-drop shape as every other emission site in this
		// codebase (see internal/tools/manager.go's emitOperationAudit):
		// the constructor rejected the record, so there is nothing valid to
		// write — say so via a drop rather than let the record's absence
		// read as "no 401 recovery was attempted".
		slog.ErrorContext(ctx, "audit: broker_auth_retry record rejected by the schema constructor; recording a drop",
			slog.String("broker", d.brokerAlias),
			slog.String("detail", err.Error()))
		audit.EmitDrop(ctx, audit.DropContext{DroppedEventType: audit.EventBrokerAuthRetry, Broker: d.brokerAlias})
		return
	}
	audit.Emit(ctx, event)
}
