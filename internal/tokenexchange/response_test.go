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
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// trackingBody is an io.ReadCloser that records whether Close was called.
// Used by T01 and T02 to verify the defer resp.Body.Close() fires in
// every code path through parseIdPResponse.
//
// T01 variant: Read always returns an error (simulates a broken body).
// T02 variant: Read delegates to an embedded io.Reader (simulates success).

type trackingBodyError struct {
	closed bool
}

func (b *trackingBodyError) Read(_ []byte) (int, error) {
	return 0, errors.New("simulated read error")
}

func (b *trackingBodyError) Close() error {
	b.closed = true
	return nil
}

type trackingBodyOK struct {
	closed bool
	r      io.Reader
}

func (b *trackingBodyOK) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

func (b *trackingBodyOK) Close() error {
	b.closed = true
	return nil
}

// T01: Body is closed even when the body read itself fails.
func TestParseIdPResponse_BodyAlwaysClosedOnError(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := &trackingBodyError{}
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       body,
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseIdPResponse(resp, now)

	if tok != nil {
		t.Errorf("tok = %v, want nil on body read error", tok)
	}
	if !errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
	}
	if !body.closed {
		t.Errorf("body.closed = false, want true — body must be closed even on read error")
	}
}

// T02: Body is closed on the happy-path success code path.
func TestParseIdPResponse_BodyAlwaysClosedOnSuccess(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	jsonBody := `{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`
	body := &trackingBodyOK{r: strings.NewReader(jsonBody)}
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseIdPResponse(resp, now)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == nil {
		t.Fatal("tok = nil, want non-nil on success")
	}
	if !body.closed {
		t.Errorf("body.closed = false, want true — body must be closed on success path")
	}
}

// T03: Response body is capped at maxResponseBody (1 MiB); oversized bodies
// are truncated. The truncated body is not valid JSON, so the error surfaces
// as ErrInvalidResponse, not ErrExchangeTransport (the read itself succeeds).
func TestParseIdPResponse_BodySizeCappedAt1MiB(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	oversized := bytes.Repeat([]byte("x"), maxResponseBody+1)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(oversized)),
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err = e.parseIdPResponse(resp, now)

	if errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = true, want false — body read succeeded; transport error must not fire")
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
}

// T04: A non-JSON Content-Type on a 2xx response is treated as an SSO/proxy
// interception and returns ErrInvalidResponse with the phrase "SSO/proxy interception".
func TestParseIdPResponse_NonJSONContentTypeReturnsInvalidResponse(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("<html>Login page</html>")),
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseIdPResponse(resp, now)

	if tok != nil {
		t.Errorf("tok = %v, want nil on non-JSON Content-Type", tok)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
	if !strings.Contains(err.Error(), "SSO/proxy interception") {
		t.Errorf("err.Error() = %q, want it to contain \"SSO/proxy interception\"", err.Error())
	}
}

// T05: A missing Content-Type header bypasses the content-type guard; if the
// body is valid JSON the call succeeds.
func TestParseIdPResponse_MissingContentTypeBypassesGuard(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	jsonBody := `{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{}, // no Content-Type
		Body:       io.NopCloser(strings.NewReader(jsonBody)),
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseIdPResponse(resp, now)

	if err != nil {
		t.Fatalf("unexpected error for missing Content-Type: %v", err)
	}
	if tok == nil {
		t.Errorf("tok = nil, want non-nil — missing Content-Type must bypass guard")
	}
}

// T06: A malformed Content-Type (mime.ParseMediaType returns an error) produces
// an empty media-type string, which bypasses the guard. Valid JSON succeeds.
func TestParseIdPResponse_MalformedContentTypeBypassesGuard(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	jsonBody := `{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{";;;"}},
		Body:       io.NopCloser(strings.NewReader(jsonBody)),
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseIdPResponse(resp, now)

	if err != nil {
		t.Fatalf("unexpected error for malformed Content-Type: %v", err)
	}
	if tok == nil {
		t.Errorf("tok = nil, want non-nil — malformed Content-Type must bypass guard")
	}
}

