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

// realOwnerValidationFixture builds an ownerValidatingHandler wrapping toolName's
// real composite tool definition and ownerValidatedTools entry, against the
// real embedded SEMP catalog (the same pair
// TestCompositeToolHandler_OutputSchema_RealCatalogWiring uses), so these
// tests exercise the actual monitor/getMsgVpnClientUsername,
// monitor/getMsgVpn, and config/* operations SOL-153080's fix depends on,
// not a hand-rolled stand-in that could drift from the real spec or from
// ownerValidatedTools itself.
func realOwnerValidationFixture(t *testing.T, toolName string) (*ownerValidatingHandler, *mockClient) {
	t.Helper()
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	var tool composite.CompositeTool
	found := false
	for _, tl := range realTools {
		if tl.Name == toolName {
			tool = tl
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%q not found in the real catalog", toolName)
	}
	spec, ok := ownerValidatedTools[toolName]
	if !ok {
		t.Fatalf("%q is not registered in ownerValidatedTools", toolName)
	}

	executor := composite.NewCompositeExecutor(operations)
	getUsername := operations[getClientUsernameOperationID]
	if getUsername == nil {
		t.Fatalf("%q not found in the real catalog", getClientUsernameOperationID)
	}
	getVpn := operations[getMsgVpnOperationID]
	if getVpn == nil {
		t.Fatalf("%q not found in the real catalog", getMsgVpnOperationID)
	}
	inner := NewCompositeToolHandler(tool, executor)
	handler := newOwnerValidatingHandler(inner, spec, getUsername, getVpn).(*ownerValidatingHandler)
	return handler, newMockClient()
}

// vpnExistsResponse is the getMsgVpn response every test below installs
// unless it's specifically exercising the missing-VPN disambiguation path —
// it stands in for "the VPN is real", so a NOT_FOUND from the client-username
// check can only mean the owner itself is missing.
func vpnExistsResponse() *sempv2.Result {
	return &sempv2.Result{Data: map[string]any{"msgVpnName": "default"}, StatusCode: 200}
}

func notFoundError(operation string) *sempv2.SEMPError {
	return &sempv2.SEMPError{Operation: operation, StatusCode: 400, SEMPCode: 6, SEMPStatus: "NOT_FOUND"}
}

func TestOwnerValidatingHandler_OwnerOmitted_PassesThroughWithoutChecking(t *testing.T) {
	handler, client := realOwnerValidationFixture(t, "create-queue")
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
	handler, client := realOwnerValidationFixture(t, "create-queue")
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
	// The VPN-existence disambiguation call must only ever run after a
	// NOT_FOUND from the username check, never on the happy path.
	if len(client.calls) != 2 || client.calls[0] != "getMsgVpnClientUsername" || client.calls[1] != "createMsgVpnQueue" {
		t.Errorf("expected [getMsgVpnClientUsername, createMsgVpnQueue] in order, got %v", client.calls)
	}
}

func TestOwnerValidatingHandler_OwnerDoesNotExist_RejectsWithoutCreating(t *testing.T) {
	handler, client := realOwnerValidationFixture(t, "create-queue")
	client.errors["getMsgVpnClientUsername"] = notFoundError("getMsgVpnClientUsername")
	client.responses["getMsgVpn"] = vpnExistsResponse()

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
	handler, client := realOwnerValidationFixture(t, "create-queue")
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

// TestOwnerValidatingHandler_MissingVpn_FallsThroughInsteadOfBlamingOwner pins
// the fix for a real bug found in review: SEMP returns the byte-identical
// NOT_FOUND/SEMPCode 6 whether msgVpnName itself doesn't exist or whether
// only clientUsername is missing (confirmed live against a broker), so a
// naive check would blame a nonexistent VPN on "no such owner" — masking the
// real problem. When the VPN itself doesn't exist, this must fall through to
// the wrapped tool's own call, which reports the correct, existing "Message
// VPN does not exist" error instead.
func TestOwnerValidatingHandler_MissingVpn_FallsThroughInsteadOfBlamingOwner(t *testing.T) {
	handler, client := realOwnerValidationFixture(t, "create-queue")
	client.errors["getMsgVpnClientUsername"] = notFoundError("getMsgVpnClientUsername")
	client.errors["getMsgVpn"] = notFoundError("getMsgVpn")
	client.errors["createMsgVpnQueue"] = notFoundError("createMsgVpnQueue")

	tc := &ToolContext{SEMPv2Client: client}
	_, err := handler.Handle(context.Background(), tc, map[string]any{
		"msgVpnName":  "totally-missing-vpn",
		"queueName":   "q1",
		"queueConfig": map[string]any{"owner": "real-user"},
	})
	var ownerErr *ownerNotFoundError
	if errors.As(err, &ownerErr) {
		t.Fatalf("a missing VPN must not be reported as a missing owner: %v", err)
	}
	if len(client.calls) != 3 || client.calls[2] != "createMsgVpnQueue" {
		t.Fatalf("expected the call to fall through to createMsgVpnQueue after confirming the VPN itself is missing, got calls %v", client.calls)
	}
}

// TestOwnerValidatingHandler_VpnCheckItselfFails_ReportsThatFailureNotStaleOwnerError
// pins a bug found in review: when the disambiguation read (getMsgVpn) fails
// inconclusively (not NOT_FOUND — a transient 503 here), the error returned
// must describe THAT failure, not silently fall back to the original
// ambiguous owner-check NOT_FOUND — which would reintroduce the exact
// misleading "owner not found" message this fix exists to avoid.
func TestOwnerValidatingHandler_VpnCheckItselfFails_ReportsThatFailureNotStaleOwnerError(t *testing.T) {
	handler, client := realOwnerValidationFixture(t, "create-queue")
	client.errors["getMsgVpnClientUsername"] = notFoundError("getMsgVpnClientUsername")
	client.errors["getMsgVpn"] = &sempv2.SEMPError{Operation: "getMsgVpn", StatusCode: 503}

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
		t.Fatalf("must not resurface the stale, ambiguous owner-check NOT_FOUND when disambiguation itself failed: %v", err)
	}
	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) || sempErr.Operation != "getMsgVpn" {
		t.Fatalf("expected the error to trace back to the failed getMsgVpn disambiguation check, got %v", err)
	}
	for _, call := range client.calls {
		if call == "createMsgVpnQueue" {
			t.Fatalf("queue must never be created when disambiguation itself failed; calls = %v", client.calls)
		}
	}
}

