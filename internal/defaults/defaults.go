// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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

// DefaultLoopbackListenAddress is the host the MCP server binds to when
// listen_address is unset and mcp_client_auth.mode is not oauth. Loopback-only
// by default keeps the dev auth modes (disabled, static) unreachable from the
// network without an explicit operator decision (see config.applyDefaults).
const DefaultLoopbackListenAddress = "127.0.0.1"

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

// DefaultShutdownDrainDelayS is the propagation window, in seconds, the server
// waits after flipping /readyz to 503 (shutting_down) on SIGTERM, BEFORE it
// begins gracefully shutting down the HTTP server. The delay lets the
// orchestrator observe the not-ready state and deregister the pod from its
// endpoint set, so no NEW traffic is routed to the draining pod while in-flight
// requests finish — avoiding 502s during a rolling update.
//
// Decided: 10 seconds.
// Reasoning: covers a typical kube-proxy / endpoint-controller propagation lag
// (a few hundred ms to a few seconds) with margin, while keeping total pod
// termination bounded. The drain runs IN-PROCESS: the distroless image has no
// shell, so there is no preStop hook to sleep for us (SOL-151288).
// Trade-off: every shutdown takes at least this long even when no traffic is in
// flight. Accepted as the cost of a 502-free rolling update.
//
// The pod's terminationGracePeriodSeconds must be at least
// DefaultShutdownDrainDelayS + DefaultShutdownTimeoutSeconds plus a small buffer
// so K8s does not SIGKILL the process mid-drain (see deploy/kubernetes/deployment.yaml).
const DefaultShutdownDrainDelayS = 10

// DefaultSEMPRequestTimeoutDuration is the per-request timeout for individual
// SEMP API calls to a broker. Matches the story spec default and the Solace
// Terraform provider convention.
const DefaultSEMPRequestTimeoutDuration = time.Minute

// DefaultOIDCHTTPTimeout bounds the HTTP client used by go-oidc for both
// startup discovery and lazy JWKS refresh during token verification.
// Without a hard ceiling, a slow or hung identity provider can wedge the
// JWKS-refresh goroutine indefinitely (the goroutine survives caller
// cancellation via context.WithoutCancel) and stall per-request token
// verification past the inbound request's own server-side deadlines.
//
// Decided: 10 seconds.
// Reasoning: a healthy JWKS fetch is sub-100ms; 10s gives ~100x headroom
// while staying at or below DefaultReadTimeoutSeconds=30, so the inbound
// MCP request still has time to respond before its own server-side bound
// fires. Matches DefaultReadHeaderTimeoutSeconds=10.
// Trade-off: a legitimately slow IdP (multi-second response) will cause
// auth to fail closed. Accepted — the inbound request was going to time
// out anyway.
const DefaultOIDCHTTPTimeout = 10 * time.Second

// MaxSEMPResponseBytes caps the in-memory buffering of a broker's SEMP
// response body. Without this cap, a misbehaving broker or a man-in-the-
// middle could stream gigabytes of data within the request timeout and OOM
// the MCP server.
//
// Decided: 16 MiB.
// Reasoning: SEMPv2 list responses are bounded by the broker's page size
// (typically 500 items). At an extreme of ~30 KB per item, a single page
// sits around 15 MB — 16 MiB gives a safety margin above any realistic
// page response while bounding worst-case allocation to a few percent of
// process memory.
// Trade-off: a broker that legitimately returned > 16 MiB in a single
// response would now fail with a typed "response too large" error. The
// SEMP pagination contract makes this implausible in practice.
const MaxSEMPResponseBytes = 16 * 1024 * 1024

// MaxMCPRequestBytes caps the size of an inbound /mcp request body. The MCP
// SDK's StreamableHTTPHandler buffers the entire POST body in memory with
// io.ReadAll; without a cap, a client can stream a multi-GB body within the
// ReadTimeout window and OOM the server. This is the inbound counterpart of
// MaxSEMPResponseBytes.
//
// Decided: 4 MiB.
// Reasoning: MCP tool-call payloads are JSON-RPC messages, typically a few
// KB. 4 MiB is ~1000x headroom above any realistic tool call while bounding
// the worst-case per-request allocation.
// Trade-off: a legitimately larger request is rejected — 413 when its
// Content-Length declares the overage, otherwise a read error the SDK
// surfaces as 400. No known MCP client produces one.
const MaxMCPRequestBytes = 4 * 1024 * 1024

