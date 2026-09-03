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
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/logging/sanitize"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

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
		sub:      sanitize.AbsentSentinel,
		iss:      sanitize.AbsentSentinel,
		clientID: sanitize.AbsentSentinel,
		jti:      sanitize.AbsentSentinel,
	}
	got := captureGroupAttrs(t, id)
	for _, key := range []string{"sub", "iss", "client_id", "jti"} {
		if got[key] != sanitize.AbsentSentinel {
			t.Errorf("expected %q=%q, got %q", key, sanitize.AbsentSentinel, got[key])
		}
	}
}

// TestNewIdentityFromPrincipal_absentPrincipal pins the disabled-mode path: a
// context no middleware touched projects to Identity{present:false}, and
// LogValue must be safe on it.
func TestNewIdentityFromPrincipal_absentPrincipal(t *testing.T) {
	id := NewIdentityFromPrincipal(auth.PrincipalFrom(context.Background()))
	if id.present {
		t.Error("absent Principal should produce present=false")
	}
	if v := id.LogValue(); v.Kind() != slog.KindGroup {
		t.Errorf("LogValue kind = %v, want group", v.Kind())
	}
}

// TestNewIdentityFromPrincipal_readsPrincipalOffContext is the contract every
// emit site relies on: the projection renders exactly the claims the
// middleware attached, field for field.
func TestNewIdentityFromPrincipal_readsPrincipalOffContext(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: "auth0|abc123",
		Scopes: []string{"openid"},
		Extra: map[string]any{
			"iss":       "https://example.auth0.com/",
			"client_id": "cursor-ide",
			"jti":       "jti-xyz",
		},
	}))

	got := captureGroupAttrs(t, NewIdentityFromPrincipal(auth.PrincipalFrom(ctx)))
	want := map[string]string{
		"sub":       "auth0|abc123",
		"iss":       "https://example.auth0.com/",
		"client_id": "cursor-ide",
		"jti":       "jti-xyz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("projected attrs = %v, want %v", got, want)
	}
}

func TestNewIdentityFromPrincipal_emptyClaims_produceAbsentSentinel(t *testing.T) {
	// TokenInfo with empty UserID and Extra keys present-but-empty (mirrors
	// what the OIDC verifier produces when the IdP omits optional claims).
	id := NewIdentityFromPrincipal(auth.NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: "",
		Extra: map[string]any{
			"iss":       "",
			"client_id": "",
			"jti":       "",
		},
	}))
	if !id.present {
		t.Fatal("present Principal should produce present=true")
	}
	for _, p := range []struct{ name, got string }{
		{"sub", id.sub},
		{"iss", id.iss},
		{"client_id", id.clientID},
		{"jti", id.jti},
	} {
		if p.got != sanitize.AbsentSentinel {
			t.Errorf("%s = %q, want %q", p.name, p.got, sanitize.AbsentSentinel)
		}
	}
}

func TestNewIdentityFromPrincipal_extraKeyMissing_normalizesToAbsent(t *testing.T) {
	// Extra map omits client_id / jti entirely (static-mode TokenInfo).
	id := NewIdentityFromPrincipal(auth.NewPrincipal(context.Background(), &sdkauth.TokenInfo{UserID: "dev-user"}))
	if id.sub != "dev-user" {
		t.Errorf("sub = %q, want %q", id.sub, "dev-user")
	}
	if id.iss != sanitize.AbsentSentinel || id.clientID != sanitize.AbsentSentinel || id.jti != sanitize.AbsentSentinel {
		t.Errorf("expected all three Extra-derived fields to be %q; got iss=%q client_id=%q jti=%q",
			sanitize.AbsentSentinel, id.iss, id.clientID, id.jti)
	}
}

// TestNewIdentityFromPrincipal_verifierBugSentinel_passesThrough pins that the
// projection keeps "<verifier-bug>" distinct from "<absent>" rather than
// normalizing it away. The ERROR record itself is asserted in internal/auth.
func TestNewIdentityFromPrincipal_verifierBugSentinel_passesThrough(t *testing.T) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(old)

	id := NewIdentityFromPrincipal(auth.NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: "user-1",
		Extra:  map[string]any{"iss": 42}, // wrong type — verifier never does this
	}))
	if id.iss != sanitize.VerifierBugSentinel {
		t.Errorf("iss = %q, want %q (verifier-bug sentinel)", id.iss, sanitize.VerifierBugSentinel)
	}
	if sanitize.VerifierBugSentinel == sanitize.AbsentSentinel {
		t.Fatal("sanitize.VerifierBugSentinel must differ from sanitize.AbsentSentinel — log consumers rely on the distinction")
	}
}

