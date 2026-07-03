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
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// readFormBody reads req.Body and parses it as an application/x-www-form-urlencoded
// body. Fatals on any read or parse error. Used by the body-asserting tests.
func readFormBody(t *testing.T, req *http.Request) url.Values {
	t.Helper()
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	form, err := url.ParseQuery(string(bodyBytes))
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	return form
}

// TestBuildIdPRequest_MandatoryRFCFields verifies that grant_type,
// subject_token, and subject_token_type are present with exact RFC 8693 URN
// values on every call, regardless of auth method or optional fields.
func TestBuildIdPRequest_MandatoryRFCFields(t *testing.T) {
	t.Parallel()

	e, err := New(validParams())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input := ExchangeInput{
		SubjectToken: "tok-abc",
		Audience:     "https://broker.example.com",
	}

	req, err := e.buildIdPRequest(context.Background(), input)
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	form := readFormBody(t, req)

	if got := form.Get("grant_type"); got != URNGrantTypeTokenExchange {
		t.Errorf("grant_type = %q, want %q", got, URNGrantTypeTokenExchange)
	}
	if got := form.Get("subject_token"); got != "tok-abc" {
		t.Errorf("subject_token = %q, want %q", got, "tok-abc")
	}
	if got := form.Get("subject_token_type"); got != URNTokenTypeAccessToken {
		t.Errorf("subject_token_type = %q, want %q", got, URNTokenTypeAccessToken)
	}
}

// TestBuildIdPRequest_ClientSecretPost verifies that client_id and
// client_secret appear in the form body and the Authorization header is
// absent when ClientSecretPost is configured.
func TestBuildIdPRequest_ClientSecretPost(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.ClientAuthMethod = ClientSecretPost
	p.ClientID = "my-client"
	p.ClientSecret = "my-secret"
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		Audience:     "aud",
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	form := readFormBody(t, req)

	if got := form.Get("client_id"); got != "my-client" {
		t.Errorf("client_id = %q, want %q", got, "my-client")
	}
	if got := form.Get("client_secret"); got != "my-secret" {
		t.Errorf("client_secret = %q, want %q", got, "my-secret")
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization header = %q, want absent for ClientSecretPost", auth)
	}
}

// TestBuildIdPRequest_ClientSecretBasic verifies that client_id and
// client_secret are encoded in the Basic Authorization header and do NOT
// appear in the form body when ClientSecretBasic is configured.
func TestBuildIdPRequest_ClientSecretBasic(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.ClientAuthMethod = ClientSecretBasic
	p.ClientID = "my-client"
	p.ClientSecret = "my-secret"
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		Audience:     "aud",
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic scheme", auth)
	}
	// net/http.SetBasicAuth writes "Basic <base64.StdEncoding>" — Go stdlib guarantee.
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		t.Fatalf("base64 decode Authorization: %v", err)
	}
	if string(decoded) != "my-client:my-secret" {
		t.Errorf("Basic credentials = %q, want %q", string(decoded), "my-client:my-secret")
	}

	form := readFormBody(t, req)

	if form.Get("client_id") != "" {
		t.Errorf("client_id in form body for ClientSecretBasic, want absent")
	}
	if form.Get("client_secret") != "" {
		t.Errorf("client_secret in form body for ClientSecretBasic, want absent")
	}
}

// TestBuildIdPRequest_NeverBothCredentials verifies the mutual-exclusion
// invariant: for any valid auth method, credentials appear in exactly one
// location — either the Basic header or the form body, never both and
// never neither.
func TestBuildIdPRequest_NeverBothCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method ClientAuthMethod
	}{
		{"ClientSecretPost", ClientSecretPost},
		{"ClientSecretBasic", ClientSecretBasic},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validParams()
			p.ClientAuthMethod = tc.method
			p.ClientID = "cid"
			p.ClientSecret = "csec"
			e, err := New(p)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
				SubjectToken: "tok",
				Audience:     "aud",
			})
			if err != nil {
				t.Fatalf("buildIdPRequest: %v", err)
			}

			form := readFormBody(t, req)

			hasBasic := strings.HasPrefix(req.Header.Get("Authorization"), "Basic ")
			hasFormCID := form.Get("client_id") != ""

			if hasBasic && hasFormCID {
				t.Errorf("both Basic header and client_id form field set — never-both invariant violated")
			}
			if !hasBasic && !hasFormCID {
				t.Errorf("neither Basic header nor client_id form field set — credentials missing")
			}
		})
	}
}

