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

// The authz_denied audit record withAuthorization emits on a hop-1 denial
// (SOL-152097), alongside the existing "tool authorization" WARN log.
//
// The two deny tests wrap a REAL ToolManager.CallTool as next (via
// auditTestManager's registered "delete-queue" destructive tool), not a
// stub recording handler: that is what makes "produced no operation
// record" a meaningful assertion rather than a vacuous one. CallTool is
// where emitOperationAudit lives, so had withAuthorization dispatched to
// next, an operation record could have appeared; a hop-1 denial refuses the
// tool before ever calling next, so CallTool never runs and genuinely
// cannot have produced one. A hop-2 broker denial (SOL-153332, a separate
// story) is a different call shape — execution had already started — and
// legitimately produces an operation record alongside its own event type;
// this file's scope stays at hop 1, where "no operation record" actually
// distinguishes something.

package tools

import (
	"context"
	"log/slog"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callToolNext builds a next handler that dispatches to mgr.CallTool for
// "delete-queue" — the real production path an allowed call would have
// taken, and the only path that can produce an operation record.
func callToolNext(mgr *ToolManager) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mgr.CallTool(ctx, "delete-queue", map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture())
	}
}

// TestAuthzDenied_MissingClaim_EmitsAuditRecord pins the missing-claim deny:
// exactly one authz_denied record, reason=missing_claim, carrying tool and
// identity but no outcome or error_type — and no operation record, because
// next (a real CallTool dispatch to a destructive, audited tool) never ran.
func TestAuthzDenied_MissingClaim_EmitsAuditRecord(t *testing.T) {
	mgr := auditTestManager(t, true)
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "delete-queue"),
		"delete-queue",
		"groups", true, nil,
		callToolNext(mgr))
	req := requestMissingGroupsClaim()
	ctx := ctxWithPrincipal(correlation.With(context.Background(), "corr-missing-claim"), req)

	records := captureAudit(t, slog.LevelInfo, func() {
		if _, err := wrapped(ctx, req); err != nil {
			t.Fatalf("wrapper returned error: %v", err)
		}
	})

	denies := ofType(records, audit.EventAuthzDenied)
	if len(denies) != 1 {
		t.Fatalf("want exactly 1 authz_denied record, got %d:\n%v", len(denies), records)
	}
	rec := denies[0]
	if got := rec["reason"]; got != "missing_claim" {
		t.Errorf("reason = %v, want %q", got, "missing_claim")
	}
	if got := rec["tool"]; got != "delete-queue" {
		t.Errorf("tool = %v, want %q", got, "delete-queue")
	}
	principal, ok := rec["principal"].(map[string]any)
	if !ok || principal["sub"] != "alice" {
		t.Errorf("principal = %#v, want {sub: alice}", rec["principal"])
	}
	for _, forbidden := range []string{"outcome", "error_type"} {
		if _, present := rec[forbidden]; present {
			t.Errorf("authz_denied record carries %s = %v, want absent", forbidden, rec[forbidden])
		}
	}
	if got := len(ofType(records, audit.EventOperation)); got != 0 {
		t.Errorf("a hop-1 denial produced %d operation record(s), want 0 — next (CallTool) never ran", got)
	}
}

// TestAuthzDenied_NotPermitted_EmitsAuditRecord pins the not-permitted deny:
// exactly one authz_denied record, reason=not_permitted, carrying
// agent_client_id — and, per the separation-of-duties rule this file's own
// package comment states, no matched/caller group membership anywhere in
// the record. Same real-CallTool next as the missing-claim test above, so
// "no operation record" is meaningful here too.
func TestAuthzDenied_NotPermitted_EmitsAuditRecord(t *testing.T) {
	mgr := auditTestManager(t, true)
	wrapped := withAuthorization(
		emptyPolicy(t),
		"delete-queue",
		"groups", true, nil,
		callToolNext(mgr))
	req := requestWithGroupsAndCorrelation([]string{"Contractors"})
	ctx := ctxWithPrincipal(correlation.With(context.Background(), "corr-not-permitted"), req)

	records := captureAudit(t, slog.LevelInfo, func() {
		if _, err := wrapped(ctx, req); err != nil {
			t.Fatalf("wrapper returned error: %v", err)
		}
	})

	denies := ofType(records, audit.EventAuthzDenied)
	if len(denies) != 1 {
		t.Fatalf("want exactly 1 authz_denied record, got %d:\n%v", len(denies), records)
	}
	rec := denies[0]
	if got := rec["reason"]; got != "not_permitted" {
		t.Errorf("reason = %v, want %q", got, "not_permitted")
	}
	if got := rec["agent_client_id"]; got != "cursor-ide" {
		t.Errorf("agent_client_id = %v, want %q", got, "cursor-ide")
	}
	for _, forbidden := range []string{"outcome", "error_type", "matched_groups", "caller_groups"} {
		if _, present := rec[forbidden]; present {
			t.Errorf("authz_denied record carries %s = %v, want absent", forbidden, rec[forbidden])
		}
	}
	if got := len(ofType(records, audit.EventOperation)); got != 0 {
		t.Errorf("a hop-1 denial produced %d operation record(s), want 0 — next (CallTool) never ran", got)
	}
}

