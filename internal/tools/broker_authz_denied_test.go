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

// A hop-2 (broker-side) authorization denial gets its own audit event and its
// own counter, separate from a hop-1 (MCP-server-side) denial (SOL-153332,
// Story 49).
//
// The two protocol shapes checked here — SEMPv1's ErrorKindPermission and
// SEMPv2's error code 72 — are both classified into the same
// broker_authz_denied record and the same error_type. The invariant that
// matters: a destructive call gets BOTH the operation record and the
// broker_authz_denied record (they coexist; see
// destructive_audit_test.go's note on why "exactly one" is scoped to
// audit_event_type="operation"), while a non-destructive call gets ONLY
// broker_authz_denied, because it never reaches the destructive gate that
// produces an operation record at all.
package tools

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// brokerAuthzTestManager builds a manager with one destructive and one
// non-destructive tool, each returning the given error from Handle, with the
// audit capability set as asked.
func brokerAuthzTestManager(t *testing.T, auditEnabled bool, handleErr error) *ToolManager {
	t.Helper()
	mgr := NewToolManager(newTestPool(t), WithAuditLog(auditEnabled))

	destructive := newStubHandler("delete-queue")
	yes := true
	destructive.annotations = Annotations{Destructive: &yes}
	destructive.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, handleErr
	}
	mgr.Register(destructive)

	readOnly := newStubHandler("read-only-tool")
	readOnly.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, handleErr
	}
	mgr.Register(readOnly)

	return mgr
}

