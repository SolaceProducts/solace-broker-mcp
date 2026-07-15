package resilience

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
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
// Sender is safe for concurrent use from multiple goroutines. The 401 re-auth
// path delegates to the broker's Authenticator, which owns the recovery
// strategy for its auth mode.
type Sender struct {
	retryClient   *retryablehttp.Client
	authenticator auth.Authenticator // for 401 re-auth: delegates recovery to the auth mode
	rateLimiter   <-chan time.Time
	rateTicker    *time.Ticker  // non-nil when rate limiting enabled; stopped by Close()
	sem           Semaphore     // bounds in-flight requests; shared per-broker across SEMPv1+v2
	brokerURL     string        // for logging context
	retryBudget   time.Duration // overall deadline for the whole retry chain; 0 disables
}

// New creates a Sender configured for a specific broker. It sets up retryablehttp
// with the retry policy from SEMPConfig and a per-broker rate limiter.
// sempCfg.Retries and sempCfg.RequestMinInterval must be non-nil.
//
// authn is the broker's Authenticator, used to delegate 401 recovery via
// HandleAuthFailure. It must be non-nil.
//
// sem bounds in-flight requests and must be non-nil. It must be shared by
// both protocol Senders of the same broker so the cap is per-broker, not per
// protocol client (see semp.NewBrokerClient). New panics on a nil sem: a
// Sender-private fallback would silently allow 2× the configured per-broker
// cap, which is exactly the bug SOL-150116 fixes. The panic is a constructor
// contract violation that any test exercising the path catches immediately;
// the only production wiring goes through semp.NewBrokerClient, which always
// supplies one.
func New(httpClient *http.Client, sempCfg *config.SEMPConfig, authn auth.Authenticator, brokerURL string, sem Semaphore) *Sender {
	if authn == nil {
		panic("resilience.New: authn must be non-nil; construct via semp.newAuthenticator in semp.NewBrokerClient")
	}
	if sem == nil {
		panic("resilience.New: sem must be non-nil; share one per broker via semp.NewBrokerClient")
	}
	d := &Sender{
		authenticator: authn,
		sem:           sem,
		brokerURL:     brokerURL,
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
	retryClient.PrepareRetry = d.prepareRetry
	retryClient.Logger = nil // manual logging in checkRetry
	d.retryClient = retryClient

	// Overall retry-chain deadline. httpClient.Timeout bounds a single attempt,
	// but retryablehttp issues up to RetryMax+1 attempts with backoff between
	// them, and the per-broker semaphore slot (see Do) is held for that entire
	// span. Without an overall deadline a slow or degraded broker keeps a slot
	// for the full chain, stalling every other call to that broker. Bound it at
	// the retry policy's own configured worst case — every attempt at its full
	// timeout plus every backoff at its cap — so the slot is released once that
	// budget is spent even if a per-attempt guard is missing or misbehaves. It
	// never cuts a chain whose backoffs stay within RetryMaxInterval.
	//
	// Exception: RateLimitLinearJitterBackoff (below) honors a broker's
	// Retry-After header verbatim, uncapped by RetryMaxInterval. A broker that
	// returns a long Retry-After can make this deadline fire mid-chain, cutting
	// the retry short. That is intentional, not a bug: without this deadline, a
	// degraded broker returning e.g. Retry-After: 300 would park the slot (and
	// every other call to that broker) for the full 300s per attempt regardless
	// of the configured budget. This deadline is what caps that exposure.
	//
	// The RetryMax+1 term counts the successful attempt too, so after up to
	// RetryMax failed attempts (each ≤ RequestTimeoutDuration) plus their
	// backoffs (each ≤ RetryMaxInterval) at least one full RequestTimeoutDuration
	// of budget always remains for the final attempt — including its response
	// body read, which the deadline also covers. That matches the per-attempt
	// http.Client.Timeout, so the read is never bounded more tightly than before.
	//
	// The budget is anchored on the per-attempt timeout, so it is only computed
	// when RequestTimeoutDuration > 0. Production always sets it (validate()
	// enforces it), so the deadline is always applied there. When it is unset
	// (a direct-construction test), retryBudget stays 0 and Do skips the
	// deadline — a backoff-only budget would allot no time to the attempts
	// themselves and fire spuriously under load.
	//
	// Caveat: an OAuth re-auth retry runs the token exchange inside
	// prepareRetry on a context detached from the request (see
	// tokenexchange.Exchange), bounded only by its own exchange timeout rather
	// than this budget. On such a retry the slot can therefore be held up to
	// that exchange timeout beyond retryBudget. Immaterial at default timeouts;
	// worth noting for very short tunings.
	if sempCfg.RequestTimeoutDuration > 0 {
		retryMax := *sempCfg.Retries
		d.retryBudget = time.Duration(retryMax+1)*sempCfg.RequestTimeoutDuration +
			time.Duration(retryMax)*sempCfg.RetryMaxInterval
	}

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

	// In-flight cap: acquire a per-broker semaphore slot for the duration of
	// the request, including retryablehttp's internal retries (the request is
	// still in flight against the broker while retrying). Acquired after the
	// rate-limiter tick so a slot is held only while genuinely in flight.
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-d.sem }()

	// Bound the whole retry chain (all attempts + backoffs), not just each
	// attempt, so a slow or degraded broker cannot pin resources for its full
	// chain. WithTimeout composes with any earlier caller deadline (it takes the
	// sooner of the two), and retryablehttp honors ctx cancellation during
	// backoff. cancel must not run while the caller is still reading the
	// response body, so on success it is transferred to Body.Close (below)
	// rather than deferred here — a premature cancel would abort the connection
	// instead of returning it to the idle pool and could truncate the body.
	var cancel context.CancelFunc
	if d.retryBudget > 0 {
		ctx, cancel = context.WithTimeout(ctx, d.retryBudget)
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
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("wrapping request: %w", err)
	}

	resp, err := d.retryClient.Do(retryReq)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}

	// Success: the body is returned open for the caller to read. Release the
	// deadline's timer when the caller closes the body, so the deadline still
	// bounds the read but the connection is pooled normally.
	if cancel != nil {
		resp.Body = &cancelOnCloseReadCloser{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

// cancelOnCloseReadCloser wraps a response body so closing it also cancels the
// retry-chain deadline's context. This defers the cancel from Do's return (when
// the body is still open for the caller to read) to Body.Close, so the deadline
// covers the read without a premature cancel aborting the pooled connection.
type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseReadCloser) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// prepareRetry re-runs AddAuth when the authenticator signalled ReAuth on the
// preceding 401 (carried as needsReauth on the retry state). Skipped for
// 429/503/5xx retries and for auth modes whose recovery does not need fresh
// credentials (e.g. Basic, where AddAuth would only re-set a static header).
func (d *Sender) prepareRetry(req *http.Request) error {
	state := getRetryState(req.Context())
	if state.needsReauth {
		state.needsReauth = false
		return d.authenticator.AddAuth(req.Context(), req)
	}
	return nil
}

// Close releases resources held by the Sender. Stops the rate limiter ticker
// if one is running. Safe to call multiple times.
func (d *Sender) Close() {
	if d.rateTicker != nil {
		d.rateTicker.Stop()
	}
}

// errorHandlerDrainLimit bounds how much of the final response body the
// errorHandler reads before closing it. Matches retryablehttp's own
// respReadLimit for the between-attempts drain. A fully drained body lets
// the transport return the connection to the idle pool; past the limit,
// closing (and tearing down the connection) is cheaper than the read.
const errorHandlerDrainLimit = 4096

// errorHandler is called by retryablehttp when retries are exhausted (RetryMax
// reached while CheckRetry kept returning true). retryablehttp drains the
// response body only between attempts — on the final attempt it returns
// through the custom ErrorHandler without touching the body, and the handler
// owns closing it (go-retryablehttp client.go ErrorHandler contract). Drain
// and close here, then construct a RetriesExhaustedError from the status code
// and attempt count.
func (d *Sender) errorHandler(resp *http.Response, err error, numTries int) (*http.Response, error) {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorHandlerDrainLimit))
		_ = resp.Body.Close()
	}

	// Extract the operation ID up front so every exit path can name it. It
	// lives on the request context, reachable via resp.Request on any path
	// where a response was seen (including the err path below, where resp
	// holds the last attempt's response).
	opID := "unknown"
	if resp != nil && resp.Request != nil {
		if id, ok := resp.Request.Context().Value(OperationIDKey{}).(string); ok {
			opID = id
		}
	}

	// err != nil covers connection errors (resp == nil) and a PrepareRetry
	// failure — e.g. the re-auth token exchange failing because the IdP is
	// down, where resp still holds the last 401. Without logging here that
	// case would go silent, and reporting StatusCode 0 would hide the 401 a
	// caller keys on; carry the last observed status through when we have it.
	if err != nil {
		if resp != nil {
			slog.Error("request failed after retries exhausted",
				slog.String("broker", d.brokerURL),
				slog.String("operation", opID),
				slog.Int("status", resp.StatusCode),
				slog.Int("attempts", numTries),
				slog.String("error", err.Error()))
			return nil, &RetriesExhaustedError{
				StatusCode: resp.StatusCode,
				Attempts:   numTries,
				Err:        err,
			}
		}
		return nil, &RetriesExhaustedError{Attempts: numTries, Err: err}
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