// T07: responseMediaType strips parameters (charset, boundary) from the
// Content-Type header and returns only the bare media type.
func TestResponseMediaType_StripsParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		want        string
	}{
		{"with charset", "application/json; charset=utf-8", "application/json"},
		{"with boundary", "multipart/form-data; boundary=something", "multipart/form-data"},
		{"bare", "application/json", "application/json"},
		{"empty", "", ""},
		{"malformed", ";;;", ""},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{Header: http.Header{}}
			if tc.contentType != "" {
				resp.Header.Set("Content-Type", tc.contentType)
			}

			got := responseMediaType(resp)

			if got != tc.want {
				t.Errorf("responseMediaType(%q) = %q, want %q", tc.contentType, got, tc.want)
			}
		})
	}
}

// T08: mime.ParseMediaType lowercases the media type — "Application/JSON"
// normalizes to "application/json".
func TestResponseMediaType_CaseInsensitive(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"Application/JSON; charset=utf-8"}},
	}

	got := responseMediaType(resp)

	if got != "application/json" {
		t.Errorf("responseMediaType = %q, want %q", got, "application/json")
	}
}

// T09: Invalid JSON in the success body surfaces as ErrInvalidResponse with
// the phrase "unparseable IdP success response".
func TestParseSuccessBody_InvalidJSONReturnsInvalidResponse(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseSuccessBody([]byte("not-json{{{"), now)

	if tok != nil {
		t.Errorf("tok = %v, want nil on invalid JSON", tok)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
	if !strings.Contains(err.Error(), "unparseable IdP success response") {
		t.Errorf("err.Error() = %q, want it to contain \"unparseable IdP success response\"", err.Error())
	}
}

// T10: A missing, null, or empty access_token field returns ErrInvalidResponse
// with "missing access_token".
func TestParseSuccessBody_MissingAccessTokenReturnsInvalidResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"absent", `{"token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`},
		{"null", `{"access_token":null,"token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`},
		{"empty string", `{"access_token":"","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(validParams(t))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

			tok, err := e.parseSuccessBody([]byte(tc.body), now)

			if tok != nil {
				t.Errorf("tok = %v, want nil", tok)
			}
			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
			}
			if !strings.Contains(err.Error(), "missing access_token") {
				t.Errorf("err.Error() = %q, want it to contain \"missing access_token\"", err.Error())
			}
		})
	}
}

// T11: token_type is matched case-insensitively against "Bearer"; non-Bearer
// types return ErrInvalidResponse.
func TestParseSuccessBody_TokenTypeCaseInsensitiveBearer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tokenType string
		wantErr   bool
	}{
		{"lowercase bearer", "bearer", false},
		{"uppercase BEARER", "BEARER", false},
		{"mixed Bearer", "Bearer", false},
		{"wrong type", "MAC", true},
		{"empty string", "", true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(validParams(t))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			body := fmt.Sprintf(`{"access_token":"tok","token_type":%q,"issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`, tc.tokenType)

			tok, err := e.parseSuccessBody([]byte(body), now)

			if tc.wantErr {
				if tok != nil {
					t.Errorf("tok = %v, want nil", tok)
				}
				if !errors.Is(err, ErrInvalidResponse) {
					t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if tok == nil {
					t.Errorf("tok = nil, want non-nil")
				}
			}
		})
	}
}

// T12: issued_token_type must exactly match the RFC 8693 access-token URN
// for GrantTypeTokenExchange. The comparison is case-sensitive.
func TestParseSuccessBody_IssuedTokenTypeExactMatchRequired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		issuedTokenType string
		wantErr         bool
	}{
		{"correct URN", "urn:ietf:params:oauth:token-type:access_token", false},
		{"wrong URN", "urn:ietf:params:oauth:token-type:jwt", true},
		{"empty (absent field)", "", true},
		{"uppercase URN (case-sensitive)", "URN:IETF:PARAMS:OAUTH:TOKEN-TYPE:ACCESS_TOKEN", true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(validParams(t)) // sets GrantTypeTokenExchange
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			body := fmt.Sprintf(`{"access_token":"tok","token_type":"Bearer","issued_token_type":%q,"expires_in":3600}`, tc.issuedTokenType)

			tok, err := e.parseSuccessBody([]byte(body), now)

			if tc.wantErr {
				if tok != nil {
					t.Errorf("tok = %v, want nil", tok)
				}
				if !errors.Is(err, ErrInvalidResponse) {
					t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if tok == nil {
					t.Errorf("tok = nil, want non-nil")
				}
			}
		})
	}
}

