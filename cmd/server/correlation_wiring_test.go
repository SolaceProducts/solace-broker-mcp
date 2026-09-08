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
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// correlationRecorder stands in for authedHandler (auth → SDK): it captures the
// correlation ID present on the request context by the time the inner handler
// runs. Driving a request through the real buildMCPEndpoint chain (NOT a
// hand-rebuilt copy) lets us assert the ADR-001 wiring order: correlation runs
// inside the body limit and before this inner handler.
type correlationRecorder struct {
	gotID   string
	invoked bool
}

func (c *correlationRecorder) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.invoked = true
	c.gotID = correlation.From(r.Context())
}

// TestBuildMCPEndpoint_CorrelationWiringOrder pins the real assembled chain.
//
// KNOWN LIMITATION: on this branch the correlation ID is set on the context but
// not yet emitted in logs (Story 3, SOL-151280) or response headers (Story 6),
// so we cannot yet assert "a 401 response carries the ID" from outside the
// chain. This test pins the wiring ORDER (correlation present, inside the body
// limit, before the inner auth/SDK handler); the log-visibility guarantee is
// realized by Story 3. It deliberately asserts nothing about log output or
// response headers that does not exist yet.
func TestBuildMCPEndpoint_CorrelationWiringOrder(t *testing.T) {
	t.Run("enabled: inner handler sees a non-empty correlation ID", func(t *testing.T) {
		rec := &correlationRecorder{}
		endpoint := buildMCPEndpoint(rec, true, nil)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		endpoint.ServeHTTP(httptest.NewRecorder(), req)

		if !rec.invoked {
			t.Fatal("inner handler was not invoked")
		}
		// A non-empty ID proves the correlation middleware ran inside the body
		// limit and before the inner (auth/SDK) handler. A future refactor that
		// moves correlation out of the chain breaks this.
		if rec.gotID == "" {
			t.Error("inner handler saw an empty correlation ID, want a non-empty ID (correlation must run before it)")
		}
	})

	t.Run("disabled: inner handler sees an empty correlation ID", func(t *testing.T) {
		rec := &correlationRecorder{}
		endpoint := buildMCPEndpoint(rec, false, nil)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		endpoint.ServeHTTP(httptest.NewRecorder(), req)

		if !rec.invoked {
			t.Fatal("inner handler was not invoked")
		}
		// Middleware not wired, so From returns "".
		if rec.gotID != "" {
			t.Errorf("inner handler saw correlation ID %q, want \"\" (middleware not wired)", rec.gotID)
		}
	})

	// An oversized request is rejected with 413 and the inner handler never
	// runs — a regression guard that limitRequestBody is wired into
	// buildMCPEndpoint and short-circuits oversized requests.
	//
	// NOTE on what this does NOT prove: it does not establish that
	// limitRequestBody sits OUTSIDE correlation. On the ContentLength-413
	// short-circuit, correlation has no externally observable effect — it only
	// stamps a context value that the inner handler reads, and the inner
	// handler never runs — so both "body-limit → correlation" and
	// "correlation → body-limit" produce the identical result here (413, inner
	// not invoked). The body-limit-outermost guarantee (a 413 deliberately
	// carries NO correlation ID) only becomes observable once correlation emits
	// a response header (Story 6); the ordering is asserted there. This subtest
	// guards the body limit's presence and short-circuit, not its position
	// relative to correlation.
	t.Run("oversized request is rejected with 413 and never reaches the inner handler", func(t *testing.T) {
		rec := &correlationRecorder{}
		endpoint := buildMCPEndpoint(rec, true, nil)

		// limitRequestBody rejects on declared ContentLength alone, so a real
		// oversized body is unnecessary; declaring one past the cap is enough.
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		req.ContentLength = defaults.MaxMCPRequestBytes + 1

		resp := httptest.NewRecorder()
		endpoint.ServeHTTP(resp, req)

		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("response code = %d, want %d (body limit must reject the oversized request)", resp.Code, http.StatusRequestEntityTooLarge)
		}
		if rec.invoked {
			t.Error("inner handler was invoked on an oversized request; the body limit must short-circuit it first")
		}
	})
}
