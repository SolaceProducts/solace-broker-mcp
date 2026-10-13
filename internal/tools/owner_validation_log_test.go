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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
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

// TestTruncateOwnerLogValue pins the exact output, not just a bound. The cases
// are chosen so each plausible regression fails one: a byte slice cuts the
// multi-byte values at the wrong place (and splits a rune at an odd offset), an
// off-by-one changes the length of a cut value or cuts a value at the limit.
// The two properties the CHANGELOG states — never mid-rune, at most
// ownerLogMaxRunes — are asserted for every case.
func TestTruncateOwnerLogValue(t *testing.T) {
	const cut = "…"
	for name, tc := range map[string]struct {
		in   string
		want string // exact
	}{
		"empty":                     {"", ""},
		"short":                     {"ghost-user", "ghost-user"},
		"ascii at limit":            {strings.Repeat("a", ownerLogMaxRunes), strings.Repeat("a", ownerLogMaxRunes)},
		"ascii one over":            {strings.Repeat("a", ownerLogMaxRunes+1), strings.Repeat("a", ownerLogMaxRunes-1) + cut},
		"multi-byte at limit":       {strings.Repeat("é", ownerLogMaxRunes), strings.Repeat("é", ownerLogMaxRunes)}, // 400 bytes, 200 runes: a byte-length check cuts it
		"multi-byte one over":       {strings.Repeat("é", ownerLogMaxRunes+1), strings.Repeat("é", ownerLogMaxRunes-1) + cut},
		"odd byte offset":           {"a" + strings.Repeat("é", 300), "a" + strings.Repeat("é", ownerLogMaxRunes-2) + cut}, // a byte cut at 199 lands inside an é
		"three and four byte runes": {strings.Repeat("日", 150) + strings.Repeat("😀", 150), strings.Repeat("日", 150) + strings.Repeat("😀", ownerLogMaxRunes-1-150) + cut},
	} {
		t.Run(name, func(t *testing.T) {
			got := truncateOwnerLogValue(tc.in)
			if got != tc.want {
				t.Errorf("truncateOwnerLogValue = %q (%d runes), want %q (%d runes)",
					got, utf8.RuneCountInString(got), tc.want, utf8.RuneCountInString(tc.want))
			}
			if !utf8.ValidString(got) {
				t.Error("result is not valid UTF-8: a rune was split")
			}
			if n := utf8.RuneCountInString(got); n > ownerLogMaxRunes {
				t.Errorf("result is %d runes, want <= %d", n, ownerLogMaxRunes)
			}
			if wasCut := got != tc.in; wasCut != strings.HasSuffix(got, cut) || (wasCut && !strings.HasPrefix(tc.in, strings.TrimSuffix(got, cut))) {
				t.Errorf("a cut value must end in %q and be a prefix of the input; got %q", cut, got)
			}
		})
	}
}

// TestToolInvokedLine_OwnerAttrs_AreBoundedAndEscaped checks the bound and JSON
// escaping on the real log line. The exact-output cases live in
// TestTruncateOwnerLogValue; this pins that LogAttrs actually applies them.
func TestToolInvokedLine_OwnerAttrs_AreBoundedAndEscaped(t *testing.T) {
	long := "a" + strings.Repeat("é", 5000) // odd byte offset: a byte cut splits a rune
	mgr := ownerLogTestManager(t, "create-queue", false,
		&ownerNotFoundError{owner: long, msgVpn: "v\"pn\nforged=line", objectKind: "queue"})
	line := ownerToolInvokedLine(t, mgr, "create-queue")

	if got, want := line["owner"], truncateOwnerLogValue(long); got != want {
		t.Errorf("logged owner is %d runes, want exactly the %d-rune truncation", utf8.RuneCountInString(got.(string)), utf8.RuneCountInString(want))
	}
	// Quote and newline are inside the kept prefix, so this does exercise
	// escaping: the value must round-trip as one field, not forge a second line
	// (ownerToolInvokedLine already requires exactly one "tool invoked" line).
	if line["msg_vpn"] != "v\"pn\nforged=line" {
		t.Errorf("msg_vpn = %q, want the value round-tripped through JSON escaping", line["msg_vpn"])
	}
}

