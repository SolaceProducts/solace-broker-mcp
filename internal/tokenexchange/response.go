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

package tokenexchange

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

const maxResponseBody = 1 << 20 // 1 MiB

// Fields defined by RFC 8693 §2.2.1. access_token, token_type, and
// issued_token_type are REQUIRED; expires_in is RECOMMENDED.
type successResponse struct {
	AccessToken     string `json:"access_token"`
	TokenType       string `json:"token_type"`
	IssuedTokenType string `json:"issued_token_type"`
	ExpiresIn       int64  `json:"expires_in"`
}

// errorResponse parses RFC 6749 §5.2 error bodies. Only the "error"
// code is used — "error_description" is deliberately ignored because
// IdPs sometimes echo unsafe content that must never reach our logs.
type errorResponse struct {
	Error string `json:"error"`
}

// parseIdPResponse classifies the IdP's HTTP response:
//   - 2xx + non-JSON Content-Type → ErrInvalidResponse (SSO/proxy interception)
//   - 2xx + valid JSON with required fields → *Token
//   - 2xx + missing/unparseable fields → ErrInvalidResponse
//   - 429 → ErrExchangeTransport (grouped with 5xx; the retry policy
//     treats 429 as a retryable transport-class signal, and after
//     retries exhaust the honest downstream sentinel is
//     ErrExchangeRetriesExhausted, not ErrInvalidResponse)
//   - 4xx (other) + OAuth error JSON → ErrExchangeRejected (wraps error code)
//   - 4xx (other) + non-OAuth body → ErrInvalidResponse (possible proxy/WAF interception)
//   - 5xx / network-level → ErrExchangeTransport
func (e *Exchanger) parseIdPResponse(resp *http.Response, now time.Time) (*Token, error) {
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, &ExchangeError{
			Sentinel:     ErrExchangeTransport,
			Message:      fmt.Sprintf("token exchange transport failure: reading IdP response body: %v", err),
			FailureClass: FailureClassBodyRead,
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if ct := responseMediaType(resp); ct != "" && ct != "application/json" {
			return nil, &ExchangeError{
				Sentinel:   ErrInvalidResponse,
				Message:    fmt.Sprintf("token exchange invalid response: IdP returned HTTP %d with Content-Type %q (expected application/json — possible SSO/proxy interception)", resp.StatusCode, ct),
				HTTPStatus: resp.StatusCode,
			}
		}
		return e.parseSuccessBody(body, now)
	}

	// 429 groups with 5xx as ErrExchangeTransport because the retry
	// policy treats it as retryable (idpclient/retrying.go). After
	// retries exhaust, classifyRetryOutcome rewraps as
	// ErrExchangeRetriesExhausted — the same downstream sentinel as
	// exhausted 5xx.
	if resp.StatusCode == http.StatusTooManyRequests {
		// Parsed on every 429, not just the last attempt — classifyRetryOutcome
		// only keeps whichever ExchangeError this call happens to produce last.
		retryAfter := parseRetryAfter(resp.Header.Values("Retry-After"), now)
		return nil, &ExchangeError{
			Sentinel:         ErrExchangeTransport,
			Message:          "token exchange transport failure: IdP returned HTTP 429 (rate limited)",
			HTTPStatus:       resp.StatusCode,
			FailureClass:     FailureClassRateLimited,
			RetryAfterResult: &retryAfter,
		}
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, classifyClientError(body, resp.StatusCode)
	}

	return nil, &ExchangeError{
		Sentinel:     ErrExchangeTransport,
		Message:      fmt.Sprintf("token exchange transport failure: IdP returned HTTP %d", resp.StatusCode),
		HTTPStatus:   resp.StatusCode,
		FailureClass: FailureClassUpstream5xx,
	}
}

func (e *Exchanger) parseSuccessBody(body []byte, now time.Time) (*Token, error) {
	var sr successResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, &ExchangeError{
			Sentinel: ErrInvalidResponse,
			Message:  fmt.Sprintf("token exchange invalid response: unparseable IdP success response: %v", err),
		}
	}

	if sr.AccessToken == "" {
		return nil, &ExchangeError{
			Sentinel: ErrInvalidResponse,
			Message:  "token exchange invalid response: IdP success response missing access_token",
		}
	}

	if !strings.EqualFold(sr.TokenType, "Bearer") {
		return nil, &ExchangeError{
			Sentinel: ErrInvalidResponse,
			Message:  fmt.Sprintf("token exchange invalid response: IdP returned token_type %q, expected \"Bearer\" — the MCP server only supports Bearer tokens", sr.TokenType),
		}
	}

	// issued_token_type is RFC 8693-specific; RFC 7523 responses don't include it.
	if e.grantType == GrantTypeTokenExchange && sr.IssuedTokenType != URNTokenTypeAccessToken {
		return nil, &ExchangeError{
			Sentinel: ErrInvalidResponse,
			Message:  fmt.Sprintf("token exchange invalid response: IdP returned issued_token_type %q, expected %q — the IdP may be misconfigured to issue a different token type", sr.IssuedTokenType, URNTokenTypeAccessToken),
		}
	}

	if sr.ExpiresIn <= 0 {
		return nil, &ExchangeError{
			Sentinel: ErrInvalidResponse,
			Message:  "token exchange invalid response: IdP success response missing or non-positive expires_in — the IdP must return expires_in in token-exchange responses (configure token lifetimes on the IdP client)",
		}
	}

	// Prevents time.Duration overflow (int64 nanoseconds, max ~292 years).
	if sr.ExpiresIn > maxExpiresInSeconds {
		return nil, &ExchangeError{
			Sentinel: ErrInvalidResponse,
			Message:  fmt.Sprintf("token exchange invalid response: IdP returned expires_in %d which exceeds the safe arithmetic limit — likely a misbehaving IdP or intercepted response", sr.ExpiresIn),
		}
	}

	// TODO(Commit C): log WARN when sr.ExpiresIn <= int64(defaults.DefaultTokenExpirySkew.Seconds())
	// — token is effectively expired at issuance, likely IdP misconfiguration.

	return &Token{
		Value: sr.AccessToken,
		// TODO(Commit E): replace direct default with e.tokenExpirySkew struct field
		ExpiresAt: now.Add(time.Duration(sr.ExpiresIn)*time.Second - defaults.DefaultTokenExpirySkew),
	}, nil
}

