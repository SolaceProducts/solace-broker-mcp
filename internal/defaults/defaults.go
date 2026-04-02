// Package defaults is the single source of truth for all default values used
// across the Solace Broker MCP Server.
//
// Constants based on assumptions (rather than validated decisions) are annotated
// with the assumption being made, why the value was chosen, and what validation
// is needed before production release. Review these before deploying to production.
package defaults

// DefaultPort is the HTTP port the MCP server listens on. Uses 9090 to avoid
// conflict with the Solace broker's SEMP management port (default 8080).
const DefaultPort = "9090"

// DefaultShutdownTimeoutSeconds is the maximum time in seconds the MCP server
// waits for in-flight SEMP API calls to complete during graceful shutdown.
//
// Assumption: 120 seconds is sufficient for worst-case request completion.
// Reasoning: A composite tool may execute up to 3 sequential SEMP calls, each
// with a 30-second timeout, plus overhead. 120s provides a 30s safety margin.
// Validation needed: Measure actual broker response times under load to confirm
// or adjust this value.
const DefaultShutdownTimeoutSeconds = 120

// DefaultSEMPRequestTimeoutSeconds is the per-request timeout in seconds for
// individual SEMP API calls to a broker.
//
// Assumption: 30 seconds is sufficient for any single SEMP request.
// Reasoning: Standard HTTP timeout for management API calls. SEMP operations
// are typically fast, but large result sets or broker load may slow responses.
// Validation needed: Measure real broker response times under typical and peak
// load to confirm or adjust this value.
const DefaultSEMPRequestTimeoutSeconds = 30

// DefaultMaxConcurrentPerBroker is the maximum number of concurrent SEMP
// requests allowed per broker, enforced via a per-broker semaphore.
//
// Assumption: 10 concurrent requests is a safe default per broker.
// Reasoning: The KA document indicates 1-10 concurrent agent sessions as
// typical usage. This limit protects the broker management plane from overload.
// Validation needed: Load test against real brokers to determine the optimal
// concurrency limit that balances throughput with broker stability.
const DefaultMaxConcurrentPerBroker = 10

// DefaultTLSSkipVerify controls whether TLS certificate verification is skipped
// when connecting to brokers. Must be false in production. Only set to true in
// development environments with self-signed certificates.
const DefaultTLSSkipVerify = false

// DefaultConfigPath is the default file path for the broker configuration YAML file.
const DefaultConfigPath = "broker-config.yaml"
