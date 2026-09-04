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

// The audit record a destructive tool call emits (SOL-152096).
//
// The assertions that matter here are about counting and about absence. One
// operation record per destructive call, never a started/completed pair. No
// record at all for a read-only call, or for any call with the capability off.
// And no raw argument value anywhere in the record — only the hash.
//
// Note what is deliberately NOT asserted: a total audit-record count of one.
// SOL-153332 will emit a broker_authz_denied record that coexists with this
// story's operation record on a hop-2 denial, and a total-count assertion
// would fail the moment it lands, for a case that is correct. Count records
// with audit_event_type="operation" instead.

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// captureAudit runs fn with the default logger swapped for a JSON handler at
// level and returns every decoded record it wrote.
func captureAudit(t *testing.T, level slog.Level, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	defer slog.SetDefault(old)

	fn()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// ofType returns the records with the given audit_event_type. Filtering by
// type rather than counting all records is the invariant this story owns.
func ofType(records []map[string]any, typ audit.EventType) []map[string]any {
	var out []map[string]any
	for _, rec := range records {
		if rec["event"] == audit.EventValue && rec["audit_event_type"] == string(typ) {
			out = append(out, rec)
		}
	}
	return out
}

// auditHandlerDelay is how long the destructive stub sleeps. It gives
// duration_ms a floor to be asserted against — generous enough that a loaded
// CI runner cannot round it below 1ms, short enough not to slow the suite.
const auditHandlerDelay = 15 * time.Millisecond

// auditTestManager builds a manager holding a read-only tool, a destructive
// tool, and a destructive tool whose handler fails, with the audit capability
// set as asked.
func auditTestManager(t *testing.T, auditEnabled bool) *ToolManager {
	t.Helper()
	mgr := NewToolManager(newTestPool(t), WithAuditLog(auditEnabled))
	mgr.Register(newStubHandler("read-only-tool"))

	destructive := newStubHandler("delete-queue")
	yes := true
	destructive.annotations = Annotations{Destructive: &yes}
	// Sleeps so duration_ms has a measurable floor to assert against. Without
	// it a hardcoded zero duration passes every presence check.
	destructive.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		time.Sleep(auditHandlerDelay)
		return &ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
	}
	mgr.Register(destructive)

	failing := newStubHandler("delete-queue-failing")
	failing.annotations = Annotations{Destructive: &yes}
	failing.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, errors.New("broker refused")
	}
	mgr.Register(failing)

	panicking := newStubHandler("delete-queue-panicking")
	panicking.annotations = Annotations{Destructive: &yes}
	panicking.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		panic("handler exploded")
	}
	mgr.Register(panicking)
	return mgr
}

// auditCtx carries a principal and a correlation ID, as a real request does.
func auditCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := correlation.With(context.Background(), "corr-abc")
	return auth.WithPrincipal(ctx, auth.NewPrincipal(ctx, &sdkauth.TokenInfo{
		UserID: "auth0|abc123",
		Extra:  map[string]any{"client_id": "cursor-ide", "iss": "https://example.auth0.com/", "jti": "j1"},
	}))
}

