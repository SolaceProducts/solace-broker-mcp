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

package sempv2_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*sempv2.HTTPClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic"},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	jar, jarErr := resilience.NewSafeCookieJar()
	if jarErr != nil {
		t.Fatalf("NewSafeCookieJar: %v", jarErr)
	}
	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), resilience.NewRateLimiter(0), auth.NewBasicAuthenticator("admin", "secret", jar), jar)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}
	return client, server
}

// newTestClientWithURL builds an HTTPClient pointed at an explicit URL rather
// than a live server — NewHTTPClient does no request I/O, so a case can
// exercise a credentialed baseURL without anything needing to resolve it.
func newTestClientWithURL(t *testing.T, url string, authn auth.Authenticator, jar *resilience.SafeCookieJar) *sempv2.HTTPClient {
	t.Helper()
	brokerCfg := &config.BrokerConfig{
		URL:  url,
		Auth: config.AuthConfig{Mode: "basic"},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), resilience.NewRateLimiter(0), authn, jar)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}
	return client
}

// TestHTTPClient_LogValue_ExcludesCredentials mirrors sempv1's test of the
// same name: slog.Any on an *HTTPClient must expose base_url but never the
// auth credentials, and — the case sempv1's original version didn't cover,
// since building from srv.URL alone carries no userinfo to strip in the
// first place — a credentialed base URL must come out with its host intact
// and its userinfo gone (SOL-152979).
func TestHTTPClient_LogValue_ExcludesCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	basicJar, err := resilience.NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	credentialedURL := strings.Replace(srv.URL, "://", "://embedded-user:embedded-pass@", 1)
	cases := []struct {
		name    string
		authn   auth.Authenticator
		jar     *resilience.SafeCookieJar
		url     string // defaults to srv.URL
		secrets []string
	}{
		{
			name:    "basic auth credentials are not logged",
			authn:   auth.NewBasicAuthenticator("SECRET_USERNAME_VAL", "SECRET_PASSWORD_VAL", basicJar),
			jar:     basicJar,
			secrets: []string{"SECRET_USERNAME_VAL", "SECRET_PASSWORD_VAL"},
		},
		{
			name:    "bearer token is not logged",
			authn:   auth.NewBearerAuthenticator("SECRET_BEARER_TOKEN_VAL"),
			secrets: []string{"SECRET_BEARER_TOKEN_VAL"},
		},
		{
			name:    "credentialed base URL is sanitized",
			authn:   auth.NewBasicAuthenticator("user", "pass", basicJar),
			jar:     basicJar,
			url:     credentialedURL,
			secrets: []string{"embedded-user", "embedded-pass"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := tc.url
			if url == "" {
				url = srv.URL
			}
			client := newTestClientWithURL(t, url, tc.authn, tc.jar)

			var buf bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			defer slog.SetDefault(old)

			slog.Info("broker", slog.Any("client", client))

			out := buf.String()

			if !strings.Contains(out, "base_url") {
				t.Errorf("expected base_url in log output, got: %s", out)
			}
			if !strings.Contains(out, srv.URL) {
				t.Errorf("expected base URL %q in log output, got: %s", srv.URL, out)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(out, secret) {
					t.Errorf("credential %q leaked into log output:\n%s", secret, out)
				}
			}
		})
	}
}

func testOp(method string, params ...sempv2.Parameter) *sempv2.Operation {
	return &sempv2.Operation{
		ID:         "testOp",
		Method:     method,
		Path:       "/SEMP/v2/monitor/msgVpns/{msgVpnName}/queues/{queueName}",
		Parameters: params,
	}
}