func TestIsBrokerAuthzDenial(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "sempv1 permission error",
			err:  &sempv1.Error{Kind: sempv1.ErrorKindPermission, StatusCode: 200, Message: "Insufficient user privileges"},
			want: true,
		},
		{
			name: "sempv2 code 72",
			err:  &sempv2.SEMPError{StatusCode: 403, SEMPCode: 72, Description: "not authorized"},
			want: true,
		},
		{
			name: "sempv1 execute-fail is not a denial",
			err:  &sempv1.Error{Kind: sempv1.ErrorKindExecuteFail, StatusCode: 200, ReasonCode: 431},
			want: false,
		},
		{
			name: "sempv2 not-found is not a denial",
			err:  &sempv2.SEMPError{StatusCode: 404, SEMPCode: 6, Description: "not found"},
			want: false,
		},
		{
			name: "plain error is not a denial",
			err:  errors.New("broker refused"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBrokerAuthzDenial(tt.err); got != tt.want {
				t.Errorf("isBrokerAuthzDenial(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestBrokerAuthzDenial_DestructiveCall_EmitsBothRecords is the coexistence
// invariant: a destructive call denied at hop 2 produces its operation record
// (error_type=broker_permission_denied) AND a broker_authz_denied record —
// two records, not one replacing the other, because Handle had already been
// called (execution started) by the time the broker refused.
func TestBrokerAuthzDenial_DestructiveCall_EmitsBothRecords(t *testing.T) {
	mgr := brokerAuthzTestManager(t, true, &sempv1.Error{
		Kind:       sempv1.ErrorKindPermission,
		StatusCode: 200,
		Message:    "Insufficient user privileges",
	})
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
	op := ops[0]
	if got := op["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("operation outcome = %v, want %q", got, audit.OutcomeError)
	}
	if got := op["error_type"]; got != "broker_permission_denied" {
		t.Errorf("operation error_type = %v, want broker_permission_denied", got)
	}

	denials := ofType(records, audit.EventBrokerAuthzDenied)
	if len(denials) != 1 {
		t.Fatalf("emitted %d broker_authz_denied record(s), want exactly 1:\n%v", len(denials), records)
	}
	denial := denials[0]
	for _, check := range []struct {
		key  string
		want any
	}{
		{"tool", "delete-queue"},
		{"broker", "dev"},
		{"reason", "permission_denied"},
	} {
		if got := denial[check.key]; got != check.want {
			t.Errorf("broker_authz_denied %s = %v, want %v", check.key, got, check.want)
		}
	}
	for _, forbidden := range []string{"outcome", "error_type", "arguments_hash"} {
		if _, present := denial[forbidden]; present {
			t.Errorf("broker_authz_denied record carries %s = %v, want absent", forbidden, denial[forbidden])
		}
	}
}

// TestBrokerAuthzDenial_NonDestructiveCall_EmitsOnlyDenialRecord covers the
// SEMPv2 shape and the non-destructive path: no operation record exists for a
// non-destructive tool regardless of outcome, so only broker_authz_denied
// appears.
func TestBrokerAuthzDenial_NonDestructiveCall_EmitsOnlyDenialRecord(t *testing.T) {
	mgr := brokerAuthzTestManager(t, true, &sempv2.SEMPError{
		Operation:   "getMsgVpnQueue",
		StatusCode:  403,
		SEMPCode:    72,
		Description: "not authorized",
	})
	args := map[string]any{"broker": "dev", "msgVpnName": "default"}

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "read-only-tool", args, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	if ops := ofType(records, audit.EventOperation); len(ops) != 0 {
		t.Errorf("non-destructive call emitted %d operation record(s), want 0:\n%v", len(ops), records)
	}
	denials := ofType(records, audit.EventBrokerAuthzDenied)
	if len(denials) != 1 {
		t.Fatalf("emitted %d broker_authz_denied record(s), want exactly 1:\n%v", len(denials), records)
	}
	if got := denials[0]["broker"]; got != "dev" {
		t.Errorf("broker_authz_denied broker = %v, want dev", got)
	}
}

// TestBrokerAuthzDenial_AuditLogOff_EmitsNoRecord mirrors the door-closing
// policy every other audit surface in this package follows: the capability is
// inert when OBS_AUDIT_LOG_ENABLED is off, not degraded to a partial record.
func TestBrokerAuthzDenial_AuditLogOff_EmitsNoRecord(t *testing.T) {
	mgr := brokerAuthzTestManager(t, false, &sempv1.Error{
		Kind:       sempv1.ErrorKindPermission,
		StatusCode: 200,
		Message:    "Insufficient user privileges",
	})
	args := map[string]any{"broker": "dev", "msgVpnName": "default"}

	records := captureAudit(t, slog.LevelDebug, func() {
		if _, err := mgr.CallTool(auditCtx(t), "delete-queue", args, idFixture()); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})
	if denials := ofType(records, audit.EventBrokerAuthzDenied); len(denials) != 0 {
		t.Errorf("audit log off but emitted %d broker_authz_denied record(s):\n%v", len(denials), records)
	}
}

// TestBrokerAuthzDenial_RecordsMetricErrorType pins that the tool RED metric
// (mcp_tool_invocation_total's error_type label) reflects the hop-2 denial
// distinctly from a generic execution_error, via the errorType captured by
// CallTool's defer.
// TestBrokerAuthzDenial_RecordsMetricErrorType is the one test that actually
// pins the tool-RED error_type claim its name makes: earlier, this test
// wired no ToolMetrics and asserted nothing beyond retryable, so a sabotaged
// classification (the errorType assignment dropped from CallTool) left it
// green — caught instead, by a different mechanism, by
// TestBrokerAuthzDenial_DestructiveCall_EmitsBothRecords and the integration
// test via audit.NewEvent's schema strictness, not by anything checking the
// metric itself. Wires a real metrics.Provider and scrapes it, asserting
// both mcp_tool_invocation_total{error_type="broker_permission_denied"} and
// mcp_broker_authz_denied_total{reason="permission_denied"} from the one
// CallTool, pinning the pairing in the direction the integration test does
// not cover.
func TestBrokerAuthzDenial_RecordsMetricErrorType(t *testing.T) {
	p, err := metrics.New("v-test", sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatal(err)
	}

	mgr := NewToolManager(newTestPool(t), WithAuditLog(false), WithToolMetrics(tm))
	destructive := newStubHandler("delete-queue")
	yes := true
	destructive.annotations = Annotations{Destructive: &yes}
	destructive.handleFn = func(context.Context, *ToolContext, map[string]any) (*ToolResult, error) {
		return nil, &sempv1.Error{
			Kind:       sempv1.ErrorKindPermission,
			StatusCode: 200,
			Message:    "Insufficient user privileges",
		}
	}
	mgr.Register(destructive)

	result, err := mgr.CallTool(context.Background(), "delete-queue",
		map[string]any{"broker": "dev", "msgVpnName": "default"}, Identity{})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	sc := result.StructuredContent.(map[string]any)
	if sc["retryable"] != false {
		t.Errorf("retryable = %v, want false", sc["retryable"])
	}

	body := scrape(t, p)
	if want := `mcp_tool_invocation_total{broker="dev",error_type="broker_permission_denied",outcome="error",tool="delete-queue"} 1`; !strings.Contains(body, want) {
		t.Errorf("scrape missing %q; the tool-RED metric did not classify this call as a broker permission denial", want)
	}
	if want := `mcp_broker_authz_denied_total{broker="dev",reason="permission_denied",tool="delete-queue"} 1`; !strings.Contains(body, want) {
		t.Errorf("scrape missing %q", want)
	}
}

// TestEmitBrokerAuthzDeniedAudit_ReasonMatchesAuditVocabulary pins that
// brokerAuthzDeniedReasonPermission — this package's local copy of the reason
// string, kept local because audit's own reason vocabulary is package-private
// — actually is a member of it. audit.NewEvent is the constructor that
// enforces the real vocabulary, so this exercises it directly rather than
// asserting against a second copy of the set.
func TestEmitBrokerAuthzDeniedAudit_ReasonMatchesAuditVocabulary(t *testing.T) {
	if _, err := audit.NewEvent(context.Background(), audit.Fields{
		Type:   audit.EventBrokerAuthzDenied,
		Reason: brokerAuthzDeniedReasonPermission,
		Tool:   "delete-queue",
		Broker: "dev",
	}); err != nil {
		t.Errorf("audit.NewEvent rejected reason %q: %v; brokerAuthzDeniedReasonPermission has drifted "+
			"from audit's brokerAuthzDeniedReasons vocabulary", brokerAuthzDeniedReasonPermission, err)
	}
}
