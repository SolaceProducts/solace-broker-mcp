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

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/logging/sanitize"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fullTokenInfo is the shape buildTokenInfo produces for a representative
// OAuth token: every committed claim present and non-empty.
func fullTokenInfo() *sdkauth.TokenInfo {
	return &sdkauth.TokenInfo{
		UserID: "auth0|abc123",
		Scopes: []string{"openid", "read:queues"},
		Extra: map[string]any{
			"iss":       "https://example.auth0.com/",
			"client_id": "cursor-ide",
			"jti":       "jti-xyz",
		},
	}
}

// TestPrincipalFrom_EmptyContext pins the absence contract: with no principal
// stashed, PrincipalFrom returns the zero value, not a panic or a sentinel.
func TestPrincipalFrom_EmptyContext(t *testing.T) {
	t.Parallel()

	got := PrincipalFrom(context.Background())
	if got.Present() {
		t.Errorf("PrincipalFrom(empty).Present() = true, want false")
	}
	if !reflect.DeepEqual(got, Principal{}) {
		t.Errorf("PrincipalFrom(empty) = %+v, want zero-value Principal", got)
	}
}

// TestPrincipalFrom_RoundTrip proves the stored value is read back, not a
// zero value that coincidentally matches: every field is non-zero, so a
// failed type assertion in PrincipalFrom would be visible.
func TestPrincipalFrom_RoundTrip(t *testing.T) {
	t.Parallel()

	want := NewPrincipal(context.Background(), fullTokenInfo())
	ctx := WithPrincipal(context.Background(), want)
	got := PrincipalFrom(ctx)
	if !got.Present() {
		t.Fatal("round-tripped Principal reports Present() == false")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PrincipalFrom(ctx) = %+v, want %+v", got, want)
	}
}

func TestNewPrincipal_nilTokenInfo_isZeroValue(t *testing.T) {
	t.Parallel()

	got := NewPrincipal(context.Background(), nil)
	if got.Present() {
		t.Error("NewPrincipal(context.Background(), nil).Present() = true, want false")
	}
	if !reflect.DeepEqual(got, Principal{}) {
		t.Errorf("NewPrincipal(context.Background(), nil) = %+v, want zero value", got)
	}
}

func TestNewPrincipal_projectsEveryCommittedClaim(t *testing.T) {
	t.Parallel()

	p := NewPrincipal(context.Background(), fullTokenInfo())
	if !p.Present() {
		t.Fatal("Present() = false for a non-nil TokenInfo")
	}
	if p.Sub() != "auth0|abc123" {
		t.Errorf("Sub() = %q", p.Sub())
	}
	if want := []string{"openid", "read:queues"}; !reflect.DeepEqual(p.Scopes(), want) {
		t.Errorf("Scopes() = %v, want %v", p.Scopes(), want)
	}
	if p.ClientID() != "cursor-ide" {
		t.Errorf("ClientID() = %q", p.ClientID())
	}
	if p.Iss() != "https://example.auth0.com/" {
		t.Errorf("Iss() = %q", p.Iss())
	}
	if p.Jti() != "jti-xyz" {
		t.Errorf("Jti() = %q", p.Jti())
	}
}

// TestNewPrincipal_staticVerifierShape mirrors what createStaticTokenVerifier
// returns: UserID only. The Principal is present with sub alone; the other
// claims are "", which the consumer renders "<absent>".
func TestNewPrincipal_staticVerifierShape(t *testing.T) {
	t.Parallel()

	p := NewPrincipal(context.Background(), &sdkauth.TokenInfo{UserID: "dev-user", Scopes: []string{}})
	if !p.Present() {
		t.Fatal("Present() = false")
	}
	if p.Sub() != "dev-user" {
		t.Errorf("Sub() = %q, want dev-user", p.Sub())
	}
	if len(p.Scopes()) != 0 {
		t.Errorf("Scopes() = %v, want empty", p.Scopes())
	}
	if p.Iss() != "" || p.ClientID() != "" || p.Jti() != "" {
		t.Errorf("optional claims should be empty; got iss=%q client_id=%q jti=%q", p.Iss(), p.ClientID(), p.Jti())
	}
}