// TestClient_Execute_OversizedResponseBody_ReturnsTypedError verifies that a
// broker (or MITM) streaming more than MaxSEMPResponseBytes fails fast with
// ErrResponseTooLarge rather than OOMing the process.
func TestClient_Execute_OversizedResponseBody_ReturnsTypedError(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 64*1024) // 64 KiB
	totalBytes := defaults.MaxSEMPResponseBytes + len(chunk)
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Stream chunks until we've sent more than the cap. Using a streaming
		// write avoids allocating 16+ MiB in a single Go buffer.
		written := 0
		for written < totalBytes {
			n, err := w.Write(chunk)
			if err != nil {
				return
			}
			written += n
		}
	})
	defer server.Close()

	op := testOp("GET",
		sempv2.Parameter{Name: "msgVpnName", In: "path"},
		sempv2.Parameter{Name: "queueName", In: "path"},
	)

	_, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName": "default",
		"queueName":  "test-queue",
	})
	if err == nil {
		t.Fatal("expected error for oversized response body, got nil")
	}
	if !errors.Is(err, resilience.ErrResponseTooLarge) {
		t.Errorf("err = %v, want errors.Is(_, resilience.ErrResponseTooLarge)", err)
	}
}

func TestClient_Execute_Success(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"queueName": "test-queue"}})
	})
	defer server.Close()

	op := testOp("GET",
		sempv2.Parameter{Name: "msgVpnName", In: "path"},
		sempv2.Parameter{Name: "queueName", In: "path"},
	)

	result, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName": "default",
		"queueName":  "test-queue",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if result.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", result.StatusCode)
	}

	data, ok := result.Data["data"].(map[string]any)
	if !ok {
		t.Fatal("result.Data[\"data\"] is not a map")
	}
	if data["queueName"] != "test-queue" {
		t.Errorf("queueName = %v, want test-queue", data["queueName"])
	}
}

func TestClient_Execute_MissingPathParam_ReturnsError(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Should never be reached — buildURL should return an error before any HTTP call.
		t.Error("handler called unexpectedly; buildURL should have errored")
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer server.Close()

	op := testOp("GET",
		sempv2.Parameter{Name: "msgVpnName", In: "path"},
		sempv2.Parameter{Name: "queueName", In: "path"},
	)

	// Provide msgVpnName but omit queueName — {queueName} stays unfilled.
	_, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName": "default",
		// queueName intentionally absent
	})
	if err == nil {
		t.Fatal("expected error for missing path parameter, got nil")
	}
	if !strings.Contains(err.Error(), "{queueName}") {
		t.Errorf("error = %q, expected it to name the missing placeholder {queueName}", err.Error())
	}
	// Error message should include the operation's path template so an operator
	// can correlate the error to the spec.
	if !strings.Contains(err.Error(), op.Path) {
		t.Errorf("error = %q, expected it to include op.Path %q for debuggability", err.Error(), op.Path)
	}
}

// TestClient_Execute_MissingPathParam_DeduplicatedInError ensures a repeated
// placeholder appears only once in the error message even when the template
// references it multiple times.
func TestClient_Execute_MissingPathParam_DeduplicatedInError(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler called unexpectedly; buildURL should have errored")
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		// {vpn} appears twice in the template; should be reported once.
		Path: "/v2/x/{vpn}/y/{vpn}/z",
		Parameters: []sempv2.Parameter{
			{Name: "vpn", In: "path"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing path parameter")
	}
	// The error embeds op.Path (which contains {vpn} twice) plus the
	// deduplicated list of unfilled placeholders. Inspect only the list
	// portion after "unfilled path parameters:" to verify dedupe.
	const marker = "unfilled path parameters:"
	idx := strings.Index(err.Error(), marker)
	if idx < 0 {
		t.Fatalf("error message missing %q section: %q", marker, err.Error())
	}
	listPortion := err.Error()[idx+len(marker):]
	occurrences := strings.Count(listPortion, "{vpn}")
	if occurrences != 1 {
		t.Errorf("expected {vpn} to appear exactly once in the unfilled-params list, got %d (list: %q)", occurrences, listPortion)
	}
}

func TestClient_Execute_AllPathParamsProvided_NoError(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := testOp("GET",
		sempv2.Parameter{Name: "msgVpnName", In: "path"},
		sempv2.Parameter{Name: "queueName", In: "path"},
	)

	_, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName": "default",
		"queueName":  "q1",
	})
	if err != nil {
		t.Fatalf("Execute() with all path params: unexpected error: %v", err)
	}
}

