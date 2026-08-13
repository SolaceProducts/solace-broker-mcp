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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// requestHost is the Host that httptest.NewRequest stamps on a request built
// from a path-only target. CrossOriginProtection's Origin fallback compares
// url.Parse(Origin).Host against req.Host, so an Origin built from this value
// is same-origin and any other is foreign.
const requestHost = "example.com"

// TestBuildMCPEndpoint_CrossOriginProtection pins cross-origin behavior through
// the REAL assembled chain rather than the bare protection handler, so the test
// fails if a future refactor drops the wrap or reorders the layers around it.
//
// The cases encode http.CrossOriginProtection.Check semantics
// ($GOROOT/src/net/http/csrf.go): safe methods pass unconditionally;
// Sec-Fetch-Site takes precedence over Origin and only same-origin/none pass;
// with neither header present the request is treated as non-browser and passes.
// That last case is the one every client we ship against relies on, so it is
// asserted for both checked methods.
func TestBuildMCPEndpoint_CrossOriginProtection(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		origin       string
		secFetchSite string
		wantAllowed  bool
	}{
		// Non-browser traffic: neither header. Covers Claude Code, SAM's Python
		// agent backend, and the Go SDK e2e client — the reason the five e2e
		// suites need no fixture configuration for this change.
		{name: "POST with no origin headers", method: http.MethodPost, wantAllowed: true},
		{name: "DELETE with no origin headers", method: http.MethodDelete, wantAllowed: true},

		// Origin fallback, used only when Sec-Fetch-Site is absent.
		{name: "POST with matching Origin", method: http.MethodPost, origin: "http://" + requestHost, wantAllowed: true},
		{name: "POST with foreign Origin", method: http.MethodPost, origin: "https://evil.example.net", wantAllowed: false},
		{name: "DELETE with foreign Origin", method: http.MethodDelete, origin: "https://evil.example.net", wantAllowed: false},

		// Opaque-origin shapes, the ones an Electron-based client could produce.
		// These pin the behaviour we could not confirm against Claude Desktop
		// directly (custom connectors are org-disabled), so a future SDK or
		// stdlib change that alters them fails here instead of in a customer's
		// desktop client. Note the pair: an opaque Origin ALONE is rejected, but
		// the same Origin with a passing Sec-Fetch-Site is allowed — because
		// Sec-Fetch-Site is consulted first and the Origin fallback is then
		// never reached. Any real Chromium/Electron client sends Sec-Fetch-Site,
		// so the second case is the reachable one.
		{name: "POST with opaque Origin null and no Sec-Fetch-Site", method: http.MethodPost, origin: "null", wantAllowed: false},
		{name: "POST with Origin null and Sec-Fetch-Site none", method: http.MethodPost, origin: "null", secFetchSite: "none", wantAllowed: true},
		{name: "POST with a custom-scheme Origin", method: http.MethodPost, origin: "app://claude", wantAllowed: false},
		{name: "POST with a hostless file Origin", method: http.MethodPost, origin: "file://", wantAllowed: false},

		// Sec-Fetch-Site wins over Origin when both are present: a foreign
		// Origin is irrelevant once the browser asserts same-origin, and a
		// matching Origin does not rescue a cross-site fetch.
		{name: "POST with Sec-Fetch-Site same-origin", method: http.MethodPost, secFetchSite: "same-origin", wantAllowed: true},
		{name: "POST with Sec-Fetch-Site none", method: http.MethodPost, secFetchSite: "none", wantAllowed: true},
		{name: "POST with Sec-Fetch-Site cross-site", method: http.MethodPost, secFetchSite: "cross-site", wantAllowed: false},
		{name: "POST with Sec-Fetch-Site same-site is still rejected", method: http.MethodPost, secFetchSite: "same-site", wantAllowed: false},
		{name: "POST with Sec-Fetch-Site same-origin overrides a foreign Origin", method: http.MethodPost, origin: "https://evil.example.net", secFetchSite: "same-origin", wantAllowed: true},
		{name: "POST with Sec-Fetch-Site cross-site overrides a matching Origin", method: http.MethodPost, origin: "http://" + requestHost, secFetchSite: "cross-site", wantAllowed: false},

		// Safe methods are exempt, so the SSE stream is never rejected on
		// origin grounds even from a foreign Origin.
		{name: "GET with foreign Origin", method: http.MethodGet, origin: "https://evil.example.net", wantAllowed: true},
		{name: "GET with Sec-Fetch-Site cross-site", method: http.MethodGet, secFetchSite: "cross-site", wantAllowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &correlationRecorder{}
			endpoint := buildMCPEndpoint(rec, true)

			req := httptest.NewRequestWithContext(context.Background(), tt.method, "/mcp", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.secFetchSite)
			}

			resp := httptest.NewRecorder()
			endpoint.ServeHTTP(resp, req)

			if tt.wantAllowed {
				if !rec.invoked {
					t.Errorf("inner handler was not invoked (response code %d), want the request allowed through", resp.Code)
				}
				return
			}
			if rec.invoked {
				t.Error("inner handler was invoked on a cross-origin request; protection must short-circuit it")
			}
			if resp.Code != http.StatusForbidden {
				t.Errorf("response code = %d, want %d", resp.Code, http.StatusForbidden)
			}
		})
	}
}

