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

// Package sanitize centralizes audit-log field hygiene. Admin- or
// IdP-controlled strings pass through Claim before they reach an slog
// record, so a hostile or buggy source cannot inject forged log lines
// (CWE-117) or spoof audit identities via bidi controls (CWE-1007).
//
// This package is a leaf: it has no dependencies outside the standard
// library and imports no other repo packages. Both the tool-authorization
// audit event and the identity-audit log line depend on it, so hosting
// the utility here — under the observability tree, alongside correlation —
// keeps every caller import-cycle-free.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLen is the byte cap applied by Claim. A real IdP-issued sub/jti is
// 20–50 chars; the cap defends against a hostile or buggy issuer shipping
// a multi-megabyte claim into audit logs.
const MaxLen = 256

// VerifierBugSentinel is the audit sentinel emitted by callers when their
// own verifier stashed a non-string value where a string was expected.
// Distinct from AbsentSentinel so log consumers can tell "IdP did not issue
// this claim" (normal) from "our code put garbage in Extra" (alarm).
const VerifierBugSentinel = "<verifier-bug>"

// AbsentSentinel is what an audit-log identity field carries when the claim
// was missing or empty. The angle brackets keep it distinct from any real
// claim value. It signals "no value to record" without raising alarm;
// alarming on an empty identity belongs to operator telemetry.
const AbsentSentinel = "<absent>"

// NormalizeAbsent collapses empty or whitespace-only input to AbsentSentinel
// and returns anything else unchanged. Presentation-layer only: auth.Principal
// keeps absent claims as "", and each consumer applies this at emit time so
// field names stay stable across every token within a mode.
func NormalizeAbsent(s string) string {
	if strings.TrimSpace(s) == "" {
		return AbsentSentinel
	}
	return s
}

// Claim defends against log-injection (CWE-117) and audit-spoofing
// (CWE-1007) by stripping non-graphic Unicode and capping the result at
// MaxLen bytes. The four stripped categories are:
//
//   - Cc — Control characters. Includes the ASCII C0 block (NUL, TAB, LF,
//     CR, ESC, …), DEL, and the C1 block (U+0080–U+009F). This is the
//     CWE-117 surface: LF and CR can break log-line framing.
//   - Cf — Format characters. Includes bidi/RTL overrides (U+202A–U+202E,
//     U+2066–U+2069), zero-width joiners, BOM, soft hyphen, language tags.
//     Bidi controls are the CWE-1007 surface: a sub like
//     "alice<RLO>nimda" renders as "aliceadmin" in any bidi-aware UI,
//     misattributing the action to the wrong user in SIEMs/terminals.
//   - Zl / Zp — Line and Paragraph separator (U+2028, U+2029). Some
//     renderers honor these as line breaks; they're CWE-117-adjacent.
//
// JSON-handler escaping already neutralizes CR/LF for the JSON sink, but
// field-level sanitization protects re-emission through error messages,
// panic strings, custom handlers, and metric labels.
//
// Allocation is bounded by MaxLen: the working buffer is sized at
// min(len(s), MaxLen) and stops growing once another rune would exceed the
// cap. CPU is NOT bounded by MaxLen, despite what this comment claimed before
// SOL-152087 — the cap check sits after the strip, so a claim made entirely of
// stripped runes never fills the buffer and the loop walks every rune of the
// input. Work is linear in input length, which upstream bounds only by
// net/http's 1 MiB default header limit: roughly 4 ms per MiB of all-stripped
// input, against 7 µs for the same size of graphic text.
//
// Left that way deliberately. Cutting the scan short would change which bytes
// survive for such input, and that is an audit-output change this story does
// not own. A hostile or badly broken IdP is a precondition either way, since
// the token must pass signature, issuer and audience verification first.
//
// Truncation lands on a rune boundary — the projected length is checked
// BEFORE writing, so the output is always valid UTF-8 even if the cap
// falls inside what would have been a multibyte codepoint.
func Claim(s string) string {
	b := strings.Builder{}
	b.Grow(min(len(s), MaxLen))

	for _, r := range s {
		// Single filter, complete spec — see godoc above. Cc covers both
		// ASCII (C0/DEL) and C1 controls, so no separate fast-path is
		// needed.
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		// Stop before writing a rune that would push us past the cap. This
		// is both the DoS defense (bounds total work) and the UTF-8 safety
		// guarantee (never emits half a multibyte codepoint).
		if b.Len()+utf8.RuneLen(r) > MaxLen {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
