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
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache"
	"github.com/sony/gobreaker/v2"
	"golang.org/x/sync/singleflight"
)

// Exchanger executes RFC 8693 token exchange against a single IdP. One
// instance per process, shared by all per-request goroutines.
//
// INVARIANT: every field is written once in New() and never mutated
// afterward, except gatedUntil (an atomic.Int64, safe by construction — see
// raiseGate). Do not assign to any OTHER field from any method; the race
// detector enforces this at test time.
type Exchanger struct {
	tokenURL         string
	clientID         string
	clientAuthMethod ClientAuthMethod
	clientSecret     string
	grantType        GrantType
	audienceParam    AudienceFormat
	httpClient       *http.Client
	// chainDeadline bounds the whole retry chain (attempts + backoffs)
	// on a detached context in Exchange. Per-attempt bound lives on
	// httpClient.Timeout (production wires NewRetryingHTTPClient, which
	// composes NewHTTPClient's SOL-150219 default).
	chainDeadline time.Duration
	cache         cache.TokenCache
	group         singleflight.Group
	nowFunc       func() time.Time
	// breaker is the process-wide circuit breaker guarding the IdP call.
	// Nil means the breaker is disabled (the escape hatch, and the default
	// for tests that don't opt in) — Exchange then calls the IdP directly
	// while retries still apply.
	breaker *gobreaker.CircuitBreaker[*Token]
	// gatedUntil (nowFunc().UnixNano(); 0 = not gated) is a shared,
	// process-wide backoff set on an exhausted 429 chain (see
	// classifyRetryOutcome) and checked in runProtectedExchange. Deliberately
	// NOT keyed per dedup-key or per-broker: it guards the same shared IdP
	// the breaker does, so one broker's throttling paces back every other
	// broker's calls too.
	gatedUntil atomic.Int64
	// maxHonoredRetryAfter caps the gate — see clampRetryAfter.
	maxHonoredRetryAfter time.Duration
}

// New constructs an Exchanger from Params. The config validator
// (internal/config.validateBrokerOAuthConfig) has already enforced
// every non-runtime field at startup, so this constructor only checks
// runtime-wired dependencies the validator cannot see — specifically
// that HTTPClient is non-nil.
//
// Tests that build Params{...} directly without going through FromConfig
// are responsible for supplying valid enum values; bad values surface at
// request-construction time via the switch default branches in the
// wire-building helpers.
func New(p Params) (*Exchanger, error) {
	if p.HTTPClient == nil {
		return nil, errors.New("tokenexchange: HTTPClient is required")
	}
	if p.Cache == nil {
		return nil, errors.New("tokenexchange: Cache is required")
	}
	// Zero is the documented "use defaultMaxHonoredRetryAfter" sentinel; a
	// negative value has no meaning and would otherwise be silently absorbed
	// by clampRetryAfter's <= 0 fallback, masking a caller bug (FromConfig's
	// validateIdPRetryAfter already rejects <= 0 at the YAML layer, so this
	// only guards direct Params construction — tests, or any future caller
	// that bypasses FromConfig).
	if p.MaxHonoredRetryAfter < 0 {
		return nil, errors.New("tokenexchange: MaxHonoredRetryAfter must not be negative")
	}

	// Build the breaker up front so a bad config fails startup rather than
	// surfacing on the first exchange. Nil config leaves breaker nil (disabled).
	var breaker *gobreaker.CircuitBreaker[*Token]
	if p.CircuitBreaker != nil {
		if err := p.CircuitBreaker.Validate(); err != nil {
			return nil, err
		}
		breaker = newTokenExchangeCircuitBreaker(*p.CircuitBreaker)
	}

	return &Exchanger{
		tokenURL:         p.TokenURL,
		clientID:         p.ClientID,
		clientAuthMethod: p.ClientAuthMethod,
		clientSecret:     p.ClientSecret,
		grantType:        p.GrantType,
		audienceParam:    p.AudienceParam,
		httpClient:       p.HTTPClient,
		// Chain deadline is derived from the retry knobs so all timing
		// decisions compose coherently: changing MaxRetries or WaitMax
		// via package defaults updates the chain bound automatically.
		// p.ChainDeadline (zero in production) is the override hook —
		// tests use it to shrink the deadline; a future YAML surface
		// would use it too. See ComputeChainDeadline in defaults.go.
		chainDeadline: ComputeChainDeadline(
			p.ChainDeadline,
			DefaultPerAttemptTimeout,
			DefaultRetryWaitMax,
			DefaultMaxRetries,
		),
		cache:   p.Cache,
		nowFunc: time.Now,
		breaker: breaker,
		// Zero resolves to the shipped default at the point of use (clampRetryAfter).
		maxHonoredRetryAfter: p.MaxHonoredRetryAfter,
	}, nil
}
