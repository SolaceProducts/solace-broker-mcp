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

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	testSub      = "auth0|abc123"
	testClientID = "cursor-ide"
	testHash     = "a3134dfcbe328174efbc0bad1443ac355d7231b72a9c03e4c06bf83327f998e0"
)

// ctxWithPrincipal returns a context carrying a present principal, built the
// way the auth middleware builds one so the test cannot pass on a shape
// production never produces.
func ctxWithPrincipal(t *testing.T) context.Context {
	t.Helper()
	ctx := context.Background()
	return auth.WithPrincipal(ctx, auth.NewPrincipal(ctx, &sdkauth.TokenInfo{
		UserID: testSub,
		Extra: map[string]any{
			"client_id": testClientID,
			"iss":       "https://example.auth0.com/",
			"jti":       "jti-xyz",
		},
	}))
}

// render emits e through a JSON handler and returns the decoded record, so
// assertions are against the bytes a SIEM would actually index rather than
// against the in-memory attrs.
func render(t *testing.T, e Event) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	rec := slog.NewRecord(time.Unix(0, 0).UTC(), e.Level(), Message, 0)
	rec.AddAttrs(e.Attrs()...)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("handling record: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decoding record %q: %v", buf.String(), err)
	}
	return out
}

// validOperation is the minimum valid operation record, the base every
// applicability case below mutates one field of.
func validOperation() Fields {
	return Fields{
		Type: EventOperation, Outcome: OutcomeSuccess,
		Tool: "delete-queue", Broker: "prod-1", ArgumentsHash: testHash,
		StartedAt: time.Unix(1750000000, 0), Duration: 250 * time.Millisecond,
	}
}

// TestEventTypes_closedSetIsSeven pins the discriminator vocabulary.
//
// The count is asserted against len(EventTypes()) and the members against an
// explicit list, so adding a type without listing it here fails, and listing
// one without adding it fails too. The set grew from five to six to seven over
// three revisions and each revision's prose undercounted it; a hardcoded
// integer would have gone stale silently every time.
func TestEventTypes_closedSetIsSeven(t *testing.T) {
	t.Parallel()
	want := []string{
		"audit_drop", "auth_failure", "auth_success", "authz_denied",
		"broker_auth_retry", "broker_authz_denied", "operation",
	}
	got := EventTypes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit_event_type set changed.\n got: %v\nwant: %v\n"+
			"This is the discriminator customers query on. Changing it is an "+
			"audit-schema change: update docs/observability.md and consider "+
			"whether schema.AuditSchemaVersion must be bumped.", got, want)
	}
	if len(got) != len(want) {
		t.Errorf("EventTypes() returned %d values, want %d", len(got), len(want))
	}
}

// TestEventTypes_everyTypeHasALevel pins that the two per-type tables cover the
// same set. A type present in one and missing from the other would emit at
// slog's zero level (INFO) by accident rather than by decision.
func TestEventTypes_everyTypeHasALevel(t *testing.T) {
	t.Parallel()
	if len(levelByType) != len(applicabilityByType) {
		t.Fatalf("levelByType has %d entries, applicabilityByType has %d", len(levelByType), len(applicabilityByType))
	}
	for typ := range applicabilityByType {
		if _, ok := levelByType[typ]; !ok {
			t.Errorf("audit_event_type %q has no emission level", typ)
		}
	}
}

// TestErrorTypes_closedVocabulary pins the error_type set against the values
// internal/tools actually computes.
//
// The count is asserted as len(ErrorTypes()), never as a literal in the
// assertion, so a value added to the vocabulary without being added here fails
// loudly. Ticket text has claimed ten, then eleven; the shipped code computes
// twelve, across four logToolResult call sites. See
// internal/tools/audit_error_type_drift_test.go, which walks those sites' AST
// and fails if the two sets ever diverge again.
func TestErrorTypes_closedVocabulary(t *testing.T) {
	t.Parallel()
	want := []string{
		"bad_request", "broker_init_error", "execution_error", "marshal_error",
		"missing_broker", "nil_result", "not_found", "output_validation_error",
		"panic", "unknown_broker", "unknown_tool", "validation_error",
	}
	got := ErrorTypes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("error_type vocabulary changed.\n got: %v\nwant: %v", got, want)
	}
	if len(got) != len(want) {
		t.Errorf("ErrorTypes() returned %d values, want %d", len(got), len(want))
	}
}

