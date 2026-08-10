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

package tokenexchange

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeJWT builds a JWT-shaped (but unsigned) token carrying claims as its
// payload segment. decodeJWTClaimsUnverified never checks the signature, so
// an empty third segment is sufficient for every test in this file.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + "."
}

// redactedKeyTestFixture mirrors cmd/server/main.go's redactedKeys — the
// production ReplaceAttr substring list. Duplicated here (that function is
// unexported in package main and can't be imported) so captureWarn's handler
// actually exercises redaction the way production logging does, rather than
// asserting against a handler that has no ReplaceAttr at all and therefore
// could never redact anything regardless of key names.
var redactedKeyTestFixture = []string{"password", "token", "secret", "authorization", "credential", "api_key", "private_key"}

// captureWarn swaps in a buffer-backed slog handler — with the same
// redaction ReplaceAttr production installs, see redactedKeyTestFixture —
// for the duration of fn, restoring the previous default afterward, and
// returns everything logged.
func captureWarn(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			key := strings.ToLower(a.Key)
			for _, r := range redactedKeyTestFixture {
				if strings.Contains(key, r) {
					a.Value = slog.StringValue("[REDACTED]")
					return a
				}
			}
			return a
		},
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

func TestDecodeJWTClaimsUnverified_SingleStringAudience(t *testing.T) {
	t.Parallel()
	token := fakeJWT(t, map[string]any{"aud": "https://broker.example.com"})

	claims, ok := decodeJWTClaimsUnverified(token)
	if !ok {
		t.Fatal("expected ok=true for a well-formed JWT")
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != "https://broker.example.com" {
		t.Errorf("Audience = %v, want [\"https://broker.example.com\"]", claims.Audience)
	}
}

func TestDecodeJWTClaimsUnverified_ArrayAudience(t *testing.T) {
	t.Parallel()
	token := fakeJWT(t, map[string]any{"aud": []string{"aud-a", "aud-b"}})

	claims, ok := decodeJWTClaimsUnverified(token)
	if !ok {
		t.Fatal("expected ok=true for a well-formed JWT")
	}
	if len(claims.Audience) != 2 || claims.Audience[0] != "aud-a" || claims.Audience[1] != "aud-b" {
		t.Errorf("Audience = %v, want [aud-a aud-b]", claims.Audience)
	}
}

func TestDecodeJWTClaimsUnverified_NotJWTShaped(t *testing.T) {
	t.Parallel()
	for _, tok := range []string{"", "opaque-reference-token", "two.parts", "a.b.c.d"} {
		if _, ok := decodeJWTClaimsUnverified(tok); ok {
			t.Errorf("decodeJWTClaimsUnverified(%q) ok=true, want false (not 3 segments)", tok)
		}
	}
}

func TestDecodeJWTClaimsUnverified_UndecodablePayload(t *testing.T) {
	t.Parallel()
	// Middle segment is not valid base64url.
	if _, ok := decodeJWTClaimsUnverified("header.!!!not-base64!!!.sig"); ok {
		t.Error("expected ok=false for a payload segment that doesn't base64url-decode")
	}
}

func TestDecodeJWTClaimsUnverified_PayloadNotJSON(t *testing.T) {
	t.Parallel()
	payload := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	if _, ok := decodeJWTClaimsUnverified("header." + payload + ".sig"); ok {
		t.Error("expected ok=false for a payload segment that decodes but isn't JSON")
	}
}

func TestWarnIfAudienceMismatch_MatchingAudienceNoWarn(t *testing.T) {
	// Not t.Parallel(): captureWarn swaps the process-wide slog default,
	// which would race with other parallel tests' own log output.
	token := fakeJWT(t, map[string]any{"aud": "https://broker.example.com"})

	out := captureWarn(t, func() {
		warnIfAudienceMismatch("my-broker", "https://broker.example.com", token)
	})
	if out != "" {
		t.Errorf("expected no log output for a matching audience, got: %q", out)
	}
}

func TestWarnIfAudienceMismatch_MismatchLogsWarnWithClaimedAudiences(t *testing.T) {
	// Not t.Parallel(): captureWarn swaps the process-wide slog default,
	// which would race with other parallel tests' own log output.
	token := fakeJWT(t, map[string]any{"aud": []string{"https://other-broker.example.com"}})

	out := captureWarn(t, func() {
		warnIfAudienceMismatch("my-broker", "https://broker.example.com", token)
	})
	if !strings.Contains(out, "WARN") {
		t.Errorf("expected a WARN line, got: %q", out)
	}
	if !strings.Contains(out, "my-broker") {
		t.Errorf("expected broker alias in output, got: %q", out)
	}
	if !strings.Contains(out, "https://broker.example.com") {
		t.Errorf("expected the requested audience in output, got: %q", out)
	}
	if !strings.Contains(out, "https://other-broker.example.com") {
		t.Errorf("expected the token's actual aud claim in output, got: %q", out)
	}
	// Rule 3's ReplaceAttr net redacts keys matching "token" (among others).
	// aud_claim/requested_audience/broker must survive it — assert the
	// redacted marker never appears, so a future rename into a matching key
	// regresses loudly instead of silently blanking the diagnostic.
	if strings.Contains(out, "REDACTED") {
		t.Errorf("expected no field to be redacted, got: %q", out)
	}
}

func TestWarnIfAudienceMismatch_EmptyRequestedAudienceSkipsCheck(t *testing.T) {
	// Not t.Parallel(): captureWarn swaps the process-wide slog default,
	// which would race with other parallel tests' own log output.
	token := fakeJWT(t, map[string]any{"aud": "anything"})

	out := captureWarn(t, func() {
		warnIfAudienceMismatch("my-broker", "", token)
	})
	if out != "" {
		t.Errorf("expected no log output when no audience was requested, got: %q", out)
	}
}

func TestWarnIfAudienceMismatch_OpaqueTokenSkipsCheck(t *testing.T) {
	// Not t.Parallel(): captureWarn swaps the process-wide slog default,
	// which would race with other parallel tests' own log output.
	out := captureWarn(t, func() {
		warnIfAudienceMismatch("my-broker", "https://broker.example.com", "opaque-reference-token")
	})
	if out != "" {
		t.Errorf("expected no log output for a non-JWT access token, got: %q", out)
	}
}

func TestBoundedAudienceList_CapsCountAndLength(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", auditLogAudienceMaxLen+50)
	aud := make(jwtAudience, auditLogAudienceCap+3)
	for i := range aud {
		aud[i] = long
	}

	out := boundedAudienceList(aud)
	if len(out) != auditLogAudienceCap {
		t.Fatalf("len(out) = %d, want %d", len(out), auditLogAudienceCap)
	}
	for _, v := range out {
		if n := len([]rune(v)); n != auditLogAudienceMaxLen {
			t.Errorf("truncated value has %d runes, want exactly %d (cap must include the ellipsis, not sit alongside it): %q", n, auditLogAudienceMaxLen, v)
		}
		if !strings.HasSuffix(v, "…") {
			t.Errorf("expected truncated value to end in an ellipsis, got: %q", v)
		}
	}
}

// TestBoundedAudienceList_TruncatesByRuneNotByte pins that truncation counts
// runes, not bytes — a byte-based slice on a multi-byte-heavy string can
// split a rune mid-character (producing invalid UTF-8) and, since a
// multi-byte rune's byte count exceeds its rune count, can overshoot
// auditLogAudienceMaxLen when the length check itself was byte-based.
func TestBoundedAudienceList_TruncatesByRuneNotByte(t *testing.T) {
	t.Parallel()
	// Each "€" is 3 bytes / 1 rune, so this string is well under the rune cap
	// but over what a byte-based cap of the same number would allow.
	short := strings.Repeat("€", auditLogAudienceMaxLen-10)
	if out := boundedAudienceList(jwtAudience{short}); out[0] != short {
		t.Errorf("a value under the rune cap should pass through unchanged, got: %q", out[0])
	}

	over := strings.Repeat("€", auditLogAudienceMaxLen+10)
	got := boundedAudienceList(jwtAudience{over})[0]
	if n := len([]rune(got)); n != auditLogAudienceMaxLen {
		t.Errorf("truncated rune count = %d, want %d", n, auditLogAudienceMaxLen)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncated value is not valid UTF-8 — a multi-byte rune was split: %q", got)
	}
}

func TestWarnIfAudienceMismatch_MissingAudClaimLogsWarn(t *testing.T) {
	// Not t.Parallel(): captureWarn swaps the process-wide slog default,
	// which would race with other parallel tests' own log output.
	token := fakeJWT(t, map[string]any{"sub": "svc-account"}) // no aud claim at all

	out := captureWarn(t, func() {
		warnIfAudienceMismatch("my-broker", "https://broker.example.com", token)
	})
	if !strings.Contains(out, "WARN") {
		t.Errorf("expected a WARN line for a token with no aud claim, got: %q", out)
	}
}
