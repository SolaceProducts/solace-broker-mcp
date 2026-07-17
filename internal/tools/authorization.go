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

// authzDeniedMessage is the caller-facing text returned when the compiled
// policy denies a tool invocation. Uniform, minimum-information — no tool
// name, no group names, no reason code — so an unauthorized caller learns
// nothing about the policy shape. Full diagnostic detail lives in the
// server-side authorization-decision log, joined to this response by the
// correlation ID that flows through the outer withRecovery wrapper.
const authzDeniedMessage = "You are not authorized to use this tool."

// authzMissingClaimMessage is the caller-facing text returned when the
// caller's token does not carry the configured groups claim. Value is
// intentionally identical to authzDeniedMessage: the caller cannot fix
// either case themselves and any distinguishing detail (which claim was
// expected, whether the token was structurally deficient vs merely
// unprivileged) is admin-configuration metadata that must not reach an
// unauthenticated-authorization caller. The two constants stay separate
// identifiers so a future evolution can amend one without touching the
// other; the audit log distinguishes the outcomes structurally.
const authzMissingClaimMessage = "You are not authorized to use this tool."

// listBrokersToolName is the exempt discovery tool. Named here (rather
// than hardcoding the literal at ValidatePolicyToolNames' use site) so
// the exemption is visible to a grep for the constant.
const listBrokersToolName = "list-brokers"

// withAuthorization gates every invocation of toolName through policy.
//
// On allow: emits an INFO-level "tool authorization" audit event, then
// dispatches to next unchanged. The wrapper does not mutate ctx or req;
// dispatch is byte-identical to the pre-RBAC path after the audit line.
//
// On deny or missing-claim: emits the audit event, then returns a
// tool-level error result (IsError: true, retryable: false) carrying the
// uniform authzDeniedMessage / authzMissingClaimMessage. The result mirrors
// withRecovery's panic branch shape so reviewers see one denial shape
// across authorization, panic recovery, and validation errors.
//
// Nil policy is a precondition violation: the composition site
// (RegisterWithServer) guarantees this wrapper is only composed when
// policy is non-nil. A nil policy reaching this call is a programmer
// error, not a runtime condition — the wrapper panics, the outer
// withRecovery catches it, and the request fails visibly rather than
// silently bypassing authorization.
//
// Panics from authz.Authorize or the audit call site are not recovered
// here — the outer withRecovery wrapper owns panic containment, so this
// wrapper reads as straight-line request logic.
//
// The audit event emitted here is a v1 placeholder: INFO level for every
// outcome, minimal field set (identity, tool, decision string). A
// follow-up ticket refines the level split, adds the bounded matched-
// groups slice, and finalizes the audit field schema.
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

// authzErrorResult builds the tool-level error result returned on deny
// and missing-claim branches. Shape mirrors withRecovery's panic branch:
// IsError true, StructuredContent with the uniform message and
// retryable=false, plus a TextContent copy of the message.
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

// ValidatePolicyToolNames verifies every tool name the admin wrote in
// cfg.AccessLevelGroups resolves to a registered handler on mgr, or to
// the structurally exempt "list-brokers" tool. Called from cmd/server/main.go
// after RegisterWithServer and RegisterListBrokers return, before
// server.Run — the union of mgr.Handlers() and "list-brokers" must be
// fully populated by then.
//
// Unknown tool names are collected across every group and returned as
// one error per unknown tool, alphabetized, joined via errors.Join.
// Format: `unknown tool "<name>" (referenced by groups: <g1>, <g2>, ...)`.
// Deduplication is on the tool name (one bug, one report), but each row
// carries the alphabetized list of every group that referenced it so the
// admin's YAML edit is a self-contained fix task.
//
// Grants of "list-brokers" are known-tool grants (the union accepts the
// literal), but the tool is structurally exempt from tool authorization —
// the grant has no effect. Each such grant is reported via slog.Warn with
// the referencing group list; they do not surface as errors and do not
// block startup.
//
// Returns nil when every configured tool name resolves.
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

// sortedKeys returns the keys of set in alphabetical order. Small helper
// used by ValidatePolicyToolNames to produce deterministic error rows.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
