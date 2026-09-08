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

// The auth_success/auth_failure schema NewAuthHook emits (SOL-152097).
// internal/auth/auth_audit_test.go covers what internal/auth calls the hook
// with (reason classification, best-effort identity); these tests cover the
// other half — what the hook actually writes to the audit stream.

package audit

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// ofAuthType returns the records matching audit_event_type typ from a
// captureRecords result, filtered to event="audit" first — mirrors
// internal/tools' ofType.
func ofAuthType(records []map[string]any, typ EventType) []map[string]any {
	var out []map[string]any
	for _, rec := range records {
		if rec["event"] == EventValue && rec["audit_event_type"] == string(typ) {
			out = append(out, rec)
		}
	}
	return out
}

// TestNewAuthHook_Success_EmitsAuthSuccess pins the full auth_success shape:
// principal identity from the TokenInfo, INFO level, and — the negative
// assertions the ticket calls for — no outcome, reason, or error_type.
func TestNewAuthHook_Success_EmitsAuthSuccess(t *testing.T) {
	hook := NewAuthHook(true)
	info := &sdkauth.TokenInfo{UserID: "auth0|user1", Extra: map[string]any{"client_id": "cursor-ide"}}

	records := captureRecords(t, 0, func() {
		hook.Success(context.Background(), info)
	})

	events := ofAuthType(records, EventAuthSuccess)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 auth_success record, got %d:\n%v", len(events), records)
	}
	rec := events[0]
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	principal, ok := rec["principal"].(map[string]any)
	if !ok || principal["sub"] != "auth0|user1" {
		t.Errorf("principal = %#v, want {sub: auth0|user1}", rec["principal"])
	}
	if got := rec["agent_client_id"]; got != "cursor-ide" {
		t.Errorf("agent_client_id = %v, want %q", got, "cursor-ide")
	}
	for _, forbidden := range []string{"outcome", "reason", "error_type"} {
		if _, present := rec[forbidden]; present {
			t.Errorf("auth_success record carries %s = %v, want absent", forbidden, rec[forbidden])
		}
	}
}

// TestNewAuthHook_Failure_EmitsAuthFailure pins the full auth_failure shape:
// reason present, identity attributed when given, WARN level (event.go's
// levelByType), and no outcome or error_type.
func TestNewAuthHook_Failure_EmitsAuthFailure(t *testing.T) {
	hook := NewAuthHook(true)

	records := captureRecords(t, 0, func() {
		hook.Failure(context.Background(), "expired", "auth0|rejected", "cursor-ide")
	})

	events := ofAuthType(records, EventAuthFailure)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 auth_failure record, got %d:\n%v", len(events), records)
	}
	rec := events[0]
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if got := rec["reason"]; got != "expired" {
		t.Errorf("reason = %v, want %q", got, "expired")
	}
	principal, ok := rec["principal"].(map[string]any)
	if !ok || principal["sub"] != "auth0|rejected" {
		t.Errorf("principal = %#v, want {sub: auth0|rejected}", rec["principal"])
	}
	if got := rec["agent_client_id"]; got != "cursor-ide" {
		t.Errorf("agent_client_id = %v, want %q", got, "cursor-ide")
	}
	for _, forbidden := range []string{"outcome", "error_type"} {
		if _, present := rec[forbidden]; present {
			t.Errorf("auth_failure record carries %s = %v, want absent", forbidden, rec[forbidden])
		}
	}
}

// TestNewAuthHook_Failure_NoIdentityWhenUnknown pins the "caller is unknown
// by definition" case (EventAuthFailure's doc, event.go): both sub and
// clientID empty means no principal group at all, not one with an empty sub.
func TestNewAuthHook_Failure_NoIdentityWhenUnknown(t *testing.T) {
	hook := NewAuthHook(true)

	records := captureRecords(t, 0, func() {
		hook.Failure(context.Background(), "missing", "", "")
	})

	events := ofAuthType(records, EventAuthFailure)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 auth_failure record, got %d:\n%v", len(events), records)
	}
	if _, present := events[0]["principal"]; present {
		t.Errorf("auth_failure record carries principal = %v with no known identity, want absent", events[0]["principal"])
	}
}

// TestNewAuthHook_Disabled_emitsNothing pins the flag-off contract: both
// methods are fully inert.
func TestNewAuthHook_Disabled_emitsNothing(t *testing.T) {
	hook := NewAuthHook(false)

	records := captureRecords(t, 0, func() {
		hook.Success(context.Background(), &sdkauth.TokenInfo{UserID: "someone"})
		hook.Failure(context.Background(), "expired", "someone", "")
	})

	for _, rec := range records {
		if rec["event"] == EventValue {
			t.Errorf("audit record emitted with the capability off: %#v", rec)
		}
	}
}

// TestNewAuthHook_ConstructorRejection_EmitsDrop covers emitAuthEvent's
// build-or-drop branch: a reason outside auth_failure's closed vocabulary
// makes NewEvent reject the record, which must surface as a drop rather than
// vanish silently.
func TestNewAuthHook_ConstructorRejection_EmitsDrop(t *testing.T) {
	hook := NewAuthHook(true)

	records := captureRecords(t, 0, func() {
		hook.Failure(context.Background(), "not-a-real-reason", "", "")
	})

	if got := len(ofAuthType(records, EventAuthFailure)); got != 0 {
		t.Errorf("a record the constructor rejected was emitted anyway (%d)", got)
	}
	drops := ofAuthType(records, EventAuditDrop)
	if len(drops) != 1 {
		t.Fatalf("want exactly 1 audit_drop when the constructor rejects the record, got %d:\n%v", len(drops), records)
	}
	if got := drops[0]["dropped_audit_event_type"]; got != string(EventAuthFailure) {
		t.Errorf("dropped_audit_event_type = %v, want %q", got, EventAuthFailure)
	}
}

// TestAuthFailureReasons_MatchesEventSchemaVocabulary guards against the two
// closed vocabularies drifting apart: auth.AuthFailureReasons() (what
// ClassifyAuthFailure returns from, and what SOL-152099 is expected to call
// for its own counter's label pre-registration) and this package's own
// authFailureReasons (what NewEvent actually accepts on an auth_failure
// record). internal/auth cannot import this package to check itself
// (AuthAuditHook's doc explains the cycle), so the check runs from this side,
// the same shape internal/tools/audit_error_type_drift_test.go uses for
// error_type.
func TestAuthFailureReasons_MatchesEventSchemaVocabulary(t *testing.T) {
	got := make([]string, 0, len(authFailureReasons))
	for r := range authFailureReasons {
		got = append(got, r)
	}
	sort.Strings(got)
	want := auth.AuthFailureReasons()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event.go's authFailureReasons = %v, want auth.AuthFailureReasons() = %v — the two closed vocabularies have drifted", got, want)
	}
}
