package sempv2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// newRetryTestClient creates an HTTPClient with retry enabled for testing.
// It connects to the provided test server and uses the given auth mode.
func newRetryTestClient(t *testing.T, server *httptest.Server, authMode string, retries int) *HTTPClient {
	t.Helper()
	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     authMode,
			Username: "admin",
			Password: "secret",
			Token:    "static-token",
		},
	}
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	client, err := NewHTTPClient(brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}
	return client
}

func simpleOp() *Operation {
	return &Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}
}

func jsonOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
}

// --- Rate Limiter Tests ---

func TestClient_RateLimiter_BlocksUntilChannelReady(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		jsonOK(w)
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 0)

	// Replace rate limiter with a manually controlled channel.
	ch := make(chan time.Time, 1)
	client.rateLimiter = ch

	// Start Execute in a goroutine — it should block on the rate limiter.
	done := make(chan error, 1)
	go func() {
		_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
		done <- err
	}()

	// Give the goroutine time to reach the rate limiter.
	time.Sleep(50 * time.Millisecond)
	if requestCount.Load() != 0 {
		t.Fatal("expected request to be blocked by rate limiter")
	}

	// Unblock the rate limiter.
	ch <- time.Now()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute() did not complete after rate limiter unblocked")
	}

	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request, got %d", requestCount.Load())
	}
}

func TestClient_RateLimiter_DisabledWhenZero(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		jsonOK(w)
	}))
	defer server.Close()

	// Zero interval = no rate limiting (closed channel, non-blocking).
	client := newRetryTestClient(t, server, "basic", 0)

	// Execute should complete immediately without blocking.
	for i := range 5 {
		_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
		if err != nil {
			t.Fatalf("Execute() #%d error: %v", i, err)
		}
	}

	if requestCount.Load() != 5 {
		t.Errorf("expected 5 requests, got %d", requestCount.Load())
	}
}

func TestClient_RateLimiter_PerBrokerIndependence(t *testing.T) {
	var countA, countB atomic.Int32
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countA.Add(1)
		jsonOK(w)
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countB.Add(1)
		jsonOK(w)
	}))
	defer serverB.Close()

	clientA := newRetryTestClient(t, serverA, "basic", 0)
	clientB := newRetryTestClient(t, serverB, "basic", 0)

	// Block client A's rate limiter.
	chA := make(chan time.Time, 1)
	clientA.rateLimiter = chA

	// Client B should work independently.
	_, err := clientB.Execute(context.Background(), simpleOp(), map[string]any{})
	if err != nil {
		t.Fatalf("clientB Execute() error: %v", err)
	}
	if countB.Load() != 1 {
		t.Errorf("expected 1 request to broker B, got %d", countB.Load())
	}
	if countA.Load() != 0 {
		t.Errorf("expected 0 requests to broker A while rate-limited, got %d", countA.Load())
	}

	// Unblock A.
	chA <- time.Now()
	_, err = clientA.Execute(context.Background(), simpleOp(), map[string]any{})
	if err != nil {
		t.Fatalf("clientA Execute() error: %v", err)
	}
	if countA.Load() != 1 {
		t.Errorf("expected 1 request to broker A after unblock, got %d", countA.Load())
	}
}

func TestClient_RateLimiter_SkippedDuringRetries(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("unavailable"))
			return
		}
		jsonOK(w)
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 5)

	// Replace rate limiter with a single-buffered channel to verify it's read exactly once.
	countingCh := make(chan time.Time, 1)
	countingCh <- time.Now()
	client.rateLimiter = countingCh

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	// 3 HTTP requests (2 retries + 1 success), but rate limiter read only once.
	if requestCount.Load() != 3 {
		t.Errorf("expected 3 HTTP requests, got %d", requestCount.Load())
	}

	// The channel should be empty now (read once at the start of Execute).
	select {
	case <-countingCh:
		t.Error("rate limiter channel should have been read exactly once, but extra value found")
	default:
		// expected: channel empty after one read
	}
}

// --- Retry Tests: 401 ---

