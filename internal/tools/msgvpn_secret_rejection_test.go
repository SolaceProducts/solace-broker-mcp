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

package tools

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// secretSentinelForMsgVpn is a recognisable secret value. It must appear nowhere
// in the agent-facing reply, the logs or the audit stream.
const secretSentinelForMsgVpn = "SECRET-SENTINEL-7f3a9c"

// SOL-156210: a replication-bridge secret sent to update-message-vpn is rejected
// by the real definition and real executor behind CallTool, before any broker
// call. The agent sees which field and what to do, and the secret value appears
// in neither the reply, the logs, nor the audit stream; the destructive call
// still produces its audit record, as a failure.
func TestUpdateMessageVPN_RejectedSecret_ReplyLogsAndAuditCarryNoValue(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	catalog, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	var tool composite.CompositeTool
	for _, tl := range catalog {
		if tl.Name == "update-message-vpn" {
			tool = tl
		}
	}
	if tool.Name == "" {
		t.Fatal("update-message-vpn missing from the real catalog")
	}

	// The call is rejected inside the executor, so the broker client the pool
	// resolves is never used.
	mgr := NewToolManager(newTestPool(t), WithAuditLog(true))
	mgr.Register(NewCompositeToolHandler(tool, composite.NewCompositeExecutor(operations)))

	var reply string
	var isError bool
	records := captureAudit(t, slog.LevelDebug, func() {
		result, callErr := mgr.CallTool(auditCtx(t), "update-message-vpn", map[string]any{
			"broker":     "dev",
			"msgVpnName": "default",
			"msgVpnConfig": map[string]any{
				"replicationBridgeAuthenticationBasicPassword": secretSentinelForMsgVpn,
			},
		}, idFixture())
		isError = result != nil && result.IsError
		reply = callToolResultText(t, result, callErr)
	})

	if !isError {
		t.Fatal("expected the call to be rejected")
	}
	for _, want := range []string{`"replicationBridgeAuthenticationBasicPassword"`, "Set it yourself", "rotate"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply = %q, want it to contain %q", reply, want)
		}
	}

	everything, _ := json.Marshal(records)
	if strings.Contains(reply, secretSentinelForMsgVpn) || strings.Contains(string(everything), secretSentinelForMsgVpn) {
		t.Errorf("the secret value leaked into the reply or the logs/audit stream:\nreply=%s\nrecords=%s", reply, everything)
	}

	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 {
		t.Fatalf("emitted %d operation record(s), want exactly 1 (a rejected destructive call is still audited):\n%v", len(ops), records)
	}
	if got := ops[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeError)
	}
	if got := ops[0]["error_type"]; got != "execution_error" {
		t.Errorf("error_type = %v, want execution_error (accepted classification for body-check rejections)", got)
	}
	// The digest is over the redacted arguments, so it cannot depend on the secret.
	want, err := audit.HashArgs(map[string]any{
		"msgVpnName":   "default",
		"msgVpnConfig": map[string]any{"replicationBridgeAuthenticationBasicPassword": "[REDACTED]"},
	})
	if err != nil {
		t.Fatalf("HashArgs: %v", err)
	}
	if got := ops[0]["arguments_hash"]; got != want {
		t.Errorf("arguments_hash = %v, want %s (digest of the redacted arguments)", got, want)
	}
}
