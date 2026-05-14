package resilience

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// idleConnTimeout is how long an idle keep-alive connection sits in the pool
// before the client closes it. Matches the value in http.DefaultTransport.
// Without this, an idle connection lives until the broker closes it — which
// can produce a "connection reset by peer" on the first request after a quiet
// period and trigger an unnecessary retry.
const idleConnTimeout = 90 * time.Second

// NewTunedTransport builds an *http.Transport sized for the per-broker
// concurrency cap. Both SEMPv1 and SEMPv2 clients use it so the connection
// pool behaviour stays consistent across protocol versions.
//
// Go's http.Transport defaults MaxIdleConnsPerHost to 2. With
// MaxConcurrentPerBroker at 10+, every request beyond the 2nd opens a new
// TCP+TLS handshake; under load this manifests as connection thrashing and
// elevated tail latency. Setting MaxIdleConnsPerHost = MaxConcurrentPerBroker
// lets the pool hold enough idle connections to absorb the configured
// concurrency without re-handshaking.
//
// MaxIdleConns (global across all hosts on this transport) gets headroom of
// 2× MaxConcurrentPerBroker so it never becomes the bottleneck on top of the
// per-host cap. Each broker has its own transport, so this only ever applies
// to connections to a single broker — the headroom is a defensive cushion,
// not a true multi-host budget.
func NewTunedTransport(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) *http.Transport {
	return &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: brokerCfg.InsecureSkipVerify}, //nolint:gosec // G402 — user-configurable TLS skip for dev environments; defaults to false
		MaxIdleConnsPerHost: sempCfg.MaxConcurrentPerBroker,
		MaxIdleConns:        sempCfg.MaxConcurrentPerBroker * 2,
		IdleConnTimeout:     idleConnTimeout,
	}
}
