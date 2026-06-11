package resilience

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/hashicorp/go-retryablehttp"
)

// RetriesExhaustedError is returned by Do() when all retry attempts fail and
// the ErrorHandler is invoked. Callers can wrap this in protocol-specific
// error types (SEMPError, sempv1.Error) as needed.
type RetriesExhaustedError struct {
	StatusCode int   // HTTP status code (0 when failure is a network error)
	Attempts   int   // total attempts made
	Err        error // underlying cause (nil for HTTP-status exhaustion)
}

func (e *RetriesExhaustedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("request failed after %d attempts: %v", e.Attempts, e.Err)
	}
	return fmt.Sprintf("request failed after %d attempts with status %d", e.Attempts, e.StatusCode)
}

// Unwrap returns the underlying error so errors.Is/As can traverse the chain.
func (e *RetriesExhaustedError) Unwrap() error { return e.Err }

// Sender wraps retryablehttp.Client with per-broker rate limiting, custom retry
// policy, and 401 re-auth. Both SEMPv1 and SEMPv2 clients compose this to get
// shared HTTP resilience without duplication.
//
// Sender is safe for concurrent use from multiple goroutines. The cookie jar
// is wrapped in a *SafeCookieJar so the 401 re-auth path can clear session
// state via an atomic swap without racing concurrent http.Client.Do reads
// of the Jar field.
//
// NOTE: if per-user broker sessions are introduced, jar replacement will
// need to be scoped per user.
type Sender struct {
	retryClient *retryablehttp.Client
	cookieJar   *SafeCookieJar    // for 401 re-auth: Clear() swaps in a fresh inner jar atomically
	authCfg     config.AuthConfig // for 401 auth-mode check
	rateLimiter <-chan time.Time
	rateTicker  *time.Ticker // non-nil when rate limiting enabled; stopped by Close()
	brokerURL   string       // for logging context
}

// New creates a Sender configured for a specific broker. It sets up retryablehttp
// with the retry policy from SEMPConfig and a per-broker rate limiter. The
// caller is expected to have set httpClient.Jar to the same *SafeCookieJar so
// 401 re-auth (Clear) and the cookie attachment in http.Client.Do operate on
// the same jar instance.
// sempCfg.Retries and sempCfg.RequestMinInterval must be non-nil.
func New(httpClient *http.Client, cookieJar *SafeCookieJar, sempCfg *config.SEMPConfig, authCfg config.AuthConfig, brokerURL string) *Sender {
	d := &Sender{
		cookieJar: cookieJar,
		authCfg:   authCfg,
		brokerURL: brokerURL,
	}

	// Configure retryablehttp client with the underlying http.Client.
	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = httpClient
	retryClient.RetryMax = *sempCfg.Retries
	retryClient.RetryWaitMin = sempCfg.RetryMinInterval
	retryClient.RetryWaitMax = sempCfg.RetryMaxInterval
	retryClient.Backoff = retryablehttp.RateLimitLinearJitterBackoff
	retryClient.CheckRetry = d.checkRetry
	retryClient.ErrorHandler = d.errorHandler
	retryClient.Logger = nil // manual logging in checkRetry
	d.retryClient = retryClient

	// Per-broker rate limiter: ticker-based interval enforcement.
	// When interval > 0, each Do() blocks until the ticker fires.
	// The very first request per broker pays one interval of latency (the ticker
	// doesn't fire immediately); this is a one-time cost at broker init and
	// avoids the complexity of a seeded channel with goroutine forwarding.
	// When interval == 0, the closed channel makes receives non-blocking (no rate limit).
	if *sempCfg.RequestMinInterval > 0 {
		d.rateTicker = time.NewTicker(*sempCfg.RequestMinInterval)
		d.rateLimiter = d.rateTicker.C
	} else {
		ch := make(chan time.Time)
		close(ch)
		d.rateLimiter = ch
	}

	return d
}

// Do sends an HTTP request through the rate limiter and retryablehttp client.
// The caller is responsible for building the request and adding authentication.
// On success, returns the HTTP response (body open for the caller to read).
// On failure after retries, returns a RetriesExhaustedError or a wrapped error.
func (d *Sender) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	// Rate limit: wait for the per-broker interval before sending a new request.
	// This does NOT apply to retries — retryablehttp handles those internally
	// with jittered backoff (RateLimitLinearJitterBackoff). During failure
	// episodes, total broker traffic can reach (1 + RetryMax) times the
	// configured rate, but retries are spread over time by the backoff and
	// jitter desynchronizes concurrent failures. The broker's Retry-After
	// header (if present on 429/503) takes priority over computed backoff.
	select {
	case <-d.rateLimiter:
		slog.Debug("rate limiter: request permitted",
			slog.String("broker", d.brokerURL))
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Attach per-request retry state for checkRetry, including the HTTP method
	// so the non-idempotent method guard can fire even on connection errors
	// (where resp is nil and resp.Request.Method is unavailable), and the
	// caller's retry-safe marker (WithRetrySafe).
	ctx = context.WithValue(ctx, retryStateKey{}, &retryState{
		method:    req.Method,
		retrySafe: isRetrySafe(ctx),
	})
	req = req.WithContext(ctx)

	retryReq, err := retryablehttp.FromRequest(req)
	if err != nil {
		return nil, fmt.Errorf("wrapping request: %w", err)
	}

	return d.retryClient.Do(retryReq)
}

// Close releases resources held by the Sender. Stops the rate limiter ticker
// if one is running. Safe to call multiple times.
func (d *Sender) Close() {
	if d.rateTicker != nil {
		d.rateTicker.Stop()
	}
}

// errorHandler is called by retryablehttp when retries are exhausted (RetryMax
// reached while CheckRetry kept returning true). At this point the response
// body has been drained/closed by retryablehttp's retry loop, so we construct
// a RetriesExhaustedError from the status code and attempt count.
func (d *Sender) errorHandler(resp *http.Response, err error, numTries int) (*http.Response, error) {
	if err != nil {
		return nil, &RetriesExhaustedError{Attempts: numTries, Err: err}
	}

	opID := "unknown"
	if resp != nil && resp.Request != nil {
		if id, ok := resp.Request.Context().Value(OperationIDKey{}).(string); ok {
			opID = id
		}
	}

	if resp != nil {
		slog.Error("request failed after retries exhausted",
			slog.String("broker", d.brokerURL),
			slog.String("operation", opID),
			slog.Int("status", resp.StatusCode),
			slog.Int("attempts", numTries))
		return nil, &RetriesExhaustedError{
			StatusCode: resp.StatusCode,
			Attempts:   numTries,
		}
	}

	return nil, fmt.Errorf("request failed after %d attempts", numTries)
}
