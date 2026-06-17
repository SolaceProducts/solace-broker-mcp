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

// Package clientaction implements the execute-client-action MCP tool.
//
// The tool executes SEMPv2 action endpoints on a connected client:
//
//	disconnect → PUT /SEMP/v2/action/msgVpns/{msgVpnName}/clients/{clientName}/disconnect
//	clearStats → PUT /SEMP/v2/action/msgVpns/{msgVpnName}/clients/{clientName}/clearStats
//
// Both are PUT requests with an empty JSON body.
//
// Confirmation: see the parallel doc in the queueaction package — the tool
// description is the operative control. The DestructiveHint annotation is
// set; the manager-side WARNING log fires automatically on every invocation.
package clientaction

import (
	"context"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

const toolName = "execute-client-action"

// Valid action values. Mirrors the queueaction defense-in-depth pattern.
const (
	actionDisconnect = "disconnect"
	actionClearStats = "clearStats"
)

// outputSchema describes the success-response envelope. Strict: exactly the
// four fields we set, with additionalProperties: false.
func outputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":     map[string]any{"type": "string", "enum": []string{"ok"}},
			"action":     map[string]any{"type": "string"},
			"msgVpnName": map[string]any{"type": "string"},
			"clientName": map[string]any{"type": "string"},
		},
		"required":             []string{"status", "action", "msgVpnName", "clientName"},
		"additionalProperties": false,
	}
}

var _ tools.ToolHandler = (*Handler)(nil)

// Handler implements the execute-client-action MCP tool.
type Handler struct{}

// NewHandler returns a client-action tool handler ready to register with a
// ToolManager.
func NewHandler() *Handler { return &Handler{} }

// Metadata describes the tool to the MCP layer. Destructive: true at the
// tool level because the disconnect action is service-impacting; the per-
// action distinction lives in the description text.
func (h *Handler) Metadata() tools.Metadata {
	destructive := true
	return tools.Metadata{
		Name: toolName,
		Description: "Execute an action on a connected client: disconnect " +
			"(service-impacting — forces an immediate session termination; " +
			"the client must reconnect to resume) or clearStats (resets " +
			"the client's per-connection statistics counters; " +
			"non-destructive). BEFORE INVOKING THIS TOOL, obtain explicit " +
			"user confirmation, restating the target (broker, VPN, client " +
			"name) and the effect — for disconnect, name the service " +
			"interruption explicitly. The confirmation MUST come as a " +
			"separate user reply received AFTER you state the target and " +
			"effect — do not treat the user's original request as a " +
			"pre-confirmation, even if it implies intent. Common operator " +
			"use: disconnect a slow subscriber identified via " +
			"list-slow-subscribers or get-client-details.",
		InputSchema: map[string]any{
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
				"action": map[string]any{
					"type": "string",
					"enum": []string{actionDisconnect, actionClearStats},
					"description": "The action to execute. disconnect " +
						"terminates the client's session (service-" +
						"impacting); clearStats resets statistics " +
						"counters (non-destructive).",
				},
			},
			"required": []string{"msgVpnName", "clientName", "action"},
		},
		OutputSchema: outputSchema(),
		Annotations: tools.Annotations{
			ReadOnly:    false,
			Destructive: &destructive,
			Idempotent:  false,
		},
	}
}

// Handle dispatches to the SEMPv2 action endpoint matching the requested
// action. The schema's enum and minLength:1 already reject empty/unknown
// values upstream; these runtime checks are defense in depth for callers
// that bypass the manager, so a malformed action request is never sent to
// the broker.
func (h *Handler) Handle(
	ctx context.Context,
	tc *tools.ToolContext,
	params map[string]any,
) (*tools.ToolResult, error) {
	msgVpnName, _ := params["msgVpnName"].(string)
	clientName, _ := params["clientName"].(string)
	action, _ := params["action"].(string)

	if msgVpnName == "" {
		return nil, fmt.Errorf("%s: msgVpnName is required", toolName)
	}
	if clientName == "" {
		return nil, fmt.Errorf("%s: clientName is required", toolName)
	}
	if action != actionDisconnect && action != actionClearStats {
		return nil, fmt.Errorf("%s: unsupported action %q (must be %q or %q)",
			toolName, action, actionDisconnect, actionClearStats)
	}

	op := buildOperation(action)
	args := map[string]any{
		"msgVpnName": msgVpnName,
		"clientName": clientName,
		"body":       map[string]any{},
	}

	if _, err := tc.SEMPv2Client.Execute(ctx, op, args); err != nil {
		return nil, err
	}

	return &tools.ToolResult{
		StructuredContent: map[string]any{
			"status":     "ok",
			"action":     action,
			"msgVpnName": msgVpnName,
			"clientName": clientName,
		},
	}, nil
}

// buildOperation constructs the sempv2.Operation for the requested action.
// Operation ID follows SEMPv2's do<Resource><Action> convention so it aligns
// with broker logs and SEMPError.Operation strings.
func buildOperation(action string) *sempv2.Operation {
	id := "doMsgVpnClient" + capitalize(action)
	return &sempv2.Operation{
		ID:     id,
		Method: "PUT",
		Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/clients/{clientName}/" + action,
		Parameters: []sempv2.Parameter{
			{Name: "msgVpnName", In: "path", Type: "string", Required: true},
			{Name: "clientName", In: "path", Type: "string", Required: true},
			{Name: "body", In: "body", Type: "object", Required: true},
		},
	}
}

// capitalize returns s with its first ASCII letter upper-cased. Used to map
// action constants (camelCase) to the SEMPv2 operationId suffix (PascalCase).
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
