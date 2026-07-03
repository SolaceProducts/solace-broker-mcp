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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// buildIdPRequest assembles a POST to the IdP token endpoint. Each
// concern — grant-type wire shape, subject token, client authentication,
// audience format, scopes — is handled by its own method so new grant
// types or auth methods grow in isolation.
//
// The request is built first with an empty body. Setters receive both
// the form (for body fields) and the request (for headers), so each
// setter can fully own its concern without leaking work back to the
// orchestrator. The form is encoded into the request body last.
func (e *Exchanger) buildIdPRequest(ctx context.Context, input ExchangeInput) (*http.Request, error) {
	form := url.Values{}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("tokenexchange: building IdP request: %w", err)
	}

	if err := e.setGrantFields(form); err != nil {
		return nil, err
	}
	if err := e.setSubjectToken(form, input); err != nil {
		return nil, err
	}
	e.setSubjectTokenType(form)
	if err := e.setClientAuth(form, req); err != nil {
		return nil, err
	}
	if err := e.setAudience(form, input); err != nil {
		return nil, err
	}
	e.setScopes(form, input)

	encoded := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(encoded))
	req.ContentLength = int64(len(encoded))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// setGrantFields sets the grant_type URN. The grant type acts as a
// protocol selector (RFC 8693 vs RFC 7523) — each protocol defines its
// own URN. Per-request values (the user's JWT) and IdP-specific
// metadata (subject_token_type) are handled by setSubjectToken.
func (e *Exchanger) setGrantFields(form url.Values) error {
	switch e.grantType {
	case GrantTypeTokenExchange:
		form.Set("grant_type", URNGrantTypeTokenExchange)
	default:
		return fmt.Errorf("tokenexchange: unknown GrantType %d (programming error — Params built outside FromConfig)", e.grantType)
	}
	return nil
}

// setSubjectToken places the user's inbound JWT into the form field
// appropriate for the configured protocol. The parameter name is
// protocol-defined (RFC 8693: "subject_token"; RFC 7523: "assertion")
// and switches on grant type. The subject_token_type is an independent
// axis that varies across IdPs within the same protocol (Keycloak:
// only access_token; Okta: also id_token) and will become its own
// config field.
func (e *Exchanger) setSubjectToken(form url.Values, input ExchangeInput) error {
	switch e.grantType {
	case GrantTypeTokenExchange:
		form.Set("subject_token", input.SubjectToken)
	default:
		return fmt.Errorf("tokenexchange: unknown GrantType %d for subject token placement", e.grantType)
	}
	return nil
}

// setSubjectTokenType sets the subject_token_type form field (required
// by RFC 8693 §2.1 whenever subject_token is present). The value is an
// architectural invariant, not an operator choice: Hop 1 receives the
// subject token as a Bearer credential (RFC 6750), which is by
// definition an access token. The MCP server never receives an ID
// token in the Authorization header, so subject_token_type is always
// access_token regardless of IdP.
func (e *Exchanger) setSubjectTokenType(form url.Values) {
	form.Set("subject_token_type", URNTokenTypeAccessToken)
}

// setClientAuth places client credentials in exactly one location:
// the Authorization header (client_secret_basic) or the form body
// (client_secret_post). Some IdPs treat credentials in both locations
// as a protocol violation, so the two paths are mutually exclusive.
func (e *Exchanger) setClientAuth(form url.Values, req *http.Request) error {
	switch e.clientAuthMethod {
	case ClientSecretBasic:
		req.SetBasicAuth(e.clientID, e.clientSecret)
	case ClientSecretPost:
		form.Set("client_id", e.clientID)
		form.Set("client_secret", e.clientSecret)
	default:
		return fmt.Errorf("tokenexchange: unknown ClientAuthMethod %d (programming error — Params built outside FromConfig)", e.clientAuthMethod)
	}
	return nil
}

// setAudience places the per-broker audience value into the form field
// selected by the configured AudienceFormat. V1 implements only the
// canonical RFC 8693 "audience" parameter.
func (e *Exchanger) setAudience(form url.Values, input ExchangeInput) error {
	switch e.audienceParam {
	case AudienceParamAudience:
		if input.Audience != "" {
			form.Set("audience", input.Audience)
		}
	default:
		return fmt.Errorf("tokenexchange: unknown AudienceFormat %d (programming error — Params built outside FromConfig)", e.audienceParam)
	}
	return nil
}

// setScopes adds the space-joined scope field per RFC 6749 §3.3.
// Omitted when the caller passed an empty slice so the IdP applies
// its per-client default scopes.
func (e *Exchanger) setScopes(form url.Values, input ExchangeInput) {
	if len(input.Scopes) > 0 {
		form.Set("scope", strings.Join(input.Scopes, " "))
	}
}
