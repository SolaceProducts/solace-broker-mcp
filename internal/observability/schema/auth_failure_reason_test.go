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

package schema

import (
	"reflect"
	"testing"
)

// TestAuthFailureReasons_WireValues pins the five strings themselves, not the
// consts that name them. These values leave the process — as an audit
// record's reason field and as an mcp_auth_failure_total{reason} label — so a
// rename here silently breaks a customer's saved SIEM query or dashboard.
// This is the only place the wire form is pinned as such: other tests do
// spell the strings out (audit/event_test.go's vocabulary table,
// auth/auth_audit_test.go's expected Failure calls), but only incidentally,
// because each of those crosses a boundary deliberately typed as a plain
// string. They would all fail on a rename too — this is the one that says why.
// docs/observability.md glosses the same five for operators; change both
// together.
func TestAuthFailureReasons_WireValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got  AuthFailureReason
		want string
	}{
		{AuthFailureReasonInvalidToken, "invalid_token"},
		{AuthFailureReasonExpired, "expired"},
		{AuthFailureReasonAudienceMismatch, "audience_mismatch"},
		{AuthFailureReasonSignatureInvalid, "signature_invalid"},
		{AuthFailureReasonMissing, "missing"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.want {
			t.Errorf("wire value = %q, want %q — this string is in customer queries", tc.got, tc.want)
		}
	}
	if got := len(AuthFailureReasons()); got != len(cases) {
		t.Errorf("AuthFailureReasons() has %d values, this test pins %d — a value was added or removed without updating docs/observability.md's gloss table and this test", got, len(cases))
	}
}

// TestAuthFailureReasons_SortedAndFresh pins the accessor's contract, which
// its callers rely on: internal/observability/audit builds the constructor's
// validation set from it at package init, and SOL-152099 will build its
// counter's label pre-registration from it. A shared backing array would let
// either mutate the other's view of the closed set.
func TestAuthFailureReasons_SortedAndFresh(t *testing.T) {
	t.Parallel()
	want := []AuthFailureReason{
		AuthFailureReasonAudienceMismatch,
		AuthFailureReasonExpired,
		AuthFailureReasonInvalidToken,
		AuthFailureReasonMissing,
		AuthFailureReasonSignatureInvalid,
	}
	if got := AuthFailureReasons(); !reflect.DeepEqual(got, want) {
		t.Errorf("AuthFailureReasons() = %v, want %v (sorted)", got, want)
	}

	first := AuthFailureReasons()
	first[0] = "mutated"
	if second := AuthFailureReasons(); !reflect.DeepEqual(second, want) {
		t.Errorf("mutating a returned slice changed a later call: %v — callers share the backing array", second)
	}
}