// TestErrorTypes_returnsACopy guards the package's own vocabulary from a
// caller that sorts, appends to, or truncates the returned slice.
func TestErrorTypes_returnsACopy(t *testing.T) {
	t.Parallel()
	first := ErrorTypes()
	n := len(first)
	first[0] = "clobbered"
	if second := ErrorTypes(); second[0] == "clobbered" || len(second) != n {
		t.Errorf("ErrorTypes() exposed package state; after mutation got %v", second)
	}
}

// TestNewEvent_commonFieldsOnEveryRecord pins the five fields a SIEM's routing
// and pinning depend on, on every record type including audit_drop.
//
// event is asserted to be the constant "audit" on all seven: a drop record
// that renamed itself would fall outside the one filter customers route on,
// which is the situation the drop record exists to report.
func TestNewEvent_commonFieldsOnEveryRecord(t *testing.T) {
	t.Parallel()
	minimal := map[EventType]Fields{
		EventOperation:         validOperation(),
		EventAuthSuccess:       {Type: EventAuthSuccess},
		EventAuthFailure:       {Type: EventAuthFailure, Reason: "expired"},
		EventAuthzDenied:       {Type: EventAuthzDenied, Reason: "not_permitted", Tool: "delete-queue"},
		EventBrokerAuthzDenied: {Type: EventBrokerAuthzDenied, Reason: "permission_denied", Tool: "delete-queue", Broker: "prod-1"},
		EventBrokerAuthRetry:   {Type: EventBrokerAuthRetry, Outcome: OutcomeSuccess, Broker: "prod-1"},
		EventAuditDrop:         {Type: EventAuditDrop},
	}
	if len(minimal) != len(applicabilityByType) {
		t.Fatalf("this test covers %d record types, the table has %d", len(minimal), len(applicabilityByType))
	}

	ctx := correlation.With(ctxWithPrincipal(t), "corr-123")
	for typ, f := range minimal {
		t.Run(string(typ), func(t *testing.T) {
			e, err := NewEvent(ctx, f)
			if err != nil {
				t.Fatalf("NewEvent(%s): %v", typ, err)
			}
			rec := render(t, e)
			if rec["event"] != EventValue {
				t.Errorf("event = %v, want %q on every record", rec["event"], EventValue)
			}
			if rec["audit_event_type"] != string(typ) {
				t.Errorf("audit_event_type = %v, want %q", rec["audit_event_type"], typ)
			}
			if rec["audit_schema_version"] != schema.AuditSchemaVersion {
				t.Errorf("audit_schema_version = %v, want %q", rec["audit_schema_version"], schema.AuditSchemaVersion)
			}
			if rec["correlation_id"] != "corr-123" {
				t.Errorf("correlation_id = %v, want %q", rec["correlation_id"], "corr-123")
			}
			ts, ok := rec["timestamp_utc"].(string)
			if !ok {
				t.Fatalf("timestamp_utc missing or not a string: %v", rec["timestamp_utc"])
			}
			if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
				t.Errorf("timestamp_utc %q is not RFC 3339: %v", ts, err)
			}
			if rec["msg"] != Message {
				t.Errorf("msg = %v, want %q", rec["msg"], Message)
			}
		})
	}
}