// TestBuildIdPRequest_UnknownGrantType verifies that any GrantType value
// outside the defined constants returns an error mentioning "GrantType"
// and a nil request. The grant-type switch fires before auth-method and
// audience switches.
func TestBuildIdPRequest_UnknownGrantType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		grantType GrantType
	}{
		{"zero value", 0},
		{"negative", -1},
		{"large unknown", 99},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Exchanger{
				tokenURL:         "https://idp.example.com/token",
				clientID:         "cid",
				clientAuthMethod: ClientSecretPost,
				clientSecret:     "sec",
				grantType:        tc.grantType,
				audienceParam:    AudienceParamAudience,
			}
			req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
				SubjectToken: "tok",
				Audience:     "aud",
			})

			if err == nil {
				t.Fatal("expected error for unknown GrantType, got nil")
			}
			if req != nil {
				t.Errorf("expected nil request on error, got non-nil")
			}
			if !strings.Contains(err.Error(), "GrantType") {
				t.Errorf("error = %q, want mention of GrantType", err.Error())
			}
		})
	}
}

// TestBuildIdPRequest_UnknownClientAuthMethod verifies that any
// ClientAuthMethod value outside the defined constants returns an error
// mentioning "ClientAuthMethod" and a nil request. audienceParam is set
// to AudienceParamAudience so the audience switch stays valid and the
// auth-method error fires first.
func TestBuildIdPRequest_UnknownClientAuthMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method ClientAuthMethod
	}{
		{"zero value", 0},
		{"negative", -1},
		{"large unknown", 99},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Exchanger{
				tokenURL:         "https://idp.example.com/token",
				clientID:         "cid",
				clientAuthMethod: tc.method,
				clientSecret:     "sec",
				grantType:        GrantTypeTokenExchange,  // REQUIRED: passes grant-type switch
				audienceParam:    AudienceParamAudience,   // REQUIRED: keeps audience switch valid so the auth-method error fires first
			}
			req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
				SubjectToken: "tok",
				Audience:     "aud",
			})

			if err == nil {
				t.Fatal("expected error for unknown ClientAuthMethod, got nil")
			}
			if req != nil {
				t.Errorf("expected nil request on error, got non-nil")
			}
			if !strings.Contains(err.Error(), "ClientAuthMethod") {
				t.Errorf("error = %q, want mention of ClientAuthMethod", err.Error())
			}
		})
	}
}

// TestBuildIdPRequest_UnknownAudienceFormat verifies that any
// AudienceFormat value outside the defined constants returns an error
// mentioning "AudienceFormat" and a nil request. clientAuthMethod is set
// to ClientSecretPost so the auth switch passes and the audience-format
// error fires.
func TestBuildIdPRequest_UnknownAudienceFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		param AudienceFormat
	}{
		{"zero value", 0},
		{"AudienceParamScope (unimplemented)", 2},
		{"AudienceParamResource (unimplemented)", 3},
		{"negative", -1},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Exchanger{
				tokenURL:         "https://idp.example.com/token",
				clientID:         "cid",
				clientAuthMethod: ClientSecretPost,        // REQUIRED: passes auth switch so the audience-format error fires
				clientSecret:     "sec",
				grantType:        GrantTypeTokenExchange,  // REQUIRED: passes grant-type switch
				audienceParam:    tc.param,
			}
			req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
				SubjectToken: "tok",
				Audience:     "aud",
			})

			if err == nil {
				t.Fatal("expected error for unknown AudienceFormat, got nil")
			}
			if req != nil {
				t.Errorf("expected nil request on error, got non-nil")
			}
			if !strings.Contains(err.Error(), "AudienceFormat") {
				t.Errorf("error = %q, want mention of AudienceFormat", err.Error())
			}
		})
	}
}

