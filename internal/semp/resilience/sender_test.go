package resilience

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
)

// mustNewSafeCookieJar creates an empty SafeCookieJar for tests; fails the
// test on the (unreachable in practice) error path.
func mustNewSafeCookieJar(t *testing.T) *SafeCookieJar {
	t.Helper()
	jar, err := NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	return jar
}

// basicAuth creates a BasicAuthenticator wired to the given jar so 401
// cookie-clearing works end-to-end in tests.
func basicAuth(t *testing.T, jar *SafeCookieJar) auth.Authenticator {
	t.Helper()
	return auth.NewBasicAuthenticator("admin", "secret", jar)
}

// bearerAuth creates a BearerAuthenticator (static token, no jar needed).
func bearerAuth(t *testing.T) auth.Authenticator {
	t.Helper()
	return auth.NewBearerAuthenticator("static-token")
}

// newTestSender creates a Sender configured for testing with the given Authenticator and retry count.
// The jar is set on httpClient.Jar so cookies flow through http.Client.Do.
func newTestSender(t *testing.T, httpClient *http.Client, authn auth.Authenticator, retries int) *Sender {
	t.Helper()
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	jar := mustNewSafeCookieJar(t)
	httpClient.Jar = jar
	return New(httpClient, sempCfg, authn, "http://test-broker", NewSemaphore(10))
}

// newTestSenderBasic creates a Sender with a BasicAuthenticator sharing the
// same SafeCookieJar as the HTTP client, so 401 jar-clearing works end-to-end.
func newTestSenderBasic(t *testing.T, httpClient *http.Client, retries int) *Sender {
	t.Helper()
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	jar := mustNewSafeCookieJar(t)
	httpClient.Jar = jar
	return New(httpClient, sempCfg, basicAuth(t, jar), "http://test-broker", NewSemaphore(10))
}

// newTestSenderWithServer creates a test server and a Sender pointed at it.
// authMode "basic" shares the jar between Authenticator and HTTP client;
// "bearer" uses a static-token Authenticator.
func newTestSenderWithServer(t *testing.T, handler http.HandlerFunc, authMode string, retries int) (*Sender, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	httpClient := server.Client()
	var d *Sender
	if authMode == "bearer" {
		d = newTestSender(t, httpClient, bearerAuth(t), retries)
	} else {
		d = newTestSenderBasic(t, httpClient, retries)
	}
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

func newMethodRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url+"/test", nil)
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

	senderA := newTestSender(t, serverA.Client(), auth.NewBasicAuthenticator("admin", "secret", nil), 0)
	senderA.brokerURL = serverA.URL
	senderB := newTestSender(t, serverB.Client(), auth.NewBasicAuthenticator("admin", "secret", nil), 0)
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
	jar := mustNewSafeCookieJar(t)
	sender := New(&http.Client{Jar: jar}, sempCfg, basicAuth(t, jar), serverURL, NewSemaphore(10))

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

func TestSender_NoRetry_POST_On503(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable"))
	}, "basic", 10)
	defer server.Close()

	req := newMethodRequest(t, http.MethodPost, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}

	// POST must never be retried — exactly 1 request regardless of status.
	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for POST), got %d", requestCount.Load())
	}
}

func TestSender_NoRetry_PATCH_On503(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable"))
	}, "basic", 10)
	defer server.Close()

	req := newMethodRequest(t, http.MethodPatch, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}

	// PATCH must never be retried — exactly 1 request regardless of status.
	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for PATCH), got %d", requestCount.Load())
	}
}

func TestSender_NoRetry_POST_On500(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}, "basic", 10)
	defer server.Close()

	req := newMethodRequest(t, http.MethodPost, server.URL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected non-error response, got error: %v", err)
	}
	if requestCount.Load() != 1 {
		t.Errorf("expected 1 request (no retry for POST on 500), got %d", requestCount.Load())
	}
}

// TestSender_NoRetry_POST_ConnectionError exercises the reason the HTTP method
// is captured on retryState: when the server is unreachable, checkRetry receives
// (nil resp, non-nil err). Without the captured method, the guard couldn't tell
// it was a POST and would fall through to retryablehttp's default policy, which
// retries connection errors. We assert exactly one attempt and a wrapped
// RetriesExhaustedError with Attempts=1.
func TestSender_NoRetry_POST_ConnectionError(t *testing.T) {
	// Bring a server up to obtain a URL, then close it so all dials are refused.
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
	}))
	serverURL := server.URL
	server.Close()

	retries := 5
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	jar := mustNewSafeCookieJar(t)
	sender := New(&http.Client{Jar: jar}, sempCfg, basicAuth(t, jar), serverURL, NewSemaphore(10))

	req := newMethodRequest(t, http.MethodPost, serverURL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected connection error for closed server")
	}

	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected RetriesExhaustedError, got %T: %v", err, err)
	}
	if exhausted.Attempts != 1 {
		t.Errorf("expected exactly 1 attempt for POST connection error (no retry), got %d", exhausted.Attempts)
	}
	if requestCount.Load() != 0 {
		t.Errorf("expected 0 successful requests (server closed), got %d", requestCount.Load())
	}
}