// TestNewEvent_fieldApplicability is the table from docs/observability.md
// asserted one cell at a time. Each case takes a valid record of its type and
// makes exactly one change, so a failure names the specific rule that broke.
//
// This is what keeps the emission sites (SOL-152096, SOL-152097, SOL-153332)
// from drifting into different shapes of the same record: the rule lives here,
// not in three call sites.
func TestNewEvent_fieldApplicability(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fields  Fields
		wantErr string // substring; empty means the record must be accepted
	}{
		// operation
		{"operation is valid", validOperation(), ""},
		{"operation requires outcome", func() Fields { f := validOperation(); f.Outcome = ""; return f }(), "outcome is required"},
		{"operation requires tool", func() Fields { f := validOperation(); f.Tool = ""; return f }(), "tool is required"},
		{"operation requires broker", func() Fields { f := validOperation(); f.Broker = ""; return f }(), "broker is required"},
		{"operation requires arguments_hash", func() Fields { f := validOperation(); f.ArgumentsHash = ""; return f }(), "arguments_hash is required"},
		{"operation requires started_at", func() Fields { f := validOperation(); f.StartedAt = time.Time{}; return f }(), "started_at is required"},
		{"operation rejects reason", func() Fields { f := validOperation(); f.Reason = "not_permitted"; return f }(), "reason must not be set"},
		{
			"operation rejects error_type on success",
			func() Fields { f := validOperation(); f.ErrorType = "execution_error"; return f }(),
			"error_type must not be set",
		},
		{
			"operation requires error_type on error",
			func() Fields { f := validOperation(); f.Outcome = OutcomeError; return f }(),
			"error_type is required",
		},
		{
			"operation accepts error_type on error",
			func() Fields {
				f := validOperation()
				f.Outcome = OutcomeError
				f.ErrorType = "execution_error"
				return f
			}(),
			"",
		},
		{
			"operation accepts cancelled",
			func() Fields { f := validOperation(); f.Outcome = OutcomeCancelled; return f }(),
			"",
		},

		// auth_success
		{"auth_success is valid", Fields{Type: EventAuthSuccess}, ""},
		{"auth_success rejects outcome", Fields{Type: EventAuthSuccess, Outcome: OutcomeSuccess}, "outcome must not be set"},
		{"auth_success rejects tool", Fields{Type: EventAuthSuccess, Tool: "delete-queue"}, "tool must not be set"},
		{"auth_success rejects reason", Fields{Type: EventAuthSuccess, Reason: "expired"}, "reason must not be set"},

		// auth_failure
		{"auth_failure is valid", Fields{Type: EventAuthFailure, Reason: "invalid_token"}, ""},
		{"auth_failure requires reason", Fields{Type: EventAuthFailure}, "reason is required"},
		{"auth_failure rejects outcome", Fields{Type: EventAuthFailure, Reason: "expired", Outcome: OutcomeError}, "outcome must not be set"},
		{"auth_failure rejects broker", Fields{Type: EventAuthFailure, Reason: "expired", Broker: "prod-1"}, "broker must not be set"},

		// authz_denied — tool but no arguments_hash: the call was refused
		// before its arguments were dispatched.
		{"authz_denied is valid", Fields{Type: EventAuthzDenied, Reason: "missing_claim", Tool: "delete-queue"}, ""},
		{"authz_denied requires tool", Fields{Type: EventAuthzDenied, Reason: "missing_claim"}, "tool is required"},
		{
			"authz_denied rejects arguments_hash",
			Fields{Type: EventAuthzDenied, Reason: "missing_claim", Tool: "delete-queue", ArgumentsHash: testHash},
			"arguments_hash must not be set",
		},
		{
			"authz_denied rejects broker",
			Fields{Type: EventAuthzDenied, Reason: "missing_claim", Tool: "delete-queue", Broker: "prod-1"},
			"broker must not be set",
		},

		// broker_authz_denied — differs from authz_denied in exactly one
		// column, broker, which is required because a hop-2 denial names the
		// broker that refused.
		{
			"broker_authz_denied is valid",
			Fields{Type: EventBrokerAuthzDenied, Reason: "permission_denied", Tool: "delete-queue", Broker: "prod-1"},
			"",
		},
		{
			"broker_authz_denied requires broker",
			Fields{Type: EventBrokerAuthzDenied, Reason: "permission_denied", Tool: "delete-queue"},
			"broker is required",
		},
		{
			"broker_authz_denied requires reason",
			Fields{Type: EventBrokerAuthzDenied, Tool: "delete-queue", Broker: "prod-1"},
			"reason is required",
		},
		{
			"broker_authz_denied rejects arguments_hash",
			Fields{Type: EventBrokerAuthzDenied, Reason: "permission_denied", Tool: "delete-queue", Broker: "prod-1", ArgumentsHash: testHash},
			"arguments_hash must not be set",
		},

		// broker_auth_retry
		{"broker_auth_retry is valid", Fields{Type: EventBrokerAuthRetry, Outcome: OutcomeSuccess, Broker: "prod-1"}, ""},
		{"broker_auth_retry requires broker", Fields{Type: EventBrokerAuthRetry, Outcome: OutcomeSuccess}, "broker is required"},
		{"broker_auth_retry requires outcome", Fields{Type: EventBrokerAuthRetry, Broker: "prod-1"}, "outcome is required"},
		{
			// No caller waits on a broker auth retry, so it cannot be cancelled.
			"broker_auth_retry rejects cancelled",
			Fields{Type: EventBrokerAuthRetry, Outcome: OutcomeCancelled, Broker: "prod-1"},
			`outcome "cancelled" is not valid`,
		},
		{"broker_auth_retry rejects tool", Fields{Type: EventBrokerAuthRetry, Outcome: OutcomeSuccess, Broker: "prod-1", Tool: "delete-queue"}, "tool must not be set"},

		// audit_drop carries only the common fields.
		{"audit_drop is valid", Fields{Type: EventAuditDrop}, ""},
		{"audit_drop rejects tool", Fields{Type: EventAuditDrop, Tool: "delete-queue"}, "tool must not be set"},
		{"audit_drop rejects outcome", Fields{Type: EventAuditDrop, Outcome: OutcomeError}, "outcome must not be set"},
		{"audit_drop rejects broker", Fields{Type: EventAuditDrop, Broker: "prod-1"}, "broker must not be set"},

		// The discriminator itself.
		{"unknown type is rejected", Fields{Type: "operation_v2"}, "unknown audit_event_type"},
		{"empty type is rejected", Fields{}, "unknown audit_event_type"},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewEvent(ctx, tc.fields)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("NewEvent rejected a valid record: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("NewEvent accepted an invalid record; want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestNewEvent_outcomeVocabulary pins the three-value enum, and in particular
// that the two values people keep reaching for are not in it: a recovered
// panic is outcome=error with error_type=panic (there is no "panic" outcome),
// and an authorization denial is a record type, not an outcome (Q-015).
func TestNewEvent_outcomeVocabulary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		outcome Outcome
		valid   bool
	}{
		{OutcomeSuccess, true},
		{OutcomeError, true},
		{OutcomeCancelled, true},
		{"panic", false},
		{"denied", false},
		{"Success", false},
		{"ok", false},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			t.Parallel()
			f := validOperation()
			f.Outcome = tc.outcome
			if tc.outcome == OutcomeError {
				f.ErrorType = "execution_error"
			}
			_, err := NewEvent(context.Background(), f)
			if tc.valid && err != nil {
				t.Errorf("outcome %q rejected: %v", tc.outcome, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("outcome %q accepted; the vocabulary is closed", tc.outcome)
			}
		})
	}
}

