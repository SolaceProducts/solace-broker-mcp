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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/authz"
	"github.com/SolaceDev/solace-broker-mcp/internal/banner"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	_ "github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess/handlers" // register handlers via init()
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceDev/solace-broker-mcp/internal/middleware/recovery"
	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/SolaceDev/solace-broker-mcp/internal/tokenexchange"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/queuemetrics"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv1/brokerstatus"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv1/discardstats"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv1/redundancy"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// newSlogHandler creates a slog handler with the ReplaceAttr safety net that
// redacts values for keys matching common credential patterns. This is defense
// in depth — credential-carrying types also implement slog.LogValuer to exclude
// secrets, but ReplaceAttr catches anything that slips through.
// See docs/secure-logging-rules.md Rule 3.
//
// The level parameter controls the minimum level emitted. main() calls this
// twice: once at INFO to bootstrap logging before LoadConfig runs (so config
// loading itself can emit logs), then again with the user-configured level
// from cfg.LogLevel after validation.
func newSlogHandler(level slog.Level) slog.Handler {
	redactedKeys := []string{"password", "token", "secret", "authorization", "credential", "api_key", "private_key"}

	jsonHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			key := strings.ToLower(a.Key)
			for _, redacted := range redactedKeys {
				if strings.Contains(key, redacted) {
					a.Value = slog.StringValue("[REDACTED]")
					return a
				}
			}
			return a
		},
	})

	// Wrap the JSON handler so every request-scoped log line carries a
	// correlation_id attribute (SOL-151280). The wrapper is installed
	// unconditionally; gating is transitive: correlation.From(ctx) returns "" when
	// the correlation middleware is not wired (OBS_CORRELATION_ID_ENABLED off) or
	// for startup/non-request logs, so no correlation_id is emitted and output
	// matches today exactly. This also covers the pre-config bootstrap handler,
	// which is built before cfg is available and so cannot consult Enabled(cfg).
	// Redaction is preserved: the wrapper delegates Handle to jsonHandler, which
	// still owns and runs the ReplaceAttr filter above.
	return correlation.NewSlogHandler(jsonHandler)
}

// buildMux creates the HTTP route multiplexer with basic routes.
// The /mcp route is registered separately in main() with auth middleware.
// Both main() and tests use this function to avoid route drift.
//
// readiness backs the /readyz endpoint (and its /ready alias), which reflects
// the MCP server's OWN readiness and is decoupled from the broker — it makes no
// broker calls.
func buildMux(readiness *health.ReadinessState) *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness probe. /livez is the canonical liveness endpoint and returns
	// {"status":"alive"}. /health is retained for backward compatibility and
	// preserves its original {"status":"healthy"} body — it is a SEPARATE handler
	// instance, NOT a body-identical alias, so external consumers that parse
	// .status == "healthy" keep working. Both probes are unconditional (200 on
	// GET, 405 otherwise) and never flag-gated.
	mux.Handle("/livez", health.LivezHandler())
	mux.Handle("/health", health.HealthHandler())

	// Readiness probe. /readyz reflects the MCP server's OWN readiness only
	// (initialized, not draining, required listeners bound) and is decoupled
	// from broker reachability (ADR-004 / ISSUE-026) — it makes no broker calls.
	// Like /livez it is unconditional (no flag).
	readyz := health.ReadyzHandler(readiness)
	mux.Handle("/readyz", readyz)

	// /ready is a body-identical alias of /readyz (SOL-151285 / Story 11). It
	// shares the SAME handler instance, so GET /ready and GET /readyz return
	// identical status and body. The legacy broker-dialing /ready handler was
	// removed: readiness is the MCP server's own state, not broker reachability.
	mux.Handle("/ready", readyz)

	return mux
}

// buildRootHandler returns the outermost HTTP handler for the server: the mux
// always wrapped with panic-recovery as the OUTERMOST layer (ADR-001 chain
// ordering). Recovery is unconditional — a safety net with no production reason
// to disable.
//
// Recovery wraps the WHOLE mux, so it covers every route — the standalone
// /livez, /health, /ready, /readyz probes (and the future /metrics), the
// catch-all 404, and the authenticated /mcp chain — sitting OUTSIDE the per-route
// correlation.Middleware that main() installs only on /mcp. The two are
// different layers and never conflict: recovery wraps the mux; correlation
// wraps a handler inside it.
//
// Extracted from main() so the composition test can assert the assembled chain
// order against the real wiring rather than a hand-rebuilt copy.
func buildRootHandler(mux *http.ServeMux) http.Handler {
	return recovery.HTTPMiddleware(mux)
}