// TestClient_Execute_RejectsUnsafePathParam verifies that empty and dot-segment
// path-parameter values ("", ".", "..") are rejected in buildURL before any HTTP
// request is issued. Because url.PathEscape escapes "/", a path-parameter value
// is always exactly one URL segment, so these three values are the complete set
// that can collapse a targeted request onto an unintended path (e.g. a proxy or
// broker that normalizes dot-segments). A legitimate value containing dots must
// be unaffected.
func TestClient_Execute_RejectsUnsafePathParam(t *testing.T) {
	for _, bad := range []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"single dot", "."},
		{"double dot", ".."},
	} {
		t.Run("rejects "+bad.name, func(t *testing.T) {
			client, server := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("handler called unexpectedly; buildURL should have errored")
				w.WriteHeader(http.StatusInternalServerError)
			})
			defer server.Close()

			op := testOp("DELETE",
				sempv2.Parameter{Name: "msgVpnName", In: "path"},
				sempv2.Parameter{Name: "queueName", In: "path"},
			)

			_, err := client.Execute(context.Background(), op, map[string]any{
				"msgVpnName": "default",
				"queueName":  bad.value,
			})
			if err == nil {
				t.Fatalf("expected error for unsafe path parameter value %q, got nil", bad.value)
			}
			// The error must name the offending parameter so an operator can
			// locate it; the quoted form distinguishes it from the {queueName}
			// placeholder in the path template.
			if !strings.Contains(err.Error(), `path parameter "queueName"`) {
				t.Errorf("error = %q, expected it to name the offending parameter queueName", err.Error())
			}
		})
	}

	t.Run("allows legitimate dotted value", func(t *testing.T) {
		var handlerCalled bool
		client, server := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			handlerCalled = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{})
		})
		defer server.Close()

		op := testOp("GET",
			sempv2.Parameter{Name: "msgVpnName", In: "path"},
			sempv2.Parameter{Name: "queueName", In: "path"},
		)

		_, err := client.Execute(context.Background(), op, map[string]any{
			"msgVpnName": "default",
			"queueName":  "my.queue.v2",
		})
		if err != nil {
			t.Fatalf("Execute() with legitimate dotted value: unexpected error: %v", err)
		}
		if !handlerCalled {
			t.Error("handler was not called; a legitimate dotted value must not be rejected")
		}
	})
}