// TestNewEvent_errorTypeVocabularyIsClosed checks every accepted value round
// trips, and that a plausible-looking value outside the set is refused. The
// loop is over ErrorTypes() rather than a literal list so a new value is
// covered the moment it is added.
func TestNewEvent_errorTypeVocabularyIsClosed(t *testing.T) {
	t.Parallel()
	for _, et := range ErrorTypes() {
		f := validOperation()
		f.Outcome = OutcomeError
		f.ErrorType = et
		if et == "panic" {
			f.PanicRecovered = true
		}
		e, err := NewEvent(context.Background(), f)
		if err != nil {
			t.Errorf("error_type %q rejected: %v", et, err)
			continue
		}
		if got := render(t, e)["error_type"]; got != et {
			t.Errorf("error_type = %v, want %q", got, et)
		}
	}
	for _, bad := range []string{"timeout", "Panic", "execution error", "broker_permission_denied"} {
		f := validOperation()
		f.Outcome = OutcomeError
		f.ErrorType = bad
		if _, err := NewEvent(context.Background(), f); err == nil {
			t.Errorf("error_type %q accepted; the vocabulary is closed", bad)
		}
	}
}

// TestNewEvent_reasonVocabulariesDoNotBleed is the assertion the three closed
// reason sets exist for. Authentication, hop-1 authorization, and hop-2
// authorization each have their own vocabulary, and a reason from one on a
// record of another corrupts the query a reviewer runs to answer "why was this
// refused". Every cross pairing is rejected.
func TestNewEvent_reasonVocabulariesDoNotBleed(t *testing.T) {
	t.Parallel()
	vocab := map[EventType][]string{
		EventAuthFailure:       {"invalid_token", "expired", "audience_mismatch", "signature_invalid", "missing"},
		EventAuthzDenied:       {"missing_claim", "not_permitted"},
		EventBrokerAuthzDenied: {"permission_denied"},
	}
	// Fill the non-reason fields each type also requires, so a rejection can
	// only be about reason.
	base := map[EventType]Fields{
		EventAuthFailure:       {Type: EventAuthFailure},
		EventAuthzDenied:       {Type: EventAuthzDenied, Tool: "delete-queue"},
		EventBrokerAuthzDenied: {Type: EventBrokerAuthzDenied, Tool: "delete-queue", Broker: "prod-1"},
	}

	for typ, own := range vocab {
		for otherType, others := range vocab {
			for _, reason := range others {
				wantValid := typ == otherType
				name := string(typ) + "/" + reason
				t.Run(name, func(t *testing.T) {
					f := base[typ]
					f.Reason = reason
					_, err := NewEvent(context.Background(), f)
					if wantValid && err != nil {
						t.Errorf("reason %q rejected on its own record type %q: %v", reason, typ, err)
					}
					if !wantValid && err == nil {
						t.Errorf("reason %q from the %q vocabulary accepted on a %q record", reason, otherType, typ)
					}
				})
			}
		}
		_ = own
	}
}

