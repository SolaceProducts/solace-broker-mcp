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

// End-to-end coverage for the OAuth token-exchange cache wiring introduced
// in SOL-151442. These tests drive tool calls through the top of the stack
// (mgr.CallTool) so any regression in the interplay between OAuthAuthenticator,
// Exchanger, and cache Get/Put surfaces here.
//
// Companion unit tests in internal/tokenexchange/exchange_test.go pin the
// direct Exchanger↔cache contract (hit, miss+store, invalidate, singleflight
// under concurrency); these tests add the wiring proof for the cache-hit path.
package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache/cachetest"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
)

// idpHitCounter installs a JSON handler that increments callCount on each
// request and hands out a distinct access token per call ("cache-tok-1",
// "cache-tok-2", …) with a real 1-hour TTL. The distinct-per-call value
// makes it trivial to prove which response a broker request carried.
func idpHitCounter(callCount *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		body := map[string]any{
			"access_token":      fmt.Sprintf("cache-tok-%d", n),
			"token_type":        "Bearer",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"expires_in":        3600,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

// sempV1OKReply is the minimal successful SEMPv1 envelope: an <rpc-reply>
// carrying an inner <rpc> element and a code="ok" <execute-result>.
const sempV1OKReply = `<rpc-reply><rpc><show><version/></show></rpc><execute-result code="ok"/></rpc-reply>`

// TestOAuthCache_TwoRequestsSameBearerHitIdPOnce is the read-side wiring proof:
// two tool invocations under the same subject-token context must exchange with
// the IdP exactly once. The second call must be served from the exchanger's
// cache without touching the IdP.
//
// If the cache is a no-op (regression: Put silently fails, Get always returns
// miss, or the deduplication key differs between Put and Get), the IdP counter
// climbs to 2 and this test fails.
func TestOAuthCache_TwoRequestsSameBearerHitIdPOnce(t *testing.T) {
	var idpHits atomic.Int32
	fakeIdP := httptest.NewTLSServer(idpHitCounter(&idpHits))
	defer fakeIdP.Close()

	var brokerHits atomic.Int32
	var seenBrokerTokens sync.Map // downstream Authorization header values
	fakeBroker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokerHits.Add(1)
		seenBrokerTokens.Store(r.Header.Get("Authorization"), true)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(sempV1OKReply))
	}))
	defer fakeBroker.Close()

	tc := cachetest.Default(t)
	exchanger, err := tokenexchange.New(tokenexchange.Params{
		TokenURL:         fakeIdP.URL,
		ClientID:         "mcp-server",
		ClientAuthMethod: tokenexchange.ClientSecretBasic,
		ClientSecret:     "fake-secret",
		GrantType:        tokenexchange.GrantTypeTokenExchange,
		AudienceParam:    tokenexchange.AudienceParamAudience,
		HTTPClient:       fakeIdP.Client(),
		Cache:            tc,
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}

	pool, mgr := buildOAuthPoolAndManager(t, fakeIdP.URL, fakeBroker.URL, exchanger)
	defer pool.Close()

	// One subject-token context reused across both invocations — the
	// (subject, broker) key is stable, so the second call must be a cache hit.
	ctx := oauthInjectSubjectToken(t, "shared-jwt-cache-wiring")

	for i := 1; i <= 2; i++ {
		res, err := mgr.CallTool(ctx, "test-oauth-tool", map[string]any{"broker": "oauth-broker"}, tools.Identity{})
		if err != nil {
			t.Fatalf("mgr.CallTool #%d: unexpected error: %v", i, err)
		}
		if res == nil || res.IsError {
			t.Fatalf("mgr.CallTool #%d: got error result: %+v", i, res)
		}
	}

	if got := idpHits.Load(); got != 1 {
		t.Errorf("IdP hits = %d, want 1 (second tool call must be served from the exchanger's cache)", got)
	}
	if got := brokerHits.Load(); got != 2 {
		t.Errorf("broker hits = %d, want 2 (both tool calls should reach the broker)", got)
	}

	// Both broker requests must have carried the SAME downstream Bearer —
	// the value the exchanger stored on the first call.
	var distinct int
	seenBrokerTokens.Range(func(_, _ any) bool {
		distinct++
		return true
	})
	if distinct != 1 {
		t.Errorf("broker saw %d distinct Authorization values, want 1 (cached token should be reused verbatim)", distinct)
	}
}
