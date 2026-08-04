package semp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

func TestBrokerClient_V2_ReturnsClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := testSEMPConfig()

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg, nil)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv2()

	if client == nil {
		t.Fatal("SEMPv2() returned nil")
	}
}

func TestBrokerClient_V2_ExecutePassesThrough(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer server.Close()

	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := testSEMPConfig()

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg, nil)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv2()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, execErr := client.Execute(context.Background(), op, map[string]any{})
	if execErr != nil {
		t.Fatalf("Execute() error: %v", execErr)
	}

	if !called {
		t.Error("HTTP server was not called — Execute did not pass through to HTTPClient")
	}
}

// TestBrokerClient_V1_ReturnsClient verifies that NewBrokerClient populates
// the SEMPv1 peer field and SEMPv1() returns a non-nil client. Parallels the
// existing V2 test to confirm both protocols are initialized at construction.
func TestBrokerClient_V1_ReturnsClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
	}))
	defer server.Close()

	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := testSEMPConfig()

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg, nil)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv1()

	if client == nil {
		t.Fatal("SEMPv1() returned nil")
	}
}

// TestBrokerClient_V1_ExecutePassesThrough verifies that calling Execute on
// the SEMPv1() client reaches the broker's HTTP endpoint. Parallels the V2
// pass-through test to prove the v1 accessor is wired to a real HTTPClient,
// not a nil-or-stub value.
func TestBrokerClient_V1_ExecutePassesThrough(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
	}))
	defer server.Close()

	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := testSEMPConfig()

	bc, err := semp.NewBrokerClient("test-broker", brokerCfg, sempCfg, nil)
	if err != nil {
		t.Fatalf("NewBrokerClient() error: %v", err)
	}
	client := bc.SEMPv1()

	_, execErr := client.Execute(context.Background(), `<rpc><show><version/></show></rpc>`)
	if execErr != nil {
		t.Fatalf("Execute() error: %v", execErr)
	}

	if !called {
		t.Error("HTTP server was not called — Execute did not pass through to HTTPClient")
	}
}

// TestBrokerClient_SharedJar_401ClearVisibleAcrossProtocols verifies the
// invariant that NewBrokerClient creates one SafeCookieJar shared by the
// Authenticator, SEMPv1, and SEMPv2 clients. The test sequence:
//
//  1. V2 request → server sets a session cookie, then returns 401.
//  2. BasicAuthenticator.HandleAuthFailure clears the jar and signals retry.
//  3. V2 retry → arrives WITHOUT the stale cookie (proves auth and V2 share the jar).
//  4. V1 request → also arrives WITHOUT the cookie (proves V1 shares the same jar).
//
// If the jar were per-client (the old design), step 3 or 4 would still carry
// the stale cookie because the Authenticator would have cleared a different jar.
func TestBrokerClient_SharedJar_401ClearVisibleAcrossProtocols(t *testing.T) {
	var v2Attempt atomic.Int32
	var v1Cookie atomic.Value // stores string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/SEMP/v2":
			n := v2Attempt.Add(1)
			if n == 1 {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "stale"})
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if _, err := r.Cookie("session"); err == nil {
				t.Error("V2 retry still carries stale cookie — jar was not cleared")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{},"meta":{}}`))

		default:
			cookieVal := ""
			if c, err := r.Cookie("session"); err == nil {
				cookieVal = c.Value
			}
			v1Cookie.Store(cookieVal)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
		}
	}))
	defer server.Close()

	retries := 3
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       5 * time.Millisecond,
	}
	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "secret"},
	}

	bc, err := semp.NewBrokerClient("shared-jar-test", brokerCfg, sempCfg, nil)
	if err != nil {
		t.Fatalf("NewBrokerClient: %v", err)
	}
	defer bc.Close()

	// Step 1-3: V2 Execute triggers 401 → jar clear → retry without cookie.
	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/about",
	}
	if _, err := bc.SEMPv2().Execute(context.Background(), op, nil); err != nil {
		t.Fatalf("V2 Execute: %v", err)
	}
	if v2Attempt.Load() != 2 {
		t.Fatalf("expected 2 V2 attempts (original + 1 retry), got %d", v2Attempt.Load())
	}

	// Step 4: V1 Execute — must not carry the stale cookie.
	if _, err := bc.SEMPv1().Execute(context.Background(), `<rpc><show><version/></show></rpc>`); err != nil {
		t.Fatalf("V1 Execute: %v", err)
	}
	if got, _ := v1Cookie.Load().(string); got != "" {
		t.Errorf("V1 request carried cookie %q — jar is not shared across protocols", got)
	}
}
