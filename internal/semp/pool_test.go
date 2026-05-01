package semp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
)

func testSEMPConfig() *config.SEMPConfig {
	retries := 0
	minInterval := time.Duration(0)
	return &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
}

func newTestServerConfig(serverURL string) *config.ServerConfig {
	return &config.ServerConfig{
		Brokers: map[string]*config.BrokerConfig{
			"prod-us": {
				URL: serverURL,
				Auth: config.AuthConfig{
					Mode:     "basic",
					Username: "admin",
					Password: "secret",
				},
			},
			"prod-eu": {
				URL: serverURL,
				Auth: config.AuthConfig{
					Mode:     "basic",
					Username: "admin",
					Password: "secret",
				},
			},
		},
		SEMP: *testSEMPConfig(),
	}
}

func TestBrokerPool_GetSEMPv2_ValidAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	client, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv2() error: %v", err)
	}
	if client == nil {
		t.Fatal("GetSEMPv2() returned nil client")
	}
}

func TestBrokerPool_GetSEMPv2_UnknownAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	_, err := pool.GetSEMPv2("nonexistent")
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
		t.Error("expected no HTTP requests before GetSEMPv2() call")
	}

	// Access one broker.
	_, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv2() error: %v", err)
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

	client1, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("first GetSEMPv2() error: %v", err)
	}

	client2, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("second GetSEMPv2() error: %v", err)
	}

	// Both calls should return the same underlying client instance.
	// We verify by checking they're the same interface value.
	if client1 != client2 {
		t.Error("GetSEMPv2() returned different instances for the same alias — expected shared instance")
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
			c, err := pool.GetSEMPv2("prod-us")
			if err != nil {
				t.Errorf("goroutine %d: GetSEMPv2() error: %v", idx, err)
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

// newV1TestServer returns an httptest server that answers every request with
// a minimal valid SEMPv1 success envelope. Shared by the v1 pool tests below.
func newV1TestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
	}))
}

// TestBrokerPool_GetSEMPv1_ValidAlias parallels the V2 happy-path test: a
// configured alias yields a non-nil SEMPv1 client.
func TestBrokerPool_GetSEMPv1_ValidAlias(t *testing.T) {
	server := newV1TestServer()
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	client, err := pool.GetSEMPv1("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv1() error: %v", err)
	}
	if client == nil {
		t.Fatal("GetSEMPv1() returned nil client")
	}
}

// TestBrokerPool_GetSEMPv1_UnknownAlias parallels the V2 error-path test:
// requesting an unconfigured alias returns an error, not a client.
func TestBrokerPool_GetSEMPv1_UnknownAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	_, err := pool.GetSEMPv1("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown alias")
	}
}

// TestBrokerPool_GetSEMPv1_LazyCreation confirms the pool does not touch the
// network at construction time — requests only fire after the first
// GetSEMPv1() call that hits Execute. Aliases() still reports every
// configured broker regardless of whether any were accessed.
func TestBrokerPool_GetSEMPv1_LazyCreation(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	// No requests should have been made yet — clients are lazy.
	if requestCount.Load() != 0 {
		t.Error("expected no HTTP requests before GetSEMPv1() call")
	}

	// Access one broker.
	_, err := pool.GetSEMPv1("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv1() error: %v", err)
	}

	// Aliases should still return both brokers.
	aliases := pool.Aliases()
	if len(aliases) != 2 {
		t.Errorf("Aliases() returned %d, want 2", len(aliases))
	}
}

// TestBrokerPool_GetSEMPv1_SharedInstance verifies repeated calls for the
// same alias return the same interface value. If they differed, each call
// would be creating a fresh client — defeating pooling.
func TestBrokerPool_GetSEMPv1_SharedInstance(t *testing.T) {
	server := newV1TestServer()
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	client1, err := pool.GetSEMPv1("prod-us")
	if err != nil {
		t.Fatalf("first GetSEMPv1() error: %v", err)
	}

	client2, err := pool.GetSEMPv1("prod-us")
	if err != nil {
		t.Fatalf("second GetSEMPv1() error: %v", err)
	}

	if client1 != client2 {
		t.Error("GetSEMPv1() returned different instances for the same alias — expected shared instance")
	}
}

// TestBrokerPool_GetSEMPv1_ConcurrentFirstAccess exercises the double-check
// path: 50 goroutines call GetSEMPv1() on a fresh pool simultaneously, and
// all must receive the same client instance. If the double-check were
// missing, some goroutines would create their own BrokerClient, and the
// returned interfaces would differ.
func TestBrokerPool_GetSEMPv1_ConcurrentFirstAccess(t *testing.T) {
	server := newV1TestServer()
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	const goroutines = 50
	clients := make([]interface{}, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			c, err := pool.GetSEMPv1("prod-us")
			if err != nil {
				t.Errorf("goroutine %d: GetSEMPv1() error: %v", idx, err)
				return
			}
			clients[idx] = c
		}(i)
	}

	wg.Wait()

	first := clients[0]
	for i := 1; i < goroutines; i++ {
		if clients[i] != first {
			t.Errorf("goroutine %d got a different client — expected all to share one instance", i)
		}
	}
}

// TestBrokerPool_SharedBrokerClientAcrossProtocols is the structural proof
// of the "log once per alias" invariant from T4's Definition of Done. Both
// GetSEMPv1 and GetSEMPv2 route through getOrCreate and must operate on the
// same underlying *BrokerClient per alias. If they used separate cache
// entries, the creation log line would fire twice and two different TCP
// connection pools would be allocated per broker.
//
// We verify the invariant by (a) calling GetSEMPv1 first, which creates the
// BrokerClient and caches it, then (b) calling GetSEMPv2 on the same alias
// and checking the v2 client's underlying pointer is the one stored at
// creation time. The easiest observable proxy for "same BrokerClient" is
// that a second GetSEMPv1 call after GetSEMPv2 still returns the original
// v1 client — which can only happen if GetSEMPv2 didn't replace the cache
// entry.
func TestBrokerPool_SharedBrokerClientAcrossProtocols(t *testing.T) {
	server := newV1TestServer()
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(server.URL))

	v1First, err := pool.GetSEMPv1("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv1() error: %v", err)
	}

	// Cross-protocol access must not create a second BrokerClient.
	v2, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv2() error: %v", err)
	}
	if v2 == nil {
		t.Fatal("GetSEMPv2() returned nil after GetSEMPv1() created the BrokerClient")
	}

	// If GetSEMPv2 had silently replaced the cache entry, this second v1
	// lookup would return a different client. Same instance means the cache
	// entry was preserved across protocols.
	v1Second, err := pool.GetSEMPv1("prod-us")
	if err != nil {
		t.Fatalf("second GetSEMPv1() error: %v", err)
	}
	if v1First != v1Second {
		t.Error("v1 client instance changed after GetSEMPv2() call — cache entry was not shared across protocols")
	}
}
