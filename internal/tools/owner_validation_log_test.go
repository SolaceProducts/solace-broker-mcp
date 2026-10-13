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

// A write blocked by SOL-153080's owner check logs which owner, VPN and
// pre-flight read were involved as structured attributes on the "tool
// invoked" ERROR line (SOL-155918). Before this, a confirmed-absent owner
// logged detail="*fmt.wrapError" and nothing else: neither owner type is on
// logToolResult's vouched-type gate, so the line carried no owner, no VPN, and
// nothing to tell this control apart from any other execution_error.
//
// The gate itself is deliberately NOT widened. ownerCheckFailedError wraps
// whatever sempv2.Client.Execute returned, which can be an unvouched
// transport/auth wrap error, so its Error() text must never reach "detail"
// unless the chain also holds a vouched type — exactly as before. The
// attributes under test carry the new information instead.
package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// ownerLogTestManager registers one tool named name (destructive as asked)
// whose Handle returns handleErr, with the audit log on so a destructive tool
// also emits its operation record alongside the line under test.
func ownerLogTestManager(t *testing.T, name string, destructive bool, handleErr error) *ToolManager {
	t.Helper()
	mgr := NewToolManager(newTestPool(t), WithAuditLog(true))
	h := newStubHandler(name)
	h.annotations = Annotations{Destructive: &destructive}
	h.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, handleErr
	}
	mgr.Register(h)
	return mgr
}

// ownerToolInvokedLine runs one call and returns the single "tool invoked" record.
func ownerToolInvokedLine(t *testing.T, mgr *ToolManager, tool string) map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, rec := range captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), tool, map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}) {
		if rec["msg"] == "tool invoked" {
			lines = append(lines, rec)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("got %d \"tool invoked\" lines, want exactly 1: %v", len(lines), lines)
	}
	return lines[0]
}

func TestToolInvokedLine_OwnerNotFound_CarriesOwnerAttrs(t *testing.T) {
	for _, tc := range []struct {
		tool        string
		destructive bool
	}{
		{"create-queue", false},
		{"update-queue", true},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			mgr := ownerLogTestManager(t, tc.tool, tc.destructive,
				&ownerNotFoundError{owner: "ghost-user", msgVpn: "default", objectKind: "queue"})
			line := ownerToolInvokedLine(t, mgr, tc.tool)

			want := map[string]any{
				"owner":        "ghost-user",
				"msg_vpn":      "default",
				"object_kind":  "queue",
				"error_source": "owner_validation",
				"error_type":   "execution_error",
			}
			for k, v := range want {
				if line[k] != v {
					t.Errorf("%s = %v, want %v (line: %v)", k, line[k], v, line)
				}
			}
			// detail stays the type-only fallback: the gate is not widened.
			if line["detail"] != "*fmt.wrapError" {
				t.Errorf("detail = %v, want the type-only fallback *fmt.wrapError", line["detail"])
			}
			if _, ok := line["error_stage"]; ok {
				t.Errorf("a confirmed rejection must not carry error_stage: %v", line)
			}
		})
	}
}

func TestToolInvokedLine_OwnerCheckFailed_VouchedCause_CarriesOwnerAndSempAttrs(t *testing.T) {
	cause := &sempv2.SEMPError{StatusCode: 503, SEMPCode: 0, Operation: "getMsgVpnClientUsername"}
	mgr := ownerLogTestManager(t, "create-queue", false, &ownerCheckFailedError{
		cause: cause, objectKind: "queue", owner: "real-user", msgVpn: "default",
		stage: ownerCheckStageClientUsername,
	})
	line := ownerToolInvokedLine(t, mgr, "create-queue")

	for k, v := range map[string]any{
		"owner":       "real-user",
		"msg_vpn":     "default",
		"object_kind": "queue",
		"error_stage": "owner_validation_check",
		"check_stage": string(ownerCheckStageClientUsername),
		"operation":   "getMsgVpnClientUsername",
	} {
		if line[k] != v {
			t.Errorf("%s = %v, want %v (line: %v)", k, line[k], v, line)
		}
	}
	// The wrapped SEMPError's own attributes must survive: the owner attrs are
	// layered on top of the protocol switch, not a case inside it.
	if got, _ := line["http_status"].(float64); got != 503 {
		t.Errorf("http_status = %v, want 503 — the wrapped cause's classification was lost", line["http_status"])
	}
	if _, ok := line["error_source"]; ok {
		t.Errorf("a check failure must not claim error_source=owner_validation: %v", line)
	}
}

