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

// Identity is the repo-owned, log-only view of an authenticated caller.
//
// The audit-log schema lives on this type by design (SOL-149606, plan §4.1
// at docs/superpowers/plans/2026-05-27-sol-149606-tool-invocation-identity-audit.md):
//
//   - sdkauth.TokenInfo is intentionally NOT logged directly. Its Extra map
//     is map[string]any; a struct-level dump would leak whatever a future
//     SDK release or our own verifier stashes there. Identity's fields are
//     the audit schema — adding one is a deliberate code-review event.
//   - The "<absent>" sentinel signals "no value to record" without raising
//     alarm. Alarming on empty identity belongs to operator telemetry
//     (SOL-149791) and to a future verifier-tightening change, not here.
//   - Disabled mode produces Identity{present:false}, whose LogValue emits
//     an empty group — log lines are byte-identical to today's, so log
//     consumers can rely on a stable schema in modes that produce audit
//     trails and a clean absence in modes that don't.
//
// Since SOL-152087, Identity projects auth.Principal rather than reading
// sdkauth.TokenInfo itself, so this line and the audit events of later
// stories name the same principal by construction. Sanitization happens once,
// in auth.NewPrincipal; this type adds only "<absent>" normalization.

package tools

import (
	"fmt"
	"log/slog"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/logging/sanitize"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// Identity carries the audit-relevant subset of OIDC claims for a single
// tool invocation.
//
// Construct via NewIdentityFromPrincipal. The zero value has present=false,
// which matches disabled-mode behavior (no middleware → no principal).
type Identity struct {
	present  bool
	sub      string
	iss      string
	clientID string
	jti      string
}

// LogValue implements slog.LogValuer.
//
// When present==false (disabled mode), returns an empty group; slog's JSON
// handler emits no key for an empty GroupValue, so disabled-mode log lines
// are byte-identical to pre-SOL-149606 lines.
//
// When present==true, emits all four fields. Values may be the "<absent>"
// sentinel but the field names are always present, so SIEM/grep can rely on
// a stable schema across all tokens within a mode.
func (i Identity) LogValue() slog.Value {
	if !i.present {
		return slog.GroupValue()
	}
	return slog.GroupValue(
		slog.String("sub", i.sub),
		slog.String("iss", i.iss),
		slog.String("client_id", i.clientID),
		slog.String("jti", i.jti),
	)
}

// NewIdentityFromPrincipal projects auth.Principal onto the audit-log schema.
// Every emit site calls it with auth.PrincipalFrom(ctx), so a log line names
// the identity the middleware attached, never a second read of the token.
//
// A non-present Principal (auth mode "disabled", tests, internal composite
// steps) yields Identity{present:false}, whose LogValue emits nothing.
//
// Values arrive already sanitized; here empty ones become
// sanitize.AbsentSentinel so field names stay stable across all tokens within
// a mode. Only four of Principal's five claims are rendered — scope rides on
// Principal for token exchange, not for the audit line.
func NewIdentityFromPrincipal(p auth.Principal) Identity {
	if !p.Present() {
		return Identity{present: false}
	}
	return Identity{
		present:  true,
		sub:      sanitize.NormalizeAbsent(p.Sub()),
		iss:      sanitize.NormalizeAbsent(p.Iss()),
		clientID: sanitize.NormalizeAbsent(p.ClientID()),
		jti:      sanitize.NormalizeAbsent(p.Jti()),
	}
}

// TokenInfo.Extra convention:
//   - Identity-claim keys (iss, client_id, jti) carry strings and are read
//     exactly once, by auth.NewPrincipal; this package never reads them.
//   - Other keys carry per-key typed values with a per-key accessor. Groups
//     are authorization input, not principal identity, so they stay on
//     TokenInfo and are read here. The drift test asserts by key, not by
//     blanket type.
//
// extraStringSlice reads a []string value by key. A missing key returns
// (nil, false); a wrong-typed one takes the same verifier-bug ERROR path
// auth.NewPrincipal uses and also returns (nil, false). A valid value is
// copied so callers may mutate freely.
func extraStringSlice(t *sdkauth.TokenInfo, key string) (values []string, present bool) {
	if t == nil {
		return nil, false
	}
	v, ok := t.Extra[key]
	if !ok {
		return nil, false
	}
	s, isSlice := v.([]string)
	if !isSlice {
		slog.Error("internal: TokenInfo.Extra has unexpected type — verifier contract violation",
			slog.String("key", key),
			slog.String("got_type", fmt.Sprintf("%T", v)))
		return nil, false
	}
	cp := make([]string, len(s))
	copy(cp, s)
	return cp, true
}

// requestGroups reads the caller's groups claim from token info.
// Returns (nil, false) when the claim is absent — callers must treat that as a deny.
//
// Takes *sdkauth.TokenInfo, not a request type, so the tools/call path and the
// tools/list middleware read the claim through one function and cannot drift.
func requestGroups(info *sdkauth.TokenInfo) (groups []string, present bool) {
	return extraStringSlice(info, authz.TokenInfoExtraKeyGroups)
}
