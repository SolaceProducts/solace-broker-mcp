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
//   - 4xx + OAuth error JSON → ErrExchangeRejected (wraps error code)
//   - 4xx + non-OAuth body → ErrExchangeTransport (possible proxy/WAF)
//   - 5xx / network-level → ErrExchangeTransport
func (e *Exchanger) parseIdPResponse(resp *http.Response, now time.Time) (*Token, error) {
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("%w: reading IdP response body: %v", ErrExchangeTransport, err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if ct := responseMediaType(resp); ct != "" && ct != "application/json" {
			return nil, fmt.Errorf("%w: IdP returned HTTP %d with Content-Type %q (expected application/json — possible SSO/proxy interception)", ErrInvalidResponse, resp.StatusCode, ct)
		}
		return e.parseSuccessBody(body, now)
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, classifyClientError(body, resp.StatusCode)
	}

	return nil, fmt.Errorf("%w: IdP returned HTTP %d", ErrExchangeTransport, resp.StatusCode)
}

func (e *Exchanger) parseSuccessBody(body []byte, now time.Time) (*Token, error) {
	var sr successResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("%w: unparseable IdP success response: %v", ErrInvalidResponse, err)
	}

	if sr.AccessToken == "" {
		return nil, fmt.Errorf("%w: IdP success response missing access_token", ErrInvalidResponse)
	}

	if !strings.EqualFold(sr.TokenType, "Bearer") {
		return nil, fmt.Errorf("%w: IdP returned token_type %q, expected \"Bearer\" — the MCP server only supports Bearer tokens", ErrInvalidResponse, sr.TokenType)
	}

	// issued_token_type is RFC 8693-specific; RFC 7523 responses don't include it.
	if e.grantType == GrantTypeTokenExchange && sr.IssuedTokenType != URNTokenTypeAccessToken {
		return nil, fmt.Errorf("%w: IdP returned issued_token_type %q, expected %q — the IdP may be misconfigured to issue a different token type", ErrInvalidResponse, sr.IssuedTokenType, URNTokenTypeAccessToken)
	}

	if sr.ExpiresIn <= 0 {
		return nil, fmt.Errorf("%w: IdP success response missing or non-positive expires_in — the IdP must return expires_in in token-exchange responses (configure token lifetimes on the IdP client)", ErrInvalidResponse)
	}

	// Prevents time.Duration overflow (int64 nanoseconds, max ~292 years).
	if sr.ExpiresIn > maxExpiresInSeconds {
		return nil, fmt.Errorf("%w: IdP returned expires_in %d which exceeds the safe arithmetic limit — likely a misbehaving IdP or intercepted response", ErrInvalidResponse, sr.ExpiresIn)
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
			return fmt.Errorf("%w: IdP returned HTTP %d with oversized error code (%d bytes) — not a standard OAuth error", ErrExchangeTransport, statusCode, len(er.Error))
		}
		return fmt.Errorf("%w: IdP error %q (HTTP %d)", ErrExchangeRejected, er.Error, statusCode)
	}

	return fmt.Errorf("%w: IdP returned HTTP %d with non-OAuth error body (possible proxy or WAF interception)", ErrExchangeTransport, statusCode)
}
