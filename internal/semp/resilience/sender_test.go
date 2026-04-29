package resilience

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

// newTestSender creates a Sender configured for testing with the given auth mode and retry count.
func newTestSender(t *testing.T, httpClient *http.Client, authMode string, retries int) *Sender {
	t.Helper()
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	authCfg := config.AuthConfig{
		Mode:     authMode,
		Username: "admin",
		Password: "secret",
		Token:    "static-token",
	}
	return New(httpClient, sempCfg, authCfg, "http://test-broker")
}

// newTestSenderWithServer creates a test server and a Sender pointed at it.
func newTestSenderWithServer(t *testing.T, handler http.HandlerFunc, authMode string, retries int) (*Sender, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	httpClient := server.Client()
	d := newTestSender(t, httpClient, authMode, retries)
	d.brokerURL = server.URL
	return d, server
}

func newGetRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "GET", url+"/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func jsonOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
}

// --- Rate Limiter Tests ---

func TestSender_RateLimiter_BlocksUntilChannelReady(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		jsonOK(w)
	}, "basic", 0)
	defer server.Close()

	// Replace rate limiter with a manually controlled channel.
	ch := make(chan time.Time, 1)
	sender.rateLimiter = ch

	// Start Do in a goroutine — it should block on the rate limiter.
	done := make(chan error, 1)
	go func() {
		req := newGetRequest(t, server.URL)
		resp, err := sender.Do(context.Background(), req)
		if err == nil {
			resp.Body.Close()
		}
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
			t.Fatalf("Do() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do() did not complete after rate limiter unblocked")
	}

	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request, got %d", requestCount.Load())
	}
}

func TestSender_RateLimiter_DisabledWhenZero(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		jsonOK(w)
	}, "basic", 0)
	defer server.Close()

	for i := range 5 {
		req := newGetRequest(t, server.URL)
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() #%d error: %v", i, err)
		}
		resp.Body.Close()
	}

	if requestCount.Load() != 5 {
		t.Errorf("expected 5 requests, got %d", requestCount.Load())
	}
}

func TestSender_RateLimiter_PerBrokerIndependence(t *testing.T) {
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

	senderA := newTestSender(t, serverA.Client(), "basic", 0)
	senderA.brokerURL = serverA.URL
	senderB := newTestSender(t, serverB.Client(), "basic", 0)
	senderB.brokerURL = serverB.URL

	// Block sender A's rate limiter.
	chA := make(chan time.Time, 1)
	senderA.rateLimiter = chA

	// Sender B should work independently.
	req := newGetRequest(t, serverB.URL)
	resp, err := senderB.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("senderB Do() error: %v", err)
	}
	resp.Body.Close()

	if countB.Load() != 1 {
		t.Errorf("expected 1 request to broker B, got %d", countB.Load())
	}
	if countA.Load() != 0 {
		t.Errorf("expected 0 requests to broker A while rate-limited, got %d", countA.Load())
	}

	// Unblock A.
	chA <- time.Now()
	reqA := newGetRequest(t, serverA.URL)
	respA, err := senderA.Do(context.Background(), reqA)
	if err != nil {
		t.Fatalf("senderA Do() error: %v", err)
	}
	respA.Body.Close()

	if countA.Load() != 1 {
		t.Errorf("expected 1 request to broker A after unblock, got %d", countA.Load())
	}
}

func TestSender_RateLimiter_SkippedDuringRetries(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("unavailable"))
			return
		}
		jsonOK(w)
	}, "basic", 5)
	defer server.Close()

	// Replace rate limiter with a single-buffered channel (read exactly once).
	countingCh := make(chan time.Time, 1)
	countingCh <- time.Now()
	sender.rateLimiter = countingCh

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	// 3 HTTP requests (2 retries + 1 success), but rate limiter read only once.
	if requestCount.Load() != 3 {
		t.Errorf("expected 3 HTTP requests, got %d", requestCount.Load())
	}

	// The channel should be empty now (read once at the start of Do).
	select {
	case <-countingCh:
		t.Error("rate limiter channel should have been read exactly once, but extra value found")
	default:
		// expected: channel empty after one read
	}
}

// --- Retry Tests: 401 ---

func TestSender_Retry_401_BasicAuth_ClearsCookiesAndRetries(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "stale"})
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`unauthorized`))
			return
		}
		// Second request: cookie jar was cleared, so no stale cookie.
		if _, err := r.Cookie("session"); err == nil {
			t.Error("stale session cookie should have been cleared before retry")
		}
		jsonOK(w)
	}, "basic", 3)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 retry), got %d", requestCount.Load())
	}
}

