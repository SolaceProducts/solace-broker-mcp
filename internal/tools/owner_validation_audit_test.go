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

// A write blocked by SOL-153080's owner check emits an owner_validation_rejected
// audit record (SOL-154703), whether or not the tool is annotated destructive.
//
// Before this, a rejected create-queue (destructive: false) produced no audit
// record at all, and a rejected update-queue (destructive: true) produced only
// an operation record whose sole distinguishing data was error_type=
// execution_error. The shape is the same problem hop-2 authorization denials
// had, and the treatment is the same: a dedicated record, emitted
// unconditionally, gated only on the audit log being on, coexisting with the
// destructive-only operation record rather than replacing it
// (broker_authz_denied_test.go).
//
// Only the confirmed rejection (ownerNotFoundError) is an audit-worthy denial.
// An ownerCheckFailedError means the check could not complete — an ordinary
// transient failure, not a confirmed refusal — and must not fire it.
package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// ownerAuditManager registers one stub tool (destructive as asked) whose Handle
// returns handleErr, with the audit log set as asked.
func ownerAuditManager(t *testing.T, name string, destructive, auditOn bool, handleErr error) *ToolManager {
	t.Helper()
	mgr := NewToolManager(newTestPool(t), WithAuditLog(auditOn))
	h := newStubHandler(name)
	h.annotations = Annotations{Destructive: &destructive}
	h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, handleErr
	}
	mgr.Register(h)
	return mgr
}

func callOwnerAuditTool(t *testing.T, mgr *ToolManager, tool string) []map[string]any {
	t.Helper()
	return captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), tool, map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
}

func TestOwnerValidationRejectedAudit_ConfirmedAbsent_FiresWhateverTheDestructiveFlag(t *testing.T) {
	for _, tc := range []struct {
		tool        string
		destructive bool
		wantOps     int
	}{
		{"create-queue", false, 0}, // the case that had no audit record at all
		{"update-queue", true, 1},  // coexists with its operation record
	} {
		t.Run(tc.tool, func(t *testing.T) {
			mgr := ownerAuditManager(t, tc.tool, tc.destructive, true,
				&ownerNotFoundError{owner: "ghost-user", msgVpn: "default", objectKind: "queue"})
			records := callOwnerAuditTool(t, mgr, tc.tool)

			rejections := ofType(records, audit.EventOwnerValidationRejected)
			if len(rejections) != 1 {
				t.Fatalf("emitted %d owner_validation_rejected record(s), want exactly 1:\n%v", len(rejections), records)
			}
			rec := rejections[0]
			for k, v := range map[string]any{
				"tool":                 tc.tool,
				"broker":               "dev",
				"reason":               "owner_not_found",
				"level":                "WARN",
				"audit_schema_version": schema.AuditSchemaVersion,
				// Identity and correlation come from the request context, never
				// from the call site.
				"agent_client_id": "cursor-ide",
				"correlation_id":  "corr-abc",
			} {
				if rec[k] != v {
					t.Errorf("%s = %v, want %v", k, rec[k], v)
				}
			}
			if principal, _ := rec["principal"].(map[string]any); principal["sub"] != "auth0|abc123" {
				t.Errorf("principal = %v, want sub auth0|abc123", rec["principal"])
			}
			// The record names neither the owner nor the VPN, and carries no
			// outcome or hash: the minimal shape of its sibling.
			for _, forbidden := range []string{"outcome", "error_type", "arguments_hash", "owner", "msg_vpn"} {
				if _, present := rec[forbidden]; present {
					t.Errorf("owner_validation_rejected carries %s = %v, want absent", forbidden, rec[forbidden])
				}
			}

			ops := ofType(records, audit.EventOperation)
			if len(ops) != tc.wantOps {
				t.Fatalf("emitted %d operation record(s), want %d:\n%v", len(ops), tc.wantOps, records)
			}
			if tc.wantOps == 1 && ops[0]["error_type"] != "execution_error" {
				t.Errorf("operation error_type = %v, want execution_error — the invocation classification must not change", ops[0]["error_type"])
			}
		})
	}
}

