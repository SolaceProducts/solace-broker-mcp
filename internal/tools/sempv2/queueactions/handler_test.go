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

package queueactions

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

// fixtureClient is a mock sempv2.Client that records the last call and can
// return a synthetic error to exercise the error-passthrough path.
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

// TestDeleteMessages_Metadata pins the destructive tool's surface: name, the
// unconditional confirmation instruction in the description, the strict output
// envelope, and the destructive annotation.
func TestDeleteMessages_Metadata(t *testing.T) {
	meta := NewDeleteMessagesHandler().Metadata()

	if meta.Name != "delete-queue-messages" {
		t.Errorf("Name = %q, want %q", meta.Name, "delete-queue-messages")
	}

	// Description is the operative confirmation control. Pin the load-bearing
	// phrases so a future refactor can't silently drop them. With per-action
	// tools the instruction is unconditional (no "for deleteMsgs, ..." fork).
	for _, phrase := range []string{
		"BEFORE INVOKING THIS TOOL, obtain explicit user confirmation",
		"irreversible",
		"restating the target",
		"separate user reply",
	} {
		if !strings.Contains(meta.Description, phrase) {
			t.Errorf("Description must contain %q", phrase)
		}
	}

	props := meta.InputSchema["properties"].(map[string]any)
	for _, name := range []string{"msgVpnName", "queueName"} {
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("InputSchema.properties.%s missing", name)
		}
		if p["minLength"] != 1 {
			t.Errorf("%s.minLength = %v, want 1", name, p["minLength"])
		}
	}
	// No action parameter on a per-action tool.
	if _, ok := props["action"]; ok {
		t.Error("per-action tool must not declare an 'action' parameter")
	}

	if got := meta.OutputSchema["additionalProperties"]; got != false {
		t.Errorf(`OutputSchema["additionalProperties"] = %v, want false`, got)
	}
	if required, ok := meta.OutputSchema["required"].([]string); !ok || len(required) != 3 {
		t.Errorf(`OutputSchema["required"] = %v, want 3 keys`, meta.OutputSchema["required"])
	}

	if meta.Annotations.Destructive == nil || !*meta.Annotations.Destructive {
		t.Errorf("Destructive = %v, want true", meta.Annotations.Destructive)
	}
	if meta.Annotations.ReadOnly {
		t.Error("ReadOnly must be false for an action tool")
	}
	if meta.Annotations.Idempotent {
		t.Error("Idempotent must be false for delete-queue-messages")
	}
}

// TestClearStats_Metadata pins the non-destructive tool's surface: it is a
// write (not read-only) but NOT destructive, and its description carries no
// confirmation instruction.
func TestClearStats_Metadata(t *testing.T) {
	meta := NewClearStatsHandler().Metadata()

	if meta.Name != "clear-queue-stats" {
		t.Errorf("Name = %q, want %q", meta.Name, "clear-queue-stats")
	}
	if meta.Annotations.ReadOnly {
		t.Error("ReadOnly must be false (clear-queue-stats mutates broker state)")
	}
	if meta.Annotations.Destructive == nil || *meta.Annotations.Destructive {
		t.Errorf("Destructive = %v, want explicit false", meta.Annotations.Destructive)
	}
	if !meta.Annotations.Idempotent {
		t.Error("Idempotent should be true for clear-queue-stats")
	}
	// A non-destructive tool must not carry the destructive confirmation
	// instruction — that would re-introduce the over-prompting the per-action
	// split is meant to remove.
	if strings.Contains(meta.Description, "BEFORE INVOKING THIS TOOL") {
		t.Error("clear-queue-stats description must not carry the confirmation instruction")
	}
	if !strings.Contains(meta.Description, "Non-destructive") {
		t.Error("clear-queue-stats description should state it is non-destructive")
	}
}

// TestDeleteMessages_Success verifies the destructive path: right PUT
// operation, right args, strict success envelope (no action field).
func TestDeleteMessages_Success(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}

	result, err := NewDeleteMessagesHandler().Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "DeadLetterQueue",
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
		t.Errorf("op.ID = %q, want doMsgVpnQueueDeleteMsgs", stub.lastOp.ID)
	}
	for k, want := range map[string]any{"status": "ok", "msgVpnName": "default", "queueName": "DeadLetterQueue"} {
		if got := result.StructuredContent[k]; got != want {
			t.Errorf("StructuredContent[%q] = %v, want %v", k, got, want)
		}
	}
	if _, ok := result.StructuredContent["action"]; ok {
		t.Error("per-action tool output must not include an 'action' field")
	}
}

// TestClearStats_Success verifies the non-destructive path uses the clearStats
// URL suffix and ID.
func TestClearStats_Success(t *testing.T) {
	stub := &fixtureClient{}
	tc := &tools.ToolContext{SEMPv2Client: stub}

	if _, err := NewClearStatsHandler().Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "Q1",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	wantPath := "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/clearStats"
	if stub.lastOp.Path != wantPath {
		t.Errorf("op.Path = %q, want %q", stub.lastOp.Path, wantPath)
	}
	if stub.lastOp.ID != "doMsgVpnQueueClearStats" {
		t.Errorf("op.ID = %q, want doMsgVpnQueueClearStats", stub.lastOp.ID)
	}
}

// TestHandle_EmptyParams_Rejected is the defense-in-depth check: minLength:1
// rejects empties upstream, but if bypassed Handle must refuse to issue a SEMP
// request with an empty path segment.
func TestHandle_EmptyParams_Rejected(t *testing.T) {
	cases := []struct {
		name, vpn, queue, wantErr string
	}{
		{"empty vpn", "", "awtest", "delete-queue-messages: msgVpnName is required"},
		{"empty queue", "default", "", "delete-queue-messages: queueName is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &fixtureClient{}
			_, err := NewDeleteMessagesHandler().Handle(context.Background(),
				&tools.ToolContext{SEMPv2Client: stub}, map[string]any{"msgVpnName": tc.vpn, "queueName": tc.queue})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want containing %q", err, tc.wantErr)
			}
			if stub.calls != 0 {
				t.Errorf("SEMP client must not be called; got %d calls", stub.calls)
			}
		})
	}
}

// TestHandle_SEMPError_Passthrough verifies *sempv2.SEMPError surfaces intact
// so the manager can extract structured fields.
func TestHandle_SEMPError_Passthrough(t *testing.T) {
	sempErr := &sempv2.SEMPError{
		Operation:  "doMsgVpnQueueDeleteMsgs",
		StatusCode: http.StatusNotFound,
		SEMPStatus: "NOT_FOUND",
	}
	stub := &fixtureClient{err: sempErr}
	_, err := NewDeleteMessagesHandler().Handle(context.Background(),
		&tools.ToolContext{SEMPv2Client: stub}, map[string]any{"msgVpnName": "default", "queueName": "Q1"})
	if err == nil {
		t.Fatal("Handle should propagate the SEMP error")
	}
	var v2Err *sempv2.SEMPError
	if !errors.As(err, &v2Err) {
		t.Fatalf("error %T not unwrappable to *sempv2.SEMPError", err)
	}
	if v2Err.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", v2Err.StatusCode)
	}
}
