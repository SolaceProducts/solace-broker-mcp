package semp

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// ErrUnknownBroker is returned by GetSEMPv1, GetSEMPv2, and the underlying
// getOrCreate when the requested alias is not in the configured broker map.
// Callers branch on this with errors.Is to distinguish a user-supplied
// unknown alias (caller's mistake — should list available aliases) from
// transport or construction failures (server-side issue — should preserve
// the underlying error).
var ErrUnknownBroker = errors.New("unknown broker")

// BrokerPool manages BrokerClient instances for all configured brokers. It is
// created at startup with broker configs from the YAML configuration file.
// BrokerClients are created lazily on the first GetSEMPv1() or GetSEMPv2()
// call for a given alias, so only brokers that are actually used allocate
// HTTP clients and resources. Thread-safe via sync.RWMutex with a
// double-check pattern for lazy creation.
type BrokerPool struct {
	mu sync.RWMutex
	// clients is keyed by canonical (lowercase) alias. Use clientFor/setClient
	// for all access — direct map reads/writes bypass case-folding and will
	// silently miss when callers pass mixed-case aliases.
	clients map[string]*BrokerClient
	// configs is keyed by canonical (lowercase) alias. Use configFor for all
	// access — direct map reads bypass case-folding and will silently miss
	// when callers pass mixed-case aliases.
	configs map[string]*config.BrokerConfig
	sempCfg *config.SEMPConfig // shared SEMP settings
}

// configFor returns the BrokerConfig for alias (any case), or false if unknown.
// All map access on p.configs MUST go through this helper.
func (p *BrokerPool) configFor(alias string) (*config.BrokerConfig, bool) {
	cfg, ok := p.configs[strings.ToLower(alias)]
	return cfg, ok
}

// clientFor returns the cached BrokerClient for alias (any case), or false.
// All reads on p.clients MUST go through this helper.
func (p *BrokerPool) clientFor(alias string) (*BrokerClient, bool) {
	c, ok := p.clients[strings.ToLower(alias)]
	return c, ok
}

// setClient stores a newly-created BrokerClient under the canonical key.
// All writes to p.clients MUST go through this helper.
func (p *BrokerPool) setClient(alias string, c *BrokerClient) {
	p.clients[strings.ToLower(alias)] = c
}

// NewBrokerPool creates a BrokerPool from the server configuration. No
// BrokerClients are allocated — they are created lazily on first access.
func NewBrokerPool(cfg *config.ServerConfig) *BrokerPool {
	return &BrokerPool{
		clients: make(map[string]*BrokerClient),
		configs: cfg.Brokers(),
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
	if client, ok := p.clientFor(alias); ok {
		p.mu.RUnlock()
		return client, nil
	}
	p.mu.RUnlock()

	// Slow path: write lock for lazy creation with double-check.
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clientFor(alias); ok {
		return client, nil
	}

	cfg, ok := p.configFor(alias)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBroker, alias)
	}

	client, err := NewBrokerClient(cfg.DisplayName(), cfg, p.sempCfg)
	if err != nil {
		return nil, err
	}
	p.setClient(alias, client)
	slog.Info("broker connection created",
		slog.String("broker", cfg.DisplayName()),
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

// BrokerConfig returns the resolved *config.BrokerConfig for alias (any case),
// or false if the alias is not configured. Intended for callers that need the
// canonical display name after a successful GetSEMPv1/GetSEMPv2 lookup.
func (p *BrokerPool) BrokerConfig(alias string) (*config.BrokerConfig, bool) {
	return p.configFor(alias)
}

// Close releases resources for all created broker clients.
func (p *BrokerPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, client := range p.clients {
		client.Close()
	}
}

// Aliases returns all configured broker aliases in their original (display)
// casing, sorted alphabetically. This includes all brokers from the
// configuration, not just those that have been accessed.
func (p *BrokerPool) Aliases() []string {
	aliases := make([]string, 0, len(p.configs))
	for _, cfg := range p.configs {
		aliases = append(aliases, cfg.DisplayName())
	}
	sort.Strings(aliases)
	return aliases
}
