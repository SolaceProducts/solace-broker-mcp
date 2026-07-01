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

// Package queueactions implements the per-action queue MCP tools that issue
// SEMPv2 action-API PUT requests against a queue:
//
//	delete-queue-messages → PUT .../action/msgVpns/{msgVpnName}/queues/{queueName}/deleteMsgs
//	clear-queue-stats      → PUT .../action/msgVpns/{msgVpnName}/queues/{queueName}/clearStats
//
// One tool per action (rather than a single tool with an `action` parameter)
// so each tool's annotations are accurate — delete-queue-messages is
// destructive, clear-queue-stats is not — and each description is
// unconditional. The manager logs a WARNING only for the destructive tool
// (Annotations.Destructive == true); the tool name itself identifies the action.
//
// Naming convention: action-API tools use <verb>-<resource>-<object>
// (delete-queue-messages, clear-queue-stats); config-API CRUD tools use
// manage-<resource> (manage-queue, manage-vpn). The two families are distinct
// on purpose — see docs/user-guide.md.
package queueactions

import (
	"context"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

const (
	deleteMessagesToolName = "delete-queue-messages"
	clearStatsToolName     = "clear-queue-stats"

	actionDeleteMsgs = "deleteMsgs"
	actionClearStats = "clearStats"
)

// inputSchema is the shared parameter schema: a queue identified by VPN and
// name. minLength:1 rejects empty path segments upstream.
func inputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "The Message VPN containing the queue.",
			},
			"queueName": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "The name of the queue to act on.",
			},
		},
		"required": []string{"msgVpnName", "queueName"},
	}
}

// outputSchema is the strict success envelope: exactly status, msgVpnName,
// queueName. additionalProperties:false rejects any leaked field.
func outputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":     map[string]any{"type": "string", "enum": []string{"ok"}},
			"msgVpnName": map[string]any{"type": "string"},
			"queueName":  map[string]any{"type": "string"},
		},
		"required":             []string{"status", "msgVpnName", "queueName"},
		"additionalProperties": false,
	}
}

// buildOperation constructs the sempv2.Operation for the requested action.
// Both queue actions share method, base path, and parameter shape; only the
// URL suffix differs. The operation ID follows SEMPv2's do<Resource><Action>
// convention so it lines up with broker logs and SEMPError.Operation strings.
func buildOperation(action string) *sempv2.Operation {
	return &sempv2.Operation{
		ID:     "doMsgVpnQueue" + capitalize(action),
		Method: "PUT",
		Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/" + action,
		Parameters: []sempv2.Parameter{
			{Name: "msgVpnName", In: "path", Type: "string", Required: true},
			{Name: "queueName", In: "path", Type: "string", Required: true},
			{Name: "body", In: "body", Type: "object", Required: true},
		},
	}
}

// run validates parameters and issues the SEMPv2 action request. The schema's
// minLength:1 already rejects empty values upstream; the empty-string checks
// are defense in depth for direct Go callers that bypass the manager, so a
// malformed request is never sent to the broker.
func run(ctx context.Context, tc *tools.ToolContext, toolName, action string, params map[string]any) (*tools.ToolResult, error) {
	msgVpnName, _ := params["msgVpnName"].(string)
	queueName, _ := params["queueName"].(string)

	if msgVpnName == "" {
		return nil, fmt.Errorf("%s: msgVpnName is required", toolName)
	}
	if queueName == "" {
		return nil, fmt.Errorf("%s: queueName is required", toolName)
	}

	args := map[string]any{
		"msgVpnName": msgVpnName,
		"queueName":  queueName,
		"body":       map[string]any{},
	}
	if _, err := tc.SEMPv2Client.Execute(ctx, buildOperation(action), args); err != nil {
		return nil, err
	}

	return &tools.ToolResult{
		StructuredContent: map[string]any{
			"status":     "ok",
			"msgVpnName": msgVpnName,
			"queueName":  queueName,
		},
	}, nil
}

// capitalize upper-cases the first ASCII letter — maps the camelCase action
// constant to the PascalCase SEMPv2 operationId suffix.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 32
	}
	return string(b)
}

func ptr(b bool) *bool { return &b }

// ---- delete-queue-messages (destructive) ------------------------------------

var _ tools.ToolHandler = (*DeleteMessagesHandler)(nil)

// DeleteMessagesHandler implements the delete-queue-messages MCP tool.
type DeleteMessagesHandler struct{}

// NewDeleteMessagesHandler returns a delete-queue-messages handler ready to
// register with a ToolManager.
func NewDeleteMessagesHandler() *DeleteMessagesHandler { return &DeleteMessagesHandler{} }

// Metadata describes the tool. It is destructive — the manager logs a WARNING
// on every invocation — and its description is the operative confirmation
// control: unconditional, since this tool only ever deletes messages.
func (h *DeleteMessagesHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: deleteMessagesToolName,
		Description: "Permanently delete ALL spooled messages from a queue. This " +
			"is irreversible — the deleted messages cannot be recovered. BEFORE " +
			"INVOKING THIS TOOL, obtain explicit user confirmation, restating the " +
			"target (broker, VPN, queue) and that all spooled messages will be " +
			"permanently lost. The confirmation MUST come as a separate user reply " +
			"received AFTER you state the target and effect — do not treat the " +
			"user's original request as a pre-confirmation, even if it implies " +
			"intent. Use after confirmed intent to drain a queue (e.g. clearing a " +
			"dead-letter backlog).",
		InputSchema:  inputSchema(),
		OutputSchema: outputSchema(),
		Annotations:  tools.Annotations{ReadOnly: false, Destructive: ptr(true), Idempotent: false},
	}
}

// Handle issues the deleteMsgs action.
func (h *DeleteMessagesHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	return run(ctx, tc, deleteMessagesToolName, actionDeleteMsgs, params)
}

// ---- clear-queue-stats (non-destructive) ------------------------------------

var _ tools.ToolHandler = (*ClearStatsHandler)(nil)

// ClearStatsHandler implements the clear-queue-stats MCP tool.
type ClearStatsHandler struct{}

// NewClearStatsHandler returns a clear-queue-stats handler ready to register
// with a ToolManager.
func NewClearStatsHandler() *ClearStatsHandler { return &ClearStatsHandler{} }

// Metadata describes the tool. It is a write (it mutates broker state, so it is
// gated behind enable_write_tools) but non-destructive, so the manager does not
// log a destructive WARNING and the description carries no confirmation
// instruction — resetting counters needs no operator sign-off.
func (h *ClearStatsHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: clearStatsToolName,
		Description: "Reset the statistics counters for a queue. Non-destructive: " +
			"this resets monitoring counters only and does not affect spooled " +
			"messages or message delivery. Useful for establishing a fresh metrics " +
			"baseline during testing.",
		InputSchema:  inputSchema(),
		OutputSchema: outputSchema(),
		Annotations:  tools.Annotations{ReadOnly: false, Destructive: ptr(false), Idempotent: true},
	}
}

// Handle issues the clearStats action.
func (h *ClearStatsHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	return run(ctx, tc, clearStatsToolName, actionClearStats, params)
}
