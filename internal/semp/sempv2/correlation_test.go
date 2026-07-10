package sempv2_test

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
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// okJSON is a minimal successful SEMPv2 response body.
const okJSON = `{"data":{},"meta":{}}`

// newTestClientRetries builds a client whose Sender allows up to maxRetries
// retries, so the retry-reuse test can drive multiple attempts.
func newTestClientRetries(t *testing.T, maxRetries int, handler http.HandlerFunc) (*sempv2.HTTPClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic"},
	}
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &maxRetries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       5 * time.Millisecond,
	}
	jar, jarErr := resilience.NewSafeCookieJar()
	if jarErr != nil {
		t.Fatalf("NewSafeCookieJar: %v", jarErr)
	}
	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg, resilience.NewSemaphore(10), auth.NewBasicAuthenticator("admin", "secret", "test-broker", jar), jar)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return client, server
}

// TestExecute_CorrelationHeaders_TraceID verifies that a 32-hex trace-id on the
// context produces both X-Correlation-ID (verbatim) and a well-formed
// traceparent, alongside the preserved auth header.
func TestExecute_CorrelationHeaders_TraceID(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	// captured is written by the httptest server goroutine and read by the test
	// goroutine. A buffered channel hands the headers across with a clean
	// happens-before edge (send before recv), so the read needs no extra lock.
	captured := make(chan http.Header, 1)
	client, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okJSON))
	})
	defer srv.Close()
	defer client.Close()

	ctx := correlation.With(context.Background(), traceID)
	op := testOp(http.MethodGet)
	args := map[string]any{"msgVpnName": "default", "queueName": "q1"}
	if _, err := client.Execute(ctx, op, args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	hdr := <-captured
	if got := hdr.Get("X-Correlation-ID"); got != traceID {
		t.Errorf("X-Correlation-ID = %q, want %q", got, traceID)
	}
	tp := hdr.Get("traceparent")
	if !strings.HasPrefix(tp, "00-"+traceID+"-") || !strings.HasSuffix(tp, "-01") {
		t.Errorf("traceparent = %q, want 00-%s-<span>-01", tp, traceID)
	}
	if hdr.Get("Authorization") == "" {
		t.Error("Authorization header missing; correlation headers must not displace auth")
	}
}

// TestExecute_CorrelationHeaders_Empty verifies that with no correlation ID,
// neither correlation header is attached.
func TestExecute_CorrelationHeaders_Empty(t *testing.T) {
	// Buffered channel hands the captured headers from the server goroutine to
	// the test goroutine with a happens-before edge, so the read is race-free.
	captured := make(chan http.Header, 1)
	client, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okJSON))
	})
	defer srv.Close()
	defer client.Close()

	op := testOp(http.MethodGet)
	args := map[string]any{"msgVpnName": "default", "queueName": "q1"}
	if _, err := client.Execute(context.Background(), op, args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	hdr := <-captured
	if got := hdr.Get("X-Correlation-ID"); got != "" {
		t.Errorf("X-Correlation-ID = %q, want absent", got)
	}
	if got := hdr.Get("traceparent"); got != "" {
		t.Errorf("traceparent = %q, want absent", got)
	}
}

// TestExecute_CorrelationHeaders_NonTraceID verifies a non-trace-id sets only
// X-Correlation-ID and never a traceparent.
func TestExecute_CorrelationHeaders_NonTraceID(t *testing.T) {
	const id = "order-12345-abc"
	// Buffered channel hands the captured headers from the server goroutine to
	// the test goroutine with a happens-before edge, so the read is race-free.
	captured := make(chan http.Header, 1)
	client, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okJSON))
	})
	defer srv.Close()
	defer client.Close()

	ctx := correlation.With(context.Background(), id)
	op := testOp(http.MethodGet)
	args := map[string]any{"msgVpnName": "default", "queueName": "q1"}
	if _, err := client.Execute(ctx, op, args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	hdr := <-captured
	if got := hdr.Get("X-Correlation-ID"); got != id {
		t.Errorf("X-Correlation-ID = %q, want %q", got, id)
	}
	if got := hdr.Get("traceparent"); got != "" {
		t.Errorf("traceparent = %q, want absent for non-trace-id", got)
	}
}

// TestExecute_CorrelationHeaders_PresentOnEveryRetryAttempt verifies the
// forwarded X-Correlation-ID is present and identical on every attempt the
// broker sees. The server 503s the first attempt (a retryable status for the
// idempotent GET) then succeeds, so the handler observes more than one attempt.
// Cross-retry survival is provided by retryablehttp replaying the same
// *http.Request object, not by per-attempt application logic; the basic
// single-attempt forwarding contract is covered by the other
// TestExecute_CorrelationHeaders_* tests.
func TestExecute_CorrelationHeaders_PresentOnEveryRetryAttempt(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	var mu sync.Mutex
	var seen []http.Header
	attempt := 0
	client, srv := newTestClientRetries(t, 3, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		attempt++
		n := attempt
		mu.Unlock()
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okJSON))
	})
	defer srv.Close()
	defer client.Close()

	ctx := correlation.With(context.Background(), traceID)
	op := testOp(http.MethodGet)
	args := map[string]any{"msgVpnName": "default", "queueName": "q1"}
	if _, err := client.Execute(ctx, op, args); err != nil {
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