// DefaultMCPSessionIdleTimeout bounds how long a streamable-HTTP MCP session
// may sit idle before the SDK closes it. It backs
// mcp.StreamableHTTPOptions.SessionTimeout.
//
// Left at the SDK's zero value, idle sessions are NEVER closed: a session
// leaves the handler's map only on an explicit DELETE or a transport close,
// so any client that initializes and then disappears (laptop sleep, container
// kill, network drop, or a client that simply never sends DELETE) leaks its
// session and the goroutines behind it for the lifetime of the process.
//
// Decided: 2 hours.
// Reasoning: long enough to survive a meeting, a lunch break, or an idle
// afternoon on a desktop client, while still bounding session growth on a
// long-running pod to a couple of hours' worth of abandoned connections.
// Trade-off: when the timeout fires, the next request bearing that session ID
// gets 404 "session not found". A spec-compliant client re-initializes and
// the user sees nothing; a client that does not will surface an error. Do NOT
// shorten this without weighing that — the value is chosen for client
// tolerance, not for how fast memory is reclaimed.
const DefaultMCPSessionIdleTimeout = 2 * time.Hour

// DefaultMaxConcurrentPerBroker is the maximum number of in-flight SEMP
// requests allowed per broker, enforced by a semaphore shared across the
// broker's SEMPv1 and SEMPv2 clients (see resilience.Semaphore and
// semp.NewBrokerClient). The HTTP transport's per-protocol-client connection
// pool is sized from the same value (see resilience.NewTunedTransport).
//
// Assumption: 10 concurrent requests is a safe default per broker.
// Reasoning: The KA document indicates 1-10 concurrent agent sessions as
// typical usage. This limit protects the broker management plane from overload.
// Validation needed: Load test against real brokers to determine the optimal
// concurrency limit that balances throughput with broker stability.
const DefaultMaxConcurrentPerBroker = 10

// MaxConcurrentPerBrokerCeiling caps the operator-configurable
// semp.max_concurrent_per_broker. The value backs the HTTP transport's
// MaxConnsPerHost / MaxIdleConnsPerHost / MaxIdleConns (×2), so it
// allocates fixed-size structures proportional to itself × the broker count.
// 1024 leaves ~100× headroom above the documented typical 1-10 range while
// rejecting pathological configurations (e.g. a million) that would OOM the
// process.
const MaxConcurrentPerBrokerCeiling = 1024

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

// DefaultReadTimeoutSeconds bounds the time from accepting a connection to
// finishing the request body. Together with DefaultReadHeaderTimeoutSeconds
// it closes the slow-body variant of the Slowloris attack: a client that
// sends headers in time but then trickles the body forever.
//
// Assumption: 30 seconds is generous for MCP request bodies.
// Reasoning: MCP tool calls carry JSON-RPC payloads that are typically a few
// KB. 30s tolerates congested networks while bounding the worst case.
// Trade-off: a request that legitimately takes >30s to send its body will
// be dropped. Accepted — MCP clients do not stream the request body.
const DefaultReadTimeoutSeconds = 30

// DefaultIdleTimeoutSeconds bounds how long an idle HTTP/1.1 keep-alive
// connection sits open after a request completes before the server closes it.
// Prevents idle-connection exhaustion: without this, an attacker can open
// thousands of sockets, send one request each, and hold them open until the
// kernel reaps the file descriptors.
//
// Assumption: 120 seconds balances reuse with resource bounds.
// Reasoning: 2 minutes lets a chatty MCP client reuse the connection for
// follow-up calls while bounding worst-case fd usage to (concurrent clients)
// × 2 minutes of idle slots.
//
// NOTE: WriteTimeout is intentionally NOT set on the MCP server. The /mcp
// endpoint serves long-lived MCP streamable HTTP / SSE responses; a server-
// wide write deadline would cut legitimate streams. The slow-read attack
// surface is mitigated by the OS's TCP send buffer plus IdleTimeout reaping
// idle sockets — neither perfect, but the alternative breaks the product.
const DefaultIdleTimeoutSeconds = 120

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