// TestNewEvent_identityComesFromContext pins that a call site cannot name a
// caller other than the authenticated one: identity is read from the context
// principal the auth middleware attached, and Fields carries no identity field
// at all.
//
// It also pins the v1 claim set. principal is a group carrying sub and nothing
// else; iss and jti are on the Principal and are deliberately not written, and
// preferred_username and email are not on it at all (D2 / Q-013).
func TestNewEvent_identityComesFromContext(t *testing.T) {
	t.Parallel()
	e, err := NewEvent(ctxWithPrincipal(t), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	rec := render(t, e)

	principal, ok := rec["principal"].(map[string]any)
	if !ok {
		t.Fatalf("principal is not a nested object: %#v", rec["principal"])
	}
	if principal["sub"] != testSub {
		t.Errorf("principal.sub = %v, want %q", principal["sub"], testSub)
	}
	if len(principal) != 1 {
		t.Errorf("principal carries %d members, want exactly 1 (sub): %#v", len(principal), principal)
	}
	if rec["agent_client_id"] != testClientID {
		t.Errorf("agent_client_id = %v, want %q", rec["agent_client_id"], testClientID)
	}
	for _, forbidden := range []string{"iss", "jti", "scope", "preferred_username", "email"} {
		if _, present := rec[forbidden]; present {
			t.Errorf("record carries claim %q; v1 writes sub and client_id only", forbidden)
		}
		if _, present := principal[forbidden]; present {
			t.Errorf("principal carries claim %q; v1 writes sub only", forbidden)
		}
	}
}

// TestNewEvent_absentPrincipalOmitsIdentity pins disabled-mode behaviour: with
// no principal on the context there is no identity to name, and the record
// omits the keys rather than emitting empty strings a SIEM would index as a
// real (empty) subject. A constructor that instead rejected the record would
// delete the audit trail on exactly the deployments that run without auth.
func TestNewEvent_absentPrincipalOmitsIdentity(t *testing.T) {
	t.Parallel()
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	rec := render(t, e)
	for _, key := range []string{"principal", "agent_client_id"} {
		if _, present := rec[key]; present {
			t.Errorf("record carries %q with no principal on the context: %#v", key, rec[key])
		}
	}
}

// TestNewEvent_auditDropCarriesNoIdentity pins that a drop notice carries only
// the common fields, even when the context has a principal to offer. The
// record says "a record could not be written"; there is no surviving subject
// for it to describe.
func TestNewEvent_auditDropCarriesNoIdentity(t *testing.T) {
	t.Parallel()
	e, err := NewEvent(ctxWithPrincipal(t), Fields{Type: EventAuditDrop})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	rec := render(t, e)
	for _, key := range []string{"principal", "agent_client_id", "tool", "broker", "outcome"} {
		if _, present := rec[key]; present {
			t.Errorf("audit_drop carries %q: %#v", key, rec[key])
		}
	}
}

// TestNewEvent_absentCorrelationIDIsOmitted pins that a missing correlation ID
// is an absent key, not an empty string, matching every other log site here.
func TestNewEvent_absentCorrelationIDIsOmitted(t *testing.T) {
	t.Parallel()
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if _, present := render(t, e)["correlation_id"]; present {
		t.Error("correlation_id emitted with no ID on the context")
	}
}

// TestNewEvent_timingFields pins the two names docs/observability.md freezes —
// started_at_utc for the instant, duration_ms for the elapsed time — and their
// renderings. The names are load-bearing: the doc's rule is "_utc for an
// instant, _ms for a duration", and they freeze at GA.
func TestNewEvent_timingFields(t *testing.T) {
	t.Parallel()
	f := validOperation()
	f.StartedAt = time.Date(2026, 9, 4, 15, 4, 5, 0, time.FixedZone("EDT", -4*3600))
	f.Duration = 1500 * time.Millisecond

	e, err := NewEvent(context.Background(), f)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	rec := render(t, e)
	if got, want := rec["started_at_utc"], "2026-09-04T19:04:05Z"; got != want {
		t.Errorf("started_at_utc = %v, want %q (RFC 3339, normalised to UTC)", got, want)
	}
	if got, want := rec["duration_ms"], float64(1500); got != want {
		t.Errorf("duration_ms = %v, want %v", got, want)
	}
	if _, present := rec["started_at"]; present {
		t.Error("record carries started_at; the frozen field name is started_at_utc")
	}
}

// TestNewEvent_panicRecoveredMustAgreeWithTheCause pins that panic_recovered
// cannot contradict the fields it annotates. A record claiming a recovered
// panic on a successful call would misreport a clean operation as a crash.
func TestNewEvent_panicRecoveredMustAgreeWithTheCause(t *testing.T) {
	t.Parallel()
	valid := validOperation()
	valid.Outcome = OutcomeError
	valid.ErrorType = "panic"
	valid.PanicRecovered = true
	e, err := NewEvent(context.Background(), valid)
	if err != nil {
		t.Fatalf("NewEvent rejected a valid recovered-panic record: %v", err)
	}
	if got := render(t, e)["panic_recovered"]; got != true {
		t.Errorf("panic_recovered = %v, want true", got)
	}

	for _, name := range []string{"on success", "on a different error_type"} {
		f := validOperation()
		f.PanicRecovered = true
		if name == "on a different error_type" {
			f.Outcome = OutcomeError
			f.ErrorType = "execution_error"
		}
		if _, err := NewEvent(context.Background(), f); err == nil {
			t.Errorf("panic_recovered accepted %s", name)
		}
	}
}

// TestNewEvent_rawArgumentsNeverReachTheRecord is the requirement both tickets
// state explicitly: only the hash, never the values.
//
// It renders a record built from arguments carrying distinctive values and
// asserts none of them appear anywhere in the emitted bytes. Fields has no
// field that could carry them, which is the structural guarantee; this asserts
// the outcome, so a future field that reintroduced them fails here.
func TestNewEvent_rawArgumentsNeverReachTheRecord(t *testing.T) {
	t.Parallel()
	args := map[string]any{
		"queueName":   "SECRET-QUEUE-NAME",
		"selector":    "customerId='SENSITIVE-VALUE'",
		"maxMessages": 500,
	}
	hash, err := HashArgs(args)
	if err != nil {
		t.Fatalf("HashArgs: %v", err)
	}
	f := validOperation()
	f.ArgumentsHash = hash

	e, err := NewEvent(ctxWithPrincipal(t), f)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	rec := slog.NewRecord(time.Unix(0, 0).UTC(), e.Level(), Message, 0)
	rec.AddAttrs(e.Attrs()...)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("handling record: %v", err)
	}

	rendered := buf.String()
	for _, secret := range []string{"SECRET-QUEUE-NAME", "SENSITIVE-VALUE", "queueName", "selector", "maxMessages"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("raw argument material %q reached the audit record: %s", secret, rendered)
		}
	}
	if !strings.Contains(rendered, hash) {
		t.Errorf("arguments_hash %q missing from the record: %s", hash, rendered)
	}
}

