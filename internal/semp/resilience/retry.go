// Package resilience provides shared HTTP retry and rate-limiting logic for
// SEMP clients (both v1 and v2). It wraps hashicorp/go-retryablehttp with a
// custom retry policy matching the Solace Terraform Provider approach.
package resilience

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hashicorp/go-retryablehttp"
)

// retryStateKey is the context key for per-request retry state.
type retryStateKey struct{}

// retryState tracks per-request retry decisions to enforce "retry once" limits.
// Each Do() call creates its own instance via context, so concurrent requests
// to the same Sender are safe.
type retryState struct {
	auth401Retried  bool   // true after first 401 re-auth attempt
	other5xxRetried bool   // true after first non-429/503 5xx retry
	method          string // HTTP method captured at Do() time for idempotency check
	retrySafe       bool   // caller-declared semantic idempotency (see WithRetrySafe)
	needsReauth     bool   // true when the next retry should re-run AddAuth (set on 401)
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

// OperationIDKey is the context key callers use to attach an operation
// identifier for logging. The Sender reads it in errorHandler and checkRetry.
type OperationIDKey struct{}

// getRetryState retrieves the per-request retryState from the context.
// The key is attached by Sender.Do() before each request; callers must go
// through Do() so the "retry once" caps (auth401Retried, other5xxRetried)
// are enforced. If the key is missing (direct retryablehttp use bypassing
// Do), the fresh retryState means both caps start at false — effectively
// allowing full RetryMax retries instead of the intended one-shot limits.
func getRetryState(ctx context.Context) *retryState {
	if s, ok := ctx.Value(retryStateKey{}).(*retryState); ok {
		return s
	}
	return &retryState{}
}

// checkRetry is the custom retry policy for retryablehttp. It implements:
//   - POST and PATCH: never retried (non-idempotent — see guard below)
//   - 401: delegate to Authenticator.HandleAuthFailure — retry once if it recovers
//   - 429, 503: full retries with exponential backoff
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
	if err != nil {
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}

	if resp == nil {
		return false, nil
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized: // 401
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

	case resp.StatusCode == http.StatusTooManyRequests, // 429
		resp.StatusCode == http.StatusServiceUnavailable: // 503
		// Transient broker conditions: full retries with exponential backoff.
		slog.Debug("retrying: transient broker error",
			slog.String("broker", d.brokerURL),
			slog.Int("status", resp.StatusCode))
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