// T13: The issued_token_type check is skipped for any grant type other than
// GrantTypeTokenExchange (e.g. jwt-bearer / Entra OBO flows that don't return
// issued_token_type at all).
func TestParseSuccessBody_IssuedTokenTypeSkippedForNonTokenExchangeGrant(t *testing.T) {
	t.Parallel()

	e := &Exchanger{
		tokenURL:         "https://idp.example.com/token",
		clientID:         "cid",
		clientAuthMethod: ClientSecretPost,
		clientSecret:     "sec",
		grantType:        GrantType(99), // not GrantTypeTokenExchange (which is 1)
		audienceParam:    AudienceParamAudience,
		httpClient:       &http.Client{},
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Body intentionally omits issued_token_type.
	body := `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`

	tok, err := e.parseSuccessBody([]byte(body), now)

	if err != nil {
		t.Fatalf("unexpected error — issued_token_type check must be skipped for non-TokenExchange grant: %v", err)
	}
	if tok == nil {
		t.Errorf("tok = nil, want non-nil")
	}
}

// T14: expires_in must be a positive integer. A missing (zero value), explicit
// zero, or negative value returns ErrInvalidResponse with "missing or
// non-positive expires_in".
func TestParseSuccessBody_ExpiresInNonPositiveReturnsInvalidResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"absent", `{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token"}`},
		{"zero", `{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":0}`},
		{"negative", `{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":-1}`},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(validParams(t))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

			tok, err := e.parseSuccessBody([]byte(tc.body), now)

			if tok != nil {
				t.Errorf("tok = %v, want nil", tok)
			}
			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
			}
			if !strings.Contains(err.Error(), "missing or non-positive expires_in") {
				t.Errorf("err.Error() = %q, want it to contain \"missing or non-positive expires_in\"", err.Error())
			}
		})
	}
}

