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
	"testing"
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
