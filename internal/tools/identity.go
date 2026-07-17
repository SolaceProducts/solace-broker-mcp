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

package tools

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/SolaceDev/solace-broker-mcp/internal/authz"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/logging/sanitize"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// absentSentinel is the single value any audit-log identity field takes when
// the underlying claim was missing or empty. The angle brackets make it
// visually distinct from any real claim value (RFC 7519 §4.1.2 / OIDC Core §2).
const absentSentinel = "<absent>"

// Identity carries the audit-relevant subset of OIDC claims for a single
// tool invocation.
//
// Construct via NewIdentityFromTokenInfo. The zero value has present=false,
// which matches disabled-mode behavior (no middleware → no identity).
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

// NewIdentityFromTokenInfo builds an Identity for the audit-log layer.
//
// A nil TokenInfo (disabled mode or test scaffolding that constructs a bare
// CallToolRequest) returns Identity{present:false}; LogValue then emits no
// attributes.
//
// A non-nil TokenInfo populates all four fields. Each field is sanitized
// (control chars stripped, capped at 256 bytes) and then normalized: empty
// or whitespace-only values become the "<absent>" sentinel.
func NewIdentityFromTokenInfo(t *sdkauth.TokenInfo) Identity {
	if t == nil {
		return Identity{present: false}
	}
	return Identity{
		present:  true,
		sub:      normalizeAbsent(sanitize.Claim(t.UserID)),
		iss:      normalizeAbsent(sanitize.Claim(extraString(t, "iss"))),
		clientID: normalizeAbsent(sanitize.Claim(extraString(t, "client_id"))),
		jti:      normalizeAbsent(sanitize.Claim(extraString(t, "jti"))),
	}
}

// normalizeAbsent collapses empty/whitespace-only input to the audit-log
// "<absent>" sentinel. Non-empty input is returned unchanged.
func normalizeAbsent(s string) string {
	if strings.TrimSpace(s) == "" {
		return absentSentinel
	}
	return s
}

// TokenInfo.Extra convention:
//   - Audit-field keys (iss, client_id, jti) carry string values, read via
//     extraString. These are the Identity audit schema fields.
//   - Other keys carry per-key typed values, read via a per-key accessor
//     (e.g. extraStringSlice for authz.TokenInfoExtraKeyGroups). The drift
//     test asserts by key, not by blanket type.
//
// extraString reads a string-typed claim from TokenInfo.Extra by key.
//
// Three outcomes:
//
//   - Key missing: returns "" — the normal "IdP did not issue this claim" case,
//     which downstream normalization maps to the "<absent>" sentinel.
//
//   - Key present, value is a string: returns the string. Happy path.
//
//   - Key present, value is NOT a string: contract violation by our own
//     verifier (audit-field keys stash only strings). We emit an ERROR-level slog
//     entry naming the key and observed type, then return sanitize.VerifierBugSentinel.
//     The audit-log line for this request still gets emitted (with the
//     sentinel as the field value) — the panic version we shipped initially
//     killed the request before logToolResult's defer registered, inverting
//     the audit guarantee on exactly the request class where it mattered
//     most. The slog.Error preserves loudness for ops alerting; the distinct
//     sentinel lets SIEM rules distinguish this case from "<absent>". See
//     plan §14 for the change-of-mind rationale.
func extraString(t *sdkauth.TokenInfo, key string) string {
	v, ok := t.Extra[key]
	if !ok {
		return ""
	}
	s, isString := v.(string)
	if !isString {
		slog.Error("internal: TokenInfo.Extra has unexpected type — verifier contract violation",
			slog.String("key", key),
			slog.String("got_type", fmt.Sprintf("%T", v)))
		return sanitize.VerifierBugSentinel
	}
	return s
}

// extraStringSlice reads a []string-typed value from TokenInfo.Extra by key.
// Missing keys return (nil, false). Present keys with the wrong type trigger
// the same verifier-bug slog.Error path as extraString, then return
// (nil, false). Present keys with a valid []string return a defensive copy
// so callers may mutate freely without affecting request-scoped storage.
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

// requestGroups extracts the caller's groups from the request identity.
// Reads under authz.TokenInfoExtraKeyGroups. Returns (nil, false) when the
// groups claim was missing from the token (the day-one IdP misconfiguration
// case).
//
//nolint:unused // Called by withAuthorization (T4 enforcement wrapper, next ticket).
func requestGroups(req *mcp.CallToolRequest) (groups []string, present bool) {
	if req == nil {
		return nil, false
	}
	if req.Extra == nil {
		return nil, false
	}
	if req.Extra.TokenInfo == nil {
		return nil, false
	}
	return extraStringSlice(req.Extra.TokenInfo, authz.TokenInfoExtraKeyGroups)
}
