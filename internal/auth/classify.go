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
	"sort"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// authFailureReasonVocabulary is the closed set ClassifyAuthFailure returns
// from, and the single source of truth for it: internal/observability/audit
// cannot import this package's unexported identifiers to check against, and
// this package cannot import that one (AuthAuditHook's doc explains why), so
// audit/event_test.go asserts its own authFailureReasons map matches
// AuthFailureReasons() exactly, the same drift guard
// internal/tools/audit_error_type_drift_test.go uses for error_type.
var authFailureReasonVocabulary = []string{
	"invalid_token",
	"expired",
	"audience_mismatch",
	"signature_invalid",
	"missing",
}

// AuthFailureReasons returns the closed reason vocabulary ClassifyAuthFailure
// draws from, sorted, as a fresh slice the caller may mutate. SOL-152099 is
// expected to call this for its mcp_auth_failure_total{reason} counter's
// label pre-registration, rather than hardcoding the five values a second
// time.
func AuthFailureReasons() []string {
	out := make([]string, len(authFailureReasonVocabulary))
	copy(out, authFailureReasonVocabulary)
	sort.Strings(out)
	return out
}

// ClassifyAuthFailure maps an auth-rejection error to the closed reason
// vocabulary the auth_failure audit record carries (SOL-152097): one of
// invalid_token, expired, audience_mismatch, signature_invalid, missing.
//
// Story 24 (SOL-152099, "coordinate with" — not a Jira dependency of this
// story, but the ticket that needed this same classification) is expected to
// call this exact function for its mcp_auth_failure_total{reason} counter
// labels, so the metric and the audit record read from one place and cannot
// drift into disagreeing about why a token was rejected — the point ADR-009
// makes about metric/log/audit consistency elsewhere in this codebase. Add a
// case here, never a second classifier, if a new failure shape needs one.
//
// err should be the richest error available at the point of rejection — the
// value verifier.Verify (or an equivalent claim/subject check) returned
// directly, before sanitizeTokenError collapses it to the client-visible
// sentinel. Classifying the sanitized sentinel instead would lose exactly the
// detail this function exists to recover.
//
// nil classifies as "missing": a caller with no error to classify has nothing
// to reject the token over except its absence.
func ClassifyAuthFailure(err error) string {
	if err == nil {
		return "missing"
	}

	// TokenExpiredError is go-oidc's one exported category (see
	// coreos/go-oidc/v3/oidc.IDTokenVerifier.Verify) — a type check here is
	// exact and survives a message-text change upstream.
	var expired *oidc.TokenExpiredError
	if errors.As(err, &expired) {
		return "expired"
	}

	// errNoSubject: RFC 9068 §2.2 makes sub hard-required (buildTokenInfo);
	// a token that verified but omits it is rejected for what it is missing,
	// not for being malformed.
	if errors.Is(err, errNoSubject) {
		return "missing"
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
		return "audience_mismatch"
	case strings.Contains(msg, "failed to verify signature"):
		return "signature_invalid"
	default:
		// Catch-all: malformed JWT, unparseable claims, issuer mismatch, a
		// static dev-token mismatch, and anything else this function does
		// not (yet) have a more specific bucket for.
		return "invalid_token"
	}
}
