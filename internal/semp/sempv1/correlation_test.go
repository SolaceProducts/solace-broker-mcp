package sempv1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
)

// newTestClientWithRetries builds a client whose Sender allows up to maxRetries
// retries, so the retry-reuse test can drive multiple attempts.
func newTestClientWithRetries(t *testing.T, srv *httptest.Server, maxRetries int) *HTTPClient {
	t.Helper()
	brokerCfg := &config.BrokerConfig{
		URL:  srv.URL,
		Auth: config.AuthConfig{Mode: config.AuthModeBasic},
	}
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 2 * time.Second,
		Retries:                &maxRetries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       5 * time.Millisecond,
	}
	client, err := NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), auth.NewBasicAuthenticator("user", "pass"))
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return client
}

// captureHeaders returns a handler that records the headers of every inbound
// request (one entry per attempt) and replies with a successful SEMPv1
// envelope. The returned slice is guarded by mu for the retry test.
func captureSEMPv1(t *testing.T) (http.HandlerFunc, *[]http.Header, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var seen []http.Header
	h := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(successEnvelope))
	}
	return h, &seen, &mu
}

// TestExecute_CorrelationHeaders_TraceID verifies that a 32-hex trace-id on the
// context produces both X-Correlation-ID (verbatim) and a well-formed
// traceparent, and that auth headers survive alongside them.
func TestExecute_CorrelationHeaders_TraceID(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	h, seen, _ := captureSEMPv1(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := newTestClient(t, srv)
	defer client.Close()

	ctx := correlation.With(context.Background(), traceID)
	if _, err := client.Execute(ctx, "<rpc><show><version/></show></rpc>"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("got %d requests, want 1", len(*seen))
	}
	hdr := (*seen)[0]
	if got := hdr.Get("X-Correlation-ID"); got != traceID {
		t.Errorf("X-Correlation-ID = %q, want %q", got, traceID)
	}
	tp := hdr.Get("traceparent")
	if !strings.HasPrefix(tp, "00-"+traceID+"-") || !strings.HasSuffix(tp, "-01") {
		t.Errorf("traceparent = %q, want 00-%s-<span>-01", tp, traceID)
	}
	// Auth preserved: BasicAuthenticator sets Authorization.
	if hdr.Get("Authorization") == "" {
		t.Error("Authorization header missing; correlation headers must not displace auth")
	}
}

// TestExecute_CorrelationHeaders_Empty verifies that with no correlation ID on
// the context, neither correlation header is attached.
func TestExecute_CorrelationHeaders_Empty(t *testing.T) {
	h, seen, _ := captureSEMPv1(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := newTestClient(t, srv)
	defer client.Close()

	if _, err := client.Execute(context.Background(), "<rpc><show><version/></show></rpc>"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	hdr := (*seen)[0]
	if got := hdr.Get("X-Correlation-ID"); got != "" {
		t.Errorf("X-Correlation-ID = %q, want absent", got)
	}
	if got := hdr.Get("traceparent"); got != "" {
		t.Errorf("traceparent = %q, want absent", got)
	}
}

// TestExecute_CorrelationHeaders_NonTraceID verifies a non-trace-id (UUIDv7)
// sets only X-Correlation-ID, never a traceparent.
func TestExecute_CorrelationHeaders_NonTraceID(t *testing.T) {
	const id = "0190f3a1-7c2e-7b3d-9f1a-2c4e6a8b0d1f"
	h, seen, _ := captureSEMPv1(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := newTestClient(t, srv)
	defer client.Close()

	ctx := correlation.With(context.Background(), id)
	if _, err := client.Execute(ctx, "<rpc><show><version/></show></rpc>"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	hdr := (*seen)[0]
	if got := hdr.Get("X-Correlation-ID"); got != id {
		t.Errorf("X-Correlation-ID = %q, want %q", got, id)
	}
	if got := hdr.Get("traceparent"); got != "" {
		t.Errorf("traceparent = %q, want absent for non-trace-id", got)
	}
}

// TestExecute_CorrelationHeaders_RetryReuse verifies that every retry attempt
// carries the SAME X-Correlation-ID. The server fails the first attempts with
// 503 (retryable for a read-only <show>) then succeeds.
func TestExecute_CorrelationHeaders_RetryReuse(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	var mu sync.Mutex
	var seen []http.Header
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		attempt++
		n := attempt
		mu.Unlock()
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(successEnvelope))
	}))
	defer srv.Close()

	client := newTestClientWithRetries(t, srv, 3)
	defer client.Close()

	ctx := correlation.With(context.Background(), traceID)
	// Use a read-only <show> so the Sender marks the request retry-safe.
	if _, err := client.Execute(ctx, "<rpc><show><version/></show></rpc>"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("got %d attempts, want >=2 (a retry must have occurred)", len(seen))
	}
	for i, hdr := range seen {
		if got := hdr.Get("X-Correlation-ID"); got != traceID {
			t.Errorf("attempt %d X-Correlation-ID = %q, want %q", i, got, traceID)
		}
	}
}