// TestNewPrincipal_extraKeysPresentButEmpty mirrors the OIDC verifier when the
// IdP omits optional claims: the Extra keys exist with "", and the Principal
// must not invent values.
func TestNewPrincipal_extraKeysPresentButEmpty(t *testing.T) {
	t.Parallel()

	p := NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: "user-1",
		Extra:  map[string]any{"iss": "", "client_id": "", "jti": ""},
	})
	if p.Iss() != "" || p.ClientID() != "" || p.Jti() != "" {
		t.Errorf("want empty optional claims; got iss=%q client_id=%q jti=%q", p.Iss(), p.ClientID(), p.Jti())
	}
}

// TestNewPrincipal_nonStringExtra_emitsErrorAndReturnsSentinel carries the
// verifier-contract-violation behaviour over from tools.Identity (SOL-149606,
// PR #74): no panic (which would drop the audit line), the distinct
// verifier-bug sentinel rather than "", and an ERROR naming the key and
// observed type but never the value.
func TestNewPrincipal_nonStringExtra_emitsErrorAndReturnsSentinel(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	// Drop the timestamp: the value-not-echoed assertion below scans the whole
	// record, and an RFC3339Nano time contains arbitrary digits (a real flake —
	// "42" matched a fractional second about one run in nine).
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelError,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})))
	defer slog.SetDefault(old)

	p := NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: "user-1",
		Extra:  map[string]any{"iss": 424242},
	})

	if p.Iss() != sanitize.VerifierBugSentinel {
		t.Errorf("Iss() = %q, want %q", p.Iss(), sanitize.VerifierBugSentinel)
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("parse ERROR record: %v\n%s", err, buf.String())
	}
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
	if rec["key"] != "iss" {
		t.Errorf("key = %v, want iss", rec["key"])
	}
	if rec["got_type"] != "int" {
		t.Errorf("got_type = %v, want int", rec["got_type"])
	}
	// The offending VALUE must never be logged, only its type.
	if strings.Contains(buf.String(), "424242") {
		t.Errorf("ERROR record echoed the offending value: %s", buf.String())
	}
}

// TestNewPrincipal_sanitizesEveryField is the single-sanitization-layer
// guarantee: no consumer can be handed a value that would forge a log line
// (CWE-117) or spoof an identity with bidi controls (CWE-1007).
func TestNewPrincipal_sanitizesEveryField(t *testing.T) {
	t.Parallel()

	p := NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: "alice\r\n{\"level\":\"INFO\",\"msg\":\"FAKE\"}",
		// \u202e is RIGHT-TO-LEFT OVERRIDE, the CWE-1007 vector; escaped so it
		// stays visible in review (staticcheck ST1018).
		Scopes: []string{"read\nwrite", "\u202eadmin"},
		Extra: map[string]any{
			"iss":       "https://idp\u202e.example",
			"client_id": "cursor\x00-ide",
			"jti":       strings.Repeat("j", sanitize.MaxLen+50),
		},
	})

	for name, got := range map[string]string{
		"sub":       p.Sub(),
		"iss":       p.Iss(),
		"client_id": p.ClientID(),
		"jti":       p.Jti(),
		"scope[0]":  p.Scopes()[0],
		"scope[1]":  p.Scopes()[1],
	} {
		if strings.ContainsAny(got, "\r\n\x00\u202e") {
			t.Errorf("%s = %q still carries a control or bidi character", name, got)
		}
		if len(got) > sanitize.MaxLen {
			t.Errorf("%s is %d bytes, over the %d cap", name, len(got), sanitize.MaxLen)
		}
	}
	if p.Sub() != `alice{"level":"INFO","msg":"FAKE"}` {
		t.Errorf("Sub() = %q, want the CR/LF stripped and nothing else changed", p.Sub())
	}
}

// TestPrincipal_Scopes_returnsCopy pins that a caller mutating the returned
// slice cannot alter the request-scoped Principal.
func TestPrincipal_Scopes_returnsCopy(t *testing.T) {
	t.Parallel()

	p := NewPrincipal(context.Background(), fullTokenInfo())
	s := p.Scopes()
	s[0] = "MUTATED"
	if p.Scopes()[0] == "MUTATED" {
		t.Error("Scopes() returned the backing slice, not a copy")
	}
	if got := (Principal{}).Scopes(); got == nil || len(got) != 0 {
		t.Errorf("zero-value Scopes() = %#v, want an empty non-nil slice", got)
	}
}

