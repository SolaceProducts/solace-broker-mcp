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

package correlation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runMiddleware sends a request carrying the given headers through Middleware
// and returns the correlation ID observed by the downstream handler.
// nextInvoked reports whether the wrapped handler actually ran (it always must:
// the middleware never rejects).
func runMiddleware(t *testing.T, headers map[string]string) (gotID string, nextInvoked bool) {
	t.Helper()
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextInvoked = true
		gotID = From(r.Context())
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return gotID, nextInvoked
}

const validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
const validTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

// TestMiddleware_TraceparentExtraction pins that a well-formed traceparent
// yields its 32-hex trace-id segment as the correlation ID.
func TestMiddleware_TraceparentExtraction(t *testing.T) {
	t.Parallel()
	gotID, nextInvoked := runMiddleware(t, map[string]string{headerTraceparent: validTraceparent})
	if !nextInvoked {
		t.Fatal("next handler was not invoked")
	}
	if gotID != validTraceID {
		t.Errorf("correlation ID = %q, want trace-id %q", gotID, validTraceID)
	}
}

// TestMiddleware_FutureVersionTraceparentToleratesTrailingFields pins forward
// compatibility: an unrecognized (future) version may carry extra trailing
// fields and still yields its trace-id. Only the known version 00 is held to
// exactly four fields (see TestMiddleware_MalformedTraceparentFallsBack).
func TestMiddleware_FutureVersionTraceparentToleratesTrailingFields(t *testing.T) {
	t.Parallel()
	tp := "01-" + validTraceID + "-00f067aa0ba902b7-01-extrafield"
	gotID, _ := runMiddleware(t, map[string]string{headerTraceparent: tp})
	if gotID != validTraceID {
		t.Errorf("correlation ID = %q, want trace-id %q from future-version traceparent", gotID, validTraceID)
	}
}

// TestMiddleware_TraceparentPrecedence pins that a valid traceparent wins over
// a present X-Correlation-ID.
func TestMiddleware_TraceparentPrecedence(t *testing.T) {
	t.Parallel()
	gotID, _ := runMiddleware(t, map[string]string{
		headerTraceparent:   validTraceparent,
		headerCorrelationID: "client-supplied-id",
	})
	if gotID != validTraceID {
		t.Errorf("correlation ID = %q, want traceparent trace-id %q to win", gotID, validTraceID)
	}
}

// TestMiddleware_MalformedTraceparentFallsBack pins that every malformed
// traceparent shape falls back to a present, valid X-Correlation-ID.
func TestMiddleware_MalformedTraceparentFallsBack(t *testing.T) {
	t.Parallel()
	const fallback = "fallback-correlation-id"
	cases := []struct {
		name        string
		traceparent string
	}{
		{"too few fields", "00-" + validTraceID + "-00f067aa0ba902b7"},
		{"empty", ""},
		{"trace-id too short", "00-4bf92f3577b34da6-00f067aa0ba902b7-01"},
		{"trace-id too long", "00-" + validTraceID + "ab-00f067aa0ba902b7-01"},
		{"non-hex trace-id", "00-zzf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{"uppercase trace-id", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"},
		{"all-zero trace-id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
		{"reserved version ff", "ff-" + validTraceID + "-00f067aa0ba902b7-01"},
		{"bad version length", "0-" + validTraceID + "-00f067aa0ba902b7-01"},
		{"bad span length", "00-" + validTraceID + "-00f067aa-01"},
		{"bad flags length", "00-" + validTraceID + "-00f067aa0ba902b7-1"},
		{"trailing fields on version 00", validTraceparent + "-extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotID, _ := runMiddleware(t, map[string]string{
				headerTraceparent:   tc.traceparent,
				headerCorrelationID: fallback,
			})
			if gotID != fallback {
				t.Errorf("correlation ID = %q, want fallback %q", gotID, fallback)
			}
		})
	}
}

// TestMiddleware_CorrelationIDEchoed pins that a valid X-Correlation-ID is
// echoed verbatim when no traceparent is present.
func TestMiddleware_CorrelationIDEchoed(t *testing.T) {
	t.Parallel()
	const id = "abc-123_XYZ.456"
	gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: id})
	if gotID != id {
		t.Errorf("correlation ID = %q, want %q echoed", gotID, id)
	}
}

// TestMiddleware_CorrelationIDTrimmed pins that surrounding whitespace is
// trimmed from an otherwise-valid X-Correlation-ID.
func TestMiddleware_CorrelationIDTrimmed(t *testing.T) {
	t.Parallel()
	gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: "  padded-id  "})
	if gotID != "padded-id" {
		t.Errorf("correlation ID = %q, want trimmed %q", gotID, "padded-id")
	}
}

