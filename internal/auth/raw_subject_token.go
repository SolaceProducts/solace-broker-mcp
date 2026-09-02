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
// InjectRawSubjectToken runs immediately downstream of the SDK middleware
// and captures the raw token from the Authorization header onto the
// request context under an unexported key. RawSubjectTokenFromContext is
// the read-side accessor used by the outbound OAuth Authenticator.
//
// Position invariant: InjectRawSubjectToken is wired so it runs only AFTER
// sdkauth.RequireBearerToken has validated the token. Therefore a value
// present under rawSubjectTokenKey{} has already been validated by the
// SDK (signature, issuer, audience, expiry). Downstream code reads it
// without re-validating.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// rawSubjectTokenKey is the unexported context key under which the raw
// bearer token is stored. The empty-struct, package-private type makes
// the key impossible to construct from outside this package, so no other
// package can collide with it or read the value except through
// RawSubjectTokenFromContext.
type rawSubjectTokenKey struct{}

// InjectRawSubjectToken returns middleware that copies the bearer token
// from the Authorization header onto the request context.
//
// On any malformed or non-Bearer Authorization header the middleware is a
// no-op — it does not reject the request and does not modify the context.
// Rejection is the responsibility of whatever component sits in front of
// this middleware in the chain; this middleware only captures.
//
// See the package doc for the position invariant and why this middleware
// exists.
func InjectRawSubjectToken(next http.Handler) http.Handler {
	// One-shot startup log per installation. Today this fires exactly
	// once at server startup (single call site in NewAuthMiddleware);
	// future multi-endpoint installs would correctly emit one line each.
	// No token content is logged here or anywhere else in this file.
	slog.Debug("InjectRawSubjectToken middleware installed")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := parseBearerToken(r.Header.Get("Authorization")); ok {
			ctx := context.WithValue(r.Context(), rawSubjectTokenKey{}, token)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// parseBearerToken extracts the token from an Authorization header using the
// same rules as sdkauth.verify in go-sdk v1.5.0 (strings.Fields +
// case-insensitive "bearer"). If the SDK ever changes its parsing rules,
// this may under-capture — the safe drift direction, but worth revisiting
// on SDK upgrades. Shared by InjectRawSubjectToken and RequestExtraMiddleware.
func parseBearerToken(authHeader string) (string, bool) {
	fields := strings.Fields(authHeader)
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && fields[1] != "" {
		return fields[1], true
	}
	return "", false
}

// RawSubjectTokenFromContext returns the raw bearer token captured by
// InjectRawSubjectToken, or ("", false) if none is present.
//
// The ok return is false when no value is stored, when the stored value
// is the empty string, or when it is not a string (the latter two are
// defensive; the middleware never stores either in production). Folding
// these into ok=false lets callers branch once.
//
// See the package doc for the validation invariant: a true return means
// the token has already been validated upstream and the caller does not
// re-validate.
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
