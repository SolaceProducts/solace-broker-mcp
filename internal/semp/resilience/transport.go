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

// tlsHandshakeTimeout bounds how long the transport waits for a TLS
// handshake. Without it, a broker stuck in handshake holds a
// MaxConcurrentPerBroker semaphore slot for the full request timeout
// window. 10s tolerates network outliers while bounding the failure
// window to a small fraction of the request timeout.
const tlsHandshakeTimeout = 10 * time.Second

// expectContinueTimeout caps how long the transport waits for a "100
// Continue" before sending the body. SEMP requests do not use
// Expect/100-continue, but setting this is standard HTTP-client
// hygiene against a misconfigured peer that signals 100-continue.
const expectContinueTimeout = 1 * time.Second

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
//
// ResponseHeaderTimeout is derived from sempCfg.RequestTimeoutDuration (half)
// rather than a hardcoded constant. The granular timeout must stay strictly
// less than the outer client-level request timeout, otherwise the outer
// timeout wins and the regression this transport tuning fixes (a stuck
// broker holding a MaxConcurrentPerBroker semaphore slot for the full
// request window) silently returns when operators set an aggressive
// request_timeout_duration in broker-config.yaml.
func NewTunedTransport(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) *http.Transport {
	return &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: brokerCfg.InsecureSkipVerify}, //nolint:gosec // G402 — user-configurable TLS skip for dev environments; defaults to false
		MaxIdleConnsPerHost:   sempCfg.MaxConcurrentPerBroker,
		MaxIdleConns:          sempCfg.MaxConcurrentPerBroker * 2,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: sempCfg.RequestTimeoutDuration / 2,
		ExpectContinueTimeout: expectContinueTimeout,
	}
}
