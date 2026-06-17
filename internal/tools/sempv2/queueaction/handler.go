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

// Package queueaction implements the execute-queue-action MCP tool.
//
// The tool executes SEMPv2 action endpoints on a queue:
//
//	deleteMsgs → PUT /SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/deleteMsgs
//	clearStats → PUT /SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/clearStats
//
// Both are PUT requests with an empty JSON body.
//
// Confirmation: the tool description instructs the calling LLM to obtain
// explicit user confirmation before invocation. The DestructiveHint MCP
// annotation is also set, but the description text is the operative control.
//
// Destructive WARNING logging is handled centrally by the tool manager for
// any tool whose Annotations.Destructive is true; this package does not log.
package queueaction

import (
	"context"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

const toolName = "execute-queue-action"

// Valid action values. The input schema's enum rejects anything else upstream;
// the constants are also used as a defense-in-depth runtime check in Handle so
// the tool never issues a SEMP request with a caller-supplied action string.
const (
	actionDeleteMsgs = "deleteMsgs"
	actionClearStats = "clearStats"
)

// outputSchema describes the success-response envelope. Strict: exactly the
// four fields we set, with additionalProperties: false to reject any leaked
// debug or unintended field.
func outputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":     map[string]any{"type": "string", "enum": []string{"ok"}},
			"action":     map[string]any{"type": "string"},
			"msgVpnName": map[string]any{"type": "string"},
			"queueName":  map[string]any{"type": "string"},
		},
		"required":             []string{"status", "action", "msgVpnName", "queueName"},
		"additionalProperties": false,
	}
}

var _ tools.ToolHandler = (*Handler)(nil)

// Handler implements the execute-queue-action MCP tool.
type Handler struct{}

// NewHandler returns a queue-action tool handler ready to register with a
// ToolManager.
func NewHandler() *Handler { return &Handler{} }

// Metadata describes the tool to the MCP layer. The tool is marked
// Destructive: true at the tool level (worst-case stance — the deleteMsgs
// action is destructive and we don't vary annotations per parameter value),
// so the manager logs a WARNING on every invocation and SDK clients that
// honor DestructiveHint apply a confirmation flow. The per-action distinction
// (irreversible vs non-destructive) lives in the description text.
func (h *Handler) Metadata() tools.Metadata {
	destructive := true
	return tools.Metadata{
		Name: toolName,
		Description: "Execute an action on a queue: deleteMsgs (irreversible " +
			"— permanently deletes ALL spooled messages in the queue) or " +
			"clearStats (resets the queue's statistics counters; " +
			"non-destructive). BEFORE INVOKING THIS TOOL, obtain explicit " +
			"user confirmation, restating the target (broker, VPN, queue) " +
			"and the effect — for deleteMsgs, name the irreversible " +
			"message loss explicitly. The confirmation MUST come as a " +
			"separate user reply received AFTER you state the target and " +
			"effect — do not treat the user's original request as a " +
			"pre-confirmation, even if it implies intent. Use after the " +
			"user has confirmed they want to drain a queue (e.g., clearing " +
			"a dead-letter backlog or resetting statistics during testing).",
		InputSchema: map[string]any{
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
				"action": map[string]any{
					"type": "string",
					"enum": []string{actionDeleteMsgs, actionClearStats},
					"description": "The action to execute. deleteMsgs deletes " +
						"all spooled messages (irreversible); clearStats " +
						"resets statistics counters (non-destructive).",
				},
			},
			"required": []string{"msgVpnName", "queueName", "action"},
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
// values upstream; these runtime checks are defense in depth for direct Go
// callers that bypass the manager's validation, so a malformed action
// request is never sent to the broker.
func (h *Handler) Handle(
	ctx context.Context,
	tc *tools.ToolContext,
	params map[string]any,
) (*tools.ToolResult, error) {
	msgVpnName, _ := params["msgVpnName"].(string)
	queueName, _ := params["queueName"].(string)
	action, _ := params["action"].(string)

	if msgVpnName == "" {
		return nil, fmt.Errorf("%s: msgVpnName is required", toolName)
	}
	if queueName == "" {
		return nil, fmt.Errorf("%s: queueName is required", toolName)
	}
	if action != actionDeleteMsgs && action != actionClearStats {
		return nil, fmt.Errorf("%s: unsupported action %q (must be %q or %q)",
			toolName, action, actionDeleteMsgs, actionClearStats)
	}

	op := buildOperation(action)
	args := map[string]any{
		"msgVpnName": msgVpnName,
		"queueName":  queueName,
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
			"queueName":  queueName,
		},
	}, nil
}

// buildOperation constructs the sempv2.Operation for the requested action.
// Both actions share method, base path, and parameter shape; only the URL
// suffix differs. The operation ID follows SEMPv2's "do<Resource><Action>"
// convention so it lines up with broker logs and SEMPError.Operation strings.
func buildOperation(action string) *sempv2.Operation {
	id := "doMsgVpnQueue" + capitalize(action)
	return &sempv2.Operation{
		ID:     id,
		Method: "PUT",
		Path:   "/SEMP/v2/action/msgVpns/{msgVpnName}/queues/{queueName}/" + action,
		Parameters: []sempv2.Parameter{
			{Name: "msgVpnName", In: "path", Type: "string", Required: true},
			{Name: "queueName", In: "path", Type: "string", Required: true},
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
