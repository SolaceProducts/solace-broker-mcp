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

// Package clientactions implements the per-action client MCP tools that issue
// SEMPv2 action-API PUT requests against a connected client:
//
//	disconnect-client  → PUT .../action/msgVpns/{msgVpnName}/clients/{clientName}/disconnect
//	clear-client-stats → PUT .../action/msgVpns/{msgVpnName}/clients/{clientName}/clearStats
//
// One tool per action so each tool's annotations are accurate —
// disconnect-client is destructive (service-impacting), clear-client-stats is
// not — and each description is unconditional. The manager logs a WARNING only
// for the destructive tool; the tool name itself identifies the action. See the
// queueactions package doc for the naming convention.
package clientactions

import (
	"context"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

const (
	disconnectToolName = "disconnect-client"
	clearStatsToolName = "clear-client-stats"

	actionDisconnect = "disconnect"
	actionClearStats = "clearStats"
)

// inputSchema is the shared parameter schema: a client identified by VPN and
// name. minLength:1 rejects empty path segments upstream.
func inputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "The Message VPN the client is connected to.",
			},
			"clientName": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "The name of the client connection to act on.",
			},
		},
		"required": []string{"msgVpnName", "clientName"},
	}
}

// outputSchema is the strict success envelope: exactly status, msgVpnName,
// clientName. additionalProperties:false rejects any leaked field.
func outputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":     map[string]any{"type": "string", "enum": []string{"ok"}},
			"msgVpnName": map[string]any{"type": "string"},
			"clientName": map[string]any{"type": "string"},
		},
		"required":             []string{"status", "msgVpnName", "clientName"},
		"additionalProperties": false,
	}
}

// buildOperation constructs the sempv2.Operation for the requested action.
// Both client actions share method, base path, and parameter shape; only the
// URL suffix differs. The operation ID follows SEMPv2's do<Resource><Action>
// convention so it lines up with broker logs and SEMPError.Operation strings.
func buildOperation(action string) *sempv2.Operation {
	return &sempv2.Operation{
		ID:     "doMsgVpnClient" + capitalize(action),
		Method: "PUT",
		Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/clients/{clientName}/" + action,
		Parameters: []sempv2.Parameter{
			{Name: "msgVpnName", In: "path", Type: "string", Required: true},
			{Name: "clientName", In: "path", Type: "string", Required: true},
			{Name: "body", In: "body", Type: "object", Required: true},
		},
	}
}

// run validates parameters and issues the SEMPv2 action request. The empty-
// string checks are defense in depth for direct Go callers that bypass the
// manager's minLength:1 validation, so a malformed request is never sent.
func run(ctx context.Context, tc *tools.ToolContext, toolName, action string, params map[string]any) (*tools.ToolResult, error) {
	msgVpnName, _ := params["msgVpnName"].(string)
	clientName, _ := params["clientName"].(string)

	if msgVpnName == "" {
		return nil, fmt.Errorf("%s: msgVpnName is required", toolName)
	}
	if clientName == "" {
		return nil, fmt.Errorf("%s: clientName is required", toolName)
	}

	args := map[string]any{
		"msgVpnName": msgVpnName,
		"clientName": clientName,
		"body":       map[string]any{},
	}
	if _, err := tc.SEMPv2Client.Execute(ctx, buildOperation(action), args); err != nil {
		return nil, err
	}

	return &tools.ToolResult{
		StructuredContent: map[string]any{
			"status":     "ok",
			"msgVpnName": msgVpnName,
			"clientName": clientName,
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

// ---- disconnect-client (destructive) ----------------------------------------

var _ tools.ToolHandler = (*DisconnectHandler)(nil)

// DisconnectHandler implements the disconnect-client MCP tool.
type DisconnectHandler struct{}

// NewDisconnectHandler returns a disconnect-client handler ready to register
// with a ToolManager.
func NewDisconnectHandler() *DisconnectHandler { return &DisconnectHandler{} }

// Metadata describes the tool. It is destructive (service-impacting) — the
// manager logs a WARNING on every invocation — and its description is the
// operative confirmation control: unconditional, since this tool only ever
// disconnects.
func (h *DisconnectHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: disconnectToolName,
		Description: "Forcibly disconnect a connected client. This is service-" +
			"impacting and irreversible for the active session — it terminates the " +
			"connection and the client must reconnect to resume. BEFORE INVOKING " +
			"THIS TOOL, obtain explicit user confirmation, restating the target " +
			"(broker, VPN, client name) and the service interruption. The " +
			"confirmation MUST come as a separate user reply received AFTER you " +
			"state the target and effect — do not treat the user's original request " +
			"as a pre-confirmation, even if it implies intent. Common use: " +
			"disconnect a slow subscriber identified via list-slow-subscribers or " +
			"get-client-details.",
		InputSchema:  inputSchema(),
		OutputSchema: outputSchema(),
		Annotations:  tools.Annotations{ReadOnly: false, Destructive: ptr(true), Idempotent: false},
	}
}

// Handle issues the disconnect action.
func (h *DisconnectHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	return run(ctx, tc, disconnectToolName, actionDisconnect, params)
}

// ---- clear-client-stats (non-destructive) -----------------------------------

var _ tools.ToolHandler = (*ClearStatsHandler)(nil)

// ClearStatsHandler implements the clear-client-stats MCP tool.
type ClearStatsHandler struct{}

// NewClearStatsHandler returns a clear-client-stats handler ready to register
// with a ToolManager.
func NewClearStatsHandler() *ClearStatsHandler { return &ClearStatsHandler{} }

// Metadata describes the tool. It is a write (gated behind enable_write_tools)
// but non-destructive, so the manager does not log a destructive WARNING and
// the description carries no confirmation instruction.
func (h *ClearStatsHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name: clearStatsToolName,
		Description: "Reset the per-connection statistics counters for a client. " +
			"Non-destructive: this resets monitoring counters only and does not " +
			"disconnect the client or affect message delivery.",
		InputSchema:  inputSchema(),
		OutputSchema: outputSchema(),
		Annotations:  tools.Annotations{ReadOnly: false, Destructive: ptr(false), Idempotent: true},
	}
}

// Handle issues the clearStats action.
func (h *ClearStatsHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	return run(ctx, tc, clearStatsToolName, actionClearStats, params)
}
