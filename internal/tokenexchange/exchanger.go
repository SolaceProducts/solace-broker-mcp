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
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
	"golang.org/x/sync/singleflight"
)

// Exchanger executes RFC 8693 token exchange against a single IdP. One
// instance per process, shared by all per-request goroutines.
//
// INVARIANT: every field is written once in New() and never mutated
// afterward. Do not assign to any field from any method. Concurrency
// safety depends on this — fields are read from hundreds of goroutines
// without synchronization. The race detector enforces this at test time.
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
	}, nil
}
