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
	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	sempv1tools "github.com/SolaceDev/solace-broker-mcp/internal/tools/sempv1"
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
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"status": "ok"}`)); err != nil {
			http.Error(w, "failed to write response", http.StatusInternalServerError)
		}
	})

	return mux
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

	// 1. Load config. config.Load handles path resolution internally
	//    (CONFIG_FILE env var, then /etc/mcp-server/config.yaml, then
	//    ./broker-config.yaml). See config.Load docs for exact semantics.
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Reconfigure slog with the user-configured level. cfg.LogLevel is
	// validated and normalized to one of debug/info/warn/error.
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	slog.SetDefault(slog.New(newSlogHandler(level)))

	slog.Info("config loaded",
		slog.String("version", version.Version()),
		slog.Int("broker_count", len(cfg.Brokers)),
		slog.Int("port", cfg.Port),
		slog.String("log_level", cfg.LogLevel))

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

	// 5. Create composite executor
	executor := composite.NewCompositeExecutor(operations)

	// 6. Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: version.Version(),
	}, nil)

	// 7. Create tool manager from composite tool definitions
	mgr := tools.NewToolManagerFromComposite(pool, compositeTools, executor)

	// 7a. Register Go-native SEMPv1 tool handlers on the same manager.
	// They flow through the same RegisterWithServer pass below.
	mgr.Register(sempv1tools.NewGetRedundancyStatusHandler())

	tools.RegisterWithServer(mgr, server, pool)
	slog.Info("registered composite tools",
		slog.Int("tool_count", len(compositeTools)))
	slog.Info("registered sempv1 tools",
		slog.Int("tool_count", 1))

	// 8. Register list-brokers discovery tool
	tools.RegisterListBrokers(server, pool)

	// 9. Set up HTTP routes
	mux := buildMux()

	// Create MCP handler
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	// Wrap MCP handler with auth middleware
	authedHandler, err := auth.NewAuthMiddleware(cfg, mcpHandler)
	if err != nil {
		slog.Error("failed to create auth middleware", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Register authenticated MCP endpoint
	mux.Handle("/mcp", authedHandler)

	// Register OAuth Protected Resource Metadata endpoint (RFC 9728)
	// This enables MCP clients to discover the authorization server for OAuth flows
	if metadataHandler := auth.NewProtectedResourceMetadataHandler(cfg); metadataHandler != nil {
		mux.Handle("/.well-known/oauth-protected-resource", metadataHandler)
		slog.Info("registered OAuth protected resource metadata endpoint")
	}

	// 10. Start server with graceful shutdown
	addr := fmt.Sprintf(":%d", cfg.Port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: time.Duration(defaults.DefaultReadHeaderTimeoutSeconds) * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		var err error
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			slog.Info("server starting with TLS",
				slog.String("addr", addr),
				slog.String("cert", cfg.TLSCertFile))
			err = httpServer.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			slog.Info("server starting",
				slog.String("addr", addr))
			err = httpServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("server shutting down", slog.String("reason", "signal"))

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
}