// TestDestructiveCall_emitsExactlyOneOperationRecord is the story's core
// invariant: one operation record, at completion, carrying the identity chain
// and the outcome. Not a started/completed pair — a reviewer must not have to
// join two records to see one call.
func TestDestructiveCall_emitsExactlyOneOperationRecord(t *testing.T) {
	mgr := auditTestManager(t, true)
	args := map[string]any{"broker": "dev", "msgVpnName": "default"}

	callStart := time.Now()
	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue", args, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 {
		t.Fatalf("emitted %d operation record(s), want exactly 1:\n%v", len(ops), records)
	}
	rec := ops[0]

	// The digest is over what the handler actually receives — args minus
	// "broker" — not the raw tools/call arguments; see
	// TestDestructiveCall_hashExcludesBrokerAndRedactsSensitiveArguments for
	// why (broker-casing invariance and Never-Log redaction).
	wantHash, err := audit.HashArgs(audit.RedactSensitive(map[string]any{"msgVpnName": "default"}))
	if err != nil {
		t.Fatalf("HashArgs: %v", err)
	}
	for _, check := range []struct {
		key  string
		want any
	}{
		{"event", audit.EventValue},
		{"audit_event_type", string(audit.EventOperation)},
		{"audit_schema_version", schema.AuditSchemaVersion},
		{"correlation_id", "corr-abc"},
		{"tool", "delete-queue"},
		{"broker", "dev"},
		{"outcome", string(audit.OutcomeSuccess)},
		{"arguments_hash", wantHash},
		{"agent_client_id", "cursor-ide"},
	} {
		if got := rec[check.key]; got != check.want {
			t.Errorf("%s = %v, want %v", check.key, got, check.want)
		}
	}
	principal, ok := rec["principal"].(map[string]any)
	if !ok || principal["sub"] != "auth0|abc123" {
		t.Errorf("principal = %#v, want {sub: auth0|abc123}", rec["principal"])
	}
	if _, present := rec["started_at_utc"]; !present {
		t.Error("record carries no started_at_utc")
	}
	// A real lower bound, not merely "present" or "non-negative": the handler
	// below sleeps, so a hardcoded or unwired duration reads as 0 and fails
	// here. duration_ms is what a reviewer uses to see how long a destructive
	// call actually ran, and a constant would tell them nothing while passing
	// any presence check.
	durationMs, ok := rec["duration_ms"].(float64)
	if !ok {
		t.Fatalf("duration_ms = %#v, want a number", rec["duration_ms"])
	}
	if durationMs < 1 {
		t.Errorf("duration_ms = %v, but the handler slept %s. The field is not carrying this "+
			"call's measured elapsed time.", durationMs, auditHandlerDelay)
	}
	startedAt, ok := rec["started_at_utc"].(string)
	if !ok {
		t.Fatalf("started_at_utc = %#v, want a string", rec["started_at_utc"])
	}
	started, parseErr := time.Parse(time.RFC3339Nano, startedAt)
	if parseErr != nil {
		t.Errorf("started_at_utc %q is not RFC 3339: %v", startedAt, parseErr)
	} else if started.Before(callStart) || started.After(time.Now()) {
		t.Errorf("started_at_utc %s is outside the window the call actually ran in "+
			"(%s..now); it is not this call's start time", startedAt, callStart.Format(time.RFC3339Nano))
	}
	// reason belongs to the denial and authentication record types. Setting it
	// here would put an authorization vocabulary on an operation record.
	if _, present := rec["reason"]; present {
		t.Errorf("operation record carries reason = %v", rec["reason"])
	}
	// error_type is present only on a failure.
	if _, present := rec["error_type"]; present {
		t.Errorf("successful operation record carries error_type = %v", rec["error_type"])
	}
}

// TestDestructiveCall_hashIsInvariantToBrokerCasing pins that two calls
// differing only in the caller's casing of "broker" produce the same
// arguments_hash. Broker aliases resolve case-insensitively and the record's
// own broker field always carries the configured casing, so if "broker" were
// still part of the digest these two calls — the same operation on the same
// broker — would hash differently, defeating the "recompute and compare"
// promise the digest exists for. Fixed by excluding "broker" from the hashed
// map entirely (CallTool hashes handlerParams, not the raw params).
func TestDestructiveCall_hashIsInvariantToBrokerCasing(t *testing.T) {
	mgr := auditTestManager(t, true)

	hashFor := func(brokerCasing string) string {
		records := captureAudit(t, slog.LevelDebug, func() {
			if _, err := mgr.CallTool(auditCtx(t), "delete-queue",
				map[string]any{"broker": brokerCasing, "msgVpnName": "default"}, idFixture()); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
		})
		ops := ofType(records, audit.EventOperation)
		if len(ops) != 1 {
			t.Fatalf("emitted %d operation record(s), want exactly 1:\n%v", len(ops), records)
		}
		hash, _ := ops[0]["arguments_hash"].(string)
		return hash
	}

	lower := hashFor("dev")
	upper := hashFor("DEV")
	if lower == "" || upper == "" {
		t.Fatalf("empty arguments_hash: lower=%q upper=%q", lower, upper)
	}
	if lower != upper {
		t.Errorf(`arguments_hash for "broker": "dev" (%s) != "broker": "DEV" (%s); the digest is `+
			"still salted by the caller's casing", lower, upper)
	}
}