// T15: expires_in at exactly maxExpiresInSeconds is accepted; one over that
// limit returns ErrInvalidResponse with "exceeds the safe arithmetic limit".
func TestParseSuccessBody_ExpiresInOverflowGuard(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	boundaryBody := fmt.Sprintf(`{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":%d}`, maxExpiresInSeconds)
	overBody := fmt.Sprintf(`{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":%d}`, maxExpiresInSeconds+1)

	tok, err := e.parseSuccessBody([]byte(boundaryBody), now)

	if err != nil {
		t.Errorf("unexpected error at boundary value: %v", err)
	}
	if tok == nil {
		t.Errorf("tok = nil, want non-nil at boundary value")
	}

	tok, err = e.parseSuccessBody([]byte(overBody), now)

	if tok != nil {
		t.Errorf("tok = %v, want nil for over-boundary value", tok)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
	if !strings.Contains(err.Error(), "exceeds the safe arithmetic limit") {
		t.Errorf("err.Error() = %q, want it to contain \"exceeds the safe arithmetic limit\"", err.Error())
	}
}

// T16: On a successful parse, Token.Value equals access_token and
// Token.ExpiresAt equals now + expires_in - 30s (the 30-second skew).
func TestParseSuccessBody_TokenConstructionAndExpiresAt(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	expiresIn := int64(3600)
	body := fmt.Sprintf(`{"access_token":"my-token","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":%d}`, expiresIn)

	tok, err := e.parseSuccessBody([]byte(body), now)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == nil {
		t.Fatal("tok = nil, want non-nil")
		return
	}
	if tok.Value != "my-token" {
		t.Errorf("tok.Value = %q, want %q", tok.Value, "my-token")
	}
	wantExpiresAt := now.Add(time.Duration(expiresIn)*time.Second - 30*time.Second)
	if !tok.ExpiresAt.Equal(wantExpiresAt) {
		t.Errorf("tok.ExpiresAt = %v, want %v", tok.ExpiresAt, wantExpiresAt)
	}
}

// T17: A 4xx response with a valid RFC 6749 §5.2 JSON body returns
// ErrExchangeRejected and includes the OAuth error code and status code in
// the error message.
func TestParseIdPResponse_FourxxOAuthErrorReturnsExchangeRejected(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp := &http.Response{
		StatusCode: 400,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseIdPResponse(resp, now)

	if tok != nil {
		t.Errorf("tok = %v, want nil", tok)
	}
	if !errors.Is(err, ErrExchangeRejected) {
		t.Errorf("errors.Is(err, ErrExchangeRejected) = false, want true; err = %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err.Error() = %q, want it to contain \"invalid_grant\"", err.Error())
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err.Error() = %q, want it to contain \"400\"", err.Error())
	}
}

// T18: A 4xx response whose body is not a valid OAuth error JSON (e.g. an HTML
// page from a WAF) returns ErrInvalidResponse with "proxy or WAF interception".
// Non-retryable — retrying an intercepted request will not yield a different response.
func TestParseIdPResponse_FourxxNonOAuthBodyReturnsInvalidResponse(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp := &http.Response{
		StatusCode: 403,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("<html>Access Denied</html>")),
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseIdPResponse(resp, now)

	if tok != nil {
		t.Errorf("tok = %v, want nil", tok)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = true, want false — proxy/WAF interception must NOT be a transport failure")
	}
	if !strings.Contains(err.Error(), "proxy or WAF interception") {
		t.Errorf("err.Error() = %q, want it to contain \"proxy or WAF interception\"", err.Error())
	}
}

// T19: 3xx, 429, and 5xx status codes return ErrExchangeTransport with the
// status code in the message (no special body parsing for these ranges).
// 429 is grouped here — not with other 4xx client errors — because the
// retry policy treats it as retryable (see idpclient/retrying.go), and
// after retries exhaust classifyRetryOutcome rewraps to
// ErrExchangeRetriesExhausted, the same downstream sentinel as an
// exhausted 5xx.
func TestParseIdPResponse_ThreexxAndFivexxReturnExchangeTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{"301 Moved Permanently", 301},
		{"302 Found", 302},
		{"429 Too Many Requests", 429},
		{"500 Internal Server Error", 500},
		{"503 Service Unavailable", 503},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(validParams(t))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("error")),
			}

			tok, err := e.parseIdPResponse(resp, now)

			if tok != nil {
				t.Errorf("tok = %v, want nil", tok)
			}
			if !errors.Is(err, ErrExchangeTransport) {
				t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", tc.status)) {
				t.Errorf("err.Error() = %q, want it to contain %q", err.Error(), fmt.Sprintf("%d", tc.status))
			}
		})
	}
}

// T20: An oversized error code (> maxErrorCodeLen bytes) routes to
// ErrInvalidResponse (non-retryable — the IdP is misbehaving or the
// response is intercepted, neither of which a retry will fix) and
// prevents log-line bloat by omitting the oversized code from the
// message. A code at exactly maxErrorCodeLen bytes routes to
// ErrExchangeRejected with the full code.
func TestClassifyClientError_OversizedErrorCodeReturnsInvalidResponse(t *testing.T) {
	t.Parallel()

	longCode := strings.Repeat("a", maxErrorCodeLen+1) // 65 bytes
	body := fmt.Sprintf(`{"error":%q}`, longCode)

	exactCode := strings.Repeat("b", maxErrorCodeLen) // 64 bytes
	exactBody := fmt.Sprintf(`{"error":%q}`, exactCode)

	errOver := classifyClientError([]byte(body), 400)
	errExact := classifyClientError([]byte(exactBody), 400)

	// Oversized assertions.
	if !errors.Is(errOver, ErrInvalidResponse) {
		t.Errorf("errors.Is(errOver, ErrInvalidResponse) = false, want true; err = %v", errOver)
	}
	if errors.Is(errOver, ErrExchangeTransport) {
		t.Errorf("errors.Is(errOver, ErrExchangeTransport) = true, want false — oversized error code must NOT be a transport failure")
	}
	if errors.Is(errOver, ErrExchangeRejected) {
		t.Errorf("errors.Is(errOver, ErrExchangeRejected) = true, want false — oversized must NOT be a rejection")
	}
	if !strings.Contains(errOver.Error(), "oversized error code") {
		t.Errorf("errOver.Error() = %q, want it to contain \"oversized error code\"", errOver.Error())
	}
	if !strings.Contains(errOver.Error(), "65") {
		t.Errorf("errOver.Error() = %q, want it to contain \"65\" (byte count)", errOver.Error())
	}
	if strings.Contains(errOver.Error(), longCode) {
		t.Errorf("errOver.Error() contains the full longCode — must NOT include the oversized code in the message")
	}

	// Boundary assertions.
	if !errors.Is(errExact, ErrExchangeRejected) {
		t.Errorf("errors.Is(errExact, ErrExchangeRejected) = false, want true; err = %v", errExact)
	}
	if !strings.Contains(errExact.Error(), exactCode) {
		t.Errorf("errExact.Error() = %q, want it to contain the full exactCode %q", errExact.Error(), exactCode)
	}
}