// healthConfig holds the minimal server settings needed by the --health probe.
type healthConfig struct {
	Port   int
	Scheme string // "http" or "https"
}

// healthConfigFromFile reads port and TLS fields from the config file without
// full validation. Used by --health so the probe targets the same port and
// scheme as the server. Returns defaults (port 0, scheme "http") if no config
// is found or fields are absent.
func healthConfigFromFile() healthConfig {
	var path string
	if v := os.Getenv("CONFIG_FILE"); v != "" {
		path = v
	} else if _, err := os.Stat(defaults.DefaultConfigPathSystem); err == nil {
		path = defaults.DefaultConfigPathSystem
	} else if _, err := os.Stat(defaults.DefaultConfigPathLocal); err == nil {
		path = defaults.DefaultConfigPathLocal
	}
	if path == "" {
		return healthConfig{Scheme: "http"}
	}
	data, err := os.ReadFile(path) //nolint:gosec // path from trusted config locations
	if err != nil {
		return healthConfig{Scheme: "http"}
	}
	var cfg struct {
		Port        int    `yaml:"port"`
		TLSCertFile string `yaml:"tls_cert_file"`
		TLSKeyFile  string `yaml:"tls_key_file"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return healthConfig{Scheme: "http"}
	}
	scheme := "http"
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		scheme = "https"
	}
	return healthConfig{Port: cfg.Port, Scheme: scheme}
}

// newHTTPServer builds the MCP server's *http.Server with the production
// timeout posture from internal/defaults. Extracted so timeout fields can be
// asserted in a unit test without spinning up a real listener. See the
// constants in internal/defaults for the rationale on each timeout value;
// WriteTimeout is deliberately zero to preserve long-lived MCP streamable
// HTTP / SSE responses.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(defaults.DefaultReadHeaderTimeoutSeconds) * time.Second,
		ReadTimeout:       time.Duration(defaults.DefaultReadTimeoutSeconds) * time.Second,
		IdleTimeout:       time.Duration(defaults.DefaultIdleTimeoutSeconds) * time.Second,
	}
}

// limitRequestBody bounds the inbound request body at maxBytes before the
// MCP SDK buffers it with io.ReadAll. A request that declares an oversized
// Content-Length is rejected with 413 before the SDK runs. Bodies without a
// declared length (or that lie) are caught by http.MaxBytesReader, which
// fails the downstream read once the limit is exceeded and closes the
// connection, so an oversized body is never fully buffered; the SDK surfaces
// that read failure to the client as 400 "failed to read body".
func limitRequestBody(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}

// buildMCPEndpoint assembles the /mcp handler chain around authedHandler.
//
// The layer order, outermost first, is: limitRequestBody → correlation →
// authedHandler. limitRequestBody is the OUTERMOST layer so an oversized
// request is rejected with 413 before any work is done. A deliberate
// consequence: a 413 is returned BEFORE the correlation middleware runs, so
// 413 responses carry no correlation ID. This is intentional. Correlation sits
// OUTSIDE auth (ADR-001) so a 401 still gets an ID, but the body limit sits
// outside correlation by design.
//
// When correlationEnabled is false the correlation layer is omitted entirely
// (correlation.From then returns "").
func buildMCPEndpoint(authedHandler http.Handler, correlationEnabled bool) http.Handler {
	endpoint := authedHandler
	if correlationEnabled {
		endpoint = correlation.Middleware(authedHandler)
	}
	return limitRequestBody(endpoint, defaults.MaxMCPRequestBytes)
}

// startServer starts httpServer in a background goroutine and returns a channel
// that receives any startup/runtime error (excluding http.ErrServerClosed, which
// is the normal result of Shutdown). The channel is buffered so the goroutine
// never blocks if the receiver is gone.
//
// This replaces the previous pattern of calling os.Exit(1) inside the goroutine,
// which bypassed all deferred cleanup and the graceful-shutdown path.
func startServer(srv *http.Server, tlsCertFile, tlsKeyFile string) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		var listenErr error
		if tlsCertFile != "" && tlsKeyFile != "" {
			slog.Info("server listening with TLS",
				slog.String("addr", srv.Addr),
				slog.String("cert", tlsCertFile))
			listenErr = srv.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
		} else {
			slog.Info("server listening",
				slog.String("addr", srv.Addr))
			listenErr = srv.ListenAndServe()
		}
		if listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("server failed", slog.String("error", listenErr.Error()))
			errCh <- listenErr
		}
	}()
	return errCh
}

// drainAndShutdown performs the SIGTERM graceful-drain sequence (SOL-151288).
// In strict order it:
//
//  1. SetShuttingDown FIRST, so /readyz flips to 503 ("shutting_down")
//     immediately — within AC #5's 1s budget, before any sleep — and the
//     orchestrator stops routing new traffic to this pod.
//  2. sleeps drainDelay (the propagation window) so K8s observes the not-ready
//     state and deregisters the pod from its endpoint set. The distroless image
//     has no shell, so there is no preStop hook; this delay runs IN-PROCESS.
//  3. gracefully shuts the HTTP server down so in-flight requests drain.
//
// Step 1 happens before the sleep so readiness flips with no dependence on the
// drain delay (verified by the zero-delay test). drainDelay <= 0 skips the
// sleep but still flips readiness and shuts down. This is the signal-path only;
// the startup-error path calls gracefulShutdown directly and does NOT drain
// (a process that never began serving has nothing to drain and no reason to
// advertise a propagation window).
//
// The shutdownTimeout budget is passed through to gracefulShutdown, which
// builds its own timeout context AFTER this function's drain sleep returns. The
// drain delay therefore does NOT eat into the shutdown budget: Shutdown gets
// the full shutdownTimeout to drain in-flight requests, exactly as the K8s
// terminationGracePeriodSeconds calculation in deploy/kubernetes/deployment.yaml
// assumes (grace = drainDelay + shutdownTimeout + buffer).
//
// forceSig carries a SECOND shutdown signal (SOL-151437). A second SIGINT/SIGTERM
// arriving while the drain window is sleeping short-circuits the rest of the
// sequence: forceClose drops in-flight connections and we return immediately,
// skipping the graceful wait. Readiness has already flipped to 503 above, so a
// forced exit still leaves /readyz correct. A nil forceSig (the startup-error
// path, which never drains) simply never fires that select case.
func drainAndShutdown(srv shutdowner, readiness *health.ReadinessState, drainDelay, shutdownTimeout time.Duration, forceSig <-chan os.Signal) error {
	readiness.SetShuttingDown()
	slog.Info("draining before shutdown",
		slog.String("reason", "signal"),
		slog.Duration("drain_delay", drainDelay))
	if drainDelay > 0 {
		timer := time.NewTimer(drainDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			// Drain window elapsed normally; fall through to the graceful wait.
		case sig := <-forceSig:
			forceClose(srv, sig)
			return nil
		}
	}
	return gracefulShutdown(srv, shutdownTimeout, forceSig)
}

// forceClose is the shared "second signal" reaction (SOL-151437): a single WARN
// line naming the triggering signal, then srv.Close() to drop in-flight
// connections immediately. Close is the only way to actually drop connections —
// cancelling Shutdown's context would merely stop the wait and leave them open.
// http.Server.Close is safe to call even if Shutdown is concurrently running or
// has already returned, so this never double-closes.
func forceClose(srv shutdowner, sig os.Signal) {
	slog.Warn("second signal received, forcing immediate shutdown; in-flight connections dropped without draining",
		slog.String("signal", sig.String()))
	if err := srv.Close(); err != nil {
		slog.Error("forced close failed", slog.String("error", err.Error()))
	}
}

// drainDelayForSignal returns the drain delay to apply for the received
// shutdown signal. SIGTERM (orchestrator-initiated, e.g. Kubernetes) gets the
// configured drain window so the orchestrator can deregister the pod before we
// stop accepting. SIGINT (os.Interrupt, a local Ctrl-C) skips the drain
// entirely — there is no orchestrator to wait for — so it returns 0.
func drainDelayForSignal(sig os.Signal, configured time.Duration) time.Duration {
	if sig == os.Interrupt { // syscall.SIGINT
		return 0
	}
	return configured
}

// shutdowner is the subset of *http.Server that gracefulShutdown drives. It
// exists so a test can substitute a fake that captures the deadline of the
// context handed to Shutdown — proving the full timeout budget is granted at
// the moment Shutdown is called, after the drain sleep
// (TestDrainAndShutdown_ShutdownGetsFullBudgetAfterDrain). *http.Server
// satisfies it; production always passes the real server.
type shutdowner interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// gracefulShutdown shuts srv down within a fresh timeout-bounded context,
// falling back to a forced Close if the graceful shutdown times out (so the
// process is never wedged by a connection that refuses to drain). It returns
// the Shutdown error for the caller's exit-status decision; the forced-close
// fallback is handled internally. This is the shared shutdown step for both the
// signal path (via drainAndShutdown) and the startup-error path (called
// directly, no drain).
//
// gracefulShutdown OWNS the timeout context: it creates it here, immediately
// before srv.Shutdown, so the deadline always covers the FULL timeout from the
// moment Shutdown begins. On the signal path this happens AFTER drainAndShutdown
// has already slept the drain delay, so the drain delay never consumes any of
// the shutdown budget.
//
// forceSig carries a SECOND shutdown signal (SOL-151437). Shutdown runs in a
// goroutine so we can select between it completing and a second signal arriving:
// on a second signal forceClose drops in-flight connections (which also unblocks
// the in-flight Shutdown) and we return immediately. The result channel is
// buffered so that goroutine can always send and exit even after we have
// returned via the force path — no goroutine leak. A nil forceSig (the
// startup-error path) never fires that case, so that path keeps its exact
// prior behavior, including the Shutdown-timed-out forced-close fallback.
func gracefulShutdown(srv shutdowner, timeout time.Duration, forceSig <-chan os.Signal) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown(ctx) }()

	select {
	case err := <-shutdownDone:
		if err != nil {
			slog.Error("shutdown timed out, forcing close", slog.String("error", err.Error()))
			if closeErr := srv.Close(); closeErr != nil {
				slog.Error("forced close failed", slog.String("error", closeErr.Error()))
			}
		}
		return err
	case sig := <-forceSig:
		forceClose(srv, sig)
		return nil
	}
}

// registerSEMPv1Tools attaches every Go-native SEMPv1 tool handler to mgr.
// New SEMPv1 tools should be added here as they land — this is the single
// source of truth for which v1 tools the server exposes. The handlers flow
// through the same RegisterWithServer pass as composite tools, so they
// inherit input-schema validation, broker resolution, and structured
// logging without further plumbing.
func registerSEMPv1Tools(mgr *tools.ToolManager) {
	mgr.Register(redundancy.NewHandler())
	mgr.Register(brokerstatus.NewHandler())
	mgr.Register(discardstats.NewHandler())
}

// registerMixedTools attaches native Go tool handlers that use BOTH the SEMPv2
// and SEMPv1 clients in one call. get-queue-metrics merges the SEMPv2 monitor
// snapshot with the authoritative SEMPv1 live-depth block; it is read-only and
// registers regardless of enable_write_tools.
func registerMixedTools(mgr *tools.ToolManager) {
	mgr.Register(queuemetrics.NewHandler())
}

// newTokenExchanger builds the process-wide Exchanger and the IdP HTTP
// client it uses. It does no gating of its own — the caller decides
// whether to invoke it.
//
// PRECONDITION: the caller has already verified that the Hop-2 OAuth
// runtime should run in this process, i.e. ServerConfig.Hop2OAuthActive()
// returned true and oauthCfg is the (non-nil) global broker_oauth block.
// Calling this function when the runtime is not active will construct
// resources for a path that has no consumer, which is exactly what the
// top-level gate in main() is there to prevent.
//
// Constructing this exchanger allocates: an *http.Client for the IdP,
// a *tokenexchange.Exchanger holding the client secret in memory, and
// a singleflight.Group for request deduplication. None of these make an
// outbound network call at construction time; the first IdP request
// happens when a request-path goroutine calls Exchanger.Exchange().
// (idpclient.NewHTTPClient does read local trust-store material —
// SSL_CERT_FILE and the system cert pool — which is filesystem/OS I/O,
// but not network.)
func newTokenExchanger(oauthCfg *config.BrokerOAuthConfig) (*tokenexchange.Exchanger, error) {
	httpClient, err := idpclient.NewHTTPClient()
	if err != nil {
		return nil, fmt.Errorf("creating IdP HTTP client: %w", err)
	}
	tokenCache, err := cache.NewTokenCache(cache.CacheConfig{
		MaxSize:   defaults.DefaultOAuthCacheMaxSize,
		ClockSkew: defaults.DefaultTokenExpirySkew,
		MaxTTL:    defaults.DefaultMaxOAuthTokenTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("creating token cache: %w", err)
	}
	exchanger, err := tokenexchange.FromConfig(oauthCfg, httpClient, tokenCache)
	if err != nil {
		// Cache already started its Otter eviction goroutine on construction.
		// Release it before returning so the failed-startup path doesn't leak
		// resources (the eventual os.Exit(1) reaps them anyway, but a test
		// that drives this path would leak per invocation).
		if closeErr := tokenCache.Close(); closeErr != nil {
			slog.Warn("closing token cache after exchanger construction failed",
				slog.String("error", closeErr.Error()))
		}
		return nil, fmt.Errorf("creating token exchanger: %w", err)
	}
	slog.Info("token exchanger created for broker OAuth")
	return exchanger, nil
}

// buildToolPolicy is the single, tested decision point for whether the server
// runs with tool authorization and which compiled Policy to use. Extracted so
// the invariant "gate enabled ⟺ returned policy is non-nil" is a
// postcondition of a named function rather than folklore scattered across
// startup.
//
// Returns (nil, nil) when the gate is off, (policy, nil) on success,
// (nil, err) on compilation failure — callers must fail startup on the last.
// Main's fail-closed guard immediately after this call asserts the mirror
// direction so any future refactor that breaks the postcondition aborts
// startup rather than silently bypassing authorization.
func buildToolPolicy(cfg *config.ServerConfig) (*authz.Policy, error) {
	if !config.ToolAuthorizationEnabled(cfg) {
		return nil, nil
	}
	return authz.NewPolicy(*cfg.MCPClientAuth.ToolAuthorization)
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version.Version())
		os.Exit(0)
	}

	if len(os.Args) == 2 && os.Args[1] == "--health" {
		hc := healthConfigFromFile()
		port := defaults.DefaultPort
		if v := os.Getenv("MCP_SERVER_PORT"); v != "" {
			if p, err := strconv.Atoi(v); err == nil {
				port = p
			}
		} else if hc.Port != 0 {
			port = hc.Port
		}
		healthURL := hc.Scheme + "://localhost:" + strconv.Itoa(port) + "/health"
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil) //nolint:gosec // localhost health check with integer port; no SSRF risk
		if err != nil {
			os.Exit(1)
		}
		client := &http.Client{}
		if hc.Scheme == "https" {
			client.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // localhost self-check; cert SAN may not match localhost
			}
		}
		resp, err := client.Do(req) //nolint:gosec // taint propagated from healthURL above
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// 0. Bootstrap slog at INFO so LoadConfig can emit logs. The handler is
	//    swapped with the user-configured level (cfg.LogLevel) after LoadConfig
	//    validates it. UnmarshalText is infallible here because validate() in
	//    the config package already proved cfg.LogLevel is one of the valid
	//    level names.
	slog.SetDefault(slog.New(newSlogHandler(slog.LevelInfo)))

	slog.Info("server starting", slog.String("version", version.Version()))

	// One-line release disclaimer, emitted once on every startup regardless of
	// auth mode. It is deliberately SEPARATE from the insecure-mode banners in
	// internal/banner (those are reserved for security-mode signals and must not
	// be diluted). Emitted here in the bootstrap-INFO window — before the slog
	// handler is reconfigured to cfg.LogLevel — so it stays visible even when the
	// operator sets a higher log level. Wording tracks the README Disclaimer
	// section and is finalized by Legal.
	slog.Info("Community-supported open-source software, provided AS IS with no warranty. " +
		"AI-driven; review output before acting. See the Disclaimer in the README.")

	// 1. Load config. config.Load handles path resolution internally
	//    (CONFIG_FILE env var, then /etc/mcp-server/config.yaml, then
	//    ./broker-config.yaml). See config.Load docs for exact semantics.
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Loud, refactor-robust signal of the configured client auth mode.
	// MUST run before the slog handler gets reconfigured to the user log
	// level — at this point the bootstrap handler is at INFO, so WARN
	// banner entries are always visible regardless of cfg.LogLevel.
	// DO NOT move this into middleware; see internal/banner/banner.go.
	banner.LogStartupAuthMode(cfg.MCPClientAuth.Mode, cfg.MCPClientAuth.Issuer, cfg.BindAddress())

	// static mode allows a non-loopback bind without an override, but that only
	// keeps the token safe if the transport is encrypted. Warn loudly when the
	// shared dev token would travel plaintext on a routable interface.
	if cfg.StaticTokenExposedCleartext() {
		banner.LogStaticCleartextExposure(cfg.BindAddress())
	}

	// oauth mode requires TLS unless the operator acknowledged upstream TLS
	// termination (tls_terminated_upstream: true). When they did, the listener
	// serves plaintext — warn loudly so a missing terminating proxy is visible.
	if cfg.OAuthPlaintextListenerAcknowledged() {
		banner.LogOAuthPlaintextListener(cfg.BindAddress())
	}

	// Reconfigure slog with the user-configured level. cfg.LogLevel is
	// validated and normalized to one of debug/info/warn/error.
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	slog.SetDefault(slog.New(newSlogHandler(level)))

	slog.Info("config loaded",
		slog.Int("broker_count", len(cfg.BrokerAliases())),
		slog.Int("port", cfg.Port),
		slog.String("log_level", cfg.LogLevel))

	// One-line summary of which observability capabilities are enabled. No
	// behavior is wired into the request path yet (SOL-151278 skeleton); this
	// line lets operators confirm the door-closing defaults and any OBS_*
	// overrides took effect at startup.
	slog.Info("observability config loaded",
		slog.Bool("correlation_id", cfg.Observability.CorrelationIDEnabled),
		slog.Bool("metrics", cfg.Observability.MetricsEnabled),
		slog.Bool("audit_log", cfg.Observability.AuditLogEnabled),
		slog.Bool("tracing", cfg.Observability.TracingEnabled),
		slog.Bool("saturation_events", cfg.Observability.SaturationEventsEnabled),
		slog.Bool("auth_failure_counter", cfg.Observability.AuthFailureCounterEnabled))

	// 2. Parse embedded OpenAPI specs
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		slog.Error("failed to parse OpenAPI specs", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slog.Info("parsed OpenAPI specs",
		slog.Int("operation_count", len(operations)))

	// 3. Build token exchanger only when the Hop-2 OAuth runtime is fully
	//    active — three preconditions must all hold: the unreleased-feature
	//    flag is set, broker_oauth: is populated, AND at least one broker
	//    uses auth.mode: oauth. When any is missing, exchanger stays nil;
	//    the broker pool constructs basic/bearer authenticators only, and
	//    no Hop-2 OAuth resources exist in this process. See
	//    ServerConfig.Hop2OAuthActive for the full contract, including
	//    lifecycle notes for ship time.
	var exchanger *tokenexchange.Exchanger
	if cfg.Hop2OAuthActive() {
		exchanger, err = newTokenExchanger(cfg.BrokerOAuth)
		if err != nil {
			slog.Error("token exchanger construction failed",
				slog.String("error", err.Error()))
			os.Exit(1)
		}
		defer func() {
			if closeErr := exchanger.Close(); closeErr != nil {
				slog.Warn("closing token exchanger on shutdown",
					slog.String("error", closeErr.Error()))
			}
		}()
	}

	// 4. Create broker pool
	pool := semp.NewBrokerPool(cfg, exchanger)
	// Release per-broker rate-limiter tickers (and any other client-held
	// resources) on the normal shutdown path. The defer fires after main()
	// returns — i.e. after httpServer.Shutdown completes — so no in-flight
	// tool call holds a Sender when pool.Close iterates clients. Earlier
	// os.Exit(1) paths in main() bypass this defer; that is acceptable
	// because process exit reaps the resources anyway. Close is idempotent
	// (see TestBrokerPool_Close_*).
	defer pool.Close()
	slog.Info("created broker pool",
		slog.Any("broker_aliases", pool.Aliases()))

	// 5. Load embedded composite tool definitions
	compositeTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		slog.Error("failed to load composite tools", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slog.Info("loaded composite tool definitions",
		slog.Int("tool_count", len(compositeTools)))

	// Cross-check that every postProcess handler's RequiredFields are covered
	// by its tool's step `select:` clauses. Catches SEMP field-name drift
	// (e.g. bindSuccessCount vs bindCount) at boot rather than first call.
	// The handlers package was registered via blank import above, so the
	// postprocess registry is populated before this runs.
	if err := composite.ValidatePostProcess(compositeTools); err != nil {
		slog.Error("composite tool postProcess validation failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// 6. Create composite executor
	executor := composite.NewCompositeExecutor(operations)

	// 7. Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: version.Version(),
	}, nil)

	// 8. Create the tool manager and register every tool the server exposes.
	// All registrations happen in one block so the log line below is a
	// reliable phase boundary — anything before it is registered, anything
	// after it sees a fully-loaded server.
	mgr := tools.NewToolManagerFromComposite(pool, compositeTools, executor)
	registerSEMPv1Tools(mgr)
	registerMixedTools(mgr)

	// Compile the tool-authorization policy. buildToolPolicy owns the
	// gate-and-compile decision so the "enabled ⟺ policy non-nil" invariant
	// is a tested postcondition of one function. The guard below defends
	// against a future refactor breaking that postcondition — silent
	// fail-open on a security switch is the SOL-149989 failure class.
	policy, err := buildToolPolicy(cfg)
	if err != nil {
		slog.Error("tool authorization startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if config.ToolAuthorizationEnabled(cfg) && policy == nil {
		slog.Error("tool authorization is enabled in config but no policy was compiled — refusing to start rather than silently bypass authorization")
		os.Exit(1)
	}

	// Pull the configured groups claim name for the missing-claim audit
	// event. applyDefaults guarantees GroupsClaimName is non-nil whenever
	// the tool_authorization block is non-nil, so this dereference is safe
	// under the same gate that produced a non-nil policy. When RBAC is
	// disabled the string is unused by RegisterWithServer.
	var groupsClaimName string
	if policy != nil {
		groupsClaimName = *cfg.MCPClientAuth.ToolAuthorization.GroupsClaimName
	}
	tools.RegisterWithServer(mgr, server, pool, cfg.EnableWriteTools, policy, groupsClaimName)
	slog.Info("tool registration complete",
		slog.Bool("enable_write_tools", cfg.EnableWriteTools))

	// list-brokers is registered directly (no broker resolution needed) and
	// takes no policy — the RBAC exemption is expressed structurally at this
	// API surface.
	tools.RegisterListBrokers(server, pool)

	// Validate every configured tool name now that both registrations have
	// populated mgr. An admin typo would silently produce a grant that never
	// takes effect at request time; catching it at startup is fatal by
	// design. Skipped when RBAC is off.
	if policy != nil {
		if err := tools.ValidatePolicyToolNames(*cfg.MCPClientAuth.ToolAuthorization, mgr); err != nil {
			slog.Error("tool authorization startup failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		slog.Info("tool authorization is enabled",
			slog.Any("policy", policy))
	} else {
		// Announce the RBAC posture with the reason so startup logs are
		// unambiguous. Config validator rejects a block outside oauth mode
		// and requires enabled to be set in oauth mode, so exactly two
		// disabled paths remain: non-oauth mode, or oauth with enabled:false.
		if cfg.MCPClientAuth.Mode == config.AuthModeOAuth {
			slog.Info("tool authorization is disabled (enabled=false in config)")
		} else {
			slog.Info("tool authorization is disabled",
				slog.String("auth_mode", cfg.MCPClientAuth.Mode))
		}
	}

	slog.Info("all tools registered")

	// 9. Set up HTTP routes
	//
	// readiness backs /readyz. It starts in the "starting" state; we mark it
	// initialized once the server begins listening (below). It is decoupled from
	// the broker and is NOT flag-gated.
	readiness := health.NewReadinessState()
	mux := buildMux(readiness)

	// Create MCP handler
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	// Wrap MCP handler with auth middleware
	authedHandler, err := auth.NewAuthMiddleware(cfg, nil, mcpHandler)
	if err != nil {
		slog.Error("failed to create auth middleware", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Stamp a correlation ID immediately OUTSIDE the auth middleware so that an
	// unauthenticated request rejected with 401 still gets a correlation ID
	// attached (ADR-001 ordering). Gated on OBS_CORRELATION_ID_ENABLED: when
	// off, the middleware is not wired and correlation.From returns "".
	correlationEnabled := correlation.Enabled(cfg.Observability)
	slog.Info("correlation-ID middleware wiring",
		slog.Bool("enabled", correlationEnabled))

	// Register authenticated MCP endpoint. buildMCPEndpoint wraps the body
	// limit on the outside so it bounds the request before any layer buffers
	// it; see buildMCPEndpoint for the full layer order and 413 rationale.
	mux.Handle("/mcp", buildMCPEndpoint(authedHandler, correlationEnabled))

	// Register OAuth Protected Resource Metadata endpoint (RFC 9728)
	// This enables MCP clients to discover the authorization server for OAuth flows.
	if metadataHandler := auth.NewProtectedResourceMetadataHandler(cfg); metadataHandler != nil {
		mux.Handle("/.well-known/oauth-protected-resource", metadataHandler)
		slog.Info("registered OAuth protected resource metadata endpoint")
	}

	// Catch-all: return JSON 404 for any unregistered path. MCP SDK clients
	// probe multiple OAuth discovery endpoints during (re-)authentication
	// (e.g. /.well-known/oauth-authorization-server, /authorize, /register).
	// Go's default plain-text "404 page not found" causes JSON parse errors
	// in those clients. A JSON body lets them fail gracefully.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not_found","error_description":"the requested endpoint does not exist"}`)
		slog.Debug("catch-all 404: no route matched", slog.String("method", r.Method), slog.String("path", r.URL.Path))
	}))

	// 10. Wrap the whole mux with panic-recovery as the OUTERMOST handler
	// (ADR-001). Recovery is unconditional. A panic in ANY handler is caught,
	// logged with a structured stack, and returned as a clean 500; the process
	// keeps running. Recovery sits OUTSIDE the per-route correlation middleware
	// wired on /mcp above — different layers, no conflict.
	rootHandler := buildRootHandler(mux)

	// 11. Start server with graceful shutdown
	addr := cfg.BindAddress()
	httpServer := newHTTPServer(addr, rootHandler)

	// Buffered at 2 (SOL-151437) so a rapidly delivered SECOND signal is not
	// dropped in the window between main()'s first receive and the shutdown
	// path arming its own select on this channel. signal.Notify does a
	// non-blocking send, so a full buffer would silently discard the "I mean
	// it, exit now" signal.
	done := make(chan os.Signal, 2)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	serverErr := startServer(httpServer, cfg.TLSCertFile, cfg.TLSKeyFile)

	// Startup is complete and the serving goroutine has been launched:
	// SetInitialized flips /readyz to ready. startServer only starts the
	// goroutine; it does not confirm the listener has bound, so there is a brief
	// window where the socket may not yet be accepting. That is acceptable for
	// MCP-only readiness, which reflects the server's own initialization rather
	// than listener bind state. SetInitialized is idempotent.
	readiness.SetInitialized()

	var startupErr error
	var fromSignal bool
	var receivedSignal os.Signal
	select {
	case sig := <-done:
		// Shutdown signal received: drain before shutting down. The drain (flip
		// /readyz, sleep the propagation window, then graceful shutdown) runs in
		// drainAndShutdown below. SIGTERM (orchestrator-initiated) honors the
		// configured drain window; SIGINT (a local Ctrl-C) skips it via
		// drainDelayForSignal — there is no orchestrator to wait for. The
		// shutdown-timeout context is NOT built here; gracefulShutdown builds it
		// after the drain sleep so Shutdown gets the full budget (see
		// drainAndShutdown / gracefulShutdown).
		fromSignal = true
		receivedSignal = sig
		slog.Info("server shutting down",
			slog.String("reason", "signal"),
			slog.String("signal", sig.String()))
	case startupErr = <-serverErr:
		// Error already logged in startServer. Capture it so future telemetry
		// hooks (e.g., metrics flush, error reporting) have access to the value,
		// then run cleanup before exiting non-zero. This path does NOT drain:
		// the server never began serving traffic, so there is nothing to drain
		// and no orchestrator routing to wait out.
	}

	// The shutdown-timeout budget is a DURATION, not a pre-built context: the
	// shutdown helpers create their own timeout context immediately before
	// srv.Shutdown so the deadline always covers the full budget. On the signal
	// path that context is built AFTER the drain sleep, so the drain delay does
	// not eat into the budget Shutdown gets to drain in-flight requests.
	shutdownTimeout := time.Duration(defaults.DefaultShutdownTimeoutSeconds) * time.Second

	if fromSignal {
		drainDelay := drainDelayForSignal(receivedSignal, time.Duration(cfg.Observability.ShutdownDrainDelayS)*time.Second)
		// Pass the still-registered signal channel so a second SIGINT/SIGTERM
		// during the drain or graceful wait forces an immediate close (SOL-151437).
		_ = drainAndShutdown(httpServer, readiness, drainDelay, shutdownTimeout, done)
	} else {
		// Startup-error path never began serving, so there is nothing to drain
		// and no second-signal semantics: pass a nil force channel.
		_ = gracefulShutdown(httpServer, shutdownTimeout, nil)
	}

	slog.Info("server stopped")
	if startupErr != nil {
		os.Exit(1)
	}
}