// TestBuildIdPRequest_AudienceConditional verifies that the "audience" form
// field is present with the correct value when ExchangeInput.Audience is
// non-empty, and absent when it is empty.
func TestBuildIdPRequest_AudienceConditional(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		audience    string
		wantPresent bool
		wantValue   string
	}{
		{"non-empty audience", "https://broker.example.com", true, "https://broker.example.com"},
		{"empty audience", "", false, ""},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := New(validParams())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
				SubjectToken: "tok",
				Audience:     tc.audience,
			})
			if err != nil {
				t.Fatalf("buildIdPRequest: %v", err)
			}

			form := readFormBody(t, req)

			_, present := form["audience"]
			if present != tc.wantPresent {
				t.Errorf("audience present = %v, want %v", present, tc.wantPresent)
			}
			if tc.wantPresent && form.Get("audience") != tc.wantValue {
				t.Errorf("audience = %q, want %q", form.Get("audience"), tc.wantValue)
			}
		})
	}
}

// TestBuildIdPRequest_ScopeConditional verifies that the "scope" form field
// is absent for nil or empty slices, and present with a space-joined value
// (per RFC 6749 §3.3) for non-empty slices.
func TestBuildIdPRequest_ScopeConditional(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scopes      []string
		wantPresent bool
		wantValue   string
	}{
		{"nil scopes", nil, false, ""},
		{"empty slice", []string{}, false, ""},
		{"single scope", []string{"read"}, true, "read"},
		{"multiple scopes", []string{"read", "write"}, true, "read write"},
		{"three scopes", []string{"a", "b", "c"}, true, "a b c"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := New(validParams())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
				SubjectToken: "tok",
				Scopes:       tc.scopes,
			})
			if err != nil {
				t.Fatalf("buildIdPRequest: %v", err)
			}

			form := readFormBody(t, req)

			_, present := form["scope"]
			if present != tc.wantPresent {
				t.Errorf("scope present = %v, want %v", present, tc.wantPresent)
			}
			if tc.wantPresent && form.Get("scope") != tc.wantValue {
				t.Errorf("scope = %q, want %q", form.Get("scope"), tc.wantValue)
			}
		})
	}
}

// TestBuildIdPRequest_ContentTypeAlwaysSet verifies that the Content-Type
// header is "application/x-www-form-urlencoded" regardless of auth method.
func TestBuildIdPRequest_ContentTypeAlwaysSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method ClientAuthMethod
	}{
		{"ClientSecretPost", ClientSecretPost},
		{"ClientSecretBasic", ClientSecretBasic},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validParams()
			p.ClientAuthMethod = tc.method
			e, err := New(p)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
				SubjectToken: "tok",
				Audience:     "aud",
			})
			if err != nil {
				t.Fatalf("buildIdPRequest: %v", err)
			}

			if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", got)
			}
		})
	}
}

// TestBuildIdPRequest_ContextPropagation verifies that the context passed
// to buildIdPRequest is the same context attached to the returned
// *http.Request (pointer identity).
func TestBuildIdPRequest_ContextPropagation(t *testing.T) {
	t.Parallel()

	e, err := New(validParams())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := e.buildIdPRequest(ctx, ExchangeInput{
		SubjectToken: "tok",
		Audience:     "aud",
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	if req.Context() != ctx {
		t.Errorf("req.Context() is not the supplied ctx — context propagation broken")
	}
}

// TestBuildIdPRequest_MalformedTokenURL verifies that a malformed tokenURL
// causes buildIdPRequest to return an error that wraps *url.Error (accessible
// via errors.As) and contains "building IdP request". Direct struct literal
// bypasses New because New does not validate TokenURL format.
func TestBuildIdPRequest_MalformedTokenURL(t *testing.T) {
	t.Parallel()

	e := &Exchanger{
		tokenURL:         "://not-a-valid-url",
		clientID:         "cid",
		clientAuthMethod: ClientSecretPost,
		clientSecret:     "sec",
		grantType:        GrantTypeTokenExchange,
		audienceParam:    AudienceParamAudience,
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		Audience:     "aud",
	})

	if err == nil {
		t.Fatal("expected error for malformed tokenURL, got nil")
	}
	if req != nil {
		t.Errorf("expected nil request on error, got non-nil")
	}
	if !strings.Contains(err.Error(), "building IdP request") {
		t.Errorf("error = %q, want 'building IdP request' prefix", err.Error())
	}
	// The error must be wrapped — the underlying url.Error must be accessible via errors.As.
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("expected wrapped *url.Error via errors.As, got %T: %v", err, err)
	}
}

// TestBuildIdPRequest_MethodIsPOST verifies that the returned request
// uses the POST method.
func TestBuildIdPRequest_MethodIsPOST(t *testing.T) {
	t.Parallel()

	e, err := New(validParams())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		Audience:     "aud",
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	if req.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", req.Method)
	}
}

