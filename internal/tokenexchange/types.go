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

// Package tokenexchange implements RFC 8693 OAuth 2.0 token exchange against
// a single IdP. The Exchanger is a process singleton: deployment-global
// protocol state (IdP endpoint, MCP server client credentials, grant type,
// audience-param wire format) lives on the struct; per-broker values
// (subject token, audience) flow through the Exchange method.
//
// Two entry points: tokenexchange.New(Params) for tests, and
// tokenexchange.FromConfig(*config.BrokerOAuthConfig, *http.Client) for
// production wiring in cmd/server/main.go. Both eventually return the same
// *Exchanger; FromConfig translates YAML schema (discriminated union of
// client-auth methods, string-typed enums) into the typed Params shape.
//
// The Exchanger does not cache exchanged tokens (deferred to a follow-up
// story), does not retry IdP failures, and does not fall back to a
// secondary IdP. It builds RFC 8693 requests, parses responses, classifies
// failures into three sentinel errors, and (optionally) deduplicates
// concurrent identical exchanges via singleflight to protect the IdP from
// stampedes.
package tokenexchange

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
)

// ClientAuthMethod identifies the OAuth client-authentication method the
// MCP server uses when calling the IdP token endpoint. Values match the
// IANA "OAuth Token Endpoint Authentication Methods" registry (RFC 7591).
// V1 supports the two client_secret_* methods; private_key_jwt and the
// two mTLS variants are tracked as follow-up work.
type ClientAuthMethod int

const (
	// ClientSecretBasic puts client_id and client_secret in the HTTP Basic
	// authorization header. RFC 6749 §2.3.1.
	ClientSecretBasic ClientAuthMethod = iota + 1
	// ClientSecretPost puts client_id and client_secret in the form body
	// alongside the other token-exchange parameters. RFC 6749 §2.3.1.
	ClientSecretPost
)

// AudienceFormat identifies the wire format the Exchanger uses to carry
// the per-broker audience value to the IdP. Different IdP families expect
// different parameter names; this enum selects between them.
type AudienceFormat int

const (
	// AudienceParamAudience uses the RFC 8693 "audience" parameter — the
	// canonical token-exchange spelling. The only V1-implemented format.
	AudienceParamAudience AudienceFormat = iota + 1
	// AudienceParamScope (Entra OBO style) and AudienceParamResource
	// (RFC 8707) are schema-accepted in internal/config but not yet
	// implemented at the wire-construction layer; FromConfig rejects
	// them with a clear error pointing at this comment.
)

// GrantType identifies the OAuth grant-type URN sent in the form body.
// V1 supports only RFC 8693 token exchange; Entra OBO's jwt-bearer is
// tracked as follow-up work.
type GrantType int

const (
	// GrantTypeTokenExchange is urn:ietf:params:oauth:grant-type:token-exchange
	// (RFC 8693). The only V1-implemented grant type.
	GrantTypeTokenExchange GrantType = iota + 1
)

// RFC 8693 wire-format URNs. Used in both request construction and
// response validation — defined once to prevent drift across files.
const (
	// URNGrantTypeTokenExchange is the grant_type value for RFC 8693.
	URNGrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" // #nosec G101 -- public RFC 8693 grant-type URN, not a credential.

	// URNTokenTypeAccessToken is the subject_token_type and
	// issued_token_type value for access tokens (RFC 8693 §3).
	URNTokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token" // #nosec G101 -- public RFC 8693 token-type URN, not a credential.
)

// Params are the construction-time inputs to New. Every field except
// HTTPClient is sourced from validated config (broker_oauth.*) and is
// trusted by the constructor — the config validator has already enforced
// non-empty values and allowlist membership at startup. HTTPClient is
// runtime-wired and is the only field New rejects when invalid.
type Params struct {
	// TokenURL is the IdP token endpoint (broker_oauth.idp_token_endpoint).
	TokenURL string
	// ClientID is the MCP server's client identifier at the IdP.
	ClientID string
	// ClientAuthMethod selects between client_secret_basic and
	// client_secret_post for authenticating to the token endpoint.
	ClientAuthMethod ClientAuthMethod
	// ClientSecret is the shared secret resolved from the populated
	// sub-block of broker_oauth.mcp_server_client_auth. Never logged.
	ClientSecret string
	// GrantType is the OAuth grant-type URN. V1: GrantTypeTokenExchange.
	GrantType GrantType
	// AudienceParam selects the wire format for the per-broker audience.
	// V1: AudienceParamAudience.
	AudienceParam AudienceFormat
	// HTTPClient is the IdP-bound HTTP client; production builds it via
	// idpclient.NewRetryingHTTPClient (transparent 5xx / connection-error
	// retries) which composes NewHTTPClient (SOL-150219 timeout bound).
	// Tests typically pass a plain *http.Client{}.
	HTTPClient *http.Client
	// Cache is the token cache for cross-time deduplication. The exchanger
	// checks the cache before hitting the IdP and stores successful
	// exchange results. Required — a nil cache is a wiring bug.
	Cache cache.TokenCache
}

// String, GoString, and LogValue redact ClientSecret so Params never leaks
// the OAuth client secret through fmt formatting or slog reflection. They
// expose only the non-credential protocol fields; the infrastructure fields
// (HTTPClient, Cache) are omitted as noise. Value receivers so *Params is
// covered too. Pattern mirrors cache.CachedCredential.
func (p Params) String() string {
	return fmt.Sprintf("Params{TokenURL: %q, ClientID: %q, ClientAuthMethod: %d, GrantType: %d, AudienceParam: %d}",
		p.TokenURL, p.ClientID, p.ClientAuthMethod, p.GrantType, p.AudienceParam)
}

