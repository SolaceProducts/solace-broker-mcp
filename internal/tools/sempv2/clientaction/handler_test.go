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

package clientaction

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// fixtureClient mirrors the queueaction package's mock — records the last
// call's operation and args for assertion, supports a synthetic error.
type fixtureClient struct {
	lastOp   *sempv2.Operation
	lastArgs map[string]any
	calls    int
	err      error
}

func (s *fixtureClient) Execute(_ context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	s.calls++
	s.lastOp = op
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return &sempv2.Result{
		Data:       map[string]any{"data": map[string]any{}, "meta": map[string]any{"responseCode": 200}},
		StatusCode: 200,
	}, nil
}

// TestHandler_Metadata pins the MCP-facing surface: name, the operative
// confirmation instruction in the description, the schema enum on action,
// minLength on string fields, the strict output envelope, and destructive
// annotation.
func TestHandler_Metadata(t *testing.T) {
	h := NewHandler()
	meta := h.Metadata()

	if meta.Name != "execute-client-action" {
		t.Errorf("Name = %q, want %q", meta.Name, "execute-client-action")
	}

	// Description is the operative confirmation mechanism.
	if !strings.Contains(meta.Description, "BEFORE INVOKING THIS TOOL, obtain explicit user confirmation") {
		t.Error("Description must carry the explicit confirmation instruction")
	}
	if !strings.Contains(meta.Description, "service-impacting") {
		t.Error("Description must explicitly name disconnect as service-impacting")
	}
	if !strings.Contains(meta.Description, "restating the target") {
		t.Error("Description must instruct the LLM to restate the target before invocation")
	}
	// Confirmation must be a separate user reply, not the original request
	// reinterpreted as preemptive consent.
	if !strings.Contains(meta.Description, "separate user reply") {
		t.Error("Description must require confirmation as a separate user reply")
	}

	// action enum: [disconnect, clearStats]
	props, ok := meta.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("InputSchema.properties missing or wrong type: %T", meta.InputSchema["properties"])
	}
	actionProp, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatalf("InputSchema.properties.action missing")
	}
	enum, ok := actionProp["enum"].([]string)
	if !ok || len(enum) != 2 || enum[0] != actionDisconnect || enum[1] != actionClearStats {
		t.Errorf("action.enum = %v, want [%q, %q]", actionProp["enum"], actionDisconnect, actionClearStats)
	}

	// minLength: 1 on string params.
	for _, name := range []string{"msgVpnName", "clientName"} {
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("InputSchema.properties.%s missing", name)
		}
		if p["minLength"] != 1 {
			t.Errorf("%s.minLength = %v, want 1", name, p["minLength"])
		}
	}

	// Output schema strictness.
	if got := meta.OutputSchema["additionalProperties"]; got != false {
		t.Errorf(`OutputSchema["additionalProperties"] = %v, want false`, got)
	}
	required, ok := meta.OutputSchema["required"].([]string)
	if !ok || len(required) != 4 {
		t.Errorf(`OutputSchema["required"] = %v, want 4 keys`, meta.OutputSchema["required"])
	}

	// Destructive annotation.
	if meta.Annotations.Destructive == nil || !*meta.Annotations.Destructive {
		t.Errorf("Destructive = %v, want true", meta.Annotations.Destructive)
	}
	if meta.Annotations.ReadOnly {
		t.Error("ReadOnly must be false for an action tool")
	}
	if meta.Annotations.Idempotent {
		t.Error("Idempotent must be false (disconnect is not idempotent)")
	}
	if !meta.Annotations.RequiresConfirmation {
		t.Error("RequiresConfirmation must be true")
	}
}