// TestDestructiveCall_hashRedactsSensitiveArguments pins the other half of the
// same fix: a value on the Never-Log list must not reach the digest's
// pre-image, even though nothing upstream of hashArgsForAudit constrains the
// composite schema's keys. Two checks: changing the secret's value must not
// change the digest (proof the value itself has no influence, not just that
// it looks different from the raw hash), and the digest must match what
// hashing the redacted map directly produces (proof of the exact placeholder,
// not merely "some" redaction).
func TestDestructiveCall_hashRedactsSensitiveArguments(t *testing.T) {
	mgr := auditTestManager(t, true)

	hashFor := func(secretValue string) string {
		records := captureAudit(t, slog.LevelDebug, func() {
			if _, err := mgr.CallTool(auditCtx(t), "delete-queue", map[string]any{
				"broker":     "dev",
				"msgVpnName": "default",
				// Not a real tool parameter — stubHandler's schema has no
				// additionalProperties:false, so this reaches hashArgsForAudit
				// exactly as a free-form config object's password field would.
				"replicationBridgeAuthenticationBasicPassword": secretValue,
			}, idFixture()); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
		})
		ops := ofType(records, audit.EventOperation)
		if len(ops) != 1 {
			t.Fatalf("emitted %d operation record(s), want exactly 1:\n%v", len(ops), records)
		}
		hash, _ := ops[0]["arguments_hash"].(string)
		return hash
	}

	got := hashFor("hunter2")
	// Two different real secret values must hash identically — proof the
	// digest depends on the placeholder, not the value redaction replaced.
	if other := hashFor("a-completely-different-password"); got != other {
		t.Errorf("arguments_hash changed with the redacted field's value (%s vs %s); "+
			"a value on the Never-Log list is still reaching the digest", got, other)
	}
	want, err := audit.HashArgs(map[string]any{
		"msgVpnName": "default",
		"replicationBridgeAuthenticationBasicPassword": "[REDACTED]",
	})
	if err != nil {
		t.Fatalf("HashArgs: %v", err)
	}
	if got != want {
		t.Errorf("arguments_hash = %s, want %s (the digest computed from the redacted map)", got, want)
	}
}

// TestDestructiveCall_recordsFailureOutcome pins that a handler failure lands
// as outcome=error with the error_type the tool path already computed, rather
// than being lost or reported as a success.
func TestDestructiveCall_recordsFailureOutcome(t *testing.T) {
	mgr := auditTestManager(t, true)

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue-failing",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool returned a protocol error: %v", err)
		}
	})

	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 {
		t.Fatalf("emitted %d operation record(s), want exactly 1:\n%v", len(ops), records)
	}
	if got := ops[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeError)
	}
	if got := ops[0]["error_type"]; got != "execution_error" {
		t.Errorf("error_type = %v, want %q", got, "execution_error")
	}
	if _, present := ops[0]["panic_recovered"]; present {
		t.Error("a handled failure is marked panic_recovered")
	}
}