// TestPrincipal_fieldSetIsCommittedClaims pins decision D2 and Q-013: exactly
// the five committed claims plus the presence discriminator, and nothing that
// identifies a person directly. Adding a field must turn this red so it is a
// reviewed schema decision, not a drive-by.
func TestPrincipal_fieldSetIsCommittedClaims(t *testing.T) {
	t.Parallel()

	want := []string{"clientID", "iss", "jti", "present", "scopes", "sub"}

	typ := reflect.TypeOf(Principal{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Principal fields = %v, want %v (committed claim set, SOL-152087 D2 / Q-013)", got, want)
	}
	for _, f := range got {
		lower := strings.ToLower(f)
		if strings.Contains(lower, "username") || strings.Contains(lower, "email") {
			t.Errorf("Principal carries PII field %q; Q-013 (2026-07-29) settled on sub-only", f)
		}
	}
}

// callToolReq builds a minimal mcp.Request carrying the given verified token,
// mirroring what the SDK hands a receiving middleware per message.
func callToolReq(info *sdkauth.TokenInfo) *mcp.ServerRequest[*mcp.CallToolParams] {
	r := &mcp.ServerRequest[*mcp.CallToolParams]{Params: &mcp.CallToolParams{Name: "t"}}
	if info != nil {
		r.Extra = &mcp.RequestExtra{TokenInfo: info}
	}
	return r
}

// runMiddleware sends req through PrincipalMiddleware and returns the Principal
// the downstream handler observes on its context.
func runPrincipalMiddleware(t *testing.T, req *mcp.ServerRequest[*mcp.CallToolParams]) Principal {
	t.Helper()
	var seen Principal
	next := func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
		seen = PrincipalFrom(ctx)
		return &mcp.CallToolResult{}, nil
	}
	if _, err := PrincipalMiddleware()(next)(context.Background(), "tools/call", req); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	return seen
}

// TestPrincipalMiddleware_noTokenInfo_attachesNothing covers auth mode
// "disabled" and any request the SDK builds without a token: the handler still
// runs and PrincipalFrom keeps its zero-value contract.
func TestPrincipalMiddleware_noTokenInfo_attachesNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		req  *mcp.ServerRequest[*mcp.CallToolParams]
	}{
		{"nil Extra", callToolReq(nil)},
		{"Extra with nil TokenInfo", &mcp.ServerRequest[*mcp.CallToolParams]{
			Params: &mcp.CallToolParams{Name: "t"},
			Extra:  &mcp.RequestExtra{},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runPrincipalMiddleware(t, tc.req); got.Present() {
				t.Errorf("Principal present without a verified token: %+v", got)
			}
		})
	}
}

func TestPrincipalMiddleware_attachesProjectedPrincipal(t *testing.T) {
	t.Parallel()

	got := runPrincipalMiddleware(t, callToolReq(fullTokenInfo()))
	if !got.Present() {
		t.Fatal("Principal not attached for a verified token")
	}
	if got.Sub() != "auth0|abc123" || got.Jti() != "jti-xyz" || got.ClientID() != "cursor-ide" {
		t.Errorf("unexpected projection: %+v", got)
	}
}

// TestPrincipalMiddleware_isPerRequest is the regression guard for the defect
// this middleware exists to avoid. Population used to live in an http.Handler
// wrapper, whose context does not reach a tool handler per message in stateful
// streamable-HTTP mode — the handler context descends from the POST that
// established the session, so every audit record named the token that opened
// the session, through every later token refresh. Here two messages carrying
// different tokens must yield different principals from one middleware value.
// test/integration/principal_freshness_test.go pins the same property through
// a real HTTP session.
func TestPrincipalMiddleware_isPerRequest(t *testing.T) {
	t.Parallel()

	mw := PrincipalMiddleware()
	var seen []string
	next := func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
		seen = append(seen, PrincipalFrom(ctx).Jti())
		return &mcp.CallToolResult{}, nil
	}
	handler := mw(next)

	for _, jti := range []string{"jti-first", "jti-second"} {
		info := fullTokenInfo()
		info.Extra["jti"] = jti
		if _, err := handler(context.Background(), "tools/call", callToolReq(info)); err != nil {
			t.Fatalf("middleware returned error: %v", err)
		}
	}

	want := []string{"jti-first", "jti-second"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("observed jti sequence = %v, want %v — the principal must track the calling token, not the first one seen", seen, want)
	}
}
