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

// Drift-detection tests in this file are intentionally brittle in ONE
// direction: an upstream contract change (sdkauth.TokenInfo shape, SDK
// version bump, verifier refactor that drops a claim) MUST turn them red.
// They are stable in the OTHER direction: refactoring our own code should
// not perturb them. If you find yourself "fixing" one by loosening its
// assertions, stop — the test is doing its job. Update the assertion AND
// audit whether the upstream change should propagate into Identity.
// Documented in plan §9.2.

package tools

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// pinnedSDKVersion is the version of github.com/modelcontextprotocol/go-sdk
// that this PR was tested against. Bumping the SDK is a deliberate event;
// update this constant AND re-run the drift-detection suite when you do.
const pinnedSDKVersion = "v1.5.0"

// --- Behavioral tests ------------------------------------------------------

func TestIdentity_LogValue_emitsExactlyKnownFields_whenPresent(t *testing.T) {
	id := Identity{
		present:  true,
		sub:      "user-1",
		iss:      "https://idp.example.com",
		clientID: "cursor-ide",
		jti:      "jti-1",
	}

	got := captureGroupAttrs(t, id)
	want := map[string]string{
		"sub":       "user-1",
		"iss":       "https://idp.example.com",
		"client_id": "cursor-ide",
		"jti":       "jti-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LogValue attrs = %v, want %v", got, want)
	}
}

func TestIdentity_LogValue_emitsNothing_whenNotPresent(t *testing.T) {
	id := Identity{} // zero value: present=false

	got := captureGroupAttrs(t, id)
	if len(got) != 0 {
		t.Errorf("expected no attributes for !present, got %v", got)
	}

	// End-to-end through the JSON handler: no identity key should appear at all.
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("test", slog.Any("identity", id))

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("parse log: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"identity", "sub", "iss", "client_id", "jti"} {
		if _, present := parsed[k]; present {
			t.Errorf("expected no %q key in disabled-mode log line, got %v", k, parsed[k])
		}
	}
}

func TestIdentity_LogValue_alwaysEmitsAllFour_inPresentMode(t *testing.T) {
	// All four normalized to <absent> still produces the full schema.
	id := Identity{
		present:  true,
		sub:      absentSentinel,
		iss:      absentSentinel,
		clientID: absentSentinel,
		jti:      absentSentinel,
	}
	got := captureGroupAttrs(t, id)
	for _, key := range []string{"sub", "iss", "client_id", "jti"} {
		if got[key] != absentSentinel {
			t.Errorf("expected %q=%q, got %q", key, absentSentinel, got[key])
		}
	}
}

func TestNewIdentityFromTokenInfo_nilTokenInfo(t *testing.T) {
	id := NewIdentityFromTokenInfo(nil)
	if id.present {
		t.Error("nil TokenInfo should produce present=false")
	}
	// Must not panic; LogValue must be safe to call.
	if v := id.LogValue(); v.Kind() != slog.KindGroup {
		t.Errorf("LogValue kind = %v, want group", v.Kind())
	}
}

func TestNewIdentityFromTokenInfo_emptyClaims_produceAbsentSentinel(t *testing.T) {
	// TokenInfo with empty UserID and Extra keys present-but-empty (mirrors
	// what the OIDC verifier produces when the IdP omits optional claims).
	info := &sdkauth.TokenInfo{
		UserID: "",
		Extra: map[string]any{
			"iss":       "",
			"client_id": "",
			"jti":       "",
		},
	}
	id := NewIdentityFromTokenInfo(info)
	if !id.present {
		t.Fatal("non-nil TokenInfo should produce present=true")
	}
	for _, p := range []struct{ name, got string }{
		{"sub", id.sub},
		{"iss", id.iss},
		{"client_id", id.clientID},
		{"jti", id.jti},
	} {
		if p.got != absentSentinel {
			t.Errorf("%s = %q, want %q", p.name, p.got, absentSentinel)
		}
	}
}