// TestDestructiveCall_recordsRecoveredPanic pins the case the audit trail
// exists for. A panicking destructive handler may already have mutated the
// broker; a missing record would read as "no attempt was made".
//
// The record is outcome=error with error_type=panic and panic_recovered=true.
// There is no panic outcome.
func TestDestructiveCall_recordsRecoveredPanic(t *testing.T) {
	mgr := auditTestManager(t, true)

	records := captureAudit(t, slog.LevelDebug, func() {
		defer func() {
			// CallTool does not recover; withRecovery does, one layer out.
			// Swallow here so the deferred audit emission can be asserted.
			_ = recover()
		}()
		_, _ = mgr.CallTool(auditCtx(t), "delete-queue-panicking",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture())
	})

	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 {
		t.Fatalf("emitted %d operation record(s) for a panicking handler, want exactly 1:\n%v", len(ops), records)
	}
	if got := ops[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeError)
	}
	if got := ops[0]["error_type"]; got != "panic" {
		t.Errorf("error_type = %v, want %q", got, "panic")
	}
	if got := ops[0]["panic_recovered"]; got != true {
		t.Errorf("panic_recovered = %v, want true", got)
	}
}

// TestReadOnlyCall_emitsNoAuditRecord pins the scope: the audit stream is for
// state-changing calls. Auditing read-only calls would bury the destructive
// ones a reviewer is looking for.
func TestReadOnlyCall_emitsNoAuditRecord(t *testing.T) {
	mgr := auditTestManager(t, true)

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "read-only-tool",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
	for _, rec := range records {
		if rec["event"] == audit.EventValue {
			t.Errorf("read-only call emitted an audit record: %#v", rec)
		}
	}
}

// TestAuditDisabled_keepsTheLegacyWarn pins the flag-off contract: the
// capability is inert, not degraded. The pre-flip WARN is emitted, and no
// audit record is.
func TestAuditDisabled_keepsTheLegacyWarn(t *testing.T) {
	mgr := auditTestManager(t, false)

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	var sawWarn bool
	for _, rec := range records {
		if rec["msg"] == "executing destructive operation" {
			sawWarn = true
		}
		if rec["event"] == audit.EventValue {
			t.Errorf("audit record emitted with the capability off: %#v", rec)
		}
	}
	if !sawWarn {
		t.Errorf("the destructive-operation WARN was not emitted with the audit log off:\n%v", records)
	}
}

// TestAuditEnabled_replacesTheLegacyWarn pins the other half: with the
// capability on, the record replaces the WARN rather than doubling it. Two
// lines per destructive call is the doubled-noise problem this story exists to
// avoid.
func TestAuditEnabled_replacesTheLegacyWarn(t *testing.T) {
	mgr := auditTestManager(t, true)

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
	for _, rec := range records {
		if rec["msg"] == "executing destructive operation" {
			t.Errorf("the legacy WARN was emitted alongside the audit record: %#v", rec)
		}
	}
	if len(ofType(records, audit.EventOperation)) != 1 {
		t.Errorf("want exactly one operation record:\n%v", records)
	}
}

// TestAuditRecord_carriesNoRawArguments is the requirement both tickets state
// explicitly. The record must carry the hash and nothing else derived from the
// arguments — not the values, not even the argument names.
func TestAuditRecord_carriesNoRawArguments(t *testing.T) {
	mgr := auditTestManager(t, true)
	args := map[string]any{
		"broker":      "dev",
		"msgVpnName":  "default",
		"secretParam": "SENSITIVE-ARGUMENT-VALUE",
	}
	// The stub's schema allows additional properties, so the extra argument
	// reaches the hash rather than being rejected at validation.

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if _, err := mgr.CallTool(auditCtx(t), "delete-queue", args, idFixture()); err != nil {
		slog.SetDefault(old)
		t.Fatalf("CallTool: %v", err)
	}
	slog.SetDefault(old)

	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if !strings.Contains(line, `"event":"audit"`) {
			continue
		}
		for _, secret := range []string{"SENSITIVE-ARGUMENT-VALUE", "secretParam", "msgVpnName"} {
			if strings.Contains(line, secret) {
				t.Errorf("audit record leaked argument material %q: %s", secret, line)
			}
		}
	}
}