// TestBuildIdPRequest_URLEqualsTokenURL verifies that the URL on the
// returned request equals the tokenURL set in Params — no path
// manipulation or query-string injection.
func TestBuildIdPRequest_URLEqualsTokenURL(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.TokenURL = "https://idp.example.com/v2/token"
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		Audience:     "aud",
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	if got := req.URL.String(); got != "https://idp.example.com/v2/token" {
		t.Errorf("URL = %q, want %q", got, "https://idp.example.com/v2/token")
	}
}

// TestBuildIdPRequest_BrokerAliasExcluded verifies that
// ExchangeInput.BrokerAlias is a logging label only and never appears in
// the IdP request body under any field name.
func TestBuildIdPRequest_BrokerAliasExcluded(t *testing.T) {
	t.Parallel()

	e, err := New(validParams())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		BrokerAlias:  "my-broker",
		Audience:     "aud",
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	form := readFormBody(t, req)

	for _, key := range []string{"broker_alias", "broker", "alias"} {
		if form.Get(key) != "" {
			t.Errorf("form field %q = %q present, BrokerAlias must not appear in IdP request", key, form.Get(key))
		}
	}
}

// TestBuildIdPRequest_EmptySubjectTokenPassThrough pins the current
// behavior where buildIdPRequest does not validate that SubjectToken is
// non-empty — it passes through to the form body as-is. If a guard is
// added to reject empty SubjectToken, update this test simultaneously.
func TestBuildIdPRequest_EmptySubjectTokenPassThrough(t *testing.T) {
	t.Parallel()

	e, err := New(validParams())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "", // intentionally empty — pins current pass-through behavior
		Audience:     "aud",
	})

	// Current behavior: no error returned for empty SubjectToken.
	if err != nil {
		t.Fatalf("expected no error for empty SubjectToken (current behavior), got: %v", err)
	}
	if req == nil {
		t.Fatal("expected non-nil request for empty SubjectToken")
	}

	form := readFormBody(t, req)

	// subject_token key must be present with empty value — not omitted.
	if _, present := form["subject_token"]; !present {
		t.Errorf("subject_token key absent for empty SubjectToken — expected present with empty value")
	}
	if got := form.Get("subject_token"); got != "" {
		t.Errorf("subject_token = %q, want empty string", got)
	}
}

// TestBuildIdPRequest_ScopeDuplicatesPreserved pins the current behavior
// where buildIdPRequest does not deduplicate scope values — duplicates are
// joined as-is into the "scope" form field.
func TestBuildIdPRequest_ScopeDuplicatesPreserved(t *testing.T) {
	t.Parallel()

	e, err := New(validParams())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		Scopes:       []string{"read", "read", "write"},
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	form := readFormBody(t, req)

	// Current behavior: duplicates are joined as-is, not deduplicated.
	if got := form.Get("scope"); got != "read read write" {
		t.Errorf("scope = %q, want %q (duplicates preserved)", got, "read read write")
	}
}

// TestBuildIdPRequest_BlankScopeElementProducesEmptyValue verifies the edge
// case where Scopes: []string{""} has len == 1, so the scope branch fires
// and strings.Join produces "". The form body contains "scope=" (key present,
// value empty) which is NOT the same as the scope field being absent — some
// IdPs treat an explicit empty scope as a parse error while omitting scope
// causes the IdP to apply its default scopes.
func TestBuildIdPRequest_BlankScopeElementProducesEmptyValue(t *testing.T) {
	t.Parallel()

	e, err := New(validParams())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := e.buildIdPRequest(context.Background(), ExchangeInput{
		SubjectToken: "tok",
		Scopes:       []string{""}, // single blank-string element — enters scope branch
	})
	if err != nil {
		t.Fatalf("buildIdPRequest: %v", err)
	}

	form := readFormBody(t, req)

	// Blank element enters the scope branch — key is present, not omitted.
	if _, present := form["scope"]; !present {
		t.Errorf("scope key absent for Scopes: []string{\"\"} — expected present with empty value (blank element enters scope branch)")
	}
	// Value is empty string (not omitted field).
	if got := form.Get("scope"); got != "" {
		t.Errorf("scope = %q, want empty string (blank element joined to empty)", got)
	}
}
