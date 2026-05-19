// Package defaults is the single source of truth for all default values used
// across the Solace Broker MCP Server.
//
// Constants based on assumptions (rather than validated decisions) are annotated
// with the assumption being made, why the value was chosen, and what validation
// is needed before production release. Review these before deploying to production.
package defaults

import "time"

// DefaultPort is the HTTP port the MCP server listens on. Uses 9090 to avoid
// conflict with the Solace broker's SEMP management port (default 8080).
const DefaultPort = 9090

// DefaultShutdownTimeoutSeconds is the maximum time in seconds the MCP server
// waits for in-flight requests to complete during graceful shutdown before
// forcibly closing remaining connections.
//
// Decided: 30 seconds.
// Reasoning: 30s gives long-lived MCP streaming sessions time to drain while
// staying under typical orchestrator kill windows.
// Trade-off: DefaultSEMPRequestTimeoutDuration is 60s, so a worst-case
// in-flight SEMP call may be aborted by httpServer.Close() at the end of
// the shutdown window. Accepted in favor of bounded, predictable shutdown.
const DefaultShutdownTimeoutSeconds = 30

// DefaultSEMPRequestTimeoutDuration is the per-request timeout for individual
// SEMP API calls to a broker. Matches the story spec default and the Solace
// Terraform provider convention.
const DefaultSEMPRequestTimeoutDuration = time.Minute

// DefaultTLSHandshakeTimeoutSeconds bounds how long the SEMP transport will
// wait for a TLS handshake to complete with a broker. Without this granular
// timeout, a broker stuck in handshake (stalled certificate validation, HSM
// latency, partial network split) ties up a MaxConcurrentPerBroker semaphore
// slot for the full DefaultSEMPRequestTimeoutDuration window before failing.
//
// Assumption: 10 seconds is generous for a real TLS handshake.
// Reasoning: Even under congested networks a TLS 1.2/1.3 handshake completes
// in well under a second; 10s tolerates extreme outliers while bounding the
// failure window to a small fraction of the request timeout.
const DefaultTLSHandshakeTimeoutSeconds = 10

// DefaultResponseHeaderTimeoutSeconds bounds the time the SEMP transport
// waits after sending a request before the broker returns response headers.
// Must stay strictly less than DefaultSEMPRequestTimeoutDuration so the
// granular timeout actually fires before the outer client.Timeout. A broker
// that accepts the TCP connection then never sends headers would otherwise
// hold the per-broker semaphore slot for the full request timeout.
//
// Assumption: 30 seconds is comfortably under the 60s request timeout.
// Reasoning: SEMP operations send response headers as soon as the broker
// dispatches the request. Anything past 30s indicates a stuck broker, not
// a slow one — fail fast.
const DefaultResponseHeaderTimeoutSeconds = 30

// DefaultExpectContinueTimeoutSeconds is the time the transport waits for a
// "100 Continue" intermediate response after sending a request with an
// "Expect: 100-continue" header before sending the body. SEMP requests do
// not use Expect/100-continue, but setting this is standard HTTP-client
// hygiene and ensures a misconfigured peer that signals 100-continue can't
// stall the request indefinitely.
const DefaultExpectContinueTimeoutSeconds = 1

// DefaultMaxConcurrentPerBroker is the maximum number of concurrent SEMP
// requests allowed per broker, enforced via a per-broker semaphore.
//
// Assumption: 10 concurrent requests is a safe default per broker.
// Reasoning: The KA document indicates 1-10 concurrent agent sessions as
// typical usage. This limit protects the broker management plane from overload.
// Validation needed: Load test against real brokers to determine the optimal
// concurrency limit that balances throughput with broker stability.
const DefaultMaxConcurrentPerBroker = 10

// DefaultInsecureSkipVerify controls whether TLS certificate verification is
// skipped when connecting to brokers. Must be false in production. Only set
// to true in development environments with self-signed certificates. Matches
// the naming of crypto/tls.Config.InsecureSkipVerify.
const DefaultInsecureSkipVerify = false

// DefaultReadHeaderTimeoutSeconds is the maximum time in seconds the HTTP
// server waits for a client to send request headers before closing the
// connection. Protects against Slowloris attacks (gosec G112).
//
// Assumption: 10 seconds is sufficient for MCP clients to send headers.
// Reasoning: MCP clients are automated software (Claude Code, ChatGPT agents)
// that send headers instantly. 10 seconds is generous for congested networks.
// This only covers header reading — once headers are received, the connection
// can stay open indefinitely for MCP streaming (SSE).
// Validation needed: Monitor for rejected connections in production to confirm
// this value is not too aggressive for real-world network conditions.
const DefaultReadHeaderTimeoutSeconds = 10

// DefaultConfigPathSystem is the production-install location for the config
// file. Tried when CONFIG_FILE is not set. Follows the conventional Linux
// /etc/<app>/config.yaml layout used by Linux services, K8s, and Docker.
const DefaultConfigPathSystem = "/etc/mcp-server/config.yaml"

// DefaultConfigPathLocal is the developer-convenience fallback checked after
// the system path. Makes `go run ./cmd/server` work in the repo without any
// env vars, while keeping production safe: if both paths exist, the system
// path wins.
const DefaultConfigPathLocal = "broker-config.yaml"

// DefaultLogLevel is the default slog level name used when log_level is not
// specified in the config file. Matches the current hardcoded behavior of the
// server (slog.LevelInfo).
const DefaultLogLevel = "info"

// DefaultRequestMinInterval is the minimum spacing between successive SEMP
// requests against the same broker, enforced by a future rate limiter (Story 5).
// Matches the story spec and the Solace Terraform provider default (100ms).
const DefaultRequestMinInterval = 100 * time.Millisecond

// DefaultRetries is the maximum number of retry attempts for a failed SEMP
// request before surfacing the error to the caller. Used by the future retry
// logic (Story 5). Zero disables retries entirely. Matches the story spec and
// Terraform provider default.
const DefaultRetries = 10

// DefaultRetryMinInterval is the starting backoff duration before the first
// retry attempt. Subsequent retries grow toward DefaultRetryMaxInterval via
// exponential backoff (Story 5). Matches the story spec and Terraform default.
const DefaultRetryMinInterval = 3 * time.Second

// DefaultRetryMaxInterval caps the retry backoff duration regardless of how
// many attempts have been made. Must be >= DefaultRetryMinInterval. Matches
// the story spec and Terraform default.
const DefaultRetryMaxInterval = 30 * time.Second
