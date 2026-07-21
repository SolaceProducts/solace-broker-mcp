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

package idpclient

import (
	"context"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// RetryOptions carries the tuning knobs for NewRetryingHTTPClient's
// retry loop. Deliberately a plain struct with no smart defaults or
// validation: the token-exchange layer owns the numbers (via its own
// Default* constants and its ComputeChainDeadline formula) and passes
// them in explicitly. This package stays a mechanical composition of
// go-retryablehttp — no policy of its own.
//
// Zero values are legal but produce a client that never retries
// (MaxRetries=0). RetryWaitMin/Max are only meaningful when MaxRetries>0.
type RetryOptions struct {
	// MaxRetries is the number of retries AFTER the first attempt.
	// Total attempts = MaxRetries + 1.
	MaxRetries int

	// RetryWaitMin and RetryWaitMax bound the jittered backoff between
	// attempts. `RateLimitLinearJitterBackoff` samples uniformly in
	// [min, max]; `Retry-After` overrides the sample when present and
	// is uncapped from RetryWaitMax by design so the IdP's guidance wins.
	RetryWaitMin time.Duration
	RetryWaitMax time.Duration
}

// NewRetryingHTTPClient returns an *http.Client that retries transient IdP
// failures automatically. Composition-only: the inner attempt client is a
// stock NewHTTPClient (same TLS roots, same SSL_CERT_FILE escape hatch, same
// per-attempt Timeout). retryablehttp sits on top and drives the retry loop.
//
// Retry policy (see checkRetry):
//   - HTTP 5xx: retry.
//   - Connection errors (DNS, TLS handshake, body-read partials): retry.
//   - Everything else (2xx, 3xx, 4xx including 401/429): no retry.
//
// The token-exchange layer deliberately does not retry 429: it talks to a
// shared IdP, so N concurrent tool-call failures retrying in lockstep would
// amplify the very throttle the IdP is signalling. Fail fast, let the
// operator adjust.
//
// The returned client has no outer Timeout — callers bound the whole
// retry chain via context.WithTimeout, and the inner client's Timeout
// bounds each attempt. This split is deliberate: an outer Timeout on
// the returned client would apply to the whole chain rather than per
// attempt, defeating the point of a chain-level context deadline.
//
// Callers that want to observe how many attempts a call took can wrap the
// per-request context with WithAttemptsCounter before calling Do; see that
// function's doc for details.
func NewRetryingHTTPClient(retry RetryOptions, opts ...Option) (*http.Client, error) {
	inner, err := NewHTTPClient(opts...)
	if err != nil {
		return nil, err
	}

	// Wrap the inner transport so we can count attempts per request.
	// Preserves the inner client's Timeout — retryablehttp calls
	// c.HTTPClient.Do on every attempt, which applies inner.Timeout.
	inner.Transport = &attemptsRecorder{inner: inner.Transport}

	rc := retryablehttp.NewClient()
	rc.HTTPClient = inner
	rc.RetryMax = retry.MaxRetries
	rc.RetryWaitMin = retry.RetryWaitMin
	rc.RetryWaitMax = retry.RetryWaitMax
	rc.Backoff = retryablehttp.RateLimitLinearJitterBackoff
	rc.CheckRetry = checkRetry
	// PassthroughErrorHandler returns the final response as-is instead of
	// dropping it into an error. Parse-layer classification runs on the
	// same shape whether or not retries were exhausted.
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
	rc.Logger = nil // silence the library's own logging; ours is at the exchange layer

	return rc.StandardClient(), nil
}

// checkRetry is the token-exchange retry policy. It reads raw HTTP, not
// parsed sentinels — the token-exchange package owns sentinel classification
// separately (see internal/tokenexchange/response.go).
func checkRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// Connection error (DNS, TLS handshake, refused, body-read partial).
	// Retryable — the retry loop is exactly the case this class of error
	// motivates.
	if err != nil {
		return true, nil
	}
	if resp != nil && resp.StatusCode >= 500 {
		return true, nil
	}
	// 2xx, 3xx, 4xx (including 401 and 429): no retry. 401 will not fix
	// itself on repeat; 429 amplifies the throttle we are being told about.
	return false, nil
}

// attemptsKey is the context key used by WithAttemptsCounter. Unexported so
// only this package can attach or read the counter — enforces the API.
type attemptsKey struct{}

// WithAttemptsCounter attaches a fresh attempts counter to ctx and returns
// the derived context along with a reader that returns the current count.
// The counter increments on every HTTP attempt (both retries and the
// original attempt, including the successful one).
//
// Nil-safe: if ctx doesn't carry a counter, the transport wrapper skips the
// increment and calls that don't need the count pay nothing. Callers that
// don't want the count simply don't call WithAttemptsCounter.
//
// Typical use:
//
//	ctx, attempts := idpclient.WithAttemptsCounter(ctx)
//	resp, err := httpClient.Do(req.WithContext(ctx))
//	log.Printf("attempts=%d", attempts())
func WithAttemptsCounter(ctx context.Context) (context.Context, func() int) {
	counter := new(int)
	ctx = context.WithValue(ctx, attemptsKey{}, counter)
	return ctx, func() int { return *counter }
}

// attemptsRecorder is a RoundTripper wrapper that increments the counter
// on ctx (attached via WithAttemptsCounter) on every RoundTrip call. Placed
// on the inner *http.Client so retryablehttp's per-attempt c.HTTPClient.Do
// walks through it exactly once per attempt.
type attemptsRecorder struct {
	inner http.RoundTripper
}

func (a *attemptsRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if c, ok := req.Context().Value(attemptsKey{}).(*int); ok && c != nil {
		*c++
	}
	return a.inner.RoundTrip(req)
}
