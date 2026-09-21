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
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// realOwnerValidationFixture builds an ownerValidatingHandler wrapping the
// real create-queue tool against the real embedded SEMP catalog (the same
// pair TestCompositeToolHandler_OutputSchema_RealCatalogWiring uses), so
// these tests exercise the actual monitor/getMsgVpnClientUsername and
// config/createMsgVpnQueue operations SOL-153080's fix depends on, not a
// hand-rolled stand-in that could drift from the real spec.
func realOwnerValidationFixture(t *testing.T) (*ownerValidatingHandler, *mockClient) {
	t.Helper()
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	var createQueue composite.CompositeTool
	found := false
	for _, tool := range realTools {
		if tool.Name == "create-queue" {
			createQueue = tool
			found = true
			break
		}
	}
	if !found {
		t.Fatal("create-queue not found in the real catalog")
	}

	executor := composite.NewCompositeExecutor(operations)
	getUsername := operations[getClientUsernameOperationID]
	if getUsername == nil {
		t.Fatalf("%q not found in the real catalog", getClientUsernameOperationID)
	}
	inner := NewCompositeToolHandler(createQueue, executor)
	handler := newOwnerValidatingHandler(inner, ownerValidationSpec{configParam: "queueConfig", objectKind: "queue"}, getUsername).(*ownerValidatingHandler)
	return handler, newMockClient()
}

func TestOwnerValidatingHandler_OwnerOmitted_PassesThroughWithoutChecking(t *testing.T) {
	handler, client := realOwnerValidationFixture(t)
	client.responses["createMsgVpnQueue"] = &sempv2.Result{Data: map[string]any{"queueName": "q1", "msgVpnName": "default"}, StatusCode: 200}

	tc := &ToolContext{SEMPv2Client: client}
	_, err := handler.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "q1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, call := range client.calls {
		if call == "getMsgVpnClientUsername" {
			t.Fatalf("owner was never supplied; expected no getMsgVpnClientUsername call, got calls %v", client.calls)
		}
	}
	if len(client.calls) != 1 || client.calls[0] != "createMsgVpnQueue" {
		t.Errorf("expected exactly one createMsgVpnQueue call, got %v", client.calls)
	}
}

func TestOwnerValidatingHandler_OwnerExists_CreatesAfterChecking(t *testing.T) {
	handler, client := realOwnerValidationFixture(t)
	client.responses["getMsgVpnClientUsername"] = &sempv2.Result{
		Data:       map[string]any{"clientUsername": "real-user", "msgVpnName": "default"},
		StatusCode: 200,
	}
	client.responses["createMsgVpnQueue"] = &sempv2.Result{Data: map[string]any{"queueName": "q1", "msgVpnName": "default"}, StatusCode: 200}

	tc := &ToolContext{SEMPv2Client: client}
	_, err := handler.Handle(context.Background(), tc, map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{"owner": "real-user"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.calls) != 2 || client.calls[0] != "getMsgVpnClientUsername" || client.calls[1] != "createMsgVpnQueue" {
		t.Errorf("expected [getMsgVpnClientUsername, createMsgVpnQueue] in order, got %v", client.calls)
	}
}

func TestOwnerValidatingHandler_OwnerDoesNotExist_RejectsWithoutCreating(t *testing.T) {
	handler, client := realOwnerValidationFixture(t)
	client.errors["getMsgVpnClientUsername"] = &sempv2.SEMPError{
		Operation:  "getMsgVpnClientUsername",
		StatusCode: 400,
		SEMPCode:   6,
		SEMPStatus: "NOT_FOUND",
	}

	tc := &ToolContext{SEMPv2Client: client}
	_, err := handler.Handle(context.Background(), tc, map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{"owner": "ghost-user", "permission": "consume"},
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ownerErr *ownerNotFoundError
	if !errors.As(err, &ownerErr) {
		t.Fatalf("expected *ownerNotFoundError, got %T: %v", err, err)
	}
	if ownerErr.owner != "ghost-user" || ownerErr.msgVpn != "default" || ownerErr.objectKind != "queue" {
		t.Errorf("unexpected ownerNotFoundError fields: %+v", ownerErr)
	}
	for _, call := range client.calls {
		if call == "createMsgVpnQueue" {
			t.Fatalf("queue must never be created when owner does not exist; calls = %v", client.calls)
		}
	}
}

func TestOwnerValidatingHandler_CheckFailsTransiently_DeniesOnDoubtWithoutCreating(t *testing.T) {
	handler, client := realOwnerValidationFixture(t)
	client.errors["getMsgVpnClientUsername"] = &sempv2.SEMPError{
		Operation:  "getMsgVpnClientUsername",
		StatusCode: 503,
	}

	tc := &ToolContext{SEMPv2Client: client}
	_, err := handler.Handle(context.Background(), tc, map[string]any{
		"msgVpnName":  "default",
		"queueName":   "q1",
		"queueConfig": map[string]any{"owner": "real-user"},
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ownerErr *ownerNotFoundError
	if errors.As(err, &ownerErr) {
		t.Fatalf("a transient check failure must not be reported as a confirmed-absent owner: %v", err)
	}
	for _, call := range client.calls {
		if call == "createMsgVpnQueue" {
			t.Fatalf("queue must never be created when the owner check itself failed; calls = %v", client.calls)
		}
	}
}

func TestBuildErrorMessage_OwnerNotFound_ShowsCraftedMessageVerbatim(t *testing.T) {
	err := &ownerNotFoundError{owner: "ghost-user", msgVpn: "default", objectKind: "queue"}
	msg, _ := buildErrorMessage(err, "")
	if msg == genericInternalMessage {
		t.Fatal("owner-not-found error must not be suppressed behind the generic internal-error message")
	}
	for _, want := range []string{`"ghost-user"`, `"default"`, "queue"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

func TestIsRetryable_OwnerNotFound_IsFalse(t *testing.T) {
	err := &ownerNotFoundError{owner: "ghost-user", msgVpn: "default", objectKind: "queue"}
	if isRetryable(err) {
		t.Error("a nonexistent owner is a deterministic caller mistake, not a transient failure — must not be retryable")
	}
}