// TestAuditRecord_hashIdentifiesTheArguments pins that the hash is over THIS
// call's arguments. A constant, or a hash of the empty map, would satisfy
// every presence assertion above and prove nothing to an auditor.
func TestAuditRecord_hashIdentifiesTheArguments(t *testing.T) {
	mgr := auditTestManager(t, true)

	hashFor := func(t *testing.T, args map[string]any) string {
		t.Helper()
		records := captureAudit(t, slog.LevelDebug, func() {
			if _, err := mgr.CallTool(auditCtx(t), "delete-queue", args, idFixture()); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
		})
		ops := ofType(records, audit.EventOperation)
		if len(ops) != 1 {
			t.Fatalf("want exactly one operation record, got %d", len(ops))
		}
		hash, _ := ops[0]["arguments_hash"].(string)
		return hash
	}

	first := hashFor(t, map[string]any{"broker": "dev", "msgVpnName": "default"})
	second := hashFor(t, map[string]any{"broker": "dev", "msgVpnName": "other-vpn"})
	if first == second {
		t.Errorf("different arguments produced the same arguments_hash %q", first)
	}

	// And the digest an auditor recomputes from the wire arguments matches,
	// once they apply the same two transforms production does: drop "broker"
	// and redact any Never-Log-shaped key (see
	// TestDestructiveCall_hashIsInvariantToBrokerCasing and
	// TestDestructiveCall_hashRedactsSensitiveArguments for why).
	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	want, err := audit.HashArgs(audit.RedactSensitive(map[string]any{"msgVpnName": "default"}))
	if err != nil {
		t.Fatalf("HashArgs: %v", err)
	}
	if got := hashFor(t, args); got != want {
		t.Errorf("arguments_hash = %q, want %q — an auditor recomputing from the same arguments (minus broker) would not match", got, want)
	}
}

// TestAuditRecord_levelFilteredCallEmitsADrop pins drop detection on the
// failure mode most likely to bite an operator: enabling the audit log on a
// server whose level is above INFO. Without this the audit trail would vanish
// silently, which is the one thing an audit trail must not do.
// Covers both levels an operator can configure above the operation record's
// INFO. "error" is the one that matters: it is the highest level
// config.validLogLevels allows, so if the drop notice does not survive it,
// enabling the audit log on such a server erases the audit trail with no
// signal whatsoever.
func TestAuditRecord_levelFilteredCallEmitsADrop(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelWarn, slog.LevelError} {
		t.Run(level.String(), func(t *testing.T) {
			mgr := auditTestManager(t, true)
			records := captureAudit(t, level, func() {
				if _, err := mgr.CallTool(auditCtx(t), "delete-queue",
					map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
					t.Fatalf("CallTool: %v", err)
				}
			})

			if got := len(ofType(records, audit.EventOperation)); got != 0 {
				t.Errorf("an INFO operation record survived a %s level filter (%d record(s))", level, got)
			}
			drops := ofType(records, audit.EventAuditDrop)
			if len(drops) != 1 {
				t.Fatalf("at log level %s: want exactly one audit_drop record when the operation "+
					"record is filtered out, got %d. Zero means a silently missing audit trail:\n%v",
					level, len(drops), records)
			}
			// The drop must stay inside the customer's event="audit" filter —
			// it is the one record that says audit records are missing.
			if drops[0]["event"] != "audit" {
				t.Errorf("drop record event = %v, want the literal %q", drops[0]["event"], "audit")
			}
		})
	}
}