func TestSender_Retry_401_BasicAuth_MaxOneRetry(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`unauthorized`))
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}

	if err != nil {
		t.Fatalf("expected non-error response (401 body), got error: %v", err)
	}

	// Should stop after 2 requests: original + 1 re-auth retry.
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 re-auth), got %d", requestCount.Load())
	}
	// When checkRetry returns false, Do() returns the response (not error).
	// The caller (sempv2/sempv1) is responsible for checking status codes.
	if resp.StatusCode != 401 {
		t.Errorf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestSender_Retry_401_Bearer_FailsImmediately(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`unauthorized`))
	}, "bearer", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}

	// Bearer token: no retry, just 1 request.
	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for bearer 401), got %d", requestCount.Load())
	}
	if resp.StatusCode != 401 {
		t.Errorf("expected status 401, got %d", resp.StatusCode)
	}
}

// --- Retry Tests: 429/503 ---

func TestSender_Retry_429_RetriesWithBackoff(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("rate limited"))
			return
		}
		jsonOK(w)
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if requestCount.Load() != 4 {
		t.Errorf("expected 4 requests (3 retries + 1 success), got %d", requestCount.Load())
	}
}

func TestSender_Retry_503_RetriesWithBackoff(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("service unavailable"))
			return
		}
		jsonOK(w)
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if requestCount.Load() != 3 {
		t.Errorf("expected 3 requests (2 retries + 1 success), got %d", requestCount.Load())
	}
}

func TestSender_Retry_429_ExhaustsRetries(t *testing.T) {
	var requestCount atomic.Int32
	maxRetries := 3
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}, "basic", maxRetries)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error when retries exhausted")
	}

	// 1 original + 3 retries = 4 total.
	expectedRequests := int32(maxRetries + 1)
	if requestCount.Load() != expectedRequests {
		t.Errorf("expected %d requests, got %d", expectedRequests, requestCount.Load())
	}

	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected RetriesExhaustedError, got %T: %v", err, err)
	}
	if exhausted.StatusCode != 429 {
		t.Errorf("expected status 429, got %d", exhausted.StatusCode)
	}
}

// --- Retry Tests: Other 5xx ---

func TestSender_Retry_500_RetriesOnce(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}

	// 500: retry once only → 2 total requests.
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 retry), got %d", requestCount.Load())
	}
}

func TestSender_Retry_502_RetriesOnce(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("bad gateway"))
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}

	// 502: retry once only → 2 total requests.
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 retry), got %d", requestCount.Load())
	}
}

func TestSender_Retry_500_SucceedsOnRetry(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("transient error"))
			return
		}
		jsonOK(w)
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests, got %d", requestCount.Load())
	}
}

// --- Error Handling Tests ---

func TestSender_ErrorHandler_ProducesRetriesExhaustedError(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("broker down"))
	}, "basic", 2)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error when retries exhausted")
	}

	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected RetriesExhaustedError, got %T: %v", err, err)
	}
	if exhausted.StatusCode != 503 {
		t.Errorf("expected status 503, got %d", exhausted.StatusCode)
	}
}

func TestSender_ErrorHandler_NetworkError_ProducesRetriesExhaustedError(t *testing.T) {
	// Point the sender at a server that is immediately closed (connection refused).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close() // close immediately so all connections are refused

	retries := 2
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	authCfg := config.AuthConfig{Mode: "basic", Username: "admin", Password: "secret"}
	sender := New(&http.Client{}, sempCfg, authCfg, serverURL)

	req := newGetRequest(t, serverURL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error for connection refused")
	}

	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected RetriesExhaustedError, got %T: %v", err, err)
	}
	if exhausted.Attempts == 0 {
		t.Error("expected Attempts > 0")
	}
	if exhausted.Err == nil {
		t.Error("expected underlying Err to be set for network errors")
	}
	if exhausted.StatusCode != 0 {
		t.Errorf("expected StatusCode 0 for network error, got %d", exhausted.StatusCode)
	}
}

// --- No-Retry Tests ---

func TestSender_NoRetry_404(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`not found`))
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}

	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for 404), got %d", requestCount.Load())
	}
}

func TestSender_NoRetry_400(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`bad request`))
	}, "basic", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}

	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for 400), got %d", requestCount.Load())
	}
}