// TestEvent_AttrsReturnsACopy guards the Event from a caller that appends to
// the slice it hands out. slog.Record.Add appends, so an emitter reusing the
// returned slice would otherwise write into the Event's own backing array and
// corrupt a second emission of the same record.
func TestEvent_AttrsReturnsACopy(t *testing.T) {
	t.Parallel()
	e, err := NewEvent(context.Background(), validOperation())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	first := e.Attrs()
	n := len(first)
	first[0] = slog.String("event", "not-audit")
	first = append(first, slog.String("injected", "x")) //nolint:staticcheck // exercising aliasing

	second := e.Attrs()
	if len(second) != n {
		t.Errorf("Attrs() length changed to %d after a caller appended, want %d", len(second), n)
	}
	if second[0].Key != "event" || second[0].Value.String() != EventValue {
		t.Errorf("Attrs() exposed the Event's own storage: first attr is now %v", second[0])
	}
	_ = first
}

// TestNewEvent_rejectionYieldsNoRecord pins that a rejected record is not
// partially built. A caller must not be able to emit the zero Event and write
// a record with no event tag and no type.
func TestNewEvent_rejectionYieldsNoRecord(t *testing.T) {
	t.Parallel()
	e, err := NewEvent(context.Background(), Fields{Type: EventOperation})
	if err == nil {
		t.Fatal("NewEvent accepted an incomplete operation record")
	}
	if len(e.Attrs()) != 0 || e.Type() != "" {
		t.Errorf("rejected record still carried content: type=%q attrs=%v", e.Type(), e.Attrs())
	}
}

// TestNewEvent_levels pins where each record type lands. Denials and drops are
// WARN because they want attention; an operation record is INFO regardless of
// outcome, since outcome carries the meaning.
func TestNewEvent_levels(t *testing.T) {
	t.Parallel()
	want := map[EventType]slog.Level{
		EventOperation:         slog.LevelInfo,
		EventAuthSuccess:       slog.LevelInfo,
		EventAuthFailure:       slog.LevelWarn,
		EventAuthzDenied:       slog.LevelWarn,
		EventBrokerAuthzDenied: slog.LevelWarn,
		EventBrokerAuthRetry:   slog.LevelInfo,
		EventAuditDrop:         slog.LevelWarn,
	}
	for typ, level := range want {
		if got := levelByType[typ]; got != level {
			t.Errorf("%s emits at %v, want %v", typ, got, level)
		}
	}
}