// TestSanitizeClaim_logInjection_endToEnd ensures a sub containing CR/LF
// cannot smuggle a forged log line through either handler — proving the
// projection inherits auth.NewPrincipal's sanitization.
func TestSanitizeClaim_logInjection_endToEnd(t *testing.T) {
	malicious := "real-user\n{\"level\":\"INFO\",\"msg\":\"FAKE\",\"admin\":true}"
	id := NewIdentityFromPrincipal(auth.NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: malicious,
		Extra:  map[string]any{"iss": "", "client_id": "", "jti": ""},
	}))

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

// --- Drift-detection tests -------------------------------------------------

// TestTokenInfoStruct_hasExpectedFields reflects over sdkauth.TokenInfo and
// asserts the field set. An SDK upgrade that ADDS a field is fine but must
// turn this test red so the human writing the upgrade PR consciously decides
// whether the new field belongs in auth.Principal and Identity.
func TestTokenInfoStruct_hasExpectedFields(t *testing.T) {
	want := []string{"Expiration", "Extra", "Scopes", "UserID"}

	typ := reflect.TypeOf(sdkauth.TokenInfo{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("sdkauth.TokenInfo fields = %v, want %v\nIf the SDK has added a field, update this assertion AND audit whether auth.Principal / Identity should carry it (plan §9.2 / §10).", got, want)
	}
}

// TestTokenInfoExtra_isMapStringAny pins the type of TokenInfo.Extra. If the
// SDK reshapes it, auth.NewPrincipal's key reads and extraStringSlice break;
// this catches it at test time.
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

// TestTokenInfoExtra_perKeyTypeConvention pins the Extra convention: identity
// keys (iss, client_id, jti) carry strings and surface through
// auth.NewPrincipal; the groups key carries []string and surfaces through
// extraStringSlice. Unknown keys are not permitted.
func TestTokenInfoExtra_perKeyTypeConvention(t *testing.T) {
	info := &sdkauth.TokenInfo{
		UserID: "alice",
		Extra: map[string]any{
			"iss":                         "https://idp.example.com",
			"client_id":                   "cursor-ide",
			"jti":                         "jti-1",
			authz.TokenInfoExtraKeyGroups: []string{"Ops", "Monitoring"},
		},
	}

	// Identity-claim keys: must surface as valid strings on the Principal.
	p := auth.NewPrincipal(context.Background(), info)
	for key, v := range map[string]string{"iss": p.Iss(), "client_id": p.ClientID(), "jti": p.Jti()} {
		if v == "" || v == sanitize.VerifierBugSentinel {
			t.Errorf("Principal claim %q = %q; expected a valid string value", key, v)
		}
	}

	// Groups key: must be readable via extraStringSlice.
	groups, present := extraStringSlice(info, authz.TokenInfoExtraKeyGroups)
	if !present {
		t.Fatalf("extraStringSlice(%q) returned present=false; expected true", authz.TokenInfoExtraKeyGroups)
	}
	if len(groups) != 2 || groups[0] != "Ops" || groups[1] != "Monitoring" {
		t.Errorf("extraStringSlice(%q) = %v; expected [Ops Monitoring]", authz.TokenInfoExtraKeyGroups, groups)
	}

	// Defensive copy: mutating the returned slice must not affect the
	// underlying Extra storage.
	groups[0] = "MUTATED"
	original, _ := info.Extra[authz.TokenInfoExtraKeyGroups].([]string)
	if original[0] == "MUTATED" {
		t.Error("extraStringSlice returned a reference instead of a defensive copy")
	}

	// Completeness: no unknown keys are permitted in Extra.
	allowedKeys := map[string]bool{
		"iss":                         true,
		"client_id":                   true,
		"jti":                         true,
		authz.TokenInfoExtraKeyGroups: true,
	}
	for k := range info.Extra {
		if !allowedKeys[k] {
			t.Errorf("unexpected key %q in TokenInfo.Extra — update the convention comment, accessor set, and this test", k)
		}
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