// TestOwnerErrorLogKeys_MirrorAgentFacingFields pins the log attributes to the
// structured fields buildErrorResult returns for the same error. The owner
// field set is written out twice — errors.go for the agent, LogAttrs for the
// log — and nothing else keeps them in step; a key added to one and not the
// other, or renamed in one, fails here. Log keys are the agent-facing ones in
// snake_case, plus check_stage, which only the log carries.
func TestOwnerErrorLogKeys_MirrorAgentFacingFields(t *testing.T) {
	m := &ToolManager{}
	agentToLog := map[string]string{
		"error_source": "error_source",
		"error_stage":  "error_stage",
		"owner":        "owner",
		"msgVpnName":   "msg_vpn",
		"objectKind":   "object_kind",
	}
	logOnly := map[string]bool{"check_stage": true}

	for name, tc := range map[string]struct {
		err   error
		attrs []slog.Attr
	}{
		"not found": {
			err:   &ownerNotFoundError{owner: "ghost", msgVpn: "default", objectKind: "queue"},
			attrs: (&ownerNotFoundError{owner: "ghost", msgVpn: "default", objectKind: "queue"}).LogAttrs(),
		},
		"check failed": {
			err: &ownerCheckFailedError{cause: errors.New("boom"), owner: "ghost", msgVpn: "default",
				objectKind: "queue", stage: ownerCheckStageClientUsername},
			attrs: (&ownerCheckFailedError{cause: errors.New("boom"), owner: "ghost", msgVpn: "default",
				objectKind: "queue", stage: ownerCheckStageClientUsername}).LogAttrs(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			structured := m.buildErrorResult(tc.err, "dev").StructuredContent.(map[string]any)
			logged := map[string]string{}
			for _, a := range tc.attrs {
				logged[a.Key] = a.Value.String()
			}

			for agentKey, logKey := range agentToLog {
				agentVal, inAgent := structured[agentKey]
				logVal, inLog := logged[logKey]
				if inAgent != inLog {
					t.Errorf("%q is in the agent result: %v, but %q is in the log: %v", agentKey, inAgent, logKey, inLog)
					continue
				}
				if inAgent && agentVal != logVal {
					t.Errorf("agent %q = %v, log %q = %q: the two disagree", agentKey, agentVal, logKey, logVal)
				}
			}
			known := map[string]bool{}
			for _, k := range agentToLog {
				known[k] = true
			}
			for k := range logged {
				if !known[k] && !logOnly[k] {
					t.Errorf("log carries %q, which has no agent-facing counterpart and is not declared log-only", k)
				}
			}
		})
	}
}

// TestToolInvokedLine_RealValidator_EndToEnd runs the real ownerValidatingHandler
// through NewToolManagerFromComposite and CallTool against a stub SEMP server,
// so the attributes are pinned to the rejection the validator actually
// produces, not only to a hand-built error handed to a stub handler.
func TestToolInvokedLine_RealValidator_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method != http.MethodGet:
			fmt.Fprint(w, `{"data":{},"meta":{"responseCode":200}}`)
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

	var lines []map[string]any
	for _, rec := range captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "create-queue", map[string]any{
			"broker": "dev", "msgVpnName": "default", "queueName": "q1",
			"queueConfig": map[string]any{"owner": "ghost"},
		}, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}) {
		if rec["msg"] == "tool invoked" {
			lines = append(lines, rec)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("got %d \"tool invoked\" lines, want 1: %v", len(lines), lines)
	}
	for k, v := range map[string]any{
		"error_source": "owner_validation",
		"owner":        "ghost",
		"msg_vpn":      "default",
		"object_kind":  "queue",
		"error_type":   "execution_error",
		"detail":       "*fmt.wrapError",
	} {
		if lines[0][k] != v {
			t.Errorf("%s = %v, want %v (line: %v)", k, lines[0][k], v, lines[0])
		}
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
