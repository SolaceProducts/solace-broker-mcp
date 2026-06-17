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

package queueaction

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// fixtureClient is a mock sempv2.Client for testing. It records the last
// call's operation and args so the test can assert the handler issued the
// correct SEMPv2 request, and can be configured to return a synthetic error
// to exercise the error-passthrough path.
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
// minLength on string fields, the strict output envelope, and the
// destructive annotation.
func TestHandler_Metadata(t *testing.T) {
	h := NewHandler()
	meta := h.Metadata()

	if meta.Name != "execute-queue-action" {
		t.Errorf("Name = %q, want %q", meta.Name, "execute-queue-action")
	}

	// The description is the operative confirmation mechanism. Pin the
	// load-bearing phrases so a future description refactor can't silently
	// drop them.
	if !strings.Contains(meta.Description, "BEFORE INVOKING THIS TOOL, obtain explicit user confirmation") {
		t.Error("Description must carry the explicit confirmation instruction")
	}
	if !strings.Contains(meta.Description, "irreversible") {
		t.Error("Description must explicitly name deleteMsgs as irreversible")
	}
	if !strings.Contains(meta.Description, "restating the target") {
		t.Error("Description must instruct the LLM to restate the target before invocation")
	}
	// Confirmation must be a separate user reply, not the original request
	// reinterpreted as preemptive consent.
	if !strings.Contains(meta.Description, "separate user reply") {
		t.Error("Description must require confirmation as a separate user reply")
	}

	// Input schema: action must be enum [deleteMsgs, clearStats] so the
	// manager's validator rejects unknown actions upstream.
	props, ok := meta.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("InputSchema.properties missing or wrong type: %T", meta.InputSchema["properties"])
	}
	actionProp, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatalf("InputSchema.properties.action missing")
	}
	enum, ok := actionProp["enum"].([]string)
	if !ok || len(enum) != 2 || enum[0] != actionDeleteMsgs || enum[1] != actionClearStats {
		t.Errorf("action.enum = %v, want [%q, %q]", actionProp["enum"], actionDeleteMsgs, actionClearStats)
	}

	// minLength: 1 rejects empty strings upstream so the tool never issues
	// a request with an empty path segment.
	for _, name := range []string{"msgVpnName", "queueName"} {
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("InputSchema.properties.%s missing", name)
		}
		if p["minLength"] != 1 {
			t.Errorf("%s.minLength = %v, want 1", name, p["minLength"])
		}
	}

	// Output schema: strict — additionalProperties: false rejects any
	// leaked field; required pins the four-key envelope.
	if got := meta.OutputSchema["additionalProperties"]; got != false {
		t.Errorf(`OutputSchema["additionalProperties"] = %v, want false`, got)
	}
	required, ok := meta.OutputSchema["required"].([]string)
	if !ok || len(required) != 4 {
		t.Errorf(`OutputSchema["required"] = %v, want 4 keys`, meta.OutputSchema["required"])
	}

	// Destructive at the tool level — manager warns on every invocation.
	if meta.Annotations.Destructive == nil || !*meta.Annotations.Destructive {
		t.Errorf("Destructive = %v, want true", meta.Annotations.Destructive)
	}
	if meta.Annotations.ReadOnly {
		t.Error("ReadOnly must be false for an action tool")
	}
	if meta.Annotations.Idempotent {
		t.Error("Idempotent must be false (deleteMsgs is not idempotent)")
	}
}

// TestHandle_DeleteMsgs_Success verifies the destructive path:
// constructs the right PUT operation, forwards the right args, and returns
// the strict success envelope.
func TestHandle_DeleteMsgs_Success(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	result, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "DeadLetterQueue",
		"action":     actionDeleteMsgs,
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
	wantPath := "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/deleteMsgs"
	if stub.lastOp.Path != wantPath {
		t.Errorf("op.Path = %q, want %q", stub.lastOp.Path, wantPath)
	}
	if stub.lastOp.ID != "doMsgVpnQueueDeleteMsgs" {
		t.Errorf("op.ID = %q, want %q (SEMPv2 do<Resource><Action> convention)",
			stub.lastOp.ID, "doMsgVpnQueueDeleteMsgs")
	}
	if stub.lastArgs["msgVpnName"] != "default" {
		t.Errorf("args[msgVpnName] = %v, want %q", stub.lastArgs["msgVpnName"], "default")
	}
	if stub.lastArgs["queueName"] != "DeadLetterQueue" {
		t.Errorf("args[queueName] = %v, want %q", stub.lastArgs["queueName"], "DeadLetterQueue")
	}
	body, ok := stub.lastArgs["body"].(map[string]any)
	if !ok || len(body) != 0 {
		t.Errorf("args[body] = %v, want empty map", stub.lastArgs["body"])
	}

	for k, want := range map[string]any{
		"status":     "ok",
		"action":     actionDeleteMsgs,
		"msgVpnName": "default",
		"queueName":  "DeadLetterQueue",
	} {
		if got := result.StructuredContent[k]; got != want {
			t.Errorf("StructuredContent[%q] = %v, want %v", k, got, want)
		}
	}
}

