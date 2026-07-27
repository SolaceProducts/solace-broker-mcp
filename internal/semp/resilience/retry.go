// Package resilience provides shared HTTP retry and rate-limiting logic for
// SEMP clients (both v1 and v2). It wraps hashicorp/go-retryablehttp with a
// custom retry policy matching the Solace Terraform Provider approach.
package resilience

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

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

// retryStateKey is the context key for per-request retry state.
type retryStateKey struct{}

// retryState tracks per-request retry decisions to enforce the retry caps: the
// "retry once" limits for 401 re-auth (auth401Retried) and non-429/503 5xx
// (other5xxRetried), plus the maxTransientRetries cap for 429/503
// (transientRetried). Each Do() call creates its own instance via context, so
// concurrent requests to the same Sender are safe.
type retryState struct {
	auth401Retried   bool   // true after first 401 re-auth attempt
	other5xxRetried  bool   // true after first non-429/503 5xx retry
	transientRetried int    // count of 429/503 retries taken (capped at maxTransientRetries)
	method           string // HTTP method captured at Do() time for idempotency check
	retrySafe        bool   // caller-declared semantic idempotency (see WithRetrySafe)
	retryUnsafe      bool   // caller-declared semantic NON-idempotency (see WithRetryUnsafe)
	needsReauth      bool   // true when the next retry should re-run AddAuth (set on 401)
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

// checkRetry is the custom retry policy for retryablehttp. It implements:
//   - POST and PATCH: never retried (non-idempotent — see guard below)
//   - 401: delegate to Authenticator.HandleAuthFailure — retry once if it recovers
//   - Caller-declared non-idempotent (WithRetryUnsafe): 401 re-auth only, never
//     replayed on a transient status or a connection error
//   - 429, 503: retry with exponential backoff, capped at maxTransientRetries
//   - Other 5xx: retry once only (likely a bug, not transient)
//   - Connection errors: delegate to retryablehttp's default policy
//   - All other status codes (4xx): no retry
func (d *Sender) checkRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
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
	if resp.StatusCode == http.StatusUnauthorized { // 401
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
		slog.Warn("auth failure: 401 persisted after recovery attempt",
			slog.String("broker", d.brokerURL))
		return false, nil
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
		slog.Debug("not retrying: caller declared the request non-idempotent",
			slog.String("broker", d.brokerURL),
			slog.Int("status", resp.StatusCode))
		return false, nil
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