// DefaultTokenExpirySkew is subtracted from the IdP-reported expires_in
// when computing a token's ExpiresAt. The result is a conservative
// "use-by" instant — callers (and the future cache) never present a
// token that might expire mid-flight to the broker.
//
// Decided: 30 seconds.
// Reasoning: a broker-bound SEMP request takes well under 1 second in
// the common case; 30s gives ~30× headroom for slow networks or
// queued requests while wasting at most 30s of a token's lifetime.
// Trade-off: a token with expires_in ≤ 30 is returned with an
// ExpiresAt in the past, so callers (and the future cache) will
// consider it immediately stale. Accepted — such short-lived tokens
// are an IdP misconfiguration for machine-to-machine flows.
const DefaultTokenExpirySkew = 30 * time.Second

// DefaultOAuthCacheMaxSize is the maximum number of entries in the in-memory
// OAuth token cache. When full, Otter's adaptive W-TinyLFU eviction policy
// removes the least valuable entries automatically.
//
// Decided: 10000 entries.
// Reasoning: enterprise deployments typically have far fewer concurrent users
// than 10000; this provides generous headroom while keeping memory bounded
// (each entry is a short string + time.Time, O(100 bytes) → ~1 MB at max).
const DefaultOAuthCacheMaxSize = 10000

// DefaultMaxOAuthTokenTTL caps the maximum TTL of any entry in the OAuth token
// cache. Handles two edge cases: IdPs that omit expires_in (RFC 8693 makes it
// RECOMMENDED, not mandatory) and IdPs that return absurdly large expires_in
// values (e.g., 100 years).
//
// Decided: 24 hours.
// Reasoning: enterprise IdPs typically issue tokens valid for minutes to hours.
// A 24h cap accepts any realistic expiry while bounding memory and ensuring
// stale tokens are eventually evicted even if the background sweeper misfires.
const DefaultMaxOAuthTokenTTL = 24 * time.Hour

// DefaultSaturationThresholdMs is the latency above which a tool call is
// considered slow enough to emit a saturation signal (a future observability
// story consumes this). Expressed in milliseconds to match the YAML field
// (observability.saturation_threshold_ms).
//
// Assumption: 10ms is a sensible default trip point.
// Reasoning: SEMP management-plane calls that complete well under this are
// healthy; sustained crossings indicate the broker or the server is saturating.
// Validation needed: tune against real broker latencies once the saturation
// emitter lands and we observe production percentiles.
const DefaultSaturationThresholdMs = 10

// DefaultProgressSignalThresholdMs is the elapsed time after which a long-
// running tool call emits a progress signal so the agent and operator know
// work is still in flight (a future observability story consumes this).
// Expressed in milliseconds to match the YAML field
// (observability.progress_signal_threshold_ms).
//
// Assumption: 5000ms (5s) balances chatter against perceived hangs.
// Reasoning: most tool calls finish well under 5s; a call still running past
// it is worth a "still working" signal rather than silence.
// Validation needed: confirm against real tool-call duration distributions.
const DefaultProgressSignalThresholdMs = 5000

// DefaultOTelSelfStatsIntervalS is how often, in seconds, the server emits its
// own OpenTelemetry self-statistics (a future observability story consumes
// this). Expressed in seconds to match the YAML field
// (observability.otel_self_stats_interval_s).
//
// Assumption: 60s is a reasonable self-stats cadence.
// Reasoning: matches common metrics scrape intervals; frequent enough to spot
// trends, infrequent enough to keep self-telemetry overhead negligible.
// Validation needed: align with the deployment's metrics backend scrape period.
const DefaultOTelSelfStatsIntervalS = 60