// TestMiddleware_GeneratesWhenNeitherPresent pins that, with no usable inbound
// header, the middleware generates a valid UUIDv7.
func TestMiddleware_GeneratesWhenNeitherPresent(t *testing.T) {
	t.Parallel()
	gotID, _ := runMiddleware(t, nil)
	if !isUUIDv7(gotID) {
		t.Errorf("correlation ID = %q, want a valid UUIDv7", gotID)
	}
}

// TestMiddleware_InvalidInputsGenerate pins that the full invalid-input domain
// for X-Correlation-ID (with no traceparent) causes a generated UUIDv7 rather
// than echoing or sanitizing the bad value.
func TestMiddleware_InvalidInputsGenerate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"whitespace only", "   \t  "},
		{"oversized", strings.Repeat("a", maxIDLen+1)},
		{"CR injection", "id\rinjected"},
		{"LF injection", "id\ninjected"},
		{"CRLF header split", "id\r\nX-Evil: 1"},
		{"NUL byte", "id\x00bad"},
		{"tab", "id\tbad"},
		{"DEL char", "id\x7fbad"},
		{"non-ascii", "idÃ© bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: tc.value})
			if !isUUIDv7(gotID) {
				t.Errorf("for input %q: correlation ID = %q, want a generated UUIDv7 (bad input must not be echoed)", tc.value, gotID)
			}
		})
	}
}

// TestMiddleware_BoundaryLength pins the maxIDLen boundary: exactly maxIDLen is
// accepted, maxIDLen+1 is rejected (and thus generated).
func TestMiddleware_BoundaryLength(t *testing.T) {
	t.Parallel()
	atLimit := strings.Repeat("a", maxIDLen)
	gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: atLimit})
	if gotID != atLimit {
		t.Errorf("at-limit (%d chars) correlation ID = %q, want it echoed", maxIDLen, gotID)
	}
}

// TestGenerate_ValidUUIDv7 pins Generate's output shape and uniqueness.
func TestGenerate_ValidUUIDv7(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := Generate()
		if !isUUIDv7(id) {
			t.Fatalf("Generate() = %q, not a valid UUIDv7", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("Generate() produced a duplicate: %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestFromHeader pins the no-generate contract used by RequestExtraMiddleware:
// usable inbound headers yield an ID; missing or unusable headers yield ok=false
// rather than a fresh UUIDv7.
func TestFromHeader(t *testing.T) {
	t.Parallel()
	t.Run("nil header", func(t *testing.T) {
		t.Parallel()
		if id, ok := FromHeader(nil); ok || id != "" {
			t.Errorf("FromHeader(nil) = %q, %v; want \"\", false", id, ok)
		}
	})
	t.Run("empty header", func(t *testing.T) {
		t.Parallel()
		if id, ok := FromHeader(http.Header{}); ok || id != "" {
			t.Errorf("FromHeader(empty) = %q, %v; want \"\", false", id, ok)
		}
	})
	t.Run("X-Correlation-ID", func(t *testing.T) {
		t.Parallel()
		h := make(http.Header)
		h.Set(headerCorrelationID, "client-id")
		id, ok := FromHeader(h)
		if !ok || id != "client-id" {
			t.Errorf("FromHeader(X-Correlation-ID) = %q, %v; want client-id, true", id, ok)
		}
	})
	t.Run("traceparent wins", func(t *testing.T) {
		t.Parallel()
		h := make(http.Header)
		h.Set(headerTraceparent, validTraceparent)
		h.Set(headerCorrelationID, "client-id")
		id, ok := FromHeader(h)
		if !ok || id != validTraceID {
			t.Errorf("FromHeader(traceparent) = %q, %v; want %q, true", id, ok, validTraceID)
		}
	})
	t.Run("invalid X-Correlation-ID", func(t *testing.T) {
		t.Parallel()
		h := make(http.Header)
		h.Set(headerCorrelationID, "id\ninjected")
		if id, ok := FromHeader(h); ok {
			t.Errorf("FromHeader(invalid) = %q, true; want false (must not generate)", id)
		}
	})
}

// TestFrom_Absent pins that From returns "" when no ID is on the context —
// the contract dependent code relies on when the capability is off (middleware
// not wired).
func TestFrom_Absent(t *testing.T) {
	t.Parallel()
	if got := From(context.Background()); got != "" {
		t.Errorf("From(background) = %q, want \"\"", got)
	}
}

// TestWithFrom_RoundTrip pins the With/From round-trip.
func TestWithFrom_RoundTrip(t *testing.T) {
	t.Parallel()
	const id = "round-trip-id"
	ctx := With(context.Background(), id)
	if got := From(ctx); got != id {
		t.Errorf("From(With(ctx, %q)) = %q, want %q", id, got, id)
	}
}

// TestMiddleware_DoesNotMutateParentContext pins that the stamped ID lands only
// on the derived context handed to next, not on the original request context.
func TestMiddleware_DoesNotMutateParentContext(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set(headerCorrelationID, "some-id")
	parentCtx := req.Context()

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := From(parentCtx); got != "" {
		t.Errorf("parent context was polluted: From = %q, want \"\"", got)
	}
}

// TestMiddleware_EndToEndPropagation is the httptest.Server integration test:
// it proves the correlation ID survives the full HTTP round-trip and reaches a
// real downstream handler's context.
func TestMiddleware_EndToEndPropagation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, From(r.Context()))
	})))
	defer srv.Close()

	// Subtests are intentionally NOT parallel: a parallel subtest defers its
	// body until the parent returns, by which point defer srv.Close() has
	// already shut the server down.
	t.Run("traceparent propagates end to end", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(headerTraceparent, validTraceparent)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != validTraceID {
			t.Errorf("downstream saw correlation ID %q, want %q", string(body), validTraceID)
		}
	})

	t.Run("generated when no header", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !isUUIDv7(string(body)) {
			t.Errorf("downstream saw %q, want a generated UUIDv7", string(body))
		}
	})
}

