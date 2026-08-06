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

// Package authz is a leaf package that owns the tool-authorization decision.
// Given a caller's groups and a tool name, it reports whether the invocation
// is allowed and which groups granted it. No I/O, no logging, no token
// parsing. Import surface: stdlib + internal/config.
package authz

import (
	"log/slog"
	"sort"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// TokenInfoExtraKeyGroups is the TokenInfo.Extra map key under which the
// auth middleware stashes extracted groups when tool authorization is
// enabled. Hosted here as a neutral leaf both internal/auth and
// internal/tools can import without creating an import cycle.
const TokenInfoExtraKeyGroups = "groups"

// Policy is a compiled tool-first authorization index built from the admin's
// ToolAuthorizationConfig. Opaque — construct only via NewPolicy.
//
// Immutable after NewPolicy returns: all fields are unexported, there are no
// mutator methods, and NewPolicy deep-copies the config's map into a fresh
// internal structure. Authorize is therefore lock-free and safe for
// concurrent use.
type Policy struct {
	// toolGrantors maps each tool name to the set of groups that grant it.
	// Used for O(1) lookup in Authorize.
	toolGrantors map[string]map[string]struct{}

	// toolCount is the number of distinct tools with at least one grant.
	toolCount int

	// groupCount is the number of groups defined in the admin's config.
	groupCount int
}

// LogValue implements slog.LogValuer. Emits bounded counts only — never
// admin-authored group or tool names.
func (p *Policy) LogValue() slog.Value {
	if p == nil {
		return slog.GroupValue()
	}
	return slog.GroupValue(
		slog.Int("tool_grant_count", p.toolCount),
		slog.Int("group_count", p.groupCount),
	)
}

// Decision is the value-typed result of an authorization check.
//
// MatchedGroups is exported so callers can read it for audit logging, but
// Decision implements slog.LogValuer to prevent accidental leakage: any
// slog call that includes a Decision emits only Allowed and the count of
// matched groups, never the group names themselves. fmt and json.Marshal
// still render MatchedGroups — log only via slog.
type Decision struct {
	Allowed       bool
	MatchedGroups []string
}

// LogValue implements slog.LogValuer. Emits Allowed and the count of
// matched groups — never group names.
func (d Decision) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("allowed", d.Allowed),
		slog.Int("matched_group_count", len(d.MatchedGroups)),
	)
}

// NewPolicy builds a Policy from a parsed ToolAuthorizationConfig.
//
// The input's AccessLevelGroups map is read but never mutated. The compiled
// Policy holds a fully independent internal structure — caller mutation of
// cfg after NewPolicy returns cannot race with Authorize.
//
// Duplicate tool names within a single group are silently deduplicated (the
// compiled index uses set semantics).
//
// NewPolicy does not consult cfg.Enabled — that is a precondition enforced
// by the caller's composition-time gate (config.ToolAuthorizationEnabled).
//
// The error return is reserved for future compilation failures; in v1 it is
// always nil.
func NewPolicy(cfg config.ToolAuthorizationConfig) (*Policy, error) {
	p := &Policy{
		toolGrantors: make(map[string]map[string]struct{}),
	}

	for groupName, tools := range cfg.AccessLevelGroups {

		toolSeen := make(map[string]struct{})
		for _, tool := range tools {
			if _, dup := toolSeen[tool]; dup {
				continue
			}
			toolSeen[tool] = struct{}{}

			grantors, ok := p.toolGrantors[tool]
			if !ok {
				grantors = make(map[string]struct{})
				p.toolGrantors[tool] = grantors
			}
			grantors[groupName] = struct{}{}
		}
	}

	p.toolCount = len(p.toolGrantors)
	p.groupCount = len(cfg.AccessLevelGroups)

	return p, nil
}

// Authorize reports whether the caller's groups may invoke toolName.
//
// Union semantics: any caller group appearing in the tool's grantor set
// results in an allow. MatchedGroups lists every granting group in sorted
// (lexicographic) order, deduplicated. On deny, MatchedGroups is nil.
//
// Nil or empty groups → deny. Unknown or empty toolName → deny.
// Lock-free — Policy is immutable post-NewPolicy.
func (p *Policy) Authorize(groups []string, toolName string) Decision {
	grantors, ok := p.toolGrantors[toolName]
	if !ok {
		return Decision{}
	}

	// Iterate the caller's groups (typically 2-10) rather than all config
	// groups (potentially 50-200). Each membership check against the
	// grantors map is O(1). This scales with the caller's group count,
	// not the admin's config size.
	var matched []string
	seen := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		if _, ok := grantors[g]; ok {
			matched = append(matched, g)
		}
	}

	if len(matched) == 0 {
		return Decision{}
	}

	sort.Strings(matched)
	return Decision{
		Allowed:       true,
		MatchedGroups: matched,
	}
}