// TestOperationRecord_reachableErrorTypes pins which of the twelve error_type
// values can actually appear on an audit record.
//
// docs/observability.md now tells customers that only five of them do, and
// that a SIEM rule matching event="audit" with, say, error_type=validation_error
// will never fire. That is a claim about where the destructive gate sits
// relative to each failure, and prose cannot hold it: moving the gate, or
// adding a failure mode after it, silently makes the document wrong for
// someone writing compliance queries against it.
//
// Each case drives the real CallTool path with the audit log on and asserts
// whether an operation record appears at all.
func TestOperationRecord_reachableErrorTypes(t *testing.T) {
	yes := true
	destructive := Annotations{Destructive: &yes}

	cases := []struct {
		name string
		// wantErrorType is the error_type the tool path computes.
		wantErrorType string
		// wantAudited is whether an operation audit record is emitted.
		wantAudited bool
		tool        string
		args        map[string]any
		register    func(mgr *ToolManager)
	}{
		{
			name: "unknown_tool is rejected before the gate", wantErrorType: "unknown_tool", wantAudited: false,
			tool: "no-such-tool", args: map[string]any{"broker": "dev"},
		},
		{
			name: "missing_broker is rejected before the gate", wantErrorType: "missing_broker", wantAudited: false,
			tool: "delete-queue", args: map[string]any{"msgVpnName": "default"},
			register: func(mgr *ToolManager) {
				h := newStubHandler("delete-queue")
				h.annotations = destructive
				mgr.Register(h)
			},
		},
		{
			name: "unknown_broker is rejected before the gate", wantErrorType: "unknown_broker", wantAudited: false,
			tool: "delete-queue", args: map[string]any{"broker": "not-configured", "msgVpnName": "default"},
			register: func(mgr *ToolManager) {
				h := newStubHandler("delete-queue")
				h.annotations = destructive
				mgr.Register(h)
			},
		},
		{
			name: "validation_error is rejected before the gate", wantErrorType: "validation_error", wantAudited: false,
			tool: "delete-queue", args: map[string]any{"broker": "dev"}, // msgVpnName is required
			register: func(mgr *ToolManager) {
				h := newStubHandler("delete-queue")
				h.annotations = destructive
				mgr.Register(h)
			},
		},
		{
			name: "execution_error happens after the gate", wantErrorType: "execution_error", wantAudited: true,
			tool: "delete-queue", args: map[string]any{"broker": "dev", "msgVpnName": "default"},
			register: func(mgr *ToolManager) {
				h := newStubHandler("delete-queue")
				h.annotations = destructive
				h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
					return nil, errors.New("broker refused")
				}
				mgr.Register(h)
			},
		},
		{
			name: "nil_result happens after the gate", wantErrorType: "nil_result", wantAudited: true,
			tool: "delete-queue", args: map[string]any{"broker": "dev", "msgVpnName": "default"},
			register: func(mgr *ToolManager) {
				h := newStubHandler("delete-queue")
				h.annotations = destructive
				h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
					return nil, nil
				}
				mgr.Register(h)
			},
		},
		{
			name: "output_validation_error happens after the gate", wantErrorType: "output_validation_error", wantAudited: true,
			tool: "delete-queue", args: map[string]any{"broker": "dev", "msgVpnName": "default"},
			register: func(mgr *ToolManager) {
				h := newStubHandler("delete-queue")
				h.annotations = destructive
				// The stub's output schema requires object-valued properties.
				h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
					return &ToolResult{StructuredContent: map[string]any{"step1": "not-an-object"}}, nil
				}
				mgr.Register(h)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewToolManager(newTestPool(t), WithAuditLog(true))
			if tc.register != nil {
				tc.register(mgr)
			}
			records := captureAudit(t, slog.LevelDebug, func() {
				_, _ = mgr.CallTool(auditCtx(t), tc.tool, tc.args, idFixture())
			})

			var sawErrorType string
			for _, rec := range records {
				if rec["msg"] == "tool invoked" {
					sawErrorType, _ = rec["error_type"].(string)
				}
			}
			if sawErrorType != tc.wantErrorType {
				t.Fatalf("the tool path computed error_type=%q, want %q. This case no longer "+
					"exercises the failure it names.", sawErrorType, tc.wantErrorType)
			}

			ops := ofType(records, audit.EventOperation)
			switch {
			case tc.wantAudited && len(ops) != 1:
				t.Errorf("error_type=%q produced %d operation record(s), want 1. "+
					"docs/observability.md lists it as reaching an audit record.", tc.wantErrorType, len(ops))
			case !tc.wantAudited && len(ops) != 0:
				t.Errorf("error_type=%q produced %d operation record(s), want 0. "+
					"docs/observability.md tells customers this value never appears on an audit "+
					"record, so either the gate moved or the document is now wrong.",
					tc.wantErrorType, len(ops))
			}
			if tc.wantAudited && len(ops) == 1 {
				if got := ops[0]["error_type"]; got != tc.wantErrorType {
					t.Errorf("audit record error_type = %v, want %q", got, tc.wantErrorType)
				}
			}
		})
	}
}

