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

// Package auth — raw subject token plumbing for RFC 8693 token exchange.
//
// The MCP SDK's RequireBearerToken validates the incoming bearer token and
// places a *TokenInfo (parsed claims) on the request context, but discards
// the raw signed JWT string. Hop 2 token exchange (SOL-150797) needs that
// raw string as the subject_token in the RFC 8693 exchange request to the
// IdP.
//
// The writer is RequestExtraMiddleware (receiving MCP middleware). It copies
// the bearer from Extra.Header via WithRawSubjectToken onto the per-request
// JSON-RPC handler ctx. RawSubjectTokenFromContext is the read-side accessor
// used by the outbound OAuth Authenticator.
//
// Validation invariant (static and oauth modes): Extra's Authorization on a
// request that reached the JSON-RPC handler already passed
// RequireBearerToken (otherwise 401). A value present under
// rawSubjectTokenKey{} has therefore already been validated by the SDK
// (signature, issuer, audience, expiry). Hop 2 reads it without
// re-validating.
//
// Disabled mode has no hop 2 oauth. RequestExtraMiddleware still runs and
// may stamp an unvalidated Authorization if the client sent one; nothing
// exchanges it.
package auth

import (
	"context"
	"strings"
)

// rawSubjectTokenKey is the unexported context key under which the raw
// bearer token is stored. The empty-struct, package-private type makes
// the key impossible to construct from outside this package, so no other
// package can collide with it or read the value except through
// RawSubjectTokenFromContext.
type rawSubjectTokenKey struct{}

// WithRawSubjectToken stores the raw bearer token on ctx. Mirrors the
// pattern of correlation.With. Called by RequestExtraMiddleware (applyRequestExtra)
// after parseBearerToken extracts the bearer from Extra.Header.
func WithRawSubjectToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, rawSubjectTokenKey{}, token)
}

// parseBearerToken extracts the token from an Authorization header using the
// same rules as sdkauth.verify in go-sdk v1.5.0 (strings.Fields +
// case-insensitive "bearer"). If the SDK ever changes its parsing rules,
// this may under-capture — the safe drift direction, but worth revisiting
// on SDK upgrades. Shared with RequestExtraMiddleware (applyRequestExtra).
func parseBearerToken(authHeader string) (string, bool) {
	fields := strings.Fields(authHeader)
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && fields[1] != "" {
		return fields[1], true
	}
	return "", false
}

// RawSubjectTokenFromContext returns the raw bearer token captured by
// receiving middleware (WithRawSubjectToken / RequestExtraMiddleware),
// or ("", false) if none is present.
//
// The ok return is false when no value is stored, when the stored value
// is the empty string, or when it is not a string (the latter two are
// defensive; the middleware never stores either in production). Folding
// these into ok=false lets callers branch once.
//
// See the package doc for the validation invariant: in static/oauth a true
// return means the token was validated upstream. Disabled mode has no hop 2
// and must not treat a present value as validated.
func RawSubjectTokenFromContext(ctx context.Context) (string, bool) {
	v := ctx.Value(rawSubjectTokenKey{})
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}