func TestNewIdentityFromTokenInfo_extraKeyMissing_normalizesToAbsent(t *testing.T) {
	// Extra map omits client_id / jti entirely (static-mode TokenInfo).
	info := &sdkauth.TokenInfo{UserID: "dev-user"}
	id := NewIdentityFromTokenInfo(info)
	if id.sub != "dev-user" {
		t.Errorf("sub = %q, want %q", id.sub, "dev-user")
	}
	if id.iss != absentSentinel || id.clientID != absentSentinel || id.jti != absentSentinel {
		t.Errorf("expected all three Extra-derived fields to be %q; got iss=%q client_id=%q jti=%q",
			absentSentinel, id.iss, id.clientID, id.jti)
	}
}

// TestExtraString_panicsOnNonStringValue pins the verifier contract: commit 1
// stashes only strings into Extra. A non-string value would indicate a real
// verifier bug, and the audit layer refuses to silently paper over it.
func TestExtraString_panicsOnNonStringValue(t *testing.T) {
	info := &sdkauth.TokenInfo{
		Extra: map[string]any{"iss": 42}, // wrong type — verifier never does this
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when Extra holds a non-string value")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, `"iss"`) || !strings.Contains(msg, "int") {
			t.Errorf("panic message should name the bad key and type, got: %v", r)
		}
	}()
	_ = NewIdentityFromTokenInfo(info)
}

func TestSanitizeClaim_stripsControlChars(t *testing.T) {
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
			if got := sanitizeClaim(c.in); got != c.want {
				t.Errorf("sanitizeClaim(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeClaim_capsAt256Bytes(t *testing.T) {
	in := strings.Repeat("a", 300)
	got := sanitizeClaim(in)
	if len(got) != claimMaxLen {
		t.Errorf("len = %d, want %d", len(got), claimMaxLen)
	}
}

// TestSanitizeClaim_logInjection_endToEnd ensures a sub containing CR/LF
// cannot smuggle a forged log line through either the JSON or text handler.
func TestSanitizeClaim_logInjection_endToEnd(t *testing.T) {
	malicious := "real-user\n{\"level\":\"INFO\",\"msg\":\"FAKE\",\"admin\":true}"
	info := &sdkauth.TokenInfo{
		UserID: malicious,
		Extra:  map[string]any{"iss": "", "client_id": "", "jti": ""},
	}
	id := NewIdentityFromTokenInfo(info)

	t.Run("json handler", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		logger.Info("tool invoked", slog.Any("", id))

		// Output is one line — assert that splitting on '\n' produces exactly
		// one non-empty line. A successful injection would produce two.
		lines := splitNonEmptyLines(buf.String())
		if len(lines) != 1 {
			t.Errorf("expected exactly 1 log line, got %d: %q", len(lines), buf.String())
		}
	})

	t.Run("text handler", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		logger.Info("tool invoked", slog.Any("", id))

		lines := splitNonEmptyLines(buf.String())
		if len(lines) != 1 {
			t.Errorf("expected exactly 1 log line, got %d: %q", len(lines), buf.String())
		}
	})
}

