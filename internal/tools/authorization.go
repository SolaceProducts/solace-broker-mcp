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
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/logging/sanitize"
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

// matchedGroupsBound caps how many matched group names the audit event
// carries on allow. 32 covers the largest realistic caller-group membership
// without truncation while keeping single log records readable. When a
// caller has more than 32 matching groups the surplus is dropped from the
// slice but the true count remains in matched_groups_total and
// matched_groups_truncated goes true.
const matchedGroupsBound = 32

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
// Audit event: one "tool authorization" slog record per invocation, allow or
// deny. Correlation ID is stamped by the correlation slog handler from ctx;
// the call site does not add it manually. Level split: INFO on allow, WARN
// on both deny outcomes (deny is not an error but is a notable event; a
// missing groups claim is a configuration event, not a system failure).
// Decision axis is a two-field split (decision + decision_reason) so
// operators can filter deny clusters without conflating the "not permitted"
// case with the "misconfigured token" case.
//
// configuredGroupsClaimName is the admin-authored JWT claim name the
// resolver was told to look for; the missing-claim event names it under
// expected_claim so operators reading the log can jump straight to the IdP
// side. It passes through sanitize.Claim before emission because it is an
// admin string reaching a log record.
func withAuthorization(policy *authz.Policy, toolName string, configuredGroupsClaimName string, next mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var info *sdkauth.TokenInfo
		if req.Extra != nil {
			info = req.Extra.TokenInfo
		}
		id := NewIdentityFromTokenInfo(info)

		groups, present := requestGroups(req)
		if !present {
			// Missing-claim: no matched_groups fields on this outcome — the
			// caller had no groups slice at all, so emitting empty ones would
			// be dead noise. expected_claim carries the diagnostic instead.
			slog.LogAttrs(ctx, slog.LevelWarn, "tool authorization",
				slog.String("tool", toolName),
				slog.String("decision", "denied"),
				slog.String("decision_reason", "missing_claim"),
				slog.String("expected_claim", sanitize.Claim(configuredGroupsClaimName)),
				slog.Any("", id))
			return authzErrorResult(authzMissingClaimMessage), nil
		}

		decision := policy.Authorize(groups, toolName)
		if !decision.Allowed {
			// Deny: matched_groups is always [] and total is always 0 by
			// construction (a match would have flipped Allowed). Keeping the
			// same three fields on deny as on allow gives SIEM parsers a
			// uniform schema across both outcomes; the truncation bool is
			// necessarily false. The caller's full extracted groups list is
			// deliberately NOT logged here — see PR description.
			slog.LogAttrs(ctx, slog.LevelWarn, "tool authorization",
				slog.String("tool", toolName),
				slog.String("decision", "denied"),
				slog.String("decision_reason", "not_permitted"),
				slog.Any("matched_groups", []string{}),
				slog.Int("matched_groups_total", 0),
				slog.Bool("matched_groups_truncated", false),
				slog.Any("", id))
			return authzErrorResult(authzDeniedMessage), nil
		}

		bounded, total, truncated := boundMatchedGroups(decision.MatchedGroups)
		slog.LogAttrs(ctx, slog.LevelInfo, "tool authorization",
			slog.String("tool", toolName),
			slog.String("decision", "allowed"),
			slog.Any("matched_groups", bounded),
			slog.Int("matched_groups_total", total),
			slog.Bool("matched_groups_truncated", truncated),
			slog.Any("", id))
		return next(ctx, req)
	}
}

// boundMatchedGroups sanitizes each element of matched and applies the
// matchedGroupsBound cap, preserving the order Authorize returned. Returns
// the bounded slice, the true count before capping, and whether truncation
// fired. A nil or empty input produces an empty (non-nil) slice so the slog
// attribute renders as [] rather than null.
func boundMatchedGroups(matched []string) (bounded []string, total int, truncated bool) {
	total = len(matched)
	limit := total
	if limit > matchedGroupsBound {
		limit = matchedGroupsBound
		truncated = true
	}
	bounded = make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		bounded = append(bounded, sanitize.Claim(matched[i]))
	}
	return bounded, total, truncated
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
