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
	"context"
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

// redactSecretAttrTestFixture applies redactedKeyTestFixture as a
// slog.HandlerOptions.ReplaceAttr, mirroring cmd/server/main.go's
// redactSecretAttr. Defined once and shared by captureWarn below and by
// TestExchangeError_LogAttrs_EndpointSurvivesRedaction in errors_test.go, so
// both exercise a single copy of the production filter's shape rather than
// two hand-copied closures that could drift apart.
func redactSecretAttrTestFixture(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(a.Key)
	for _, r := range redactedKeyTestFixture {
		if strings.Contains(key, r) {
			a.Value = slog.StringValue("[REDACTED]")
			return a
		}
	}
	return a
}

// captureWarn swaps in a buffer-backed slog handler — with the same
// redaction ReplaceAttr production installs, see redactedKeyTestFixture —
// for the duration of fn, restoring the previous default afterward, and
// returns everything logged.
func captureWarn(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: redactSecretAttrTestFixture,
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
		(&Exchanger{}).warnIfAudienceMismatch(context.Background(), "my-broker", "https://broker.example.com", token)
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
		(&Exchanger{}).warnIfAudienceMismatch(context.Background(), "my-broker", "https://broker.example.com", token)
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
		(&Exchanger{}).warnIfAudienceMismatch(context.Background(), "my-broker", "", token)
	})
	if out != "" {
		t.Errorf("expected no log output when no audience was requested, got: %q", out)
	}
}

func TestWarnIfAudienceMismatch_OpaqueTokenSkipsCheck(t *testing.T) {
	// Not t.Parallel(): captureWarn swaps the process-wide slog default,
	// which would race with other parallel tests' own log output.
	out := captureWarn(t, func() {
		(&Exchanger{}).warnIfAudienceMismatch(context.Background(), "my-broker", "https://broker.example.com", "opaque-reference-token")
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
		(&Exchanger{}).warnIfAudienceMismatch(context.Background(), "my-broker", "https://broker.example.com", token)
	})
	if !strings.Contains(out, "WARN") {
		t.Errorf("expected a WARN line for a token with no aud claim, got: %q", out)
	}
}

// ---------- SOL-155161: per-broker-alias throttle ----------

const mismatchLogMessage = "token exchange: issued access token's aud claim does not include the requested audience"

// mismatchLevelCounts tallies logRecord entries for mismatchLogMessage by
// level, for the throttle tests below.
func mismatchLevelCounts(recs []logRecord) (warn, debug int) {
	for _, rec := range recs {
		if rec.Message != mismatchLogMessage {
			continue
		}
		switch rec.Level {
		case slog.LevelWarn:
			warn++
		case slog.LevelDebug:
			debug++
		}
	}
	return warn, debug
}

// TestWarnIfAudienceMismatch_SecondCallOnSameBrokerLogsDebugNotWarn is the
// direct-unit proof of the throttle itself (the end-to-end
// TestExchange_JWTBearer_AudienceMismatchThrottledLikeTokenExchange in
// exchange_test.go proves the same thing through the real Exchange() path,
// including cache/singleflight; this isolates just the throttle). Two
// calls on one Exchanger, same broker alias: first WARN, second DEBUG —
// AC "does not log a WARN on steady-state operation."
func TestWarnIfAudienceMismatch_SecondCallOnSameBrokerLogsDebugNotWarn(t *testing.T) {
	token := fakeJWT(t, map[string]any{"aud": "00000000-0000-0000-0000-000000000000"})
	e := &Exchanger{}

	records, restore := captureLogs(t)
	defer restore()

	e.warnIfAudienceMismatch(context.Background(), "prod-us", "api://46dfa38b.../.default", token)
	e.warnIfAudienceMismatch(context.Background(), "prod-us", "api://46dfa38b.../.default", token)

	warn, debug := mismatchLevelCounts(records())
	if warn != 1 {
		t.Errorf("WARN count = %d, want 1 (first occurrence)", warn)
	}
	if debug != 1 {
		t.Errorf("DEBUG count = %d, want 1 (second occurrence, throttled)", debug)
	}
}