func TestNormalizeAbsent(t *testing.T) {
	cases := map[string]string{
		"":          absentSentinel,
		"   ":       absentSentinel,
		"\t":        absentSentinel,
		"x":         "x",
		"auth0|123": "auth0|123",
	}
	for in, want := range cases {
		if got := normalizeAbsent(in); got != want {
			t.Errorf("normalizeAbsent(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- Drift-detection tests -------------------------------------------------

// TestTokenInfoStruct_hasExpectedFields reflects over sdkauth.TokenInfo and
// asserts the field set. An SDK upgrade that ADDS a field is fine but must
// turn this test red so the human writing the upgrade PR consciously decides
// whether the new field belongs in Identity.
func TestTokenInfoStruct_hasExpectedFields(t *testing.T) {
	want := []string{"Expiration", "Extra", "Scopes", "UserID"}

	typ := reflect.TypeOf(sdkauth.TokenInfo{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("sdkauth.TokenInfo fields = %v, want %v\nIf the SDK has added a field, update this assertion AND audit whether Identity should carry it (plan §9.2 / §10).", got, want)
	}
}

// TestTokenInfoExtra_isMapStringAny pins the type of TokenInfo.Extra. If the
// SDK ever changes Extra to a strongly-typed struct or a different map shape,
// our extraString helper breaks; this test catches it at compile/test time.
func TestTokenInfoExtra_isMapStringAny(t *testing.T) {
	field, ok := reflect.TypeOf(sdkauth.TokenInfo{}).FieldByName("Extra")
	if !ok {
		t.Fatal("sdkauth.TokenInfo has no Extra field — verify pinned SDK shape")
	}
	want := reflect.TypeOf(map[string]any{})
	if field.Type != want {
		t.Errorf("TokenInfo.Extra type = %v, want %v", field.Type, want)
	}
}

// TestIdentity_audited_fields_matchLogValueOutput pins the invariant that
// every field on Identity (except `present`) corresponds to exactly one key
// emitted by LogValue. Adding a field to Identity without wiring it into
// LogValue — or vice versa — turns this test red.
func TestIdentity_audited_fields_matchLogValueOutput(t *testing.T) {
	// Reflect over Identity's audit fields. `present` is the discriminator,
	// not an audit attribute, so it is excluded.
	identityFields := map[string]bool{}
	typ := reflect.TypeOf(Identity{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if name == "present" {
			continue
		}
		identityFields[name] = true
	}

	id := Identity{present: true, sub: "x", iss: "x", clientID: "x", jti: "x"}
	emitted := captureGroupAttrs(t, id)

	// Map LogValue keys back to Identity field names.
	logKeyToField := map[string]string{
		"sub":       "sub",
		"iss":       "iss",
		"client_id": "clientID",
		"jti":       "jti",
	}

	emittedFields := map[string]bool{}
	for k := range emitted {
		f, ok := logKeyToField[k]
		if !ok {
			t.Errorf("LogValue emits unmapped key %q — update logKeyToField or remove the emit", k)
			continue
		}
		emittedFields[f] = true
	}

	if !reflect.DeepEqual(identityFields, emittedFields) {
		t.Errorf("Identity audit fields = %v, but LogValue emits = %v.\nA field was added to Identity but not wired into LogValue (or vice versa).", identityFields, emittedFields)
	}
}

// TestSDKVersion_pinned reads go.mod and asserts the SDK version matches
// pinnedSDKVersion. SDK upgrades are intentional events; bump the constant
// AND re-run drift detection when you upgrade.
func TestSDKVersion_pinned(t *testing.T) {
	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	const modPath = "github.com/modelcontextprotocol/go-sdk"
	var got string
	for _, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, modPath+" ") {
			continue
		}
		fields := strings.Fields(s)
		if len(fields) >= 2 {
			got = fields[1]
		}
		break
	}
	if got == "" {
		t.Fatalf("go.mod has no require for %q", modPath)
	}
	if got != pinnedSDKVersion {
		t.Errorf("SDK version drift: go.mod %s = %q, pinnedSDKVersion = %q.\nIf this upgrade is intentional, update pinnedSDKVersion AND re-verify drift-detection assertions (plan §9.2).", modPath, got, pinnedSDKVersion)
	}
}

// --- helpers ---------------------------------------------------------------

// captureGroupAttrs invokes LogValue and returns the emitted attrs as a
// string-keyed map. Asserts there's exactly one Group level (no nested
// groups) so callers can flat-compare.
func captureGroupAttrs(t *testing.T, id Identity) map[string]string {
	t.Helper()
	v := id.LogValue()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind = %v, want group", v.Kind())
	}
	out := map[string]string{}
	for _, a := range v.Group() {
		if a.Value.Kind() != slog.KindString {
			t.Fatalf("attr %q kind = %v, want string", a.Key, a.Value.Kind())
		}
		out[a.Key] = a.Value.String()
	}
	return out
}

func splitNonEmptyLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