// TestSender_NoRetry_PATCH_ConnectionError mirrors the POST connection-error
// case for PATCH.
func TestSender_NoRetry_PATCH_ConnectionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	retries := 5
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	jar := mustNewSafeCookieJar(t)
	sender := New(&http.Client{Jar: jar}, sempCfg, basicAuth(t, jar), serverURL, NewSemaphore(10))

	req := newMethodRequest(t, http.MethodPatch, serverURL)
	resp, err := sender.Do(context.Background(), req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected connection error for closed server")
	}

	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected RetriesExhaustedError, got %T: %v", err, err)
	}
	if exhausted.Attempts != 1 {
		t.Errorf("expected exactly 1 attempt for PATCH connection error (no retry), got %d", exhausted.Attempts)
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

// newTestSenderWithSem creates a Sender pointed at serverURL that shares sem.
// Rate limiting is disabled and retries are zero so tests exercise only the
// in-flight semaphore.
func newTestSenderWithSem(t *testing.T, httpClient *http.Client, serverURL string, sem Semaphore) *Sender {
	t.Helper()
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	jar := mustNewSafeCookieJar(t)
	httpClient.Jar = jar
	return New(httpClient, sempCfg, basicAuth(t, jar), serverURL, sem)
}

// TestSenderDo_SharedSemaphoreEnforcesPerBrokerCap proves that two Senders
// sharing one Semaphore — as the SEMPv1 and SEMPv2 clients of a single broker
// do — never have more than the cap in flight against the broker combined.
// Requests beyond the cap must queue until a slot frees.
//
// The handler blocks every request until released, so without enforcement all
// twelve requests reach the server immediately and the cap is exceeded.
func TestSenderDo_SharedSemaphoreEnforcesPerBrokerCap(t *testing.T) {
	const maxInFlight = 3
	const totalRequests = 12

	arrived := make(chan struct{}, totalRequests)
	release := make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		jsonOK(w)
	}))
	defer server.Close()
	// On a failure path, unblock the handlers before the deferred
	// server.Close() — it waits for in-flight requests and would deadlock.
	defer releaseAll()

	sem := NewSemaphore(maxInFlight)
	senderA := newTestSenderWithSem(t, server.Client(), server.URL, sem)
	senderB := newTestSenderWithSem(t, server.Client(), server.URL, sem)
	defer senderA.Close()
	defer senderB.Close()

	var wg sync.WaitGroup
	errs := make(chan error, totalRequests)
	for i := range totalRequests {
		sender := senderA
		if i%2 == 1 {
			sender = senderB
		}
		req := newGetRequest(t, server.URL)
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := sender.Do(context.Background(), req)
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
		}()
	}

	// Exactly maxInFlight requests reach the server while all slots are held...
	for range maxInFlight {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("expected %d requests to reach the server", maxInFlight)
		}
	}
	// ...and no further request may arrive until a slot frees.
	select {
	case <-arrived:
		t.Fatalf("in-flight requests exceeded the cap of %d", maxInFlight)
	case <-time.After(300 * time.Millisecond):
	}

	releaseAll()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Do: %v", err)
	}
}

// TestSenderDo_SemaphoreWaitRespectsContextCancel proves a request waiting for
// an in-flight slot honors context cancellation instead of blocking until a
// slot frees.
func TestSenderDo_SemaphoreWaitRespectsContextCancel(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		jsonOK(w)
	}))
	defer server.Close()
	// On a failure path, unblock the handlers before the deferred
	// server.Close() — it waits for in-flight requests and would deadlock.
	defer releaseAll()

	sem := NewSemaphore(1)
	sender := newTestSenderWithSem(t, server.Client(), server.URL, sem)
	defer sender.Close()

	// First request occupies the only slot and blocks in the handler.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		resp, err := sender.Do(context.Background(), newGetRequest(t, server.URL))
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach the server")
	}

	// Second request cannot get a slot; cancel it while it waits.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		resp, doErr := sender.Do(ctx, req)
		if doErr == nil {
			resp.Body.Close()
		}
		errCh <- doErr
	}()
	time.Sleep(50 * time.Millisecond) // let the second request reach the semaphore wait
	cancel()

	select {
	case doErr := <-errCh:
		if !errors.Is(doErr, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", doErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request did not return")
	}

	releaseAll()
	<-firstDone
}

// TestNew_PanicsOnNilSemaphore pins the constructor contract: a nil sem would
// silently recreate the per-Sender 2× cap that SOL-150116 removed, so New
// refuses it outright.
func TestNew_PanicsOnNilSemaphore(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New() with nil sem did not panic")
		}
	}()
	retries := 1
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
	}
	jar := mustNewSafeCookieJar(t)
	New(&http.Client{Jar: jar}, sempCfg, basicAuth(t, jar), "http://test-broker", nil)
}
