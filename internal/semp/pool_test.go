package semp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
)

func newTestServerConfig(serverURL string) *config.ServerConfig {
	return &config.ServerConfig{
		Brokers: map[string]*config.BrokerConfig{
			"prod-us": {
				URL: serverURL,
				Auth: config.AuthConfig{
					Method:   "basic",
					Username: "admin",
					Password: "secret",
				},
			},
			"prod-eu": {
				URL: serverURL,
				Auth: config.AuthConfig{
					Method:   "basic",
					Username: "admin",
					Password: "secret",
				},
			},
		},
		SEMP: config.SEMPConfig{
			RequestTimeoutSeconds: 5,
		},
	}
}

func TestBrokerPool_GetSempV2_ValidAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	client, err := pool.GetSempV2("prod-us")
	if err != nil {
		t.Fatalf("GetSempV2() error: %v", err)
	}
	if client == nil {
		t.Fatal("GetSempV2() returned nil client")
	}
}

func TestBrokerPool_GetSempV2_UnknownAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	_, err := pool.GetSempV2("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown alias")
	}
}

func TestBrokerPool_LazyCreation(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	// No requests should have been made yet — clients are lazy.
	if requestCount.Load() != 0 {
		t.Error("expected no HTTP requests before GetSempV2() call")
	}

	// Access one broker.
	_, err := pool.GetSempV2("prod-us")
	if err != nil {
		t.Fatalf("GetSempV2() error: %v", err)
	}

	// Aliases should still return both brokers.
	aliases := pool.Aliases()
	if len(aliases) != 2 {
		t.Errorf("Aliases() returned %d, want 2", len(aliases))
	}
}

func TestBrokerPool_SharedInstance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	client1, err := pool.GetSempV2("prod-us")
	if err != nil {
		t.Fatalf("first GetSempV2() error: %v", err)
	}

	client2, err := pool.GetSempV2("prod-us")
	if err != nil {
		t.Fatalf("second GetSempV2() error: %v", err)
	}

	// Both calls should return the same underlying client instance.
	// We verify by checking they're the same interface value.
	if client1 != client2 {
		t.Error("GetSempV2() returned different instances for the same alias — expected shared instance")
	}
}

func TestBrokerPool_ConcurrentFirstAccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	const goroutines = 50
	clients := make([]interface{}, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			c, err := pool.GetSempV2("prod-us")
			if err != nil {
				t.Errorf("goroutine %d: GetSempV2() error: %v", idx, err)
				return
			}
			clients[idx] = c
		}(i)
	}

	wg.Wait()

	// All goroutines should have received the same client.
	first := clients[0]
	for i := 1; i < goroutines; i++ {
		if clients[i] != first {
			t.Errorf("goroutine %d got a different client — expected all to share one instance", i)
		}
	}
}

func TestBrokerPool_Aliases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	aliases := pool.Aliases()
	if len(aliases) != 2 {
		t.Fatalf("Aliases() returned %d entries, want 2", len(aliases))
	}

	// Should be sorted.
	if aliases[0] != "prod-eu" || aliases[1] != "prod-us" {
		t.Errorf("Aliases() = %v, want [prod-eu, prod-us]", aliases)
	}
}
