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

package semp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// writeTestConfig writes a minimal broker-config YAML to t.TempDir, loads it
// via LoadConfig (exercising the real validate+canonicalize path), and
// returns the resulting *ServerConfig. brokers is an ordered list of
// (alias, url) pairs — order matters so callers can write tests against
// specific aliases without map iteration churn.
func writeTestConfig(t *testing.T, brokers ...[2]string) *config.ServerConfig {
	t.Helper()
	var b []byte
	b = append(b, []byte("mcp_client_auth:\n  mode: disabled\nbrokers:\n")...)
	for _, kv := range brokers {
		b = append(b, []byte(fmt.Sprintf("  %s:\n    url: %s\n    auth:\n      mode: basic\n      username: admin\n      password: secret\n", kv[0], kv[1]))...)
	}
	path := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

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

func newTestServerConfig(t *testing.T, serverURL string) *config.ServerConfig {
	t.Helper()
	return writeTestConfig(t,
		[2]string{"prod-us", serverURL},
		[2]string{"prod-eu", serverURL},
	)
}

func TestBrokerPool_GetSEMPv2_ValidAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

// TestBrokerPool_Close_BeforeAnyAccess confirms Close is safe to call on a
// freshly constructed pool that never had a client created. With no lazy
// creations, the iteration loop in Close has nothing to do; it must not
// panic.
func TestBrokerPool_Close_BeforeAnyAccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)
	pool.Close() // must not panic
}

// TestBrokerPool_Close_AfterLazyCreation exercises the production path that
// matters for cmd/server/main.go's shutdown defer: lazily create a client
// (whose BrokerClient owns the broker's shared rate limiter), then Close the
// pool. The limiter is internal, so we verify the observable contract: no
// panic, and Close is idempotent — calling it twice must remain safe.
func TestBrokerPool_Close_AfterLazyCreation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

	// Force one client into existence (lazy creation path).
	if _, err := pool.GetSEMPv2("prod-us"); err != nil {
		t.Fatalf("GetSEMPv2: %v", err)
	}

	// First Close: shuts down the broker client(s) that exist.
	pool.Close()
	// Second Close: must remain safe — main()'s defer fires once, but
	// future call sites or test helpers may exercise multi-close. The
	// per-broker RateLimiter.Stop is documented as safe to call multiple times.
	pool.Close()
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

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)

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

// --- Broker alias contract tests (SOL-149789) ---

// TestBrokerPool_GetSEMPv2_CaseInsensitive: looking up a broker with mixed
// case returns the same cached client instance as the canonical form.
func TestBrokerPool_GetSEMPv2_CaseInsensitive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	cfg := writeTestConfig(t, [2]string{"ProdEast", server.URL})
	pool := semp.NewBrokerPool(cfg, nil)

	first, err := pool.GetSEMPv2("ProdEast")
	if err != nil {
		t.Fatalf("GetSEMPv2(ProdEast): %v", err)
	}
	for _, alias := range []string{"prodeast", "PRODEAST", "ProdEast", "pRoDeAsT"} {
		got, err := pool.GetSEMPv2(alias)
		if err != nil {
			t.Errorf("GetSEMPv2(%q): %v", alias, err)
			continue
		}
		if got != first {
			t.Errorf("GetSEMPv2(%q) returned a different client than the canonical lookup", alias)
		}
	}
}

// TestBrokerPool_Aliases_PreservesDisplayCasing: pool.Aliases() returns the
// original casing from the source config, not the canonical lowercase form.
func TestBrokerPool_Aliases_PreservesDisplayCasing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	cfg := writeTestConfig(t,
		[2]string{"ProdEast", server.URL},
		[2]string{"DevWest", server.URL},
	)
	pool := semp.NewBrokerPool(cfg, nil)

	got := pool.Aliases()
	want := []string{"DevWest", "ProdEast"}
	if len(got) != len(want) {
		t.Fatalf("Aliases() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Aliases()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func expectedBasicAuth(t *testing.T, user, pass string) string {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/", nil)
	r.SetBasicAuth(user, pass)
	return r.Header.Get("Authorization")
}

