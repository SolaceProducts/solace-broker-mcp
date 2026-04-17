package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/registry"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// buildMux creates the HTTP route multiplexer with all registered routes.
// It only handles route registration — no middleware, no server config.
// Both main() and tests use this function to avoid route drift.
func buildMux(server *mcp.Server) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil))

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

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version.Version)
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
		slog.String("version", version.Version),
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
	tools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		slog.Error("failed to load composite tools", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slog.Info("loaded composite tool definitions",
		slog.Int("tool_count", len(tools)))

	// 5. Create composite executor
	executor := composite.NewCompositeExecutor(operations)

	// 6. Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: version.Version,
	}, nil)

	// 7. Register composite tools
	reg := registry.NewRegistry(server, pool, executor)
	if err := reg.RegisterAll(tools); err != nil {
		slog.Error("failed to register tools", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slog.Info("registered composite tools",
		slog.Int("tool_count", len(tools)))

	// 8. Register list-brokers discovery tool
	reg.RegisterListBrokers()

	// 9. Set up HTTP routes
	mux := buildMux(server)

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
