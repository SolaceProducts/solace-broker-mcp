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
// The middleware is intended to run AFTER the MCP SDK's RequireBearerToken
// in the chain, so the token it captures has already been validated by
// the SDK. The position is established at wiring time in NewAuthMiddleware.
//
// On any malformed or non-Bearer Authorization header the middleware is a
// no-op — it does not reject the request and does not modify the context.
// The SDK middleware upstream is the authority on whether a request is
// allowed through.
func InjectRawSubjectToken(next http.Handler) http.Handler {
	// One-shot startup log per installation. Today this fires exactly
	// once at server startup (single call site in NewAuthMiddleware);
	// future multi-endpoint installs would correctly emit one line each.
	// No token content is logged here or anywhere else in this file.
	slog.Debug("InjectRawSubjectToken middleware installed")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The parse here intentionally mirrors sdkauth.verify in
		// go-sdk v1.5.0 (auth/auth.go: strings.Fields + case-insensitive
		// "bearer" check). Because the SDK runs upstream of us, anything
		// it rejected we never see; anything it accepted should match
		// these conditions. If the SDK ever changes its parsing rules,
		// this code may under-capture (we'd silently fail to stash a
		// token the SDK approved) — the safe drift direction, but worth
		// revisiting on SDK upgrades.
		authHeader := r.Header.Get("Authorization")
		fields := strings.Fields(authHeader)
		if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && fields[1] != "" {
			ctx := context.WithValue(r.Context(), rawSubjectTokenKey{}, fields[1])
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RawSubjectTokenFromContext returns the raw bearer token captured by
// InjectRawSubjectToken, or ("", false) if none is present.
//
// The ok return is false when:
//   - no value is stored under the key (InjectRawSubjectToken was not in
//     the chain, or the request had no valid Bearer header);
//   - the stored value is the empty string (defensive; the middleware
//     never stores an empty token, but the accessor folds the check in
//     so callers branch once);
//   - the stored value is not a string (impossible in production; only
//     this package writes the key).
//
// A successful return implies the token has been validated by the SDK
// middleware upstream of InjectRawSubjectToken (signature, issuer,
// audience, expiry). The caller does not re-validate.
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
