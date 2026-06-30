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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/banner"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	_ "github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess/handlers" // register handlers via init()
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/middleware/recovery"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv1/brokerstatus"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv1/discardstats"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv1/redundancy"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv2/clientactions"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv2/queueactions"
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

	return slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
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
}

// buildMux creates the HTTP route multiplexer with basic routes.
// The /mcp route is registered separately in main() with auth middleware.
// Both main() and tests use this function to avoid route drift.
//
// aliases returns the list of broker names to check.
// probeBroker tests connectivity to a single broker; it returns nil on success.
// readiness backs the /readyz endpoint, which reflects the MCP server's OWN
// readiness and is decoupled from the broker (it consults neither aliases nor
// probeBroker).
func buildMux(aliases func() []string, probeBroker func(ctx context.Context, broker string) error, readiness *health.ReadinessState) *http.ServeMux {
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
	mux.Handle("/readyz", health.ReadyzHandler(readiness))

	// TODO(SOL-151285 / Story 11): /readyz now exists (MCP-server-only readiness).
	// /ready below is still the legacy broker-coupled inline handler; Story 11
	// will alias /ready to /readyz and drop the broker dial.
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		// Config validation ensures there is at least one broker configured, so aliases()
		// is never empty in production.
		brokers := aliases()
		type brokerResult struct {
			broker string
			err    error
		}
		results := make(chan brokerResult, len(brokers))

		var wg sync.WaitGroup
		wg.Add(len(brokers))

		// Probe each broker concurrently, then collect in configured order.
		for _, broker := range brokers {
			go func(broker string) {
				defer wg.Done()
				timeout := time.Duration(defaults.DefaultReadinessProbeTimeoutSeconds) * time.Second
				reqCtx, cancel := context.WithTimeout(r.Context(), timeout)
				defer cancel()
				results <- brokerResult{broker: broker, err: probeBroker(reqCtx, broker)}
			}(broker)
		}

		wg.Wait()
		close(results)

		resultsByBroker := make(map[string]error, len(brokers))
		for res := range results {
			resultsByBroker[res.broker] = res.err
		}
		readyBrokers := []string{}
		errs := []string{}
		for _, broker := range brokers {
			if err := resultsByBroker[broker]; err != nil {
				errs = append(errs, fmt.Sprintf("%s: unreachable", broker))
			} else {
				readyBrokers = append(readyBrokers, broker)
			}
		}

		if len(errs) > 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			resp, _ := json.Marshal(map[string]any{"ready": false, "errors": errs})
			_, _ = w.Write(resp)
			return
		}
		w.WriteHeader(http.StatusOK)
		resp, _ := json.Marshal(map[string]any{"ready": true, "brokers": readyBrokers})
		_, _ = w.Write(resp)
	})

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

// newBrokerReachabilityProbe returns a probe function that checks whether a
// broker's SEMP port is accepting TCP connections. The returned function
// resolves the broker alias against cfg, parses the URL, defaults the port
// (443 for HTTPS, 80 otherwise), and performs a TCP dial. It is the
// production implementation of buildMux's probeBroker parameter.
func newBrokerReachabilityProbe(cfg *config.ServerConfig) func(context.Context, string) error {
	return func(ctx context.Context, broker string) error {
		brokerCfg, ok := cfg.Broker(broker)
		if !ok {
			return fmt.Errorf("unknown broker %q", broker)
		}
		u, err := url.Parse(brokerCfg.URL)
		if err != nil {
			return fmt.Errorf("broker %q: invalid URL: %w", broker, err)
		}
		host := u.Hostname()
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			slog.Warn("broker TCP connectivity check failed",
				slog.String("broker", broker),
				slog.String("error", err.Error()))
			return err
		}
		_ = conn.Close()
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

// registerSEMPv2Tools attaches every Go-native SEMPv2 tool handler to mgr.
// These are the per-action write tools that issue PUT requests to the SEMPv2
// action API. They share the same registration pipeline as the SEMPv1
// handlers; RegisterWithServer gates them behind enable_write_tools (they are
// not read-only), and the manager logs a WARNING for the destructive ones
// (delete-queue-messages, disconnect-client).
func registerSEMPv2Tools(mgr *tools.ToolManager) {
	mgr.Register(queueactions.NewDeleteMessagesHandler())
	mgr.Register(queueactions.NewClearStatsHandler())
	mgr.Register(clientactions.NewDisconnectHandler())
	mgr.Register(clientactions.NewClearStatsHandler())
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
	banner.LogStartupAuthMode(cfg.MCPClientAuth.Mode, cfg.MCPClientAuth.Issuer)

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

	// 3. Create broker pool
	pool := semp.NewBrokerPool(cfg)
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

	// 4. Load embedded composite tool definitions
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

	// 5. Create composite executor
	executor := composite.NewCompositeExecutor(operations)

	// 6. Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: version.Version(),
	}, nil)

	// 7. Create the tool manager and register every tool the server exposes.
	// All registrations happen in one block so the log line below is a
	// reliable phase boundary — anything before it is registered, anything
	// after it sees a fully-loaded server.
	mgr := tools.NewToolManagerFromComposite(pool, compositeTools, executor)
	registerSEMPv1Tools(mgr)
	registerSEMPv2Tools(mgr)
	tools.RegisterWithServer(mgr, server, pool, cfg.EnableWriteTools)
	slog.Info("tool registration complete",
		slog.Bool("enable_write_tools", cfg.EnableWriteTools))

	// list-brokers is a discovery tool registered directly on the MCP
	// server (it doesn't need broker resolution or the ToolManager pipeline).
	tools.RegisterListBrokers(server, pool)

	slog.Info("all tools registered")

	// 8. Set up HTTP routes
	//
	// readiness backs /readyz. It starts in the "starting" state; we mark it
	// initialized once the server begins listening (below). It is decoupled from
	// the broker and is NOT flag-gated.
	readiness := health.NewReadinessState()
	mux := buildMux(pool.Aliases, newBrokerReachabilityProbe(cfg), readiness)

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

	// 9. Wrap the whole mux with panic-recovery as the OUTERMOST handler
	// (ADR-001). Recovery is unconditional. A panic in ANY handler is caught,
	// logged with a structured stack, and returned as a clean 500; the process
	// keeps running. Recovery sits OUTSIDE the per-route correlation middleware
	// wired on /mcp above — different layers, no conflict.
	rootHandler := buildRootHandler(mux)

	// 10. Start server with graceful shutdown
	addr := fmt.Sprintf(":%d", cfg.Port)
	httpServer := newHTTPServer(addr, rootHandler)

	done := make(chan os.Signal, 1)
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
	select {
	case <-done:
		// TODO(SOL-151288 / Story 31a): call readiness.SetShuttingDown() here on
		// SIGTERM, before httpServer.Shutdown, so /readyz reports "shutting_down"
		// during graceful drain. The mechanism exists on ReadinessState today;
		// wiring it to the signal is Story 31a's scope.
		slog.Info("server shutting down", slog.String("reason", "signal"))
	case startupErr = <-serverErr:
		// Error already logged in startServer. Capture it so future telemetry
		// hooks (e.g., metrics flush, error reporting) have access to the value,
		// then run cleanup before exiting non-zero.
	}

	shutdownTimeout := time.Duration(defaults.DefaultShutdownTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("shutdown timed out, forcing close", slog.String("error", err.Error()))
		if closeErr := httpServer.Close(); closeErr != nil {
			slog.Error("forced close failed", slog.String("error", closeErr.Error()))
		}
	}

	slog.Info("server stopped")
	if startupErr != nil {
		os.Exit(1)
	}
}