// TestOwnerValidatingHandler_GhostOwner_RejectsForEveryRegisteredTool iterates
// the real ownerValidatedTools map — not a hand-picked copy of it — so a
// copy-paste mistake in one entry (e.g. update-queue's configParam
// accidentally set to "topicEndpointConfig") is caught here: this test's
// expected param shape per tool is independent ground truth, taken directly
// from tools.yaml's own parameter names, not derived from the map under
// test.
func TestOwnerValidatingHandler_GhostOwner_RejectsForEveryRegisteredTool(t *testing.T) {
	cases := []struct {
		tool        string
		idParam     string
		idValue     string
		configParam string
		writeOp     string
	}{
		{tool: "create-queue", idParam: "queueName", idValue: "q1", configParam: "queueConfig", writeOp: "createMsgVpnQueue"},
		{tool: "update-queue", idParam: "queueName", idValue: "q1", configParam: "queueConfig", writeOp: "updateMsgVpnQueue"},
		{tool: "create-topic-endpoint", idParam: "topicEndpointName", idValue: "te1", configParam: "topicEndpointConfig", writeOp: "createMsgVpnTopicEndpoint"},
		{tool: "update-topic-endpoint", idParam: "topicEndpointName", idValue: "te1", configParam: "topicEndpointConfig", writeOp: "updateMsgVpnTopicEndpoint"},
	}
	// Every case above must correspond to a real ownerValidatedTools entry;
	// this catches a case silently going stale (e.g. a renamed tool) just as
	// much as it catches ownerValidatedTools itself drifting.
	if len(cases) != len(ownerValidatedTools) {
		t.Fatalf("this table has %d cases but ownerValidatedTools has %d entries — keep them in exact sync", len(cases), len(ownerValidatedTools))
	}

	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			handler, client := realOwnerValidationFixture(t, c.tool)
			client.errors["getMsgVpnClientUsername"] = notFoundError("getMsgVpnClientUsername")
			client.responses["getMsgVpn"] = vpnExistsResponse()
			// A generic success in case the ghost owner is wrongly accepted —
			// makes the failure mode "write happened" rather than "mock had no
			// canned response", which would fail for the wrong reason.
			client.responses[c.writeOp] = &sempv2.Result{Data: map[string]any{}, StatusCode: 200}

			params := map[string]any{
				"msgVpnName":  "default",
				c.idParam:     c.idValue,
				c.configParam: map[string]any{"owner": "ghost-user", "permission": "consume"},
			}
			_, err := handler.Handle(context.Background(), &ToolContext{SEMPv2Client: client}, params)
			var ownerErr *ownerNotFoundError
			if !errors.As(err, &ownerErr) {
				t.Fatalf("%s: expected *ownerNotFoundError for a nonexistent owner, got %T: %v", c.tool, err, err)
			}
			for _, call := range client.calls {
				if call == c.writeOp {
					t.Fatalf("%s: %s must never be called when owner does not exist; calls = %v", c.tool, c.writeOp, client.calls)
				}
			}
		})
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
