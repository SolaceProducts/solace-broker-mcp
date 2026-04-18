// Package semp manages connections to Solace brokers. It provides BrokerClient
// (per-broker client holder) and BrokerPool (lazy creation and lookup by alias).
// The pool is created at startup with all broker configs but only allocates
// resources for brokers that are actually used.
package semp

import (
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// BrokerClient holds protocol-specific clients for a single broker. Created
// lazily by BrokerPool on first use. One instance per broker, shared across
// all MCP sessions targeting that broker.
type BrokerClient struct {
	sempV2 sempv2.Client // SEMPv2 protocol client
	alias  string        // broker alias (for error messages)
}

// NewBrokerClient creates a BrokerClient for the given broker configuration.
// It initializes the SEMPv2 HTTP client with the broker's connection settings.
func NewBrokerClient(alias string, brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) (*BrokerClient, error) {
	httpClient, err := sempv2.NewHTTPClient(brokerCfg, sempCfg)
	if err != nil {
		return nil, fmt.Errorf("creating SEMP client for broker %q: %w", alias, err)
	}
	return &BrokerClient{
		sempV2: httpClient,
		alias:  alias,
	}, nil
}

// SempV2 returns the SEMPv2 client for this broker. This is the client that
// gets passed to the composite executor for making SEMP API calls.
func (b *BrokerClient) SempV2() sempv2.Client {
	return b.sempV2
}
