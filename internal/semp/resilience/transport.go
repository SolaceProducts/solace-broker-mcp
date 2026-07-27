package resilience

import (
	"crypto/tls"
	"net"
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
// handshake. Without it, a broker stuck in handshake holds one of the
// MaxConnsPerHost connection slots for the full request timeout
// window. 10s tolerates network outliers while bounding the failure
// window to a small fraction of the request timeout.
const tlsHandshakeTimeout = 10 * time.Second

// expectContinueTimeout caps how long the transport waits for a "100
// Continue" before sending the body. SEMP requests do not use
// Expect/100-continue, but setting this is standard HTTP-client
// hygiene against a misconfigured peer that signals 100-continue.
const expectContinueTimeout = 1 * time.Second

// dialTimeoutCeiling is the upper bound on how long the transport waits for a
// TCP connection to a broker. Without a bound the transport falls back to
// net/http's zeroDialer, which has no Timeout, so a broker whose network path
// silently drops SYNs — a security group that omits this server's CIDR, a
// default-deny NetworkPolicy, a stale DNS answer after a failover — stalls in
// connect() until the outer client timeout, holding one of the
// MaxConnsPerHost connection slots and its per-broker semaphore slot for that
// whole window. Because a connection error is retryable, the chain repeats.
// 10s matches tlsHandshakeTimeout: the same tolerance for network outliers,
// applied one layer down.
const dialTimeoutCeiling = 10 * time.Second

// dialKeepAlive matches http.DefaultTransport's dialer. Note what this does and
// does not do: a zero-value net.Dialer already sends keep-alive probes, at Go's
// 15s default (see net.Dialer.KeepAlive — only a negative value disables them),
// so this does not turn anything on. It relaxes that idle interval to 30s. Both
// values sit far inside idleConnTimeout, and on an active connection
// ResponseHeaderTimeout bounds a stall long before probes conclude, so the
// change is immaterial to pool hygiene; it is here for consistency with
// DefaultTransport rather than for effect.
const dialKeepAlive = 30 * time.Second

// dialTimeout derives the TCP connect bound from the outer per-request timeout,
// for the same reason ResponseHeaderTimeout is derived (see NewTunedTransport):
// a granular timeout must stay strictly below the outer client-level timeout or
// the outer one wins and the granular bound never fires. A hardcoded 10s would
// be inert for an operator who sets request_timeout_duration to 5s.
//
// A non-positive requestTimeout falls back to the ceiling rather than to zero.
// Production cannot reach that state — config.Load substitutes
// defaults.DefaultSEMPRequestTimeoutDuration when the field is unset and
// validation rejects a non-positive value — but a directly constructed
// SEMPConfig can, and there http.Client.Timeout and ResponseHeaderTimeout are
// zero too, leaving this the only bound on the TCP connect. (TLSHandshakeTimeout
// and ExpectContinueTimeout are constants and still apply, but they bound later
// stages, not connect.) Zero would mean unbounded, which is the defect.
//
// The result is never zero. Integer division truncates, so any requestTimeout
// below 2ns would otherwise halve to zero — positive, so it clears validation,
// yet producing exactly the unbounded dial this function exists to prevent.
// A time.Nanosecond bound fails the dial immediately, which is the faithful
// reading of a 1ns request budget and is in any case strictly better than
// waiting forever. Spelled with the unit rather than a bare 1, since a raw
// literal in a duration context reads as easily as one second.
func dialTimeout(requestTimeout time.Duration) time.Duration {
	if requestTimeout <= 0 {
		return dialTimeoutCeiling
	}
	return max(time.Nanosecond, min(dialTimeoutCeiling, requestTimeout/2))
}

// newSEMPDialer builds the transport's dialer. It exists so the dial bound is
// observable at all: net.Dialer.Timeout cannot be read back off a DialContext
// closure, so a test can otherwise only assert that DialContext is non-nil,
// never what timeout it carries.
//
// This narrows the untested surface rather than eliminating it. A test can now
// pin that a given requestTimeout yields a dialer with the derived Timeout, but
// nothing can observe which argument NewTunedTransport actually passes — that
// one expression stays guarded only by review.
func newSEMPDialer(requestTimeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout(requestTimeout),
		KeepAlive: dialKeepAlive,
	}
}

// NewTunedTransport builds an *http.Transport sized for the per-broker
// concurrency cap. Both SEMPv1 and SEMPv2 clients use it so the connection
// pool behaviour stays consistent across protocol versions.
//
// The enforcing per-broker in-flight bound is the shared resilience.Semaphore
// acquired in Sender.Do (one per broker, see semp.NewBrokerClient).
// MaxConnsPerHost = MaxConcurrentPerBroker sizes the connection pool
// consistently with that cap: supplying a custom TLSClientConfig disables
// Go's automatic HTTP/2, so SEMP traffic is HTTP/1.1 and one connection
// carries one in-flight request. With the semaphore admitting at most
// MaxConcurrentPerBroker requests per broker across both protocol clients,
// each client's pool can never be asked for more connections than this, so
// the transport limit is sizing, not a second gate.
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
// ResponseHeaderTimeout and the DialContext bound are derived from
// sempCfg.RequestTimeoutDuration rather than hardcoded. A granular timeout must
// stay strictly less than the outer client-level request timeout, otherwise the
// outer timeout wins and the regression this transport tuning fixes (a stuck
// broker holding a MaxConnsPerHost connection slot for the full request window)
// silently returns when operators set an aggressive request_timeout_duration in
// broker-config.yaml. See dialTimeout for the dial derivation.
//
// DialContext is set by hand rather than by cloning http.DefaultTransport.
// Clone would copy ForceAttemptHTTP2: true, which overrides the conservative
// auto-disable that the custom TLSClientConfig above relies on — HTTP/2 would
// then multiplex several in-flight requests onto one connection and invalidate
// the MaxConnsPerHost sizing described above. It would also bring
// Proxy: ProxyFromEnvironment, making SEMP traffic newly sensitive to
// HTTPS_PROXY. Supplying our own DialContext is HTTP/2-neutral: net/http lists
// a custom DialContext in the same conservative-disable set as a custom
// TLSClientConfig.
func NewTunedTransport(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) *http.Transport {
	return &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: brokerCfg.InsecureSkipVerify}, //nolint:gosec // G402 — user-configurable TLS skip for dev environments; defaults to false
		DialContext:           newSEMPDialer(sempCfg.RequestTimeoutDuration).DialContext,
		MaxConnsPerHost:       sempCfg.MaxConcurrentPerBroker,
		MaxIdleConnsPerHost:   sempCfg.MaxConcurrentPerBroker,
		MaxIdleConns:          sempCfg.MaxConcurrentPerBroker * 2,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: sempCfg.RequestTimeoutDuration / 2,
		ExpectContinueTimeout: expectContinueTimeout,
	}
}
