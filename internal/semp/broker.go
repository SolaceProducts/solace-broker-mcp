// Package semp manages connections to Solace brokers. It provides BrokerClient
// (per-broker client holder) and BrokerPool (lazy creation and lookup by alias).
// The pool is created at startup with all broker configs but only allocates
// resources for brokers that are actually used.
package semp

import (
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
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
	sempV1Client *sempv1.HTTPClient // SEMPv1 protocol client (concrete for Close)
	sempV2Client *sempv2.HTTPClient // SEMPv2 protocol client (concrete for Close)
	// authenticator is the single Authenticator built for a broker. Both
	// protocol clients already hold this pointer via their Sender. Retained
	// here for future accessors (e.g. health checks, token introspection).
	authenticator auth.Authenticator
	alias         string // broker alias (for error messages)
}

// NewBrokerClient creates a BrokerClient for the given broker configuration.
// It initializes the SEMPv1 and SEMPv2 HTTP clients with the broker's
// connection settings. Both clients share one in-flight semaphore so
// semp.max_concurrent_per_broker caps the broker as a whole, not each
// protocol client separately.
//
// NewBrokerClient is the single builder of per-broker Authenticators. It
// constructs exactly one Authenticator from brokerCfg.Auth and passes the
// same pointer to both protocol clients. Layers below this one never call
// auth.NewAuthenticator.
func NewBrokerClient(alias string, brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) (*BrokerClient, error) {
	jar, err := resilience.NewSafeCookieJar()
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar for broker %q: %w", alias, err)
	}
	authn, err := auth.NewAuthenticator(brokerCfg.Auth, jar)
	if err != nil {
		return nil, fmt.Errorf("creating authenticator for broker %q: %w", alias, err)
	}
	sem := resilience.NewSemaphore(sempCfg.MaxConcurrentPerBroker)
	sempV1Client, err := sempv1.NewHTTPClient(brokerCfg, sempCfg, sem, authn, jar)
	if err != nil {
		return nil, fmt.Errorf("creating SEMPv1 client for broker %q: %w", alias, err)
	}
	sempV2Client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, sem, authn, jar)
	if err != nil {
		return nil, fmt.Errorf("creating SEMPv2 client for broker %q: %w", alias, err)
	}
	return &BrokerClient{
		sempV1Client:  sempV1Client,
		sempV2Client:  sempV2Client,
		authenticator: authn,
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

// Close releases resources held by both protocol clients (rate limiter tickers).
func (b *BrokerClient) Close() {
	b.sempV1Client.Close()
	b.sempV2Client.Close()
}