// TestHandle_Disconnect_Success verifies the destructive path.
func TestHandle_Disconnect_Success(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	result, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"clientName": "SlowConsumer_12345",
		"action":     actionDisconnect,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if stub.calls != 1 {
		t.Errorf("calls = %d, want 1", stub.calls)
	}
	if stub.lastOp.Method != "PUT" {
		t.Errorf("op.Method = %q, want PUT", stub.lastOp.Method)
	}
	wantPath := "/SEMP/v2/action/msgVpns/{msgVpnName}/clients/{clientName}/disconnect"
	if stub.lastOp.Path != wantPath {
		t.Errorf("op.Path = %q, want %q", stub.lastOp.Path, wantPath)
	}
	if stub.lastOp.ID != "doMsgVpnClientDisconnect" {
		t.Errorf("op.ID = %q, want %q (SEMPv2 do<Resource><Action> convention)",
			stub.lastOp.ID, "doMsgVpnClientDisconnect")
	}
	if stub.lastArgs["msgVpnName"] != "default" {
		t.Errorf("args[msgVpnName] = %v, want %q", stub.lastArgs["msgVpnName"], "default")
	}
	if stub.lastArgs["clientName"] != "SlowConsumer_12345" {
		t.Errorf("args[clientName] = %v, want %q", stub.lastArgs["clientName"], "SlowConsumer_12345")
	}
	body, ok := stub.lastArgs["body"].(map[string]any)
	if !ok || len(body) != 0 {
		t.Errorf("args[body] = %v, want empty map", stub.lastArgs["body"])
	}

	for k, want := range map[string]any{
		"status":     "ok",
		"action":     actionDisconnect,
		"msgVpnName": "default",
		"clientName": "SlowConsumer_12345",
	} {
		if got := result.StructuredContent[k]; got != want {
			t.Errorf("StructuredContent[%q] = %v, want %v", k, got, want)
		}
	}
}

// TestHandle_ClearStats_Success verifies the non-destructive path.
func TestHandle_ClearStats_Success(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"clientName": "ClientA",
		"action":     actionClearStats,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	wantPath := "/SEMP/v2/action/msgVpns/{msgVpnName}/clients/{clientName}/clearStats"
	if stub.lastOp.Path != wantPath {
		t.Errorf("op.Path = %q, want %q", stub.lastOp.Path, wantPath)
	}
	if stub.lastOp.ID != "doMsgVpnClientClearStats" {
		t.Errorf("op.ID = %q, want %q", stub.lastOp.ID, "doMsgVpnClientClearStats")
	}
}

// TestHandle_InvalidAction_Rejected is the defense-in-depth check.
func TestHandle_InvalidAction_Rejected(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"clientName": "ClientA",
		"action":     "bogusAction",
	})
	if err == nil {
		t.Fatal("Handle should reject unknown action")
	}
	if !strings.Contains(err.Error(), "execute-client-action: unsupported action") {
		t.Errorf("error message %q should attribute the failure to this tool and name the unsupported action", err)
	}
	if stub.calls != 0 {
		t.Errorf("SEMP client must not be called for an invalid action; got %d calls", stub.calls)
	}
}

// TestHandle_SEMPError_Passthrough verifies *sempv2.SEMPError surfaces
// intact for errors.As() extraction.
func TestHandle_SEMPError_Passthrough(t *testing.T) {
	sempErr := &sempv2.SEMPError{
		Operation:   "doMsgVpnClientDisconnect",
		StatusCode:  http.StatusNotFound,
		Description: "client does not exist",
		SEMPCode:    6,
		SEMPStatus:  "NOT_FOUND",
	}
	stub := &fixtureClient{err: sempErr}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"clientName": "ClientA",
		"action":     actionDisconnect,
	})
	if err == nil {
		t.Fatal("Handle should propagate the SEMP error")
	}
	var v2Err *sempv2.SEMPError
	if !errors.As(err, &v2Err) {
		t.Fatalf("returned error %T not unwrappable to *sempv2.SEMPError: %v", err, err)
	}
	if v2Err.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", v2Err.StatusCode)
	}
	if v2Err.SEMPStatus != "NOT_FOUND" {
		t.Errorf("SEMPStatus = %q, want NOT_FOUND", v2Err.SEMPStatus)
	}
}