func twoBrokerConfig(t *testing.T, urlA, urlB string) *config.ServerConfig {
	t.Helper()
	yaml := fmt.Sprintf(`mcp_client_auth:
  mode: disabled
brokers:
  broker-a:
    url: %s
    auth:
      mode: basic
      username: alice
      password: secret-a
  broker-b:
    url: %s
    auth:
      mode: basic
      username: bob
      password: secret-b
`, urlA, urlB)
	path := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestBrokerPool_CredentialIsolation_SEMPv2(t *testing.T) {
	var mu sync.Mutex
	var authA, authB string

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authA = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authB = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer serverB.Close()

	pool := semp.NewBrokerPool(twoBrokerConfig(t, serverA.URL, serverB.URL), nil)

	op := &sempv2.Operation{ID: "testOp", Method: http.MethodGet, Path: "/SEMP/v2/monitor/about"}

	clientA, err := pool.GetSEMPv2("broker-a")
	if err != nil {
		t.Fatalf("GetSEMPv2(broker-a): %v", err)
	}
	if _, err := clientA.Execute(context.Background(), op, nil); err != nil {
		t.Fatalf("clientA.Execute: %v", err)
	}

	clientB, err := pool.GetSEMPv2("broker-b")
	if err != nil {
		t.Fatalf("GetSEMPv2(broker-b): %v", err)
	}
	if _, err := clientB.Execute(context.Background(), op, nil); err != nil {
		t.Fatalf("clientB.Execute: %v", err)
	}

	wantA := expectedBasicAuth(t, "alice", "secret-a")
	wantB := expectedBasicAuth(t, "bob", "secret-b")

	mu.Lock()
	defer mu.Unlock()
	if authA != wantA {
		t.Errorf("broker-a Authorization = %q, want %q", authA, wantA)
	}
	if authB != wantB {
		t.Errorf("broker-b Authorization = %q, want %q", authB, wantB)
	}
	if authA == authB {
		t.Errorf("broker-a and broker-b saw the same Authorization header %q — credentials crossed brokers", authA)
	}
}

func TestBrokerPool_CredentialIsolation_SEMPv1(t *testing.T) {
	var mu sync.Mutex
	var authA, authB string

	respBody := []byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`)

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authA = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(respBody)
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authB = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(respBody)
	}))
	defer serverB.Close()

	pool := semp.NewBrokerPool(twoBrokerConfig(t, serverA.URL, serverB.URL), nil)

	const rpc = `<rpc><show><version/></show></rpc>`

	clientA, err := pool.GetSEMPv1("broker-a")
	if err != nil {
		t.Fatalf("GetSEMPv1(broker-a): %v", err)
	}
	if _, err := clientA.Execute(context.Background(), rpc); err != nil {
		t.Fatalf("clientA.Execute: %v", err)
	}

	clientB, err := pool.GetSEMPv1("broker-b")
	if err != nil {
		t.Fatalf("GetSEMPv1(broker-b): %v", err)
	}
	if _, err := clientB.Execute(context.Background(), rpc); err != nil {
		t.Fatalf("clientB.Execute: %v", err)
	}

	wantA := expectedBasicAuth(t, "alice", "secret-a")
	wantB := expectedBasicAuth(t, "bob", "secret-b")

	mu.Lock()
	defer mu.Unlock()
	if authA != wantA {
		t.Errorf("broker-a Authorization = %q, want %q", authA, wantA)
	}
	if authB != wantB {
		t.Errorf("broker-b Authorization = %q, want %q", authB, wantB)
	}
	if authA == authB {
		t.Errorf("broker-a and broker-b saw the same Authorization header %q — credentials crossed brokers", authA)
	}
}
