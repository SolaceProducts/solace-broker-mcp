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

package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The entry-span middleware must not cost the handler below it the optional
// ResponseWriter interfaces, because the MCP streamable transport needs them:
// notifications are pushed over a long-lived server-sent-event stream, and a
// handler that cannot Flush buffers the whole stream instead of delivering
// each event as it happens. Nothing about that failure is visible from the
// span side — traces would look entirely correct while the client silently
// stopped receiving notifications.
//
// Both paths are covered because they reach the handler differently. GET is
// skipped by the span filter and gets the untouched writer; POST and DELETE
// are traced, so otelhttp re-wraps the writer via httpsnoop — which promises
// to carry these interfaces across, and this pins that promise rather than
// trusting the dependency to keep it across upgrades.
func TestHTTPMiddleware_PreservesOptionalResponseWriterInterfaces(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var (
				invoked         bool
				flusher, hijack bool
			)
			h := HTTPMiddleware("/mcp", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				invoked = true
				_, flusher = w.(http.Flusher)
				_, hijack = w.(http.Hijacker)
			}))

			// A real server, not httptest.NewRecorder: the recorder implements
			// Flusher itself, so it would report success even if the wrapping
			// dropped it from a net/http writer.
			srv := httptest.NewServer(h)
			defer srv.Close()

			req, err := http.NewRequestWithContext(context.Background(), method, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if !invoked {
				t.Fatal("inner handler was not reached")
			}
			if !flusher {
				t.Error("http.Flusher lost at the inner handler: SSE notifications would buffer instead of streaming")
			}
			if !hijack {
				t.Error("http.Hijacker lost at the inner handler")
			}
		})
	}
}

// recordEntrySpans installs a fresh always-sampling provider for this test and
// restores the previous one afterwards. Per test rather than once per binary,
// because provider_test.go in this package also swaps the global provider.
func recordEntrySpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// entrySpanAttributeAllowlist is every attribute key the entry span is allowed
// to carry. otelhttp populates all but correlation_id from the OTel HTTP
// semantic conventions.
//
// An allowlist, not a denylist, because the risk is a key ARRIVING: OTel's HTTP
// semconv has opt-in query-string and header capture, and this repo takes
// Dependabot bumps of otelhttp. A denylist naming Authorization and Cookie
// would keep passing the day a bump starts exporting url.query or a captured
// header under some other name.
var entrySpanAttributeAllowlist = map[string]bool{
	"client.address":               true,
	"correlation_id":               true,
	"http.request.method":          true,
	"http.request.method_original": true,
	"http.response.status_code":    true,
	"http.route":                   true,
	"network.peer.address":         true,
	"network.peer.port":            true,
	"network.protocol.name":        true,
	"network.protocol.version":     true,
	"server.address":               true,
	"server.port":                  true,
	"url.path":                     true,
	"url.scheme":                   true,
	"user_agent.original":          true,
}

// docs/observability.md tells operators, as a data-flow assurance they plan
// reviews around, that the Authorization header, cookies and the URL query
// string are not exported. This is what earns that sentence.
//
// It was prose-only before, which is worse than no documentation: an otelhttp
// minor bump or an OTEL_SEMCONV_STABILITY_OPT_IN change could start exporting
// query strings or headers with nothing failing and the document still
// asserting they are not. The request below deliberately carries a bearer
// token, a session cookie and a query string with a secret-looking parameter,
// so a widened attribute set fails here rather than reaching a collector.
func TestHTTPMiddleware_EntrySpanExportsNoSensitiveRequestData(t *testing.T) {
	sr := recordEntrySpans(t)

	h := HTTPMiddleware("/mcp", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	const (
		bearer     = "Bearer supersecrettoken"
		cookie     = "session=abc123"
		queryParam = "leakcanary"
	)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/mcp?apikey="+queryParam, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Cookie", cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	span := ended[0]

	for _, kv := range span.Attributes() {
		key := string(kv.Key)
		if !entrySpanAttributeAllowlist[key] {
			t.Errorf("entry span carries unexpected attribute %q = %v. If otelhttp began exporting this, confirm it holds nothing sensitive before adding it to entrySpanAttributeAllowlist — and update the data-flow note in docs/observability.md",
				key, kv.Value.AsInterface())
		}
		// Belt and braces: even an allowlisted key must not carry the secrets.
		for _, secret := range []string{"supersecrettoken", "abc123", queryParam} {
			if strings.Contains(kv.Value.String(), secret) {
				t.Errorf("entry span attribute %q leaks request data: %q contains %q", key, kv.Value.String(), secret)
			}
		}
	}

	// url.path must be present and must be the path alone — the query string is
	// the specific thing the assurance is about, and `url.query` is the key
	// OTel semconv would use if capture were ever switched on.
	attrs := map[string]string{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if got := attrs["url.path"]; got != "/mcp" {
		t.Errorf("url.path = %q, want %q", got, "/mcp")
	}
	if _, present := attrs["url.query"]; present {
		t.Error("entry span carries url.query: query-string capture has been enabled somewhere and the data-flow note in docs/observability.md is now false")
	}
}

// client.address comes from X-Forwarded-For verbatim, with no validation and no
// trusted-proxy handling, so it is caller-controllable. network.peer.address is
// the attested transport peer. docs/observability.md has to point forensic use
// at the latter, and this pins the distinction that makes that advice necessary.
func TestHTTPMiddleware_ClientAddressIsCallerControllable(t *testing.T) {
	sr := recordEntrySpans(t)

	h := HTTPMiddleware("/mcp", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	attrs := map[string]string{}
	for _, kv := range ended[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if got := attrs["client.address"]; got != "203.0.113.9" {
		t.Errorf("client.address = %q, want the spoofed X-Forwarded-For value %q — if otelhttp began validating this, the docs note can be softened",
			got, "203.0.113.9")
	}
	if got := attrs["network.peer.address"]; got != "127.0.0.1" {
		t.Errorf("network.peer.address = %q, want the real transport peer %q", got, "127.0.0.1")
	}
}

// The notification-stream exclusion keys on the method AND the Accept header,
// so a GET that is not the SSE stream is still traced. Keying on the method
// alone meant that if the streamable transport ever carried requests over GET,
// tracing would go dark for them silently — no error, no failing test.
func TestHTTPMiddleware_TracesEverythingExceptTheNotificationStream(t *testing.T) {
	for _, tt := range []struct {
		name     string
		method   string
		accept   string
		wantSpan bool
	}{
		// The real MCP shapes, measured from the SDK: the SSE stream is a GET
		// asking for text/event-stream; requests are POSTs asking for both;
		// teardown is a DELETE with no Accept.
		{name: "SSE notification stream", method: http.MethodGet, accept: "text/event-stream", wantSpan: false},
		{name: "request", method: http.MethodPost, accept: "application/json, text/event-stream", wantSpan: true},
		{name: "teardown", method: http.MethodDelete, wantSpan: true},
		// The case the method-only filter got wrong.
		{name: "GET that is not the stream", method: http.MethodGet, accept: "application/json", wantSpan: true},
		{name: "GET with no Accept", method: http.MethodGet, wantSpan: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sr := recordEntrySpans(t)
			srv := httptest.NewServer(HTTPMiddleware("/mcp",
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
			defer srv.Close()

			req, err := http.NewRequestWithContext(context.Background(), tt.method, srv.URL+"/mcp", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if got := len(sr.Ended()) > 0; got != tt.wantSpan {
				t.Errorf("entry span created = %v, want %v (method %s, Accept %q)",
					got, tt.wantSpan, tt.method, tt.accept)
			}
		})
	}
}