func TestClient_Retry_401_BasicAuth_ClearsCookiesAndRetries(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			// First request: set a session cookie, then return 401 (expired session).
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "stale"})
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"meta":{"error":{"status":"UNAUTHORIZED"}}}`))
			return
		}
		// Second request: cookie jar was cleared, so no stale cookie.
		if _, err := r.Cookie("session"); err == nil {
			t.Error("stale session cookie should have been cleared before retry")
		}
		jsonOK(w)
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 3)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 retry), got %d", requestCount.Load())
	}
}

func TestClient_Retry_401_BasicAuth_MaxOneRetry(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"meta":{"error":{"status":"UNAUTHORIZED"}}}`))
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for persistent 401")
	}

	// Should stop after 2 requests: original + 1 re-auth retry.
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 re-auth), got %d", requestCount.Load())
	}

	var sempErr *SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected SEMPError, got %T: %v", err, err)
	}
	if sempErr.StatusCode != 401 {
		t.Errorf("expected status 401, got %d", sempErr.StatusCode)
	}
}

func TestClient_Retry_401_Bearer_FailsImmediately(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"meta":{"error":{"status":"UNAUTHORIZED"}}}`))
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "bearer", 10)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for 401 with bearer token")
	}

	// Bearer token: no retry, just 1 request.
	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for bearer 401), got %d", requestCount.Load())
	}

	var sempErr *SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected SEMPError, got %T: %v", err, err)
	}
	if sempErr.StatusCode != 401 {
		t.Errorf("expected status 401, got %d", sempErr.StatusCode)
	}
}

// --- Retry Tests: 429/503 ---

func TestClient_Retry_429_RetriesWithBackoff(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("rate limited"))
			return
		}
		jsonOK(w)
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	result, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if requestCount.Load() != 4 {
		t.Errorf("expected 4 requests (3 retries + 1 success), got %d", requestCount.Load())
	}
}

func TestClient_Retry_503_RetriesWithBackoff(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("service unavailable"))
			return
		}
		jsonOK(w)
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	result, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if requestCount.Load() != 3 {
		t.Errorf("expected 3 requests (2 retries + 1 success), got %d", requestCount.Load())
	}
}

func TestClient_Retry_429_ExhaustsRetries(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer server.Close()

	maxRetries := 3
	client := newRetryTestClient(t, server, "basic", maxRetries)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error when retries exhausted")
	}

	// 1 original + 3 retries = 4 total.
	expectedRequests := int32(maxRetries + 1)
	if requestCount.Load() != expectedRequests {
		t.Errorf("expected %d requests, got %d", expectedRequests, requestCount.Load())
	}

	var sempErr *SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected SEMPError, got %T: %v", err, err)
	}
	if sempErr.StatusCode != 429 {
		t.Errorf("expected status 429, got %d", sempErr.StatusCode)
	}
}

// --- Retry Tests: Other 5xx ---

func TestClient_Retry_500_RetriesOnce(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for persistent 500")
	}

	// 500: retry once only → 2 total requests.
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 retry), got %d", requestCount.Load())
	}
}

func TestClient_Retry_502_RetriesOnce(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("bad gateway"))
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for persistent 502")
	}

	// 502: retry once only → 2 total requests.
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 retry), got %d", requestCount.Load())
	}
}

func TestClient_Retry_500_SucceedsOnRetry(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("transient error"))
			return
		}
		jsonOK(w)
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	result, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests, got %d", requestCount.Load())
	}
}

// --- Error Handling Tests ---

func TestClient_ErrorHandler_ProducesSEMPError(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("broker down"))
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 2)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error when retries exhausted")
	}

	var sempErr *SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected SEMPError from ErrorHandler, got %T: %v", err, err)
	}
	if sempErr.StatusCode != 503 {
		t.Errorf("expected status 503, got %d", sempErr.StatusCode)
	}
	if sempErr.Operation != "testOp" {
		t.Errorf("expected operation 'testOp', got %q", sempErr.Operation)
	}
}

// --- No-Retry Tests ---

func TestClient_NoRetry_404(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"meta":{"error":{"status":"NOT_FOUND"}}}`))
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for 404")
	}

	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for 404), got %d", requestCount.Load())
	}
}

func TestClient_NoRetry_400(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"meta":{"error":{"status":"BAD_REQUEST"}}}`))
	}))
	defer server.Close()

	client := newRetryTestClient(t, server, "basic", 10)

	_, err := client.Execute(context.Background(), simpleOp(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for 400")
	}

	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for 400), got %d", requestCount.Load())
	}
}
