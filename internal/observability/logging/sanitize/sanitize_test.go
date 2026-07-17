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

// The tests in this file pin the algorithm the previous unexported
// sanitizeClaim carried; the relocation from internal/tools/identity.go
// is a rename-plus-relocation only. A change here that widens or narrows
// what Claim strips is a public log-contract change, not a refactor.

package sanitize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClaim_stripsControlChars(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"CR LF", "foo\r\nbar", "foobar"},
		{"tab", "foo\tbar", "foobar"},
		{"ANSI ESC", "foo\x1B[31mred\x1B[0m", "foo[31mred[0m"},
		{"DEL byte", "foo\x7Fbar", "foobar"},
		{"NUL byte", "foo\x00bar", "foobar"},
		{"vertical tab + form feed", "foo\x0B\x0Cbar", "foobar"},
		{"plain", "auth0|abc123", "auth0|abc123"},
		{"unicode passes through", "user-éñ", "user-éñ"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Claim(c.in); got != c.want {
				t.Errorf("Claim(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestClaim_capsAt256Bytes(t *testing.T) {
	in := strings.Repeat("a", 300)
	got := Claim(in)
	if len(got) != MaxLen {
		t.Errorf("len = %d, want %d", len(got), MaxLen)
	}
}

// TestClaim_stripsBidiAndFormatChars pins the CWE-1007 audit-spoofing
// defense: a malicious IdP issues a sub with a RIGHT-TO-LEFT OVERRIDE
// between "alice" and "nimda". Without the Cf-category strip, the bytes
// pass through unchanged, and any bidi-aware UI (SIEM dashboards,
// terminals, JSON viewers) renders the value visually as "aliceadmin" —
// misattributing the action to a different user.
func TestClaim_stripsBidiAndFormatChars(t *testing.T) {
	// Each test case names the codepoint in the row label (e.g.
	// "RLO (U+202E)") so a reviewer viewing this file in an editor that
	// renders bidi controls literally can still identify which character
	// is under test. The character itself lives inside the input-string
	// literal; the label is the safe hook to grep on.
	cases := []struct {
		name, in, want string
	}{
		{"RLO (U+202E)", "alice\u202enimda", "alicenimda"},
		{"PDF (U+202C)", "x\u202cy", "xy"},
		{"LRO (U+202D)", "a\u202db", "ab"},
		{"zero-width joiner (U+200D)", "user\u200dname", "username"},
		{"zero-width non-joiner (U+200C)", "user\u200cname", "username"},
		{"BOM (U+FEFF)", "\xef\xbb\xbfuser", "user"},
		{"soft hyphen (U+00AD)", "ad\u00admin", "admin"},
		{"line separator (U+2028 Zl)", "a\u2028b", "ab"},
		{"paragraph separator (U+2029 Zp)", "a\u2029b", "ab"},
		{"C1 control NEL (U+0085)", "a\u0085b", "ab"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Claim(c.in); got != c.want {
				t.Errorf("Claim(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestClaim_truncatesOnRuneBoundary_repeated pins the UTF-8 safety
// property for the simple "all runes have the same width" case: when the
// cap falls inside what would have been a multibyte codepoint, we stop
// BEFORE writing the codepoint, never mid-bytes. Output is always
// utf8.ValidString.
//
// The 'e-acute' rune (U+00E9) is 2 bytes in UTF-8 (0xC3 0xA9). 128 of
// them = 256 bytes exactly, which fits. 129 of them = 258 bytes would
// overflow the cap; the sanitizer must stop at 128 and return 256 valid
// bytes — not 257 with a dangling 0xC3.
func TestClaim_truncatesOnRuneBoundary_repeated(t *testing.T) {
	in := strings.Repeat("é", 200) // 400 bytes, well over the 256 cap
	got := Claim(in)

	if !utf8.ValidString(got) {
		t.Errorf("sanitized output is not valid UTF-8: %q", got)
	}
	if len(got) > MaxLen {
		t.Errorf("len = %d, exceeds cap %d", len(got), MaxLen)
	}
	// 128 two-byte runes = 256 bytes exactly.
	if want := MaxLen; len(got) != want {
		t.Errorf("len = %d, want exactly %d (128 x 2-byte runes)", len(got), want)
	}
}

// TestClaim_boundedWork_largeInput pins the DoS-defense property: the
// sanitizer must not iterate or allocate proportionally to input length.
// A 10 MB input is fed and the result is still capped at MaxLen bytes.
func TestClaim_boundedWork_largeInput(t *testing.T) {
	in := strings.Repeat("a", 10_000_000)
	got := Claim(in)
	if len(got) != MaxLen {
		t.Errorf("len = %d, want %d (cap must hold against multi-MB input)", len(got), MaxLen)
	}
}

// TestClaim_truncatesOnRuneBoundary_explicit pins the "cap falls inside a
// multi-byte rune" case explicitly, which the simple-repeated form
// exercises by accident but does not name. Constructs an input where the
// 257th byte would split a rune: 255 ASCII bytes followed by a 3-byte
// rune. The sanitizer must stop at 255 (not truncate into the middle of
// the rune) and produce valid UTF-8.
//
// U+3042 (HIRAGANA LETTER A) is 3 bytes in UTF-8 (0xE3 0x81 0x82).
// Feeding 255 'a's + U+3042 gives a 258-byte input where writing the
// rune would push the buffer to 258 bytes, overshooting the cap by 2.
// The sanitizer's projected-length check must observe this and stop at
// 255.
func TestClaim_truncatesOnRuneBoundary_explicit(t *testing.T) {
	in := strings.Repeat("a", 255) + "あ"
	got := Claim(in)

	if !utf8.ValidString(got) {
		t.Errorf("output is not valid UTF-8: %q", got)
	}
	if len(got) > MaxLen {
		t.Errorf("len = %d exceeds cap %d", len(got), MaxLen)
	}
	// Result must be exactly 255 ASCII bytes — the 3-byte rune could not
	// be written without overshooting, so it is dropped entirely.
	if want := strings.Repeat("a", 255); got != want {
		t.Errorf("output = %q, want %q (the trailing rune must be dropped, not split)", got, want)
	}
}

// TestClaim_nonASCIIGraphicUnicodePassesThrough pins the complement of
// TestClaim_stripsBidiAndFormatChars: legitimate non-ASCII graphic runes
// (accented Latin, an emoji, a CJK character) must survive unchanged. A
// future contributor reading the "control-character stripping" contract
// could interpret it more broadly than the current implementation — this
// test locks the boundary in.
func TestClaim_nonASCIIGraphicUnicodePassesThrough(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"accented Latin", "Ünïcödé"},
		{"emoji", "hello \U0001F680 world"},
		{"CJK", "訊息代理"},
		{"mixed graphic", "café-☕-你好"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Claim(c.in); got != c.in {
				t.Errorf("Claim(%q) = %q, want %q (non-ASCII graphic runes must pass through)", c.in, got, c.in)
			}
		})
	}
}

// TestClaim_stripsZeroWidthAndBidiMarks is the sharper companion to
// TestClaim_stripsBidiAndFormatChars: it names three specific characters
// operators are likely to reach for when writing a SIEM rule
// ("zero-width joiner", "RTL mark", "byte order mark") and pins that
// each is stripped, so a future refactor that narrowed the Cf category
// filter (e.g. "strip only bidi overrides, keep joiners") would fail
// this test loudly.
func TestClaim_stripsZeroWidthAndBidiMarks(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"zero-width joiner U+200D", "user\u200dname", "username"},
		{"right-to-left mark U+200F", "user\u200fname", "username"},
		{"byte order mark U+FEFF", "\xef\xbb\xbfvalue", "value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Claim(c.in); got != c.want {
				t.Errorf("Claim(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestConstants pins the literal values of the exported constants. The
// values are load-bearing: MaxLen is the audit-log per-field byte cap,
// VerifierBugSentinel is the string SIEM rules grep for when the
// middleware stashes a non-string value.
func TestConstants(t *testing.T) {
	if MaxLen != 256 {
		t.Errorf("MaxLen = %d, want 256", MaxLen)
	}
	if VerifierBugSentinel != "<verifier-bug>" {
		t.Errorf("VerifierBugSentinel = %q, want %q", VerifierBugSentinel, "<verifier-bug>")
	}
}