// TestAuthzDenied_AllowedCallWithSameNext_ProducesOperationRecord is the
// control for the two "no operation record" assertions above: it drives the
// identical callToolNext through an ALLOWING policy instead, proving an
// operation record really was reachable from this call shape and that the
// two deny tests' zero counts mean something rather than being structurally
// guaranteed regardless of what withAuthorization does.
func TestAuthzDenied_AllowedCallWithSameNext_ProducesOperationRecord(t *testing.T) {
	mgr := auditTestManager(t, true)
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "delete-queue"),
		"delete-queue",
		"groups", true, nil,
		callToolNext(mgr))
	req := requestWithGroups([]string{"Ops"})
	ctx := ctxWithPrincipal(context.Background(), req)

	records := captureAudit(t, slog.LevelInfo, func() {
		if _, err := wrapped(ctx, req); err != nil {
			t.Fatalf("wrapper returned error: %v", err)
		}
	})

	if got := len(ofType(records, audit.EventAuthzDenied)); got != 0 {
		t.Errorf("an allowed call emitted %d authz_denied record(s), want 0", got)
	}
	if got := len(ofType(records, audit.EventOperation)); got != 1 {
		t.Fatalf("an allowed call through the same next produced %d operation record(s), want exactly 1 — "+
			"if this fails, the two deny tests' \"0 operation records\" assertions are vacuous", got)
	}
}

// TestAuthzDenied_Allow_EmitsNoAuditRecord pins scope: an allowed call emits
// no authz_denied record.
func TestAuthzDenied_Allow_EmitsNoAuditRecord(t *testing.T) {
	wrapped := withAuthorization(
		policyGranting(t, []string{"Ops"}, "get-broker-status"),
		"get-broker-status",
		"groups", true, nil,
		newRecordingHandler().handler())
	req := requestWithGroups([]string{"Ops"})
	ctx := ctxWithPrincipal(context.Background(), req)

	records := captureAudit(t, slog.LevelInfo, func() {
		if _, err := wrapped(ctx, req); err != nil {
			t.Fatalf("wrapper returned error: %v", err)
		}
	})

	if got := len(ofType(records, audit.EventAuthzDenied)); got != 0 {
		t.Errorf("an allowed call emitted %d authz_denied record(s), want 0", got)
	}
}

// TestAuthzDenied_AuditDisabled_emitsNoRecord pins the flag-off contract: the
// existing "tool authorization" WARN still fires (byte-identical to
// pre-SOL-152097 behaviour), and no audit record does.
func TestAuthzDenied_AuditDisabled_emitsNoRecord(t *testing.T) {
	wrapped := withAuthorization(
		emptyPolicy(t),
		"delete-queue",
		"groups", false, nil,
		newRecordingHandler().handler())
	req := requestWithGroups([]string{"Contractors"})
	ctx := ctxWithPrincipal(context.Background(), req)

	records := captureAudit(t, slog.LevelInfo, func() {
		if _, err := wrapped(ctx, req); err != nil {
			t.Fatalf("wrapper returned error: %v", err)
		}
	})

	var sawWarn bool
	for _, rec := range records {
		if rec["msg"] == "tool authorization" && rec["decision"] == "denied" {
			sawWarn = true
		}
		if rec["event"] == audit.EventValue {
			t.Errorf("audit record emitted with the capability off: %#v", rec)
		}
	}
	if !sawWarn {
		t.Errorf("the 'tool authorization' WARN was not emitted with the audit log off:\n%v", records)
	}
}