func (p Params) GoString() string {
	return p.String()
}

func (p Params) LogValue() slog.Value {
	return slog.GroupValue(
		// Key is "idp_endpoint", not "token_url": the ReplaceAttr net in
		// cmd/server/main.go redacts any key containing "token", which would
		// blank this non-credential URL and mislead operators.
		slog.String("idp_endpoint", p.TokenURL),
		slog.String("client_id", p.ClientID),
		slog.Int("client_auth_method", int(p.ClientAuthMethod)),
		slog.Int("grant_type", int(p.GrantType)),
		slog.Int("audience_param", int(p.AudienceParam)),
	)
}

// ExchangeInput are the per-call inputs to Exchange. Subject token comes
// from ctx via Hop 1's InjectRawSubjectToken middleware (see
// internal/auth.RawSubjectTokenFromContext) and is extracted by the
// per-broker Authenticator (T6) before the call to Exchange.
type ExchangeInput struct {
	// SubjectToken is the inbound JWT to exchange. Hashed into the
	// singleflight key but never logged or otherwise echoed.
	SubjectToken string
	// BrokerAlias identifies which broker this exchange is for. Logging
	// label only — does not appear in the IdP request body.
	BrokerAlias string
	// Audience is the per-broker audience value
	// (brokers.<alias>.auth.oauth.audience). Sent to the IdP in the form
	// field selected by Params.AudienceParam.
	Audience string
}

// String, GoString, and LogValue redact SubjectToken (the inbound JWT) so
// ExchangeInput never leaks it through fmt formatting or slog reflection.
// Value receivers so *ExchangeInput is covered too. Pattern mirrors
// cache.CachedCredential.
func (i ExchangeInput) String() string {
	return fmt.Sprintf("ExchangeInput{BrokerAlias: %q, Audience: %q}", i.BrokerAlias, i.Audience)
}

func (i ExchangeInput) GoString() string {
	return i.String()
}

func (i ExchangeInput) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("broker_alias", i.BrokerAlias),
		slog.String("audience", i.Audience),
	)
}

// Token is the result of a successful token exchange. Value is the
// exchanged bearer token; ExpiresAt is computed from the IdP-reported
// expires_in minus a 30-second skew so callers (and the future cache)
// have a safe "use-by" instant rather than a fragile duration.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// String, GoString, and LogValue redact Value (the exchanged bearer token)
// so Token never leaks it through fmt formatting or slog reflection. Value
// receivers so the *Token returned by Exchange is covered too. Pattern
// mirrors cache.CachedCredential.
func (t Token) String() string {
	return fmt.Sprintf("Token{ExpiresAt: %v}", t.ExpiresAt)
}

func (t Token) GoString() string {
	return t.String()
}

func (t Token) LogValue() slog.Value {
	return slog.GroupValue(slog.Time("expires_at", t.ExpiresAt))
}

// Sentinel errors the Exchanger returns. The per-call error is always
// one of these sentinels (possibly wrapped via fmt.Errorf with %w or
// carried on an *ExchangeError); upper layers classify by errors.Is and
// map to broker-side HTTP status codes in T7b.
var (
	// ErrExchangeRejected — the IdP returned a 4xx with an OAuth-shaped
	// JSON body. The exchange was refused; retrying without changing the
	// inputs will not help. Mapped to broker-side 401.
	ErrExchangeRejected = errors.New("token exchange rejected by IdP")

	// ErrExchangeTransport — network failure, request timeout, or 5xx
	// from the IdP. The exchange may succeed on retry. Mapped to
	// broker-side 503.
	ErrExchangeTransport = errors.New("token exchange transport failure")

	// ErrInvalidResponse — the IdP returned 2xx but the response body
	// could not be parsed as a token-exchange response. Either the IdP
	// is misconfigured or the response shape has drifted; manual
	// investigation required. Mapped to broker-side 502.
	ErrInvalidResponse = errors.New("token exchange invalid response")

	// ErrExchangeMissingSubject — the caller's context did not carry
	// a subject token (Hop 1 token). The exchange cannot proceed
	// without an RFC 8693 subject_token. This indicates a middleware
	// ordering bug; retrying will not help.
	ErrExchangeMissingSubject = errors.New("token exchange missing subject token")

	// ErrExchangeRequestBuild — the outbound IdP request could not be
	// constructed (e.g. an unparseable URL or invalid HTTP method). This
	// is a deterministic config or code defect surfaced at request-build
	// time, not a transient network condition; retrying reproduces the
	// same failure. Non-retryable.
	ErrExchangeRequestBuild = errors.New("token exchange request build failure")

	// ErrExchangeRetriesExhausted — the server-side retry loop tried the
	// exchange up to its attempt cap and every attempt failed with a
	// retryable condition (ErrExchangeTransport). The last attempt's
	// state is carried on the same *ExchangeError envelope; this sentinel
	// only signals "we gave up." Non-retryable at the tools layer — the
	// agent should not immediately retry a chain the server itself just
	// exhausted (see SOL-151520 design docs).
	ErrExchangeRetriesExhausted = errors.New("token exchange retries exhausted")
)
