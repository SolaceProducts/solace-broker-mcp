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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// buildIdPRequest assembles a POST to the IdP token endpoint. Each
// concern — grant-type wire shape, subject token, client authentication,
// target placement — is handled by its own method so new grant types or
// auth methods grow in isolation.
//
// Note on scopes: the request omits the RFC 6749 §3.3 "scope" parameter
// entirely, so the IdP grants its per-client / per-user default scopes.
// A future ticket may reintroduce scopes as a per-user value derived
// from the subject token; if it ever varies per call for the same
// subject token, it must join the dedup key (see dedup_key.go).
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
	e.setRequestedTokenUse(form)
	if err := e.setClientAuth(form, req); err != nil {
		return nil, err
	}
	if err := e.setTarget(form, input); err != nil {
		return nil, err
	}

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
	case GrantTypeJWTBearer:
		form.Set("grant_type", URNGrantTypeJWTBearer)
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
	case GrantTypeJWTBearer:
		// RFC 7523 §2.1: the user's JWT is the "assertion", not "subject_token".
		form.Set("assertion", input.SubjectToken)
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
//
// RFC 7523 jwt-bearer has no subject_token_type parameter at all — the
// assertion's type is implicit in the grant itself — so this is a no-op
// for GrantTypeJWTBearer.
func (e *Exchanger) setSubjectTokenType(form url.Values) {
	if e.grantType == GrantTypeTokenExchange {
		form.Set("subject_token_type", URNTokenTypeAccessToken)
	}
}

// setRequestedTokenUse sets the fixed requested_token_use=on_behalf_of
// field Entra's On-Behalf-Of flow requires on the wire (not a YAML key —
// SOL-153245 FD lock). RFC 8693 token-exchange has no such field, so this
// is a no-op for GrantTypeTokenExchange.
func (e *Exchanger) setRequestedTokenUse(form url.Values) {
	if e.grantType == GrantTypeJWTBearer {
		form.Set("requested_token_use", requestedTokenUseOnBehalfOf)
	}
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

// setTarget places the per-broker target (brokers.<alias>.auth.target) into
// the form field the grant type defines — the operator names one downstream
// API, and the protocol decides how it is spelled on the wire. RFC 8693
// token exchange carries it as "audience", optional (omitted when unset).
// RFC 7523 jwt-bearer carries it as "scope", required — Microsoft marks
// On-Behalf-Of's scope Required, so an empty or whitespace-only target
// fails here, before any HTTP call, rather than at the IdP with a less
// actionable error.
func (e *Exchanger) setTarget(form url.Values, input ExchangeInput) error {
	switch e.grantType {
	case GrantTypeTokenExchange:
		if input.Target != "" {
			form.Set("audience", input.Target)
		}
	case GrantTypeJWTBearer:
		if strings.TrimSpace(input.Target) == "" {
			return errors.New("jwt-bearer request missing scope")
		}
		form.Set("scope", input.Target)
	default:
		return fmt.Errorf("tokenexchange: unknown GrantType %d for target placement (programming error — Params built outside FromConfig)", e.grantType)
	}
	return nil
}