// TestTruncateForLog covers the bound on untrusted header values. The deny path
// runs before auth, so these values are unauthenticated input.
func TestTruncateForLog(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty is unchanged", input: "", want: ""},
		{name: "short value is unchanged", input: "https://evil.example.net", want: "https://evil.example.net"},
		{
			name:  "value at the limit is unchanged",
			input: strings.Repeat("a", maxLoggedHeaderBytes),
			want:  strings.Repeat("a", maxLoggedHeaderBytes),
		},
		{
			name:  "value past the limit is cut and marked",
			input: strings.Repeat("a", maxLoggedHeaderBytes+1),
			want:  strings.Repeat("a", maxLoggedHeaderBytes) + "…(truncated)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateForLog(tt.input); got != tt.want {
				t.Errorf("truncateForLog(%d bytes) = %q, want %q", len(tt.input), got, tt.want)
			}
		})
	}
}

// TestBuildMCPEndpoint_CrossOriginRejectionCarriesCorrelationID pins the layer
// order that makes an origin rejection diagnosable: correlation must sit OUTSIDE
// cross-origin protection, so the 403 it writes carries an ID the operator can
// match against the WARN log line. Moving the wrap outside correlation would
// still return 403 — but a 403 with no ID, which this catches.
func TestBuildMCPEndpoint_CrossOriginRejectionCarriesCorrelationID(t *testing.T) {
	t.Run("enabled: 403 carries a correlation ID", func(t *testing.T) {
		endpoint := buildMCPEndpoint(&correlationRecorder{}, true)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		req.Header.Set("Origin", "https://evil.example.net")

		resp := httptest.NewRecorder()
		endpoint.ServeHTTP(resp, req)

		if resp.Code != http.StatusForbidden {
			t.Fatalf("response code = %d, want %d", resp.Code, http.StatusForbidden)
		}
		if got := resp.Header().Get(correlation.HeaderCorrelationID); got == "" {
			t.Errorf("403 response carries no %s header, want a non-empty ID (correlation must wrap cross-origin protection)", correlation.HeaderCorrelationID)
		}
	})

	t.Run("disabled: 403 carries no correlation ID", func(t *testing.T) {
		endpoint := buildMCPEndpoint(&correlationRecorder{}, false)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		req.Header.Set("Origin", "https://evil.example.net")

		resp := httptest.NewRecorder()
		endpoint.ServeHTTP(resp, req)

		// Protection is unconditional, so the 403 stands on its own regardless
		// of whether correlation is wired.
		if resp.Code != http.StatusForbidden {
			t.Errorf("response code = %d, want %d (protection must not depend on correlation being enabled)", resp.Code, http.StatusForbidden)
		}
		if got := resp.Header().Get(correlation.HeaderCorrelationID); got != "" {
			t.Errorf("403 response carries %s = %q, want no header (correlation middleware not wired)", correlation.HeaderCorrelationID, got)
		}
	})
}