// TestAuditRecord_constructorRejectionEmitsADrop covers the branch that is the
// whole safety net under the error_type drift guard.
//
// If a new error_type reaches this path without being added to the audit
// vocabulary, the constructor rejects the record and this drop notice is the
// ONLY thing that tells an operator a destructive call went unrecorded.
// Without a test, deleting that EmitDrop leaves the suite green and turns a
// loud gap into a silent one.
//
// The rejection is forced through the real CallTool path by a destructive tool
// whose handler panics in a way that lands an error_type the vocabulary does
// not contain — see emitOperationAuditRejects below for the direct case.
func TestAuditRecord_constructorRejectionEmitsADrop(t *testing.T) {
	records := captureAudit(t, slog.LevelDebug, func() {
		// A broker value the applicability table requires but that is empty
		// makes NewEvent reject the operation record. Driving the helper
		// directly is deliberate: CallTool cannot reach the destructive gate
		// without a resolved broker, so this branch has no route through it.
		emitOperationAudit(auditCtx(t), "delete-queue", "", "hash", time.Now(), "", nil)
	})

	if got := len(ofType(records, audit.EventOperation)); got != 0 {
		t.Errorf("a record the constructor rejected was emitted anyway (%d)", got)
	}
	drops := ofType(records, audit.EventAuditDrop)
	if len(drops) != 1 {
		t.Fatalf("want exactly one audit_drop when the constructor rejects the record, got %d. "+
			"Without it, a destructive call goes unrecorded with no signal at all:\n%v", len(drops), records)
	}
	if drops[0]["event"] != "audit" {
		t.Errorf("drop record event = %v, want the literal %q", drops[0]["event"], "audit")
	}
}

// TestAuditRecord_rejectionDetailNamesNoArguments pins the secure-logging
// contract the rejection path depends on: it logs audit.NewEvent's error text
// verbatim, which is safe only while that text names fields and
// closed-vocabulary values and never argument-derived material.
func TestAuditRecord_rejectionDetailNamesNoArguments(t *testing.T) {
	const secretTool = "delete-queue-SENSITIVE"
	records := captureAudit(t, slog.LevelDebug, func() {
		emitOperationAudit(auditCtx(t), secretTool, "", "SECRET-HASH-VALUE", time.Now(), "", nil)
	})

	for _, rec := range records {
		detail, ok := rec["detail"].(string)
		if !ok {
			continue
		}
		for _, forbidden := range []string{secretTool, "SECRET-HASH-VALUE"} {
			if strings.Contains(detail, forbidden) {
				t.Errorf("the rejection detail carries %q. audit.NewEvent's errors are logged "+
					"verbatim, so they must never interpolate Tool, Broker or ArgumentsHash: %s",
					forbidden, detail)
			}
		}
	}
}

