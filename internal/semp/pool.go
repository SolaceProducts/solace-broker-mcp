package semp

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

// ErrUnknownBroker is returned by GetSEMPv1, GetSEMPv2, and the underlying
// getOrCreate when the requested alias is not in the configured broker map.
// Callers branch on this with errors.Is to distinguish a user-supplied
// unknown alias (caller's mistake — should list available aliases) from
// transport or construction failures (server-side issue — should preserve
// the underlying error).
var ErrUnknownBroker = errors.New("unknown broker")

// BrokerSource is the minimum config surface the pool depends on for broker
// resolution. *config.ServerConfig satisfies it implicitly via Go's structural
// typing — both methods already exist there with these exact signatures.
//
// Narrowing the pool's config dependency to this interface is deliberate
// (plan §13): it makes any other config field (Port, ClientAuth, SEMP, etc.)
// structurally unreachable from inside the pool, so future pool methods
// cannot accidentally read or mutate unrelated config state.
type BrokerSource interface {
	Broker(alias string) (*config.BrokerConfig, bool)
	BrokerAliases() []string
}

// BrokerPool manages BrokerClient instances for all configured brokers. It is
// created at startup with broker configs from the YAML configuration file.
// BrokerClients are created lazily on the first GetSEMPv1() or GetSEMPv2()
// call for a given alias, so only brokers that are actually used allocate
// HTTP clients and resources. Thread-safe via sync.RWMutex with a
// double-check pattern for lazy creation.
type BrokerPool struct {
	mu sync.RWMutex
	// clients caches lazily-constructed BrokerClient instances, keyed by
	// canonical (lowercase) alias. Holds realized clients only; for the set
	// of configured aliases, ask p.src.
	//
	// Index with strings.ToLower(alias) — mixed-case input would silently
	// miss without it. Access must hold p.mu (RLock for read-only,
	// Lock for read+write); bare map access faults under -race.
	clients   map[string]*BrokerClient
	src       BrokerSource              // broker resolution surface (see BrokerSource doc)
	sempCfg   *config.SEMPConfig        // shared SEMP settings
	exchanger *tokenexchange.Exchanger  // process-wide token exchanger; nil when no broker uses OAuth
}

// NewBrokerPool creates a BrokerPool from the server configuration. No
// BrokerClients are allocated — they are created lazily on first access.
//
// The pool's broker-resolution dependency is narrowed to the BrokerSource
// interface; SEMP knobs are taken separately because they are a distinct
// concern (network behavior, not broker identity).
//
// exchanger is the process-wide token exchanger for OAuth brokers. Pass
// nil when no broker uses OAuth.
func NewBrokerPool(cfg *config.ServerConfig, exchanger *tokenexchange.Exchanger) *BrokerPool {
	return &BrokerPool{
		clients:   make(map[string]*BrokerClient),
		src:       cfg,
		sempCfg:   &cfg.SEMP,
		exchanger: exchanger,
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
//
// This is the sole reader/writer of p.clients. See the field doc on p.clients
// for the invariants any future method must observe.
func (p *BrokerPool) getOrCreate(alias string) (*BrokerClient, error) {
	canonical := strings.ToLower(alias)

	p.mu.RLock()
	if client, ok := p.clients[canonical]; ok {
		p.mu.RUnlock()
		return client, nil
	}
	p.mu.RUnlock()

	// Slow path: write lock for lazy creation with double-check.
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clients[canonical]; ok {
		return client, nil
	}

	cfg, ok := p.src.Broker(alias)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBroker, alias)
	}

	client, err := NewBrokerClient(cfg.DisplayName(), cfg, p.sempCfg, p.exchanger)
	if err != nil {
		return nil, err
	}
	p.clients[canonical] = client
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
	return p.src.Broker(alias)
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
// casing, sorted alphabetically. Delegates to the BrokerSource, which performs
// the sort.
func (p *BrokerPool) Aliases() []string {
	return p.src.BrokerAliases()
}