// TestToolInvokedLine_OwnerCheckFailed_UnvouchedCause_DetailStaysTypeOnly pins
// the reason the vouched-type gate was left alone. The cause here stands in for
// the raw transport error sempv2.Client.Execute can return ("executing X: %w");
// its text must not reach the log, while the owner attributes still do.
func TestToolInvokedLine_OwnerCheckFailed_UnvouchedCause_DetailStaysTypeOnly(t *testing.T) {
	const leak = "dial tcp 10.9.8.7:943: connect: sensitive-intermediary-text"
	mgr := ownerLogTestManager(t, "create-queue", false, &ownerCheckFailedError{
		cause: errors.New(leak), objectKind: "queue", owner: "real-user", msgVpn: "default",
		stage: ownerCheckStageVpnDisambiguation,
	})
	line := ownerToolInvokedLine(t, mgr, "create-queue")

	if line["detail"] != "*fmt.wrapError" {
		t.Errorf("detail = %v, want the type-only fallback — the unvouched cause was laundered into the log", line["detail"])
	}
	for k, v := range line {
		if s, ok := v.(string); ok && strings.Contains(s, "sensitive-intermediary-text") {
			t.Errorf("unvouched cause text leaked via %q: %q", k, s)
		}
	}
	for k, v := range map[string]any{
		"owner":       "real-user",
		"msg_vpn":     "default",
		"error_stage": "owner_validation_check",
		"check_stage": string(ownerCheckStageVpnDisambiguation),
	} {
		if line[k] != v {
			t.Errorf("%s = %v, want %v (line: %v)", k, line[k], v, line)
		}
	}
}

func TestToolInvokedLine_OwnerAttrs_AreBoundedAndEscaped(t *testing.T) {
	long := strings.Repeat("é", 5000) // multi-byte: truncation must not split a rune
	mgr := ownerLogTestManager(t, "create-queue", false,
		&ownerNotFoundError{owner: long + "\nforged=line", msgVpn: "v\"pn", objectKind: "queue"})
	line := ownerToolInvokedLine(t, mgr, "create-queue")

	owner, _ := line["owner"].(string)
	if n := len([]rune(owner)); n > ownerLogMaxRunes+1 { // +1 for the truncation marker
		t.Errorf("logged owner is %d runes, want <= %d plus a marker", n, ownerLogMaxRunes)
	}
	if !strings.HasSuffix(owner, "…") {
		t.Errorf("a truncated owner should say so; got suffix %q", owner[max(0, len(owner)-8):])
	}
	if strings.Contains(owner, "�") {
		t.Error("truncation split a multi-byte rune")
	}
	if line["msg_vpn"] != "v\"pn" {
		t.Errorf("msg_vpn = %q, want the value round-tripped through JSON escaping", line["msg_vpn"])
	}
}

func TestToolInvokedLine_OtherErrors_CarryNoOwnerAttrs(t *testing.T) {
	mgr := ownerLogTestManager(t, "create-queue", false, errors.New("broker refused"))
	line := ownerToolInvokedLine(t, mgr, "create-queue")
	for _, k := range []string{"owner", "msg_vpn", "object_kind", "error_source", "error_stage", "check_stage"} {
		if _, ok := line[k]; ok {
			t.Errorf("unrelated failure carries %q: %v", k, line)
		}
	}
}
