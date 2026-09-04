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

	wantHash, err := audit.HashArgs(args)
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
	if _, present := rec["duration_ms"]; !present {
		t.Error("record carries no duration_ms")
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

	// And the digest an auditor recomputes from the wire arguments matches.
	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	want, err := audit.HashArgs(args)
	if err != nil {
		t.Fatalf("HashArgs: %v", err)
	}
	if got := hashFor(t, args); got != want {
		t.Errorf("arguments_hash = %q, want %q — an auditor recomputing from the same arguments would not match", got, want)
	}
}

// TestAuditRecord_levelFilteredCallEmitsADrop pins drop detection on the
// failure mode most likely to bite an operator: enabling the audit log on a
// server whose level is above INFO. Without this the audit trail would vanish
// silently, which is the one thing an audit trail must not do.
func TestAuditRecord_levelFilteredCallEmitsADrop(t *testing.T) {
	mgr := auditTestManager(t, true)

	// ERROR only: above the operation record's INFO, below the drop's WARN...
	// which is also filtered here, so assert on the harsher setting where the
	// drop itself survives: WARN.
	records := captureAudit(t, slog.LevelWarn, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	if got := len(ofType(records, audit.EventOperation)); got != 0 {
		t.Errorf("an INFO operation record survived a WARN level filter (%d record(s))", got)
	}
	drops := ofType(records, audit.EventAuditDrop)
	if len(drops) != 1 {
		t.Fatalf("want exactly one audit_drop record when the operation record is filtered out, got %d:\n%v", len(drops), records)
	}
	// The drop must stay inside the customer's event="audit" filter — it is
	// the one record that says audit records are missing.
	if drops[0]["event"] != audit.EventValue {
		t.Errorf("drop record event = %v, want %q", drops[0]["event"], audit.EventValue)
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
