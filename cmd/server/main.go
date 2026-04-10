package main

import (
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	// 1. Load config
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = defaults.DefaultConfigPath
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	slog.Info("Loaded config", "broker_count", len(cfg.Brokers)) //nolint:gosec // G706 — slog attrs are auto-escaped; fix in gosec v2.26.0

	// 2. Parse embedded OpenAPI specs
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		log.Fatalf("Failed to parse OpenAPI specs: %v", err)
	}
	slog.Info("Parsed operations from embedded specs", "count", len(operations))

	// 3. Create broker pool
	pool := semp.NewBrokerPool(cfg)
	slog.Info("Created broker pool", "aliases", pool.Aliases()) //nolint:gosec // G706 — slog attrs are auto-escaped; fix in gosec v2.26.0

	// 4. Create MCP server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: "0.1.0",
	}, nil)

	// 5. Register tools
	// TEMPORARY: Remove this line and delete smoketest.go when the composite tool executor is implemented.
	registerSmokeTestTools(server, pool, operations)

	// 6. Set up HTTP routes
	mux := http.NewServeMux()

	mux.Handle("/", mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
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

	// 7. Start server
	addr := fmt.Sprintf(":%s", cfg.Port)
	slog.Info("MCP server listening", "addr", addr) //nolint:gosec // G706 — slog attrs are auto-escaped; fix in gosec v2.26.0
	if err := http.ListenAndServe(addr, mux); err != nil { //nolint:gosec // G114: no timeout by design — MCP uses long-lived streaming HTTP connections
		log.Fatalf("Server failed: %v", err)
	}
}