// TestClient_Execute_EscapesReservedCharInCompositeKeyPath pins the URL built
// for a bridge whose name begins with "#" — the form SEMP gives every
// replication bridge ("#MSGVPN_REPLICATION_BRIDGE") — on the one path template
// in this server whose final segment is a two-part composite key,
// {bridgeName},{bridgeVirtualRouter}.
//
// Two requirements pull in opposite directions here, which is why this is worth
// pinning rather than trusting to url.PathEscape by inspection:
//
//   - The "#" MUST be percent-encoded to %23. Left raw, everything after it is
//     a URL fragment, so the request would hit the bridges *collection* rather
//     than one bridge — and the broker would answer 200 with the wrong body,
//     making the failure silent instead of a 404.
//   - The comma joining the composite key MUST stay a literal comma. It comes
//     from the path template, not from a parameter value, so no escaping should
//     reach it. %2C-encoding it would make the broker read the whole segment as
//     a single bridge name and 404.
//
// get-bridge-status is the only tool that can hit this: bridgeVirtualRouter is
// "auto" on a standalone broker, so both the composite key and the reserved
// bridge name only appear together on an HA pair querying its replication
// bridge. Verified against a live HA broker during SOL-151996 — a GET on
// .../bridges/%23MSGVPN_REPLICATION_BRIDGE,backup returns that bridge.
func TestClient_Execute_EscapesReservedCharInCompositeKeyPath(t *testing.T) {
	var gotEscaped, gotDecoded string
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		gotDecoded = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "getMsgVpnBridge",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/bridges/{bridgeName},{bridgeVirtualRouter}",
		Parameters: []sempv2.Parameter{
			{Name: "msgVpnName", In: "path"},
			{Name: "bridgeName", In: "path"},
			{Name: "bridgeVirtualRouter", In: "path"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName":          "test-dr-active-vpn",
		"bridgeName":          "#MSGVPN_REPLICATION_BRIDGE",
		"bridgeVirtualRouter": "backup",
	})
	if err != nil {
		t.Fatalf("Execute() with reserved-char bridge name: unexpected error: %v", err)
	}

	const wantSegment = "/bridges/%23MSGVPN_REPLICATION_BRIDGE,backup"
	if !strings.Contains(gotEscaped, wantSegment) {
		t.Errorf("escaped path = %q, want it to contain %q", gotEscaped, wantSegment)
	}
	// Guard each half of the requirement separately, so a regression names
	// which one broke rather than just failing the combined match above.
	if strings.Contains(gotEscaped, "/bridges/#") {
		t.Errorf("escaped path = %q: '#' was not encoded, so the bridge name "+
			"becomes a URL fragment and the request targets the collection", gotEscaped)
	}
	if strings.Contains(gotEscaped, "%2C") || strings.Contains(gotEscaped, "%2c") {
		t.Errorf("escaped path = %q: the composite-key comma must stay literal, not %%2C", gotEscaped)
	}
	// The decoded path is what the broker's router matches on: one segment
	// holding the raw bridge name, a comma, then the virtual router.
	const wantDecoded = "/SEMP/v2/monitor/msgVpns/test-dr-active-vpn/bridges/#MSGVPN_REPLICATION_BRIDGE,backup"
	if gotDecoded != wantDecoded {
		t.Errorf("decoded path = %q, want %q", gotDecoded, wantDecoded)
	}
}