// TestWarnIfAudienceMismatch_DifferentBrokerAliasesEachGetOwnFirstWarn is
// the AC's "must not silence real problems" guarantee: one broker's
// already-throttled, known-noisy mismatch must never suppress a different
// broker's first — possibly genuine — mismatch.
func TestWarnIfAudienceMismatch_DifferentBrokerAliasesEachGetOwnFirstWarn(t *testing.T) {
	tokenA := fakeJWT(t, map[string]any{"aud": "aud-for-broker-a"})
	tokenB := fakeJWT(t, map[string]any{"aud": "aud-for-broker-b"})
	e := &Exchanger{}

	records, restore := captureLogs(t)
	defer restore()

	e.warnIfAudienceMismatch(context.Background(), "broker-a", "requested-a", tokenA)
	e.warnIfAudienceMismatch(context.Background(), "broker-a", "requested-a", tokenA) // throttled
	e.warnIfAudienceMismatch(context.Background(), "broker-b", "requested-b", tokenB) // independent alias

	warn, debug := mismatchLevelCounts(records())
	if warn != 2 {
		t.Errorf("WARN count = %d, want 2 (broker-a's first, and broker-b's independent first)", warn)
	}
	if debug != 1 {
		t.Errorf("DEBUG count = %d, want 1 (broker-a's second, throttled)", debug)
	}
}

// TestWarnIfAudienceMismatch_SameBrokerGenuinelyDifferentMismatchStillWarns
// is the AC's "must not silence real problems" guarantee in the case the
// other throttle tests don't cover: the SAME broker alias, not a different
// one. An alias's already-throttled, expected canonicalization mismatch
// (Entra's shape) must never swallow a later, genuinely different mismatch
// on that identical alias — e.g. an IdP misconfiguration that starts
// returning an unrelated aud partway through a process's lifetime. Keying
// the throttle on broker alias alone (the pre-fix-round-2 behavior) would
// have logged the second mismatch at DEBUG, hiding it.
func TestWarnIfAudienceMismatch_SameBrokerGenuinelyDifferentMismatchStillWarns(t *testing.T) {
	entraShaped := fakeJWT(t, map[string]any{"aud": "46dfa38b-989c-41b1-8103-b4eda8d7de2e"})
	unrelated := fakeJWT(t, map[string]any{"aud": "totally-unrelated-value"})
	e := &Exchanger{}

	records, restore := captureLogs(t)
	defer restore()

	const alias, requested = "prod-us", "api://46dfa38b-989c-41b1-8103-b4eda8d7de2e/.default"
	e.warnIfAudienceMismatch(context.Background(), alias, requested, entraShaped)
	e.warnIfAudienceMismatch(context.Background(), alias, requested, entraShaped) // throttled: same shape repeated
	e.warnIfAudienceMismatch(context.Background(), alias, requested, unrelated)   // genuinely different: must still warn
	e.warnIfAudienceMismatch(context.Background(), alias, requested, unrelated)   // throttled: that new shape repeated

	warn, debug := mismatchLevelCounts(records())
	if warn != 2 {
		t.Errorf("WARN count = %d, want 2 (the Entra shape's first, and the unrelated shape's independent first)", warn)
	}
	if debug != 2 {
		t.Errorf("DEBUG count = %d, want 2 (each shape's second occurrence, throttled)", debug)
	}
}

// TestWarnIfAudienceMismatch_EntraReportRegression pins the exact shape
// reported against a live Entra tenant (PR #463 comment, amitmorade):
// requesting "api://<GUID>/.default" as the target, Entra's token-exchange
// response carries only the bare GUID in aud. First occurrence still warns
// — this fix throttles repetition, it does not hide the mismatch — with
// every field amit's report showed.
func TestWarnIfAudienceMismatch_EntraReportRegression(t *testing.T) {
	const (
		requestedAudience = "api://46dfa38b-989c-41b1-8103-b4eda8d7de2e/.default"
		entraAud          = "46dfa38b-989c-41b1-8103-b4eda8d7de2e"
	)
	token := fakeJWT(t, map[string]any{"aud": entraAud})

	out := captureWarn(t, func() {
		(&Exchanger{}).warnIfAudienceMismatch(context.Background(), "prod-us", requestedAudience, token)
	})

	for _, want := range []string{"WARN", "prod-us", requestedAudience, entraAud} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got: %q", want, out)
		}
	}
}