// T21: error_description is deliberately excluded from the classified error
// message to prevent unsafe IdP content from reaching logs.
func TestClassifyClientError_ErrorDescriptionNotInMessage(t *testing.T) {
	t.Parallel()

	body := `{"error":"invalid_grant","error_description":"the token has expired and cannot be refreshed"}`

	err := classifyClientError([]byte(body), 400)

	if !errors.Is(err, ErrExchangeRejected) {
		t.Fatalf("errors.Is(err, ErrExchangeRejected) = false, want true; err = %v", err)
	}
	if strings.Contains(err.Error(), "the token has expired") {
		t.Errorf("err.Error() = %q, must NOT contain error_description text", err.Error())
	}
	if strings.Contains(err.Error(), "error_description") {
		t.Errorf("err.Error() = %q, must NOT contain the field name \"error_description\"", err.Error())
	}
}

// T22: Full happy-path integration through parseIdPResponse: 200 + JSON +
// correct fields → Token with expected Value and ExpiresAt.
func TestParseIdPResponse_HappyPath(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	jsonBody := `{"access_token":"exchanged-token","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":7200}`
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(jsonBody)),
	}

	tok, err := e.parseIdPResponse(resp, now)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == nil {
		t.Fatal("tok = nil, want non-nil")
		return
	}
	if tok.Value != "exchanged-token" {
		t.Errorf("tok.Value = %q, want %q", tok.Value, "exchanged-token")
	}
	wantExpiresAt := now.Add(7200*time.Second - 30*time.Second)
	if !tok.ExpiresAt.Equal(wantExpiresAt) {
		t.Errorf("tok.ExpiresAt = %v, want %v", tok.ExpiresAt, wantExpiresAt)
	}
}

// T23: Short-lived tokens (expires_in <= 30s) are not an error; the ExpiresAt
// computation may land in the past relative to now, but the function still
// succeeds. Callers decide what to do with an already-expired token.
func TestParseSuccessBody_ShortLivedTokenReturnsNonErrorTokenWithPastExpiresAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		expiresIn int64
		wantSign  int // -1 = before now, 0 = equal, +1 = after now
	}{
		{"expires_in=1", int64(1), -1},  // ExpiresAt = now - 29s → before now
		{"expires_in=30", int64(30), 0}, // ExpiresAt = now + 0s  → equal to now
		{"expires_in=31", int64(31), 1}, // ExpiresAt = now + 1s  → after now
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(validParams(t))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			body := fmt.Sprintf(`{"access_token":"tok","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":%d}`, tc.expiresIn)

			tok, err := e.parseSuccessBody([]byte(body), now)

			if err != nil {
				t.Fatalf("unexpected error — short-lived tokens must not be an error (current behavior): %v", err)
			}
			if tok == nil {
				t.Fatal("tok = nil, want non-nil")
				return
			}

			wantExpiresAt := now.Add(time.Duration(tc.expiresIn)*time.Second - 30*time.Second)
			if !tok.ExpiresAt.Equal(wantExpiresAt) {
				t.Errorf("tok.ExpiresAt = %v, want %v", tok.ExpiresAt, wantExpiresAt)
			}

			switch tc.wantSign {
			case -1:
				if !tok.ExpiresAt.Before(now) {
					t.Errorf("tok.ExpiresAt = %v, want Before(now = %v)", tok.ExpiresAt, now)
				}
			case 0:
				if !tok.ExpiresAt.Equal(now) {
					t.Errorf("tok.ExpiresAt = %v, want Equal(now = %v)", tok.ExpiresAt, now)
				}
			case 1:
				if !tok.ExpiresAt.After(now) {
					t.Errorf("tok.ExpiresAt = %v, want After(now = %v)", tok.ExpiresAt, now)
				}
			}
		})
	}
}