// TestOwnerValidationRejectedAudit_BrokerIsTheConfiguredName pins that the
// record carries the broker's configured (display) name, not whatever casing the
// caller typed. CallTool rewrites its local alias after resolution for exactly
// this reason, and every other test here already passes the canonical "dev",
// which would hide an emission that read the raw argument instead.
func TestOwnerValidationRejectedAudit_BrokerIsTheConfiguredName(t *testing.T) {
	mgr := ownerAuditManager(t, "create-queue", false, true,
		&ownerNotFoundError{owner: "ghost-user", msgVpn: "default", objectKind: "queue"})
	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "create-queue", map[string]any{"broker": "DEV", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
	rejections := ofType(records, audit.EventOwnerValidationRejected)
	if len(rejections) != 1 {
		t.Fatalf("emitted %d owner_validation_rejected record(s), want exactly 1:\n%v", len(rejections), records)
	}
	if got := rejections[0]["broker"]; got != "dev" {
		t.Errorf("broker = %v, want the configured name %q, not the caller's %q", got, "dev", "DEV")
	}
}

func TestOwnerValidationRejectedAudit_AuditLogOff_EmitsNoRecord(t *testing.T) {
	mgr := ownerAuditManager(t, "create-queue", false, false,
		&ownerNotFoundError{owner: "ghost-user", msgVpn: "default", objectKind: "queue"})
	records := callOwnerAuditTool(t, mgr, "create-queue")
	if got := ofType(records, audit.EventOwnerValidationRejected); len(got) != 0 {
		t.Errorf("audit log off but emitted %d owner_validation_rejected record(s):\n%v", len(got), records)
	}
}

// TestOwnerValidationRejectedAudit_InconclusiveCheck_EmitsNoRejection pins the
// scope the ticket draws: a check that could not complete is not a confirmed
// refusal, and recording it as one would be a false signal for exactly the
// operator who alerts on this record.
func TestOwnerValidationRejectedAudit_InconclusiveCheck_EmitsNoRejection(t *testing.T) {
	checkFailed := func(cause error) error {
		return &ownerCheckFailedError{cause: cause, objectKind: "queue", owner: "real-user", msgVpn: "default",
			stage: ownerCheckStageClientUsername}
	}
	for name, tc := range map[string]struct {
		err        error
		wantHopTwo int
	}{
		"503 on the pre-flight read": {checkFailed(&sempv2.SEMPError{StatusCode: 503}), 0},
		"transport failure":          {checkFailed(errors.New("dial tcp: connection refused")), 0},
		// Documented expectation, not an accident: a code-72 denial of the read
		// is a real hop-2 refusal and already fires broker_authz_denied. It is
		// still not a confirmed owner rejection.
		"code 72 on the pre-flight read": {checkFailed(&sempv2.SEMPError{StatusCode: 403, SEMPCode: sempv2.SEMPCodePermissionDenied}), 1},
	} {
		t.Run(name, func(t *testing.T) {
			mgr := ownerAuditManager(t, "create-queue", false, true, tc.err)
			records := callOwnerAuditTool(t, mgr, "create-queue")
			if got := ofType(records, audit.EventOwnerValidationRejected); len(got) != 0 {
				t.Errorf("an inconclusive check emitted %d owner_validation_rejected record(s):\n%v", len(got), records)
			}
			if got := ofType(records, audit.EventBrokerAuthzDenied); len(got) != tc.wantHopTwo {
				t.Errorf("emitted %d broker_authz_denied record(s), want %d:\n%v", len(got), tc.wantHopTwo, records)
			}
		})
	}
}

func TestOwnerValidationRejectedAudit_UnrelatedError_EmitsNoRejection(t *testing.T) {
	mgr := ownerAuditManager(t, "create-queue", false, true, errors.New("broker refused"))
	if got := ofType(callOwnerAuditTool(t, mgr, "create-queue"), audit.EventOwnerValidationRejected); len(got) != 0 {
		t.Errorf("an unrelated failure emitted %d owner_validation_rejected record(s)", len(got))
	}
}

func TestIsOwnerValidationRejection(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                       {nil, false},
		"confirmed absent":          {&ownerNotFoundError{owner: "ghost", msgVpn: "default"}, true},
		"confirmed absent, wrapped": {fmt.Errorf("executing tool %q: %w", "create-queue", &ownerNotFoundError{owner: "ghost"}), true},
		"check failed":              {&ownerCheckFailedError{cause: errors.New("boom")}, false},
		"check failed, code 72":     {&ownerCheckFailedError{cause: &sempv2.SEMPError{SEMPCode: sempv2.SEMPCodePermissionDenied}}, false},
		"plain error":               {errors.New("broker refused"), false},
		"sempv2 not found":          {&sempv2.SEMPError{StatusCode: 400, SEMPCode: 6}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isOwnerValidationRejection(tc.err); got != tc.want {
				t.Errorf("isOwnerValidationRejection(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestEmitOwnerValidationRejectedAudit_ReasonMatchesAuditVocabulary pins that
// ownerValidationRejectedReasonNotFound — this package's local copy of the
// reason string, kept local because audit's reason vocabularies are
// package-private — is a member of it. audit.NewEvent is the constructor that
// enforces the real vocabulary, so this exercises it directly rather than
// asserting against a second copy of the set.
func TestEmitOwnerValidationRejectedAudit_ReasonMatchesAuditVocabulary(t *testing.T) {
	if _, err := audit.NewEvent(context.Background(), audit.Fields{
		Type:   audit.EventOwnerValidationRejected,
		Reason: ownerValidationRejectedReasonNotFound,
		Tool:   "create-queue",
		Broker: "dev",
	}); err != nil {
		t.Fatalf("reason %q is not in the audit vocabulary: %v", ownerValidationRejectedReasonNotFound, err)
	}
}

// TestOwnerValidationRejectedAudit_RealValidator_EndToEnd runs the real
// ownerValidatingHandler through NewToolManagerFromComposite and CallTool
// against a stub SEMP server, so the record is pinned to the rejection the
// validator actually produces, not only to a hand-built error handed to a stub.
func TestOwnerValidationRejectedAudit_RealValidator_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method != http.MethodGet:
			fmt.Fprint(w, `{"data":{},"meta":{"responseCode":200}}`)
		case strings.Contains(r.URL.Path, "clientUsernames/real-user"):
			fmt.Fprint(w, `{"data":{"clientUsername":"real-user"},"meta":{"responseCode":200}}`)
		case strings.Contains(r.URL.Path, "clientUsernames"):
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"meta":{"responseCode":400,"error":{"code":6,"description":"not found","status":"NOT_FOUND"}}}`)
		default:
			fmt.Fprint(w, `{"data":{"msgVpnName":"default"},"meta":{"responseCode":200}}`)
		}
	}))
	t.Cleanup(srv.Close)

	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatal(err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pool := semp.NewBrokerPool(writeTestConfig(t, [2]string{"dev", srv.URL}), nil)
	mgr := NewToolManagerFromComposite(pool, realTools, composite.NewCompositeExecutor(ops), WithAuditLog(true))

	call := func(owner string) []map[string]any {
		t.Helper()
		return captureAudit(t, slog.LevelDebug, func() {
			if _, err := mgr.CallTool(auditCtx(t), "create-queue", map[string]any{
				"broker": "dev", "msgVpnName": "default", "queueName": "q1",
				"queueConfig": map[string]any{"owner": owner},
			}, idFixture()); err != nil {
				t.Fatalf("CallTool: %v", err)
			}
		})
	}

	if got := ofType(call("real-user"), audit.EventOwnerValidationRejected); len(got) != 0 {
		t.Errorf("an owner that exists emitted %d owner_validation_rejected record(s)", len(got))
	}
	records := call("ghost")
	rejections := ofType(records, audit.EventOwnerValidationRejected)
	if len(rejections) != 1 {
		t.Fatalf("emitted %d owner_validation_rejected record(s), want exactly 1:\n%v", len(rejections), records)
	}
	if rejections[0]["tool"] != "create-queue" || rejections[0]["broker"] != "dev" || rejections[0]["reason"] != "owner_not_found" {
		t.Errorf("record = %v, want tool create-queue, broker dev, reason owner_not_found", rejections[0])
	}
	if ops := ofType(records, audit.EventOperation); len(ops) != 0 {
		t.Errorf("create-queue is not destructive but emitted %d operation record(s)", len(ops))
	}
}