// TestHandle_ClearStats_Success verifies the non-destructive path uses a
// different URL suffix but the same envelope shape and ID convention.
func TestHandle_ClearStats_Success(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "Q1",
		"action":     actionClearStats,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	wantPath := "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/clearStats"
	if stub.lastOp.Path != wantPath {
		t.Errorf("op.Path = %q, want %q", stub.lastOp.Path, wantPath)
	}
	if stub.lastOp.ID != "doMsgVpnQueueClearStats" {
		t.Errorf("op.ID = %q, want %q", stub.lastOp.ID, "doMsgVpnQueueClearStats")
	}
}

// TestHandle_EmptyMsgVpnName_Rejected verifies the defense-in-depth check
// for an empty msgVpnName. The manager's minLength:1 should reject empty
// strings upstream, but if bypassed Handle must refuse to issue a SEMP
// request with an empty path segment.
func TestHandle_EmptyMsgVpnName_Rejected(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "",
		"queueName":  "awtest",
		"action":     actionClearStats,
	})
	if err == nil {
		t.Fatal("Handle should reject empty msgVpnName")
	}
	if !strings.Contains(err.Error(), "execute-queue-action: msgVpnName is required") {
		t.Errorf("error message %q should attribute the failure and name the missing field", err)
	}
	if stub.calls != 0 {
		t.Errorf("SEMP client must not be called for an empty msgVpnName; got %d calls", stub.calls)
	}
}

// TestHandle_EmptyQueueName_Rejected verifies the parallel guard for an
// empty queueName.
func TestHandle_EmptyQueueName_Rejected(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "",
		"action":     actionClearStats,
	})
	if err == nil {
		t.Fatal("Handle should reject empty queueName")
	}
	if !strings.Contains(err.Error(), "execute-queue-action: queueName is required") {
		t.Errorf("error message %q should attribute the failure and name the missing field", err)
	}
	if stub.calls != 0 {
		t.Errorf("SEMP client must not be called for an empty queueName; got %d calls", stub.calls)
	}
}

// TestHandle_InvalidAction_Rejected is the defense-in-depth check: the
// manager's schema enum should reject unknown actions before Handle runs,
// but if it's bypassed (direct Go caller, buggy validator) Handle must
// still refuse to issue a SEMP request with a caller-supplied action.
func TestHandle_InvalidAction_Rejected(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "Q1",
		"action":     "bogusAction",
	})
	if err == nil {
		t.Fatal("Handle should reject unknown action")
	}
	if !strings.Contains(err.Error(), "execute-queue-action: unsupported action") {
		t.Errorf("error message %q should attribute the failure to this tool and name the unsupported action", err)
	}
	if stub.calls != 0 {
		t.Errorf("SEMP client must not be called for an invalid action; got %d calls", stub.calls)
	}
}

// TestHandle_SEMPError_Passthrough verifies that *sempv2.SEMPError surfaces
// to the manager intact so errors.As(err, &v2Err) can extract structured
// fields (status code, SEMP description) — same convention as the SEMPv1
// tools' SEMPv1.Error passthrough.
func TestHandle_SEMPError_Passthrough(t *testing.T) {
	sempErr := &sempv2.SEMPError{
		Operation:   "doMsgVpnQueueDeleteMsgs",
		StatusCode:  http.StatusNotFound,
		Description: "queue does not exist",
		SEMPCode:    6,
		SEMPStatus:  "NOT_FOUND",
	}
	stub := &fixtureClient{err: sempErr}
	tc := &tools.ToolContext{SEMPv2Client: stub}
	h := NewHandler()

	_, err := h.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "Q1",
		"action":     actionDeleteMsgs,
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
