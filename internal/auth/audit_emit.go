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

package auth

import (
	"context"
	"encoding/json"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// AuthAuditHook receives per-verification outcomes so this package can
// trigger auth_success/auth_failure audit-record emission (SOL-152097)
// without importing internal/observability/audit directly: that package
// already imports internal/auth for identity (auth.PrincipalFrom,
// auth.Principal), and Go disallows the reverse edge that would create — a
// direct call from here into audit.NewEvent would be a cycle, not merely
// bad layering.
//
// cmd/server wires the real implementation — audit.NewAuthHook(audit.Enabled
// (cfg.Observability)) — into NewAuthMiddleware / NewTokenVerifier. Every
// pre-SOL-152097 call site passes nil explicitly (not a variadic default —
// see NewAuthMiddleware's doc for why a variadic slot here would be the
// wrong shape for an audit observer), which reportAuthSuccess/
// reportAuthFailure below treat as fully inert — the same "off means inert,
// not degraded" contract every other audit gate in this codebase has
// (tools.WithAuditLog, resilience.WithAuditLog).
//
// This is the ONE call site inside the TokenVerifier closures
// (createStaticTokenVerifier, createOIDCTokenVerifier) that sees the rich,
// pre-sanitization error — every return path out of those closures collapses
// it to a client-visible sentinel (errVerificationFailed, errMalformedClaims,
// sanitizeTokenError's category sentinels), so a caller outside this package
// classifying one of those sentinels with ClassifyAuthFailure gets
// "invalid_token" for nearly everything. SOL-152099's mcp_auth_failure_total
// {reason} counter needs that same rich classification, which means it
// cannot be recorded from an arbitrary outside call site — it has to be
// driven from here too. The lowest-friction way to do that without this
// interface growing a second slot: implement AuthAuditHook once, have
// Failure both increment the counter (with the same reason string it
// receives) and delegate to audit.NewAuthHook's Failure for the audit
// record, and wire that single combined hook in cmd/server. A second,
// independent hook parameter is deliberately not provided speculatively;
// add one if a combined implementation turns out not to fit.
type AuthAuditHook interface {
	// Success is called once a token has fully verified, with the same
	// TokenInfo the verifier is about to return to the SDK.
	Success(ctx context.Context, info *sdkauth.TokenInfo)
	// Failure is called once a token is rejected. reason is one of
	// ClassifyAuthFailure's return values, as a plain string:
	// schema.AuthFailureReason is a string type, and an implementor's job is
	// to record the value it is handed, not to re-derive it, so this
	// signature deliberately does not name the vocabulary type. Widening it
	// to schema.AuthFailureReason would buy no safety here (the value is
	// already classified by the only classifier) and would move an interface
	// SOL-152099 is also going to implement. sub and clientID are
	// best-effort attribution: both "" unless the token parsed far enough to
	// yield them.
	Failure(ctx context.Context, reason, sub, clientID string)
}

// reportAuthSuccess calls hook.Success when hook is non-nil.
func reportAuthSuccess(ctx context.Context, hook AuthAuditHook, info *sdkauth.TokenInfo) {
	if hook == nil {
		return
	}
	hook.Success(ctx, info)
}

// reportAuthFailure calls hook.Failure when hook is non-nil. It takes the
// classified schema.AuthFailureReason and is the ONE place that widens it to
// the plain string AuthAuditHook.Failure accepts, so every call site upstream
// of here stays typed and no site has to decide for itself whether a bare
// string is acceptable.
func reportAuthFailure(ctx context.Context, hook AuthAuditHook, reason schema.AuthFailureReason, sub, clientID string) {
	if hook == nil {
		return
	}
	hook.Failure(ctx, string(reason), sub, clientID)
}

// bestEffortIdentity extracts sub and client_id from claims that decoded as
// valid JSON, for attribution on an auth_failure record whose rejection
// happened downstream of that decode — "the token parsed far enough to
// yield them" (SOL-152097). Errors are swallowed: this is best-effort audit
// attribution, never a decision input, so a claim that fails to decode here
// (the same failure buildTokenInfo already rejected the token over) simply
// leaves that field blank rather than surfacing a second error.
func bestEffortIdentity(raw map[string]json.RawMessage) (sub, clientID string) {
	c := Claims{raw: raw}
	sub, _ = c.String("sub")
	clientID, _ = c.String("client_id")
	return sub, clientID
}
