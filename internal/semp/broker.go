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

// Package semp manages connections to Solace brokers. It provides BrokerClient
// (per-broker client holder) and BrokerPool (lazy creation and lookup by alias).
// The pool is created at startup with all broker configs but only allocates
// resources for brokers that are actually used.
package semp

import (
	"fmt"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

// BrokerClient holds protocol-specific clients for a single broker. Created
// lazily by BrokerPool on first use. One instance per broker, shared across
// all MCP sessions targeting that broker.
//
// The authenticator field is the single Authenticator built for this broker.
// SEMPv1 and SEMPv2 receive the same pointer at construction and share it for
// the broker's lifetime — see Decision 7 in
// docs/oauth/token-exchange-SOL-150070/architecture-plan.md for the rationale.
type BrokerClient struct {
	sempV1Client *sempv1.HTTPClient // SEMPv1 protocol client
	sempV2Client *sempv2.HTTPClient // SEMPv2 protocol client
	// authenticator is the single Authenticator built for a broker. Both
	// protocol clients already hold this pointer via their Sender. Retained
	// here for future accessors (e.g. health checks, token introspection).
	authenticator auth.Authenticator
	// rateLimiter is the broker's shared request pacer, held by both protocol
	// clients' Senders. Owned here so Close() is its single stop site.
	rateLimiter *resilience.RateLimiter
	alias       string // broker alias (for error messages)
}

// NewBrokerClient creates a BrokerClient for the given broker configuration.
// It initializes the SEMPv1 and SEMPv2 HTTP clients with the broker's
// connection settings. Both clients share one in-flight semaphore and one
// rate limiter, so semp.max_concurrent_per_broker and
// semp.request_min_interval cap the broker as a whole rather than each
// protocol client separately. Per-client copies of either would silently
// allow 2× the configured value (SOL-150116 for the cap, SOL-152401 for the
// rate).
//
// NewBrokerClient is the single builder of per-broker Authenticators. It
// delegates to newAuthenticator to construct exactly one Authenticator
// from brokerCfg.Auth and passes the same pointer to both protocol clients.
// newAuthenticator is a pure dispatcher: it maps brokerCfg.Auth.Mode to
// the matching Authenticator constructor and passes through the wiring
// dependencies. Precondition invariants (non-nil jar for basic, non-nil
// exchanger for oauth) are owned upstream — by newCookieJar + this
// function's error propagation for jar, and by internal/config
// validateBroker + validateBrokerOAuthConfig + cmd/server/main.go for
// exchanger. See each constructor's docstring for the invariant owner.
//
// exchanger is the process-wide token exchanger for OAuth brokers. Pass
// nil when no broker uses OAuth; the OAuth branch of newAuthenticator
// assumes a non-nil exchanger and does not re-check — the invariant is
// owned by internal/config validateBroker + validateBrokerOAuthConfig
// (which reject an OAuth broker without exchanger coordinates at
// startup) and by main.go, which builds the exchanger whenever
// Hop2OAuthActive() returns true.
func NewBrokerClient(alias string, brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig, exchanger *tokenexchange.Exchanger) (*BrokerClient, error) {
	jar, err := newCookieJar(alias, brokerCfg.Auth.Mode)
	if err != nil {
		return nil, err
	}
	authn, err := newAuthenticator(alias, brokerCfg, jar, exchanger)
	if err != nil {
		return nil, fmt.Errorf("creating authenticator for broker %q: %w", alias, err)
	}
	sem := resilience.NewSemaphore(sempCfg.MaxConcurrentPerBroker)
	limiter := resilience.NewRateLimiter(*sempCfg.RequestMinInterval)
	sempV1Client, err := sempv1.NewHTTPClient(brokerCfg, sempCfg, sem, limiter, authn, jar)
	if err != nil {
		limiter.Stop()
		return nil, fmt.Errorf("creating SEMPv1 client for broker %q: %w", alias, err)
	}
	sempV2Client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, sem, limiter, authn, jar)
	if err != nil {
		limiter.Stop()
		return nil, fmt.Errorf("creating SEMPv2 client for broker %q: %w", alias, err)
	}
	return &BrokerClient{
		sempV1Client:  sempV1Client,
		sempV2Client:  sempV2Client,
		authenticator: authn,
		rateLimiter:   limiter,
		alias:         alias,
	}, nil
}

// SEMPv1 returns the SEMPv1 client for this broker. Tools that need to send
// raw XML commands (e.g., <show><version/></show>) use this client.
func (b *BrokerClient) SEMPv1() sempv1.Client {
	return b.sempV1Client
}

// SEMPv2 returns the SEMPv2 client for this broker. This is the client that
// gets passed to the composite executor for making SEMP API calls.
func (b *BrokerClient) SEMPv2() sempv2.Client {
	return b.sempV2Client
}

// Close releases the broker's shared rate limiter. It is the single stop site:
// the limiter is created once per broker and both protocol clients only read
// from it, so there is nothing per-client left to release. Safe to call more
// than once.
func (b *BrokerClient) Close() {
	b.rateLimiter.Stop()
}

// newCookieJar returns a SafeCookieJar for basic auth brokers, nil for
// all other modes. Only basic auth uses session cookies; other modes
// send credentials on every request and the broker never sets
// Set-Cookie headers.
func newCookieJar(alias string, mode string) (*resilience.SafeCookieJar, error) {
	if mode != config.AuthModeBasic {
		return nil, nil
	}
	jar, err := resilience.NewSafeCookieJar()
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar for broker %q: %w", alias, err)
	}
	return jar, nil
}

// newAuthenticator builds the per-broker Authenticator from the broker's
// auth config. Each mode has its own constructor with mode-specific deps;
// this switch is the single dispatch point so NewBrokerClient stays
// focused on wiring clients.
func newAuthenticator(alias string, brokerCfg *config.BrokerConfig, jar *resilience.SafeCookieJar, exchanger *tokenexchange.Exchanger) (auth.Authenticator, error) {
	cfg := brokerCfg.Auth
	switch cfg.Mode {
	case config.AuthModeBasic:
		return auth.NewBasicAuthenticator(cfg.Username, cfg.Password, jar), nil
	case config.AuthModeBearer:
		return auth.NewBearerAuthenticator(cfg.Token), nil
	case config.AuthModeOAuth:
		return auth.NewOAuthAuthenticator(exchanger, cfg.Audience, alias), nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q for broker %q", cfg.Mode, alias)
	}
}
