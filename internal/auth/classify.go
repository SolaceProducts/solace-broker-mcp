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
	"errors"
	"strings"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	"github.com/coreos/go-oidc/v3/oidc"
)

// ClassifyAuthFailure maps an auth-rejection error to schema.AuthFailureReason,
// the closed vocabulary the auth_failure audit record and the
// mcp_auth_failure_total{reason} counter both carry (SOL-152097).
//
// The vocabulary itself is owned by internal/observability/schema, not by this
// package (SOL-154163): the five values are output schema, and a leaf package
// both this package and internal/observability/{audit,metrics} can import is
// the only owner that needs no drift guard. There IS an import-cycle
// constraint in play, but only in one direction — internal/observability/audit
// imports this package for identity (auth.PrincipalFrom), so this package
// cannot import that one back (see AuthAuditHook's doc). It never blocked
// importing schema, which imports nothing of ours.
//
// Story 24 (SOL-152099, "coordinate with" — not a Jira dependency of this
// story, but the ticket that needed this same classification) is expected to
// call this exact function for its counter labels, so the metric and the audit
// record read from one classifier and cannot drift into disagreeing about why
// a token was rejected — the point ADR-009 makes about metric/log/audit
// consistency elsewhere in this codebase. Add a case here, never a second
// classifier, if a new failure shape needs one.
//
// err should be the richest error available at the point of rejection — the
// value verifier.Verify (or an equivalent claim/subject check) returned
// directly, before sanitizeTokenError collapses it to the client-visible
// sentinel. Classifying the sanitized sentinel instead would lose exactly the
// detail this function exists to recover.
//
// nil classifies as "missing": a caller with no error to classify has nothing
// to reject the token over except its absence.
func ClassifyAuthFailure(err error) schema.AuthFailureReason {
	if err == nil {
		return schema.AuthFailureReasonMissing
	}

	// TokenExpiredError is go-oidc's one exported category (see
	// coreos/go-oidc/v3/oidc.IDTokenVerifier.Verify) — a type check here is
	// exact and survives a message-text change upstream.
	var expired *oidc.TokenExpiredError
	if errors.As(err, &expired) {
		return schema.AuthFailureReasonExpired
	}

	// errNoSubject: RFC 9068 §2.2 makes sub hard-required (buildTokenInfo);
	// a token that verified but omits it is rejected for what it is missing,
	// not for being malformed.
	if errors.Is(err, errNoSubject) {
		return schema.AuthFailureReasonMissing
	}

	// go-oidc does not export types for these two checks (see
	// IDTokenVerifier.Verify), so classification matches the exact,
	// currently-stable message substrings it emits. The real pin against a
	// go-oidc version bump is auth_audit_test.go's
	// TestAuthHook_OIDC_WrongAudience_ClassifiesAudienceMismatch and
	// TestAuthHook_OIDC_TamperedSignature_ClassifiesSignatureInvalid, which
	// provoke these two errors from the actual library (via a real
	// oidc.Provider/Verifier) rather than asserting against a hardcoded
	// string — a wording change in go.mod's pinned
	// github.com/coreos/go-oidc/v3 would fail one of those, not merely this
	// package's own unit-level TestClassifyAuthFailure (whose two cases for
	// these branches use hand-written errors.New strings and would stay
	// green through such a change).
	msg := err.Error()
	switch {
	case strings.Contains(msg, "expected audience"):
		return schema.AuthFailureReasonAudienceMismatch
	case strings.Contains(msg, "failed to verify signature"):
		return schema.AuthFailureReasonSignatureInvalid
	default:
		// Catch-all: malformed JWT, unparseable claims, issuer mismatch, a
		// static dev-token mismatch, and anything else this function does
		// not (yet) have a more specific bucket for.
		return schema.AuthFailureReasonInvalidToken
	}
}