func TestClient_Execute_PathParams(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/msgVpns/my-vpn/queues/my-queue") {
			t.Errorf("URL path = %q, expected path params substituted", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := testOp("GET",
		sempv2.Parameter{Name: "msgVpnName", In: "path"},
		sempv2.Parameter{Name: "queueName", In: "path"},
	)

	_, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName": "my-vpn",
		"queueName":  "my-queue",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

// TestPathParamNames covers the extraction PathParamNames does for callers
// outside this package (the composite loader's catalog tests use it to
// verify every declared path parameter is actually wired in a tool's step
// args) — dedup, multiple placeholders, and no placeholders at all.
func TestPathParamNames(t *testing.T) {
	tests := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "two distinct placeholders",
			path: "/msgVpns/{msgVpnName}/topicEndpoints/{topicEndpointName}",
			want: []string{"msgVpnName", "topicEndpointName"},
		},
		{
			name: "no placeholders",
			path: "/msgVpns",
			want: nil,
		},
		{
			name: "repeated placeholder deduplicated",
			path: "/msgVpns/{msgVpnName}/queues/{queueName}/bindCount/{msgVpnName}",
			want: []string{"msgVpnName", "queueName"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sempv2.PathParamNames(tc.path)
			if len(got) != len(tc.want) {
				t.Fatalf("PathParamNames(%q) = %v, want %v", tc.path, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("PathParamNames(%q)[%d] = %q, want %q", tc.path, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestClient_Execute_QueryParams(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("select") != "queueName,spoolUsage" {
			t.Errorf("query select = %q, want queueName,spoolUsage", r.URL.Query().Get("select"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
		Parameters: []sempv2.Parameter{
			{Name: "select", In: "query"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"select": "queueName,spoolUsage",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

// TestClient_Execute_QueryParams_ArrayCSVRawCommas pins the wire format for
// SEMP v2 array query params (select, where): a single param whose value uses
// raw commas, not %2C. The broker treats %2C-encoded commas as part of the
// attribute name and rejects the request with "not a valid attribute".
func TestClient_Execute_QueryParams_ArrayCSVRawCommas(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		if !strings.Contains(raw, "select=clientName,msgVpnName,uptime") {
			t.Errorf("RawQuery = %q, expected raw-comma-joined select=clientName,msgVpnName,uptime", raw)
		}
		if strings.Contains(raw, "%2C") || strings.Contains(raw, "%2c") {
			t.Errorf("RawQuery = %q, expected raw commas in array params, not %%2C", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
		Parameters: []sempv2.Parameter{
			{Name: "select", In: "query", Type: "array"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"select": "clientName,msgVpnName,uptime",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

// TestClient_Execute_QueryParams_ArrayCSV_StringSlice pins the []string input
// path: array params constructed in Go code (not via YAML) must still produce
// raw-comma CSV.
func TestClient_Execute_QueryParams_ArrayCSV_StringSlice(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		if !strings.Contains(raw, "select=a,b,c") {
			t.Errorf("RawQuery = %q, expected select=a,b,c", raw)
		}
		if strings.Contains(raw, "%2C") || strings.Contains(raw, "%2c") {
			t.Errorf("RawQuery = %q, expected raw commas in array params, not %%2C", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
		Parameters: []sempv2.Parameter{
			{Name: "select", In: "query", Type: "array"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"select": []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

// TestClient_Execute_QueryParams_ArrayCSV_AnySlice pins the []any input path:
// YAML unmarshalling produces []any for a list, and that shape must serialize
// the same as []string.
func TestClient_Execute_QueryParams_ArrayCSV_AnySlice(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		if !strings.Contains(raw, "select=a,b,c") {
			t.Errorf("RawQuery = %q, expected select=a,b,c", raw)
		}
		if strings.Contains(raw, "%2C") || strings.Contains(raw, "%2c") {
			t.Errorf("RawQuery = %q, expected raw commas in array params, not %%2C", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
		Parameters: []sempv2.Parameter{
			{Name: "select", In: "query", Type: "array"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"select": []any{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

// TestClient_Execute_QueryParams_ArrayCSV_TrimsAndDropsEmpty pins the
// whitespace-trim and empty-element-drop behavior so a sloppy input like
// "a, b , ,c" still produces a clean "a,b,c" on the wire.
func TestClient_Execute_QueryParams_ArrayCSV_TrimsAndDropsEmpty(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		if !strings.Contains(raw, "select=a,b,c") {
			t.Errorf("RawQuery = %q, expected trimmed select=a,b,c", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
		Parameters: []sempv2.Parameter{
			{Name: "select", In: "query", Type: "array"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"select": "a, b , ,c",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

// TestClient_Execute_QueryParams_ArrayCSV_CoexistsWithRegularParam ensures the
// raw-comma array param and a standard-encoded query param both reach the
// broker in the same request.
func TestClient_Execute_QueryParams_ArrayCSV_CoexistsWithRegularParam(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		if !strings.Contains(raw, "select=a,b") {
			t.Errorf("RawQuery = %q, missing raw-comma select=a,b", raw)
		}
		if r.URL.Query().Get("count") != "100" {
			t.Errorf("count = %q, want 100", r.URL.Query().Get("count"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
		Parameters: []sempv2.Parameter{
			{Name: "select", In: "query", Type: "array"},
			{Name: "count", In: "query"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"select": "a,b",
		"count":  "100",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_RequestBody(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("Method = %s, want PUT", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		if body["replayLogName"] != "replay-log-1" {
			t.Errorf("body.replayLogName = %v, want replay-log-1", body["replayLogName"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "PUT",
		Path:   "/SEMP/v2/action/test",
		Parameters: []sempv2.Parameter{
			{Name: "body", In: "body"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"body": `{"replayLogName": "replay-log-1"}`,
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_BasicAuth(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			t.Fatal("BasicAuth not present on request")
		}
		if user != "admin" || pass != "secret" {
			t.Errorf("BasicAuth = (%q, %q), want (admin, secret)", user, pass)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_404(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"meta":{"error":{"status":"NOT_FOUND"}}}`))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, expected it to contain status code 404", err.Error())
	}
}

func TestClient_Execute_500(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, expected it to contain status code 500", err.Error())
	}
}

func TestClient_Execute_SEMPErrorParsed(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"meta":{"error":{"code":6,"status":"NOT_FOUND","description":"Message VPN Not Found"},"responseCode":404}}`))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "getMsgVpn",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{"msgVpnName": "missing"})
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected *SEMPError, got %T: %v", err, err)
	}

	if sempErr.Description != "Message VPN Not Found" {
		t.Errorf("Description = %q, want %q", sempErr.Description, "Message VPN Not Found")
	}
	if sempErr.SEMPCode != 6 {
		t.Errorf("SEMPCode = %d, want 6", sempErr.SEMPCode)
	}
	if sempErr.SEMPStatus != "NOT_FOUND" {
		t.Errorf("SEMPStatus = %q, want %q", sempErr.SEMPStatus, "NOT_FOUND")
	}
	if !strings.Contains(sempErr.Error(), "Message VPN Not Found") {
		t.Errorf("Error() = %q, want it to contain the Description", sempErr.Error())
	}
}

func TestClient_Execute_SEMPErrorMalformedBody(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<html>Access Denied</html>"))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 403 response")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected *SEMPError, got %T: %v", err, err)
	}

	// Structured fields should be zero when body is not valid JSON.
	if sempErr.Description != "" {
		t.Errorf("Description = %q, want empty for non-JSON body", sempErr.Description)
	}
	if sempErr.SEMPCode != 0 {
		t.Errorf("SEMPCode = %d, want 0 for non-JSON body", sempErr.SEMPCode)
	}
	if sempErr.Body != "<html>Access Denied</html>" {
		t.Errorf("Body = %q, want raw HTML preserved", sempErr.Body)
	}
	// Error() must never render Body: it is unaudited content that may
	// originate from an intermediary (proxy/gateway/WAF), not the broker
	// (SOL-153766). Body stays available as a struct field for callers that
	// need it, but no sink — including Error() — renders it.
	if strings.Contains(sempErr.Error(), "<html>Access Denied</html>") {
		t.Errorf("Error() = %q, must not leak raw Body content", sempErr.Error())
	}
}

func TestClient_Execute_SEMPErrorPartialMeta(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"meta":{"error":{"status":"INVALID_PARAMETER"}}}`))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected *SEMPError, got %T: %v", err, err)
	}

	if sempErr.SEMPStatus != "INVALID_PARAMETER" {
		t.Errorf("SEMPStatus = %q, want %q", sempErr.SEMPStatus, "INVALID_PARAMETER")
	}
	if sempErr.Description != "" {
		t.Errorf("Description = %q, want empty when not in response", sempErr.Description)
	}
	if sempErr.SEMPCode != 0 {
		t.Errorf("SEMPCode = %d, want 0 when not in response", sempErr.SEMPCode)
	}
}

func TestClient_Execute_InvalidJSON(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not json</html>"))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}

	if !strings.Contains(err.Error(), "parsing JSON") {
		t.Errorf("error = %q, expected it to mention JSON parsing", err.Error())
	}
}

func TestClient_Execute_PathEncoding(t *testing.T) {
	cases := []struct {
		name          string
		clientName    string
		wantInPath    string
		wantNotInPath string
	}{
		{
			name:       "plain value unchanged",
			clientName: "my-consumer",
			wantInPath: "/clients/my-consumer",
		},
		{
			name:          "slash encoded",
			clientName:    "app/client-1",
			wantInPath:    "/clients/app%2Fclient-1",
			wantNotInPath: "/clients/app/client-1",
		},
		{
			name:       "space encoded",
			clientName: "my client",
			wantInPath: "/clients/my%20client",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.RequestURI
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{})
			})
			defer server.Close()

			op := &sempv2.Operation{
				ID:     "testOp",
				Method: "GET",
				Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/clients/{clientName}",
				Parameters: []sempv2.Parameter{
					{Name: "msgVpnName", In: "path"},
					{Name: "clientName", In: "path"},
				},
			}

			_, _ = client.Execute(context.Background(), op, map[string]any{
				"msgVpnName": "default",
				"clientName": tc.clientName,
			})

			if !strings.Contains(gotPath, tc.wantInPath) {
				t.Errorf("path = %q, want it to contain %q", gotPath, tc.wantInPath)
			}
			if tc.wantNotInPath != "" && strings.Contains(gotPath, tc.wantNotInPath) {
				t.Errorf("path = %q, must not contain unencoded %q", gotPath, tc.wantNotInPath)
			}
		})
	}
}

func TestClient_Execute_BearerAuth(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify bearer token is sent
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer my-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"meta":{"error":{"status":"UNAUTHORIZED"}}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data": {"status": "ok"}}`))
	})
	_ = client // not using the basic auth client from newTestClient
	defer server.Close()

	// Create a bearer auth client pointing at the same test server.
	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "bearer"},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	bearerClient, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), resilience.NewRateLimiter(0), auth.NewBearerAuthenticator("my-test-token"), nil)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	result, err := bearerClient.Execute(context.Background(), op, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected 200, got %d", result.StatusCode)
	}
}

func TestClient_Execute_CookieJar(t *testing.T) {
	callCount := 0
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if callCount == 0 {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc123"})
		} else {
			cookie, err := r.Cookie("session")
			if err != nil {
				t.Error("cookie not sent back on second request")
			} else if cookie.Value != "abc123" {
				t.Errorf("cookie value = %q, want abc123", cookie.Value)
			}
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
		t.Fatalf("first Execute() error: %v", err)
	}
	if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
		t.Fatalf("second Execute() error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("handler called %d times, want 2", callCount)
	}
}

func TestClient_Execute_UserAgent(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if !strings.HasPrefix(ua, "solace/broker-mcp-server/") {
			t.Errorf("User-Agent = %q, want prefix solace/broker-mcp-server/", ua)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_NoBody_NoContentType(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			t.Errorf("Content-Type = %q, want empty for GET request with no body", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_AcceptHeader(t *testing.T) {
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if accept := r.Header.Get("Accept"); accept != "application/json" {
					t.Errorf("Accept = %q, want application/json", accept)
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{})
			})
			defer server.Close()

			op := &sempv2.Operation{
				ID:     "testOp",
				Method: method,
				Path:   "/SEMP/v2/monitor/test",
			}

			if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
				t.Fatalf("Execute() method=%s error: %v", method, err)
			}
		})
	}
}

func TestClient_Execute_Timeout(t *testing.T) {
	_, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	})
	defer server.Close()

	// Create a client with a very short timeout.
	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic"},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	jar, jarErr := resilience.NewSafeCookieJar()
	if jarErr != nil {
		t.Fatalf("NewSafeCookieJar: %v", jarErr)
	}
	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), resilience.NewRateLimiter(0), auth.NewBasicAuthenticator("admin", "secret", jar), jar)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, execErr := client.Execute(context.Background(), op, map[string]any{})
	if execErr == nil {
		t.Fatal("expected timeout error")
	}
}

// TestClient_TransportPool_ReusesConnections proves the transport pool is sized
// to hold every connection opened during a burst of MaxConcurrentPerBroker
// requests, so a subsequent burst reuses them all without re-handshaking.
//
// Without the fix, http.Transport's default MaxIdleConnsPerHost=2 would close
// 8 of the 10 connections after batch 1, forcing batch 2 to open 8 fresh
// TCP+TLS connections. The test counts distinct connections via the server's
// ConnState callback (StateNew) and asserts batch 2 opens zero new ones.
func TestClient_TransportPool_ReusesConnections(t *testing.T) {
	const concurrency = 10

	var newConns atomic.Int32

	// batch coordinates the in-flight requests of a single round: each request
	// signals arrival and then blocks on release, so all `concurrency` requests
	// are forced to occupy distinct TCP connections simultaneously rather than
	// pipelining over one keep-alive socket.
	type batch struct {
		arrived chan struct{}
		release chan struct{}
	}
	var current atomic.Pointer[batch]

	handler := func(w http.ResponseWriter, _ *http.Request) {
		bs := current.Load()
		bs.arrived <- struct{}{}
		<-bs.release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic"},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
		MaxConcurrentPerBroker: concurrency,
	}
	jar, jarErr := resilience.NewSafeCookieJar()
	if jarErr != nil {
		t.Fatalf("NewSafeCookieJar: %v", jarErr)
	}
	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), resilience.NewRateLimiter(0), auth.NewBasicAuthenticator("admin", "secret", jar), jar)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	op := &sempv2.Operation{ID: "testOp", Method: "GET", Path: "/SEMP/v2/monitor/test"}

	runBatch := func() {
		bs := &batch{
			arrived: make(chan struct{}, concurrency),
			release: make(chan struct{}),
		}
		current.Store(bs)
		var wg sync.WaitGroup
		for range concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, execErr := client.Execute(context.Background(), op, map[string]any{}); execErr != nil {
					t.Errorf("Execute: %v", execErr)
				}
			}()
		}
		for range concurrency {
			<-bs.arrived
		}
		close(bs.release)
		wg.Wait()
	}

	runBatch()
	afterBatch1 := newConns.Load()

	// Give clients time to return connections to the idle pool before batch 2.
	time.Sleep(50 * time.Millisecond)

	runBatch()
	afterBatch2 := newConns.Load()

	if afterBatch1 != concurrency {
		t.Errorf("batch 1: expected exactly %d new connections, got %d", concurrency, afterBatch1)
	}
	if afterBatch2 != afterBatch1 {
		t.Errorf("batch 2: expected zero new connections (full pool reuse), but %d new connections opened (total: %d → %d). "+
			"This usually means MaxIdleConnsPerHost is smaller than the concurrency cap.",
			afterBatch2-afterBatch1, afterBatch1, afterBatch2)
	}
}

// TestClient_TLSWiring verifies InsecureSkipVerify threads through
// NewHTTPClient into the transport by driving a real TLS handshake against
// httptest.NewTLSServer's self-signed cert.
func TestClient_TLSWiring(t *testing.T) {
	newTLSClient := func(t *testing.T, srv *httptest.Server, insecure bool) *sempv2.HTTPClient {
		t.Helper()
		brokerCfg := &config.BrokerConfig{
			URL:                srv.URL,
			Auth:               config.AuthConfig{Mode: "basic"},
			InsecureSkipVerify: insecure,
		}
		retries := 0
		minInterval := time.Duration(0)
		sempCfg := &config.SEMPConfig{
			RequestTimeoutDuration: 2 * time.Second,
			Retries:                &retries,
			RequestMinInterval:     &minInterval,
		}
		jar, err := resilience.NewSafeCookieJar()
		if err != nil {
			t.Fatalf("NewSafeCookieJar: %v", err)
		}
		client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), resilience.NewRateLimiter(0), auth.NewBasicAuthenticator("admin", "secret", jar), jar)
		if err != nil {
			t.Fatalf("NewHTTPClient: %v", err)
		}
		return client
	}

	op := &sempv2.Operation{
		ID:     "getMsgVpn",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}",
		Parameters: []sempv2.Parameter{
			{Name: "msgVpnName", In: "path"},
		},
	}

	t.Run("verification on rejects self-signed cert", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}, "meta": map[string]any{}})
		}))
		defer srv.Close()

		client := newTLSClient(t, srv, false)
		_, err := client.Execute(context.Background(), op, map[string]any{"msgVpnName": "default"})
		if err == nil {
			t.Fatal("expected TLS verification error, got nil")
		}
		var unknownCA x509.UnknownAuthorityError
		if !errors.As(err, &unknownCA) {
			t.Errorf("err = %v, want errors.As(_, *x509.UnknownAuthorityError)", err)
		}
	})

	t.Run("verification skipped accepts self-signed cert", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}, "meta": map[string]any{}})
		}))
		defer srv.Close()

		client := newTLSClient(t, srv, true)
		if _, err := client.Execute(context.Background(), op, map[string]any{"msgVpnName": "default"}); err != nil {
			t.Fatalf("Execute with InsecureSkipVerify=true: unexpected error: %v", err)
		}
	})
}
