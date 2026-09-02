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
	"context"
	"testing"
)

// TestParseBearerToken_HeaderShapes pins the parse rules: which
// Authorization header values cause parseBearerToken to return the token,
// and which return ("", false). parseBearerToken never rejects on its own —
// rejection is the SDK's responsibility.
func TestParseBearerToken_HeaderShapes(t *testing.T) {
	t.Parallel()
	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.signature"

	cases := []struct {
		name        string
		header      string
		wantToken   string
		wantOK      bool
		description string
	}{
		{"missing header", "", "", false, "no Authorization at all"},
		{"valid Bearer", "Bearer " + jwt, jwt, true, "canonical shape"},
		{"lowercase bearer", "bearer " + jwt, jwt, true, "scheme matched case-insensitively"},
		{"uppercase BEARER", "BEARER " + jwt, jwt, true, "scheme matched case-insensitively"},
		{"mixed case", "BeArEr " + jwt, jwt, true, "scheme matched case-insensitively"},
		{"Bearer alone", "Bearer", "", false, "one field after split"},
		{"Bearer trailing space", "Bearer ", "", false, "trailing whitespace collapses to one field"},
		{"Basic scheme", "Basic dXNlcjpwYXNz", "", false, "wrong scheme"},
		{"DPoP scheme", "DPoP " + jwt, "", false, "wrong scheme"},
		{"token only no scheme", jwt, "", false, "one field"},
		{"three fields", "Bearer " + jwt + " extra", "", false, "extra token"},
		{"extra spaces", "Bearer   " + jwt, jwt, true, "strings.Fields collapses whitespace"},
		{"tab separator", "Bearer\t" + jwt, jwt, true, "strings.Fields handles tabs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotToken, ok := parseBearerToken(tc.header)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (%s)", ok, tc.wantOK, tc.description)
			}
			if gotToken != tc.wantToken {
				t.Errorf("token = %q, want %q (%s)", gotToken, tc.wantToken, tc.description)
			}
		})
	}
}

// TestRawSubjectTokenFromContext_AccessorContract covers the accessor's
// edge cases: nil value, wrong type, empty string. The middleware never
// writes any of these in production, but the accessor folds the checks
// in so callers branch once.
func TestRawSubjectTokenFromContext_AccessorContract(t *testing.T) {
	t.Parallel()

	t.Run("background context has no token", func(t *testing.T) {
		t.Parallel()
		s, ok := RawSubjectTokenFromContext(context.Background())
		if ok || s != "" {
			t.Errorf("got (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("context with non-string value returns not-ok", func(t *testing.T) {
		t.Parallel()
		// Only this package can construct rawSubjectTokenKey{}, so this
		// test is defensive only — guards against a future bug inside
		// this package that stores the wrong type.
		ctx := context.WithValue(context.Background(), rawSubjectTokenKey{}, 42)
		s, ok := RawSubjectTokenFromContext(ctx)
		if ok || s != "" {
			t.Errorf("got (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("context with empty string returns not-ok", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(context.Background(), rawSubjectTokenKey{}, "")
		s, ok := RawSubjectTokenFromContext(ctx)
		if ok || s != "" {
			t.Errorf("got (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("exact round-trip preserves token bytes", func(t *testing.T) {
		t.Parallel()
		const want = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.payload.signature_with-special.chars"
		ctx := WithRawSubjectToken(context.Background(), want)
		got, ok := RawSubjectTokenFromContext(ctx)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
