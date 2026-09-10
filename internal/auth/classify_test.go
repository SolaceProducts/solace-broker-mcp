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
	"fmt"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	"github.com/coreos/go-oidc/v3/oidc"
)

// TestClassifyAuthFailure pins every branch directly and fast, using
// hand-written errors — including the two cases keyed on a go-oidc message
// substring ("expected audience", "failed to verify signature"). These two
// hardcoded strings do NOT protect against a go-oidc wording change: that
// protection is auth_audit_test.go's TestAuthHook_OIDC_WrongAudience_
// ClassifiesAudienceMismatch and TestAuthHook_OIDC_TamperedSignature_
// ClassifiesSignatureInvalid, which provoke the real errors from the
// library. This test exists for the branches those two cannot reach as
// cheaply (nil, the sentinels, the catch-all) and to pin the exact strings
// this function currently matches on.
//
// want is named as a schema const, not a bare string: this test pins which
// branch produces which reason, and the const's own wire value is pinned by
// internal/observability/schema's TestAuthFailureReasons_WireValues.
func TestClassifyAuthFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want schema.AuthFailureReason
	}{
		{"nil classifies as missing", nil, schema.AuthFailureReasonMissing},
		{"go-oidc TokenExpiredError", &oidc.TokenExpiredError{}, schema.AuthFailureReasonExpired},
		{"wrapped TokenExpiredError", fmt.Errorf("wrap: %w", &oidc.TokenExpiredError{}), schema.AuthFailureReasonExpired},
		{"errNoSubject", errNoSubject, schema.AuthFailureReasonMissing},
		{"wrapped errNoSubject", fmt.Errorf("wrap: %w", errNoSubject), schema.AuthFailureReasonMissing},
		{"go-oidc audience message", errors.New(`oidc: expected audience "want" got ["got"]`), schema.AuthFailureReasonAudienceMismatch},
		{"go-oidc signature message", errors.New("failed to verify signature: bad sig"), schema.AuthFailureReasonSignatureInvalid},
		{"go-oidc issuer mismatch falls to catch-all", errors.New("oidc: id token issued by a different provider"), schema.AuthFailureReasonInvalidToken},
		{"go-oidc malformed jwt falls to catch-all", errors.New("oidc: malformed jwt: bad"), schema.AuthFailureReasonInvalidToken},
		{"unrelated error falls to catch-all", errVerificationFailed, schema.AuthFailureReasonInvalidToken},
		{"errMalformedClaims falls to catch-all", errMalformedClaims, schema.AuthFailureReasonInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAuthFailure(tc.err); got != tc.want {
				t.Errorf("ClassifyAuthFailure(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyAuthFailure_ReturnsOnlyClosedVocabulary pins that every case
// above returns a member of the closed vocabulary, read from its owner
// (SOL-154163) rather than restated as a literal here.
//
// The named return type does not make this tautological: AuthFailureReason is
// a string type, so a new case could return AuthFailureReason("weird") and
// still compile. This is the assertion that a case has to draw from the
// consts.
func TestClassifyAuthFailure_ReturnsOnlyClosedVocabulary(t *testing.T) {
	closedSet := make(map[schema.AuthFailureReason]bool)
	for _, r := range schema.AuthFailureReasons() {
		closedSet[r] = true
	}
	for _, err := range []error{
		nil,
		&oidc.TokenExpiredError{},
		errNoSubject,
		errors.New(`oidc: expected audience "a" got ["b"]`),
		errors.New("failed to verify signature: x"),
		errors.New("anything else"),
	} {
		got := ClassifyAuthFailure(err)
		if !closedSet[got] {
			t.Errorf("ClassifyAuthFailure(%v) = %q, not in the closed vocabulary", err, got)
		}
	}
}
