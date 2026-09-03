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

// Package auth — the canonical, context-carried caller identity.
//
// PrincipalMiddleware populates Principal once per request and attaches it to
// the context. Audit and metrics code reads it back with PrincipalFrom, and
// tools.Identity projects it, so nothing decodes the token a second time and
// the log line and audit event cannot drift (SOL-152087, ADR-006).
//
// Freshness invariant: the principal describes the token presented on THIS
// request. That is why population is an mcp.Middleware over the SDK's
// per-request RequestExtra and not an http.Handler middleware: in stateful
// streamable-HTTP mode the context a tool handler receives descends from the
// session-establishing POST, not the current one, so an http-middleware
// principal would name the token that opened the session for as long as it
// lived — through every client token refresh. RequestExtra is rebuilt per
// message, so it is the only carrier that tracks the caller.
package auth

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/logging/sanitize"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// principalKey is the unexported context key under which the authenticated
// Principal is stored. Mirrors the rawSubjectTokenKey{} pattern in
// raw_subject_token.go: an empty-struct, package-private type makes the key
// impossible to construct from outside this package, so no other package can
// collide with it or read the value except through PrincipalFrom.
type principalKey struct{}

// Principal identifies the authenticated caller behind a request.
//
// Committed claim set (decision D2): sub, scope, client_id, iss, jti.
// preferred_username and email are deliberately excluded — Q-013 (decided
// 2026-07-29) chose sub-only, because directly identifying data in an
// append-only audit store conflicts with GDPR/PIPL erasure, and adding a
// field later is compatible while removing one is not.
//
// Every string has passed through sanitize.Claim at construction, so all
// consumers emit safe values from one sanitization layer. Absent claims are
// ""; sentinels like sanitize.AbsentSentinel belong to the consumer.
//
// Fields are unexported so only NewPrincipal can build a present Principal.
// The zero value reports Present() == false — what PrincipalFrom returns when
// no middleware ran (auth mode "disabled", or a non-HTTP call site).
//
// Safe to read from any goroutine because it is effectively immutable: nothing
// writes a field after NewPrincipal returns, and Scopes() hands out a copy of
// the one slice. A mutable field or a setter would end that, so add neither.
type Principal struct {
	present  bool
	sub      string
	scopes   []string
	clientID string
	iss      string
	jti      string
}

// Present reports whether a verified token stood behind this Principal.
func (p Principal) Present() bool { return p.present }

// Sub returns the subject: the opaque identifier audit records name the
// acting principal by.
func (p Principal) Sub() string { return p.sub }

// Scopes returns a copy of the scope values, so callers may mutate freely.
func (p Principal) Scopes() []string {
	out := make([]string, len(p.scopes))
	copy(out, p.scopes)
	return out
}

// ClientID returns the client_id claim, or "" when the IdP omitted it.
func (p Principal) ClientID() string { return p.clientID }

// Iss returns the issuer claim, or "" when absent.
func (p Principal) Iss() string { return p.iss }

// Jti returns the JWT ID claim, or "" when the IdP omitted it.
func (p Principal) Jti() string { return p.jti }

// NewPrincipal projects a verified *sdkauth.TokenInfo onto Principal. As the
// only constructor of a present Principal, it is the one place identity
// claims are read and the one place they are sanitized.
//
// A nil TokenInfo returns the zero value: auth mode "disabled" never verifies
// a token, and test scaffolding may build requests with no Extra. The static
// verifier sets only UserID, so its Principal carries sub alone.
//
// The mapping follows what buildTokenInfo stores; the three Extra keys are
// present with "" when the IdP omitted the claim. ctx is used only to
// correlate the verifier-contract ERROR below.
func NewPrincipal(ctx context.Context, info *sdkauth.TokenInfo) Principal {
	if info == nil {
		return Principal{}
	}
	scopes := make([]string, len(info.Scopes))
	for i, s := range info.Scopes {
		scopes[i] = sanitize.Claim(s)
	}
	return Principal{
		present:  true,
		sub:      sanitize.Claim(info.UserID),
		scopes:   scopes,
		clientID: sanitize.Claim(extraString(ctx, info, "client_id")),
		iss:      sanitize.Claim(extraString(ctx, info, "iss")),
		jti:      sanitize.Claim(extraString(ctx, info, "jti")),
	}
}

// extraString reads a string-typed identity claim from TokenInfo.Extra. A
// missing key returns "" — the IdP did not issue the claim.
//
// A present non-string value is a contract violation by our own verifier
// (buildTokenInfo stores only strings under these keys). We emit an ERROR
// naming the key and observed type — never the value — and return
// sanitize.VerifierBugSentinel, which SIEM rules can tell apart from
// "<absent>". Deliberately not a panic: the panic version shipped under
// SOL-149606 killed the request before the audit defer registered, dropping
// the audit line on exactly the requests where it mattered most.
func extraString(ctx context.Context, info *sdkauth.TokenInfo, key string) string {
	v, ok := info.Extra[key]
	if !ok {
		return ""
	}
	s, isString := v.(string)
	if !isString {
		// ErrorContext, not Error: the correlation slog handler reads the ID
		// off the record's context, so this record carries one at all rather
		// than none. It is the same ID the audit line showing <verifier-bug>
		// carries, which is what makes them joinable — but note that ID is
		// session-scoped, not per-request, in stateful streamable HTTP (see
		// the caveat in tools/register.go), so it narrows the search to a
		// session rather than to the one request.
		slog.ErrorContext(ctx, "internal: TokenInfo.Extra has unexpected type — verifier contract violation",
			slog.String("key", key),
			slog.String("got_type", fmt.Sprintf("%T", v)))
		return sanitize.VerifierBugSentinel
	}
	return s
}

// WithPrincipal returns a copy of ctx carrying p. PrincipalMiddleware is the
// production writer; tests stand in for it. The key stays unexported, so this
// and PrincipalFrom are the only ways in and out.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the Principal stored on ctx, or the zero value when
// none is set. Absence reads as the zero value, never a panic or sentinel —
// the property every consumer relies on.
func PrincipalFrom(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{}
}

// PrincipalMiddleware returns MCP receiving middleware that builds the
// request's Principal from the SDK's per-request RequestExtra and attaches it
// to the context every downstream handler sees.
//
// With no TokenInfo on the request it attaches nothing: auth mode "disabled"
// verifies no token, so PrincipalFrom keeps returning the zero value and
// absence stays a single code path. It never rejects — RequireBearerToken
// upstream is the authority on that.
//
// Registration order matters. AddReceivingMiddleware wraps the current
// handler, so the LAST middleware registered runs FIRST; this one must be
// registered after any middleware that reads the principal (today
// tools.WithListFiltering). cmd/server expresses that order in one place,
// installRequestMiddleware, which TestPrincipalReachesListFiltering calls
// directly so a reorder there fails the test.
func PrincipalMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			var info *sdkauth.TokenInfo
			if extra := req.GetExtra(); extra != nil {
				info = extra.TokenInfo
			}
			if info == nil {
				return next(ctx, method, req)
			}
			return next(WithPrincipal(ctx, NewPrincipal(ctx, info)), method, req)
		}
	}
}