// TestMiddleware_StampsResolvedIDOnInboundHeader pins that the resolved ID is
// written onto the inbound X-Correlation-ID before next runs, equal to the
// response echo. That is how Extra.Header sees a generated ID (SOL-153935).
// A client-supplied header is replaced with the sanitized resolved value.
// Authorization is left unchanged.
func TestMiddleware_StampsResolvedIDOnInboundHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"omitted both headers", nil},
		{"from X-Correlation-ID", map[string]string{headerCorrelationID: "client-supplied-id"}},
		{"trimmed X-Correlation-ID", map[string]string{headerCorrelationID: "  padded-id  "}},
		{"from traceparent", map[string]string{headerTraceparent: validTraceparent}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const authz = "Bearer leave-me-alone"
			var inboundID, ctxID string
			var inboundAuth string
			handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inboundID = r.Header.Get(headerCorrelationID)
				inboundAuth = r.Header.Get("Authorization")
				ctxID = From(r.Context())
			}))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			req.Header.Set("Authorization", authz)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			echoed := rec.Header().Get(headerCorrelationID)
			if echoed == "" {
				t.Fatalf("response %s is empty", headerCorrelationID)
			}
			if inboundID != echoed {
				t.Errorf("inbound %s = %q, want the echoed response value %q", headerCorrelationID, inboundID, echoed)
			}
			if inboundID != ctxID {
				t.Errorf("inbound %s = %q, want the handler ctx ID %q", headerCorrelationID, inboundID, ctxID)
			}
			if inboundAuth != authz {
				t.Errorf("inbound Authorization = %q, want %q (must not be mutated)", inboundAuth, authz)
			}
			if tc.headers == nil && !isUUIDv7(inboundID) {
				t.Errorf("generated inbound ID = %q, want a UUIDv7", inboundID)
			}
		})
	}
}

// TestMiddleware_SetsResponseHeader pins that Middleware echoes the resolved
// correlation ID back to the caller in the X-Correlation-ID response header,
// and that the header value is exactly the ID the downstream handler observes
// on its context (SOL-151282). It covers all three resolution sources:
// traceparent, a client-supplied X-Correlation-ID, and a generated UUIDv7.
func TestMiddleware_SetsResponseHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"from traceparent", map[string]string{headerTraceparent: validTraceparent}},
		{"from X-Correlation-ID", map[string]string{headerCorrelationID: "client-supplied-id"}},
		{"generated", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seenID string
			handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenID = From(r.Context())
			}))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			gotHeader := rec.Header().Get(headerCorrelationID)
			if gotHeader == "" {
				t.Fatalf("response %s header is empty, want the resolved correlation ID", headerCorrelationID)
			}
			if gotHeader != seenID {
				t.Errorf("response %s header = %q, want it to equal the ID the handler saw %q",
					headerCorrelationID, gotHeader, seenID)
			}
		})
	}
}

// TestMiddleware_ResponseHeaderSetBeforeHandlerWrites pins that the response
// header is set before next writes the body. A header set after the handler
// has already flushed status/body is silently dropped by net/http, so this
// guards the ordering: even when the inner handler writes a body, the header
// survives.
func TestMiddleware_ResponseHeaderSetBeforeHandlerWrites(t *testing.T) {
	t.Parallel()
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(headerCorrelationID); got == "" {
		t.Errorf("response %s header is empty after the handler wrote a body; it must be set before next.ServeHTTP", headerCorrelationID)
	}
}

// isUUIDv7 reports whether s is a canonical lowercase UUID with version nibble
// 7 and RFC 4122/9562 variant bits (10xx). It is a test-only structural check.
func isUUIDv7(s string) bool {
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	// Version nibble sits at index 14; variant high bits at index 19.
	if s[14] != '7' {
		return false
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
		return true
	default:
		return false
	}
}
