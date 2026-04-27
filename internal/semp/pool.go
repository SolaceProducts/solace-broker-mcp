package semp

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// BrokerPool manages BrokerClient instances for all configured brokers. It is
// created at startup with broker configs from the YAML configuration file.
// BrokerClients are created lazily on first GetSEMPv1() or GetSEMPv2() call
// configured, only active brokers allocate HTTP clients and resources.
// Thread-safe via sync.RWMutex with a double-check pattern for lazy creation.
type BrokerPool struct {
	mu      sync.RWMutex
	clients map[string]*BrokerClient        // broker alias → client (lazily populated)
	configs map[string]*config.BrokerConfig // broker alias → config (all brokers)
	sempCfg *config.SEMPConfig              // shared SEMP settings
}

// NewBrokerPool creates a BrokerPool from the server configuration. No
// BrokerClients are allocated — they are created lazily on first access.
func NewBrokerPool(cfg *config.ServerConfig) *BrokerPool {
	return &BrokerPool{
		clients: make(map[string]*BrokerClient),
		configs: cfg.Brokers,
		sempCfg: &cfg.SEMP,
	}
}

// getOrCreate returns the BrokerClient for alias, creating it on first access.
// Uses a double-checked locking pattern: a fast path under an RLock for the
// common case where the client already exists, and a slow path under a write
// lock for lazy creation. The second check after acquiring the write lock
// handles the race where two goroutines both saw "not cached" during the fast
// path. The log line is emitted exactly once per alias inside the creation
// branch — both GetSEMPv1 and GetSEMPv2 delegate here, so neither accessor
// can trigger a duplicate log.
func (p *BrokerPool) getOrCreate(alias string) (*BrokerClient, error) {
	p.mu.RLock()
	if client, ok := p.clients[alias]; ok {
		p.mu.RUnlock()
		return client, nil
	}
	p.mu.RUnlock()

	// Slow path: write lock for lazy creation with double-check.
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clients[alias]; ok {
		return client, nil
	}

	cfg, ok := p.configs[alias]
	if !ok {
		return nil, fmt.Errorf("unknown broker: %q", alias)
	}

	client, err := NewBrokerClient(alias, cfg, p.sempCfg)
	if err != nil {
		return nil, err
	}
	p.clients[alias] = client
	slog.Info("broker connection created",
		slog.String("broker", alias),
		slog.String("url", cfg.URL),
		slog.String("auth_mode", cfg.Auth.Mode))
	return client, nil
}

// GetSEMPv1 returns the SEMPv1 client for the broker identified by alias. The
// underlying BrokerClient is created lazily on the first call for a given
// alias and reused for all subsequent calls. Returns an error if the alias
// is not in the configured broker map.
func (p *BrokerPool) GetSEMPv1(alias string) (sempv1.Client, error) {
	client, err := p.getOrCreate(alias)
	if err != nil {
		return nil, err
	}
	return client.SEMPv1(), nil
}

// GetSEMPv2 returns the SEMPv2 client for the broker identified by alias. The
// underlying BrokerClient is created lazily on the first call for a given
// alias and reused for all subsequent calls. Returns an error if the alias
// is not in the configured broker map.
func (p *BrokerPool) GetSEMPv2(alias string) (sempv2.Client, error) {
	client, err := p.getOrCreate(alias)
	if err != nil {
		return nil, err
	}
	return client.SEMPv2(), nil
}

// Aliases returns all configured broker aliases in sorted order. This includes
// all brokers from the configuration, not just those that have been accessed.
func (p *BrokerPool) Aliases() []string {
	aliases := make([]string, 0, len(p.configs))
	for alias := range p.configs {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}