func responseMediaType(resp *http.Response) string {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return mediaType
}

// retryAfterResult is the outcome of parsing a Retry-After header. ok is
// false when absent or unparseable — raw preserves the original value so
// callers can distinguish "absent" from "present but malformed" in logs.
type retryAfterResult struct {
	delay time.Duration
	ok    bool
	raw   string
}

// parseRetryAfter parses delta-seconds or HTTP-date per RFC 9110 §10.2.3.
// A negative delta or a past HTTP-date (e.g. IdP clock skew) floors to a
// zero delay rather than a negative one, since a gate must never close
// before it opens.
func parseRetryAfter(headerValues []string, now time.Time) retryAfterResult {
	if len(headerValues) == 0 || headerValues[0] == "" {
		return retryAfterResult{ok: false}
	}
	raw := headerValues[0]
	// Field values may carry optional surrounding whitespace (RFC 9110
	// §5.6.3); trim only for parsing so a proxy/library that fails to strip
	// it doesn't make an otherwise-valid header look unparseable. raw itself
	// stays untrimmed so logs faithfully reflect what the IdP actually sent.
	trimmed := strings.TrimSpace(raw)

	if seconds, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		if seconds < 0 {
			return retryAfterResult{delay: 0, ok: true, raw: raw}
		}
		// Bound BEFORE multiplying by time.Second: strconv.ParseInt accepts
		// any int64, but time.Duration is also int64 nanoseconds, so a huge
		// seconds value overflows the multiplication and can wrap to an
		// arbitrary (possibly negative, possibly small-and-plausible-looking)
		// duration that would silently corrupt the gate. Reuses
		// maxExpiresInSeconds — same arithmetic ceiling, same file.
		if seconds > maxExpiresInSeconds {
			return retryAfterResult{ok: false, raw: raw}
		}
		return retryAfterResult{delay: time.Duration(seconds) * time.Second, ok: true, raw: raw}
	}

	if when, err := http.ParseTime(trimmed); err == nil {
		if delta := when.Sub(now); delta > 0 {
			return retryAfterResult{delay: delta, ok: true, raw: raw}
		}
		return retryAfterResult{delay: 0, ok: true, raw: raw}
	}

	return retryAfterResult{ok: false, raw: raw}
}

// maxExpiresInSeconds is the largest expires_in we accept before
// treating the response as invalid. The limit is purely arithmetic:
// time.Duration is int64 nanoseconds, so the maximum representable
// duration is ~292 years ≈ 9.2e18 ns. We cap well below that at
// ~100 years to leave headroom and catch obviously bogus values
// (MITM, misbehaving IdP) without overriding legitimate operator-
// configured token lifetimes.
const maxExpiresInSeconds int64 = 100 * 365 * 24 * 3600 // ~100 years

// Cap prevents log-line bloat from a misbehaving IdP's error code.
const maxErrorCodeLen = 64

func classifyClientError(body []byte, statusCode int) error {
	var er errorResponse
	if err := json.Unmarshal(body, &er); err == nil && er.Error != "" {
		if len(er.Error) > maxErrorCodeLen {
			return &ExchangeError{
				Sentinel:   ErrInvalidResponse,
				Message:    fmt.Sprintf("token exchange invalid response: IdP returned HTTP %d with oversized error code (%d bytes) — not a standard OAuth error", statusCode, len(er.Error)),
				HTTPStatus: statusCode,
			}
		}
		return &ExchangeError{
			Sentinel:   ErrExchangeRejected,
			Message:    fmt.Sprintf("token exchange rejected by IdP: IdP error %q (HTTP %d)", er.Error, statusCode),
			HTTPStatus: statusCode,
		}
	}

	return &ExchangeError{
		Sentinel:   ErrInvalidResponse,
		Message:    fmt.Sprintf("token exchange invalid response: IdP returned HTTP %d with non-OAuth error body (possible proxy or WAF interception)", statusCode),
		HTTPStatus: statusCode,
	}
}
