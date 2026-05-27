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
	"unicode"
	"unicode/utf8"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// absentSentinel is the single value any audit-log identity field takes when
// the underlying claim was missing or empty. The angle brackets make it
// visually distinct from any real claim value (RFC 7519 §4.1.2 / OIDC Core §2).
const absentSentinel = "<absent>"

// claimMaxLen caps each sanitized identity field at 256 bytes. Real sub/jti
// values from production IdPs are 20–50 chars; the cap fires only on malicious
// or buggy IdPs and prevents log-flood DoS (plan §4.4).
const claimMaxLen = 256

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
		sub:      normalizeAbsent(sanitizeClaim(t.UserID)),
		iss:      normalizeAbsent(sanitizeClaim(extraString(t, "iss"))),
		clientID: normalizeAbsent(sanitizeClaim(extraString(t, "client_id"))),
		jti:      normalizeAbsent(sanitizeClaim(extraString(t, "jti"))),
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

// sanitizeClaim defends against log-injection (CWE-117) by stripping every
// control character (any rune below 0x20, plus 0x7F DEL) and capping the
// result at claimMaxLen bytes. JSON-handler escaping already neutralizes
// CR/LF for the JSON sink, but downstream re-emission (errors, panics,
// custom handlers) is the reason we sanitize at the field level (plan §4.4).
//
// Allocation and CPU are bounded by claimMaxLen: the working buffer is sized
// at min(len(s), claimMaxLen) and the loop breaks as soon as another rune
// would exceed the cap. A malicious or buggy IdP shipping a multi-megabyte
// claim cannot force proportional work here.
//
// Truncation lands on a rune boundary — we check the projected length BEFORE
// writing, so the output is always valid UTF-8 even if the cap falls inside
// what would have been a multibyte codepoint.
func sanitizeClaim(s string) string {
	b := strings.Builder{}
	b.Grow(min(len(s), claimMaxLen))

	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			continue
		}
		// Belt-and-suspenders: drop any other unicode-classified control
		// runes (ANSI escape leads, format effectors, etc.).
		if unicode.IsControl(r) {
			continue
		}
		// Stop before writing a rune that would push us past the cap. This
		// is both the DoS defense (bounds total work) and the UTF-8 safety
		// guarantee (never emits half a multibyte codepoint).
		if b.Len()+utf8.RuneLen(r) > claimMaxLen {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// extraString reads a string-typed claim from TokenInfo.Extra by key.
//
// A missing key returns "" — the normal "IdP did not issue this claim" case,
// which downstream normalization maps to the "<absent>" sentinel.
//
// A present-but-non-string value is a contract violation by the verifier
// (commit 1 stashes only strings) and must be loud, not silent: a quiet
// fallback would mask a verifier bug from every audit log. We panic with
// the offending key and the observed type so the bug surfaces immediately.
func extraString(t *sdkauth.TokenInfo, key string) string {
	v, ok := t.Extra[key]
	if !ok {
		return ""
	}
	s, isString := v.(string)
	if !isString {
		panic(fmt.Sprintf("internal: TokenInfo.Extra[%q] is %T, expected string", key, v))
	}
	return s
}
