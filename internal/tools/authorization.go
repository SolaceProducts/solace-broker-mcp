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
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/SolaceDev/solace-broker-mcp/internal/authz"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Caller-facing messages. Deliberately identical in v1 so a caller cannot
// distinguish "your groups don't grant this" from "your token has no groups
// claim" — that distinction is admin-configuration metadata. Kept as separate
// identifiers so a future evolution can amend one without touching the other;
// the audit log distinguishes the outcomes via the decision field.
const (
	authzDeniedMessage       = "You are not authorized to use this tool."
	authzMissingClaimMessage = "You are not authorized to use this tool."
)

// listBrokersToolName is the discovery tool that is structurally exempt from
// tool authorization. Exemption lives at the registration API
// (RegisterListBrokers takes no policy argument), not here — this constant
// exists so a grep finds every exempt-name touchpoint at once.
const listBrokersToolName = "list-brokers"

// withAuthorization gates every invocation of toolName through policy.
// On allow, dispatches to next unchanged. On deny or missing-claim, returns a
// tool-level error result and skips next.
//
// Precondition: policy must be non-nil. The composition site
// (RegisterWithServer) guarantees this by only composing the wrapper on the
// non-nil-policy branch. A nil policy reaching here is a programmer error
// that must not silently bypass authorization; the outer withRecovery catches
// the resulting panic.
//
// Panic containment (from Authorize or the audit call site) is owned by the
// outer withRecovery, so this closure reads as straight-line request logic.
//
// The audit event is a v1 placeholder — INFO for every outcome, minimal
// fields. A follow-up ticket refines the level split and field schema.
func withAuthorization(policy *authz.Policy, toolName string, next mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var info *sdkauth.TokenInfo
		if req.Extra != nil {
			info = req.Extra.TokenInfo
		}
		id := NewIdentityFromTokenInfo(info)

		groups, present := requestGroups(req)
		if !present {
			slog.LogAttrs(ctx, slog.LevelInfo, "tool authorization",
				slog.String("tool", toolName),
				slog.String("decision", "denied_missing_claim"),
				slog.Any("", id))
			return authzErrorResult(authzMissingClaimMessage), nil
		}

		decision := policy.Authorize(groups, toolName)
		if !decision.Allowed {
			slog.LogAttrs(ctx, slog.LevelInfo, "tool authorization",
				slog.String("tool", toolName),
				slog.String("decision", "denied"),
				slog.Any("", id))
			return authzErrorResult(authzDeniedMessage), nil
		}

		slog.LogAttrs(ctx, slog.LevelInfo, "tool authorization",
			slog.String("tool", toolName),
			slog.String("decision", "allowed"),
			slog.Any("", id))
		return next(ctx, req)
	}
}

// authzErrorResult builds the deny / missing-claim tool-level error result.
// Shape mirrors withRecovery's panic branch so callers see one denial shape
// across authorization, panic recovery, and validation errors.
func authzErrorResult(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		StructuredContent: map[string]any{
			"error":     message,
			"retryable": false,
		},
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
		IsError: true,
	}
}

// ValidatePolicyToolNames checks every tool name in cfg.AccessLevelGroups
// against the union of mgr.Handlers() and list-brokers. Call after both
// registrations have populated mgr.
//
// Returns one error row per unknown tool (deduped on the tool name so one
// typo is one report), alphabetized by tool then by referencing group, joined
// via errors.Join. Grants of list-brokers are inert — emitted as a WARN, not
// an error — because the tool is exempt from authorization.
func ValidatePolicyToolNames(cfg config.ToolAuthorizationConfig, mgr *ToolManager) error {
	known := make(map[string]struct{})
	for _, h := range mgr.Handlers() {
		known[h.Metadata().Name] = struct{}{}
	}
	known[listBrokersToolName] = struct{}{}

	unknownToGroups := make(map[string]map[string]struct{})
	exemptToGroups := make(map[string]struct{})

	for groupName, tools := range cfg.AccessLevelGroups {
		for _, tool := range tools {
			if tool == listBrokersToolName {
				exemptToGroups[groupName] = struct{}{}
				continue
			}
			if _, ok := known[tool]; ok {
				continue
			}
			groupsForTool, ok := unknownToGroups[tool]
			if !ok {
				groupsForTool = make(map[string]struct{})
				unknownToGroups[tool] = groupsForTool
			}
			groupsForTool[groupName] = struct{}{}
		}
	}

	if len(exemptToGroups) > 0 {
		sortedExempt := sortedKeys(exemptToGroups)
		slog.Warn("tool authorization grant has no effect; tool is exempt and always available to authenticated callers",
			slog.String("exempt_tool", listBrokersToolName),
			slog.String("referenced_by_groups", strings.Join(sortedExempt, ", ")))
	}

	if len(unknownToGroups) == 0 {
		return nil
	}

	sortedTools := make([]string, 0, len(unknownToGroups))
	for tool := range unknownToGroups {
		sortedTools = append(sortedTools, tool)
	}
	sort.Strings(sortedTools)

	rowErrs := make([]error, 0, len(sortedTools))
	for _, tool := range sortedTools {
		sortedGroups := sortedKeys(unknownToGroups[tool])
		rowErrs = append(rowErrs, fmt.Errorf(
			"unknown tool %q (referenced by groups: %s)",
			tool,
			strings.Join(sortedGroups, ", "),
		))
	}
	return errors.Join(rowErrs...)
}

// sortedKeys returns the keys of set in alphabetical order.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
