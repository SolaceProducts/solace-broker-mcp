// Package resilience provides shared HTTP retry and rate-limiting logic for
// SEMP clients (both v1 and v2). It wraps hashicorp/go-retryablehttp with a
// custom retry policy matching the Solace Terraform Provider approach.
package resilience

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/hashicorp/go-retryablehttp"
)

// retryStateKey is the context key for per-request retry state.
type retryStateKey struct{}

// retryState tracks per-request retry decisions to enforce "retry once" limits.
// Each Do() call creates its own instance via context, so concurrent requests
// to the same Sender are safe.
type retryState struct {
	auth401Retried  bool // true after first 401 re-auth attempt (basic auth only)
	other5xxRetried bool // true after first non-429/503 5xx retry
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
//   - 401: basic auth → clear cookie jar + retry once; bearer → fail immediately
//   - 429, 503: full retries with exponential backoff
//   - Other 5xx: retry once only (likely a bug, not transient)
//   - Connection errors: delegate to retryablehttp's default policy
//   - All other status codes (4xx): no retry
func (d *Sender) checkRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// Context cancellation: never retry.
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	// Connection errors: delegate to retryablehttp's default policy which
	// handles network errors, DNS failures, TLS handshake errors, etc.
	if err != nil {
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}

	if resp == nil {
		return false, nil
	}

	state := getRetryState(ctx)

	switch {
	case resp.StatusCode == http.StatusUnauthorized: // 401
		if d.authCfg.Mode == config.AuthModeBasic && !state.auth401Retried {
			state.auth401Retried = true
			// Stale session cookie is the most likely cause. Clear the jar so
			// the retried request re-sends Basic Auth credentials from scratch.
			// This assignment races with http.Client.Do reads of the Jar field
			// (see Sender doc comment) but is benign.
			jar, _ := cookiejar.New(nil)
			d.httpClient.Jar = jar
			slog.Warn("retrying: 401 received, clearing session cookies",
				slog.String("broker", d.brokerURL),
				slog.String("auth_mode", d.authCfg.Mode))
			return true, nil
		}
		// Bearer token (static, can't refresh) or already retried basic auth.
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