// T24: An oversized body passed through parseIdPResponse is silently truncated
// by io.LimitReader. The truncated body fails JSON parsing and surfaces as
// ErrInvalidResponse — there is no explicit "body too large" or "1 MiB"
// language in the error message.
func TestParseIdPResponse_OversizedBodySurfacesAsInvalidResponseNotTransport(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	oversized := bytes.Repeat([]byte("x"), maxResponseBody+1)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(oversized)),
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err = e.parseIdPResponse(resp, now)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
	if strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err.Error() = %q, must NOT contain \"exceeds\"", err.Error())
	}
	if strings.Contains(err.Error(), "limit") {
		t.Errorf("err.Error() = %q, must NOT contain \"limit\"", err.Error())
	}
	if strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("err.Error() = %q, must NOT contain \"1 MiB\"", err.Error())
	}
}

// T25: `null` is valid JSON but json.Unmarshal into a struct produces a zero
// value (all fields empty). The access_token check fires and returns
// ErrInvalidResponse with "missing access_token" — not the "unparseable" error.
func TestParseSuccessBody_NullBodyTriggersAccessTokenMissingError(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tok, err := e.parseSuccessBody([]byte("null"), now)

	if tok != nil {
		t.Errorf("tok = %v, want nil", tok)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
	if strings.Contains(err.Error(), "unparseable") {
		t.Errorf("err.Error() = %q, must NOT contain \"unparseable\" — json.Unmarshal(\"null\") succeeds", err.Error())
	}
	if !strings.Contains(err.Error(), "missing access_token") {
		t.Errorf("err.Error() = %q, want it to contain \"missing access_token\"", err.Error())
	}
}

// T26: An explicit empty string in the "error" field falls through to the
// non-OAuth-body path (no code to report), which classifies as
// ErrInvalidResponse (proxy/WAF-shaped, non-retryable), not the rejection path.
func TestClassifyClientError_ExplicitEmptyErrorFieldRoutesToInvalidResponse(t *testing.T) {
	t.Parallel()

	err := classifyClientError([]byte(`{"error":""}`), 400)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = true, want false — empty error field must NOT be a transport failure")
	}
	if errors.Is(err, ErrExchangeRejected) {
		t.Errorf("errors.Is(err, ErrExchangeRejected) = true, want false — empty error field must NOT be a rejection")
	}
}

// T27: Unknown fields in the success body are silently ignored — the JSON
// decoder does not use DisallowUnknownFields, so extra fields from the IdP
// are allowed without error.
func TestParseSuccessBody_UnknownFieldsSilentlyIgnored(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	body := `{
		"access_token": "tok",
		"token_type": "Bearer",
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"expires_in": 3600,
		"refresh_token": "reftok",
		"scope": "openid profile",
		"expires_on": 1234567890
	}`

	tok, err := e.parseSuccessBody([]byte(body), now)

	if err != nil {
		t.Fatalf("unexpected error — unknown fields must be silently ignored: %v", err)
	}
	if tok == nil {
		t.Fatal("tok = nil, want non-nil")
		return
	}
	if tok.Value != "tok" {
		t.Errorf("tok.Value = %q, want %q", tok.Value, "tok")
	}
}