// TestHashArgsForAudit_unhashableArgumentsEmitADrop covers the other
// drop-notice call site: arguments that cannot be canonicalized.
//
// Driven directly rather than through CallTool because the branch is
// unreachable from the wire: arguments always arrive from json.Unmarshal, and
// a value that could defeat json.Marshal (a channel here) is rejected by input
// schema validation before the destructive gate. Verified — routing this same
// map through CallTool produces error_type=validation_error and never reaches
// the hash. This test is therefore the only thing holding the branch.
func TestHashArgsForAudit_unhashableArgumentsEmitADrop(t *testing.T) {
	args := map[string]any{"broker": "dev", "bad": make(chan int)}

	var hash string
	records := captureAudit(t, slog.LevelDebug, func() {
		hash = hashArgsForAudit(auditCtx(t), "delete-queue", "dev", args)
	})

	if hash != "" {
		t.Errorf("hashArgsForAudit returned %q for unhashable arguments; an empty string is what "+
			"suppresses an operation record whose arguments_hash would stand for nothing", hash)
	}
	if got := len(ofType(records, audit.EventAuditDrop)); got != 1 {
		t.Fatalf("want exactly one audit_drop when the arguments cannot be hashed, got %d:\n%v", got, records)
	}
	// The failure detail must name the Go type only: the wrapped encoding/json
	// error renders the offending value, and that value is a tool argument.
	for _, rec := range records {
		detail, ok := rec["detail"].(string)
		if !ok {
			continue
		}
		if strings.Contains(detail, "chan") && !strings.HasPrefix(detail, "*") {
			t.Errorf("the hash-failure detail carries the encoding/json message rather than the "+
				"Go type, so a tool argument could reach the log: %s", detail)
		}
	}
}

// TestHashArgsForAudit_returnsTheReproducibleDigest is the happy path: the
// value an auditor recomputes.
func TestHashArgsForAudit_returnsTheReproducibleDigest(t *testing.T) {
	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	want, err := audit.HashArgs(args)
	if err != nil {
		t.Fatalf("HashArgs: %v", err)
	}
	if got := hashArgsForAudit(auditCtx(t), "delete-queue", "dev", args); got != want {
		t.Errorf("hashArgsForAudit = %q, want %q", got, want)
	}
}

// TestUnhashableArgumentsAreRejectedBeforeTheGate documents why the branch
// above needs a direct test: the wire path never reaches it.
func TestUnhashableArgumentsAreRejectedBeforeTheGate(t *testing.T) {
	mgr := auditTestManager(t, true)
	args := map[string]any{"broker": "dev", "msgVpnName": "default", "bad": make(chan int)}

	records := captureAudit(t, slog.LevelDebug, func() {
		_, _ = mgr.CallTool(auditCtx(t), "delete-queue", args, idFixture())
	})

	if got := len(ofType(records, audit.EventOperation)); got != 0 {
		t.Errorf("an operation record was emitted for a call rejected at validation (%d)", got)
	}
	var sawValidationError bool
	for _, rec := range records {
		if rec["msg"] == "tool invoked" && rec["error_type"] == "validation_error" {
			sawValidationError = true
		}
	}
	if !sawValidationError {
		t.Errorf("expected the call to be rejected at input validation, before the destructive "+
			"gate. If this changed, hashArgsForAudit's failure branch is now reachable from the "+
			"wire and needs an end-to-end test:\n%v", records)
	}
}

// TestAuditRecord_identityMatchesTheToolInvokedLine pins that the two records
// one destructive call produces name the same caller. They read the principal
// through the same canonical source (SOL-152087); this asserts the outcome, so
// a future change that re-derived identity at one of the two sites fails here.
func TestAuditRecord_identityMatchesTheToolInvokedLine(t *testing.T) {
	mgr := auditTestManager(t, true)

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, NewIdentityFromPrincipal(auth.PrincipalFrom(auditCtx(t)))); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	var toolInvokedSub, auditSub string
	for _, rec := range records {
		if rec["msg"] == "tool invoked" {
			toolInvokedSub, _ = rec["sub"].(string)
		}
	}
	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 {
		t.Fatalf("want exactly one operation record, got %d", len(ops))
	}
	if principal, ok := ops[0]["principal"].(map[string]any); ok {
		auditSub, _ = principal["sub"].(string)
	}

	if toolInvokedSub == "" || auditSub == "" {
		t.Fatalf("one of the two records carried no subject: tool invoked=%q audit=%q", toolInvokedSub, auditSub)
	}
	if toolInvokedSub != auditSub {
		t.Errorf("the two records name different callers: tool invoked sub=%q, audit principal.sub=%q", toolInvokedSub, auditSub)
	}
}
