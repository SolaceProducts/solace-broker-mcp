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

package resilience

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/hashicorp/go-retryablehttp"
)

// RetriesExhaustedError is returned by Do() when all retry attempts fail and
// the ErrorHandler is invoked. Callers can wrap this in protocol-specific
// error types (SEMPError, sempv1.Error) as needed.
type RetriesExhaustedError struct {
	StatusCode int   // HTTP status code (0 when failure is a network error)
	Attempts   int   // total attempts made
	Err        error // underlying cause (nil for HTTP-status exhaustion)

	// NonIdempotent reports that the caller declared the request
	// non-idempotent (WithRetryUnsafe), so the retry policy deliberately did
	// not replay it. Callers must not present this failure as "try again":
	// the broker may already have carried out the request, which is the whole
	// reason no retry was attempted. Upper layers key their agent-facing
	// retryable flag off this.
	NonIdempotent bool

	// Body is the final response body, truncated at errorHandlerDrainLimit and
	// empty when the failure left no response. errorHandler has to drain the
	// body to release the connection, which would otherwise destroy the only
	// explanation the broker gave. That matters most on the NonIdempotent path:
	// a 503 carries a reason (e.g. "Replication Is Standby") that often shows
	// the operation was rejected before execution, which is exactly what tells
	// an operator whether the purge they are worried about actually ran.
	//
	// Protocol-agnostic on purpose — this type is shared by SEMPv1 and SEMPv2,
	// so the bytes are kept raw and each client parses them in its own terms.
	Body []byte

	// Detail is the broker's human-readable reason, parsed out of Body by the
	// protocol client that understands the framing. Empty when the body carried
	// none. Already broker-sourced text, so callers must sanitize before showing
	// it to an agent.
	Detail string
}

func (e *RetriesExhaustedError) Error() string {
	if e.NonIdempotent {
		return fmt.Sprintf("request failed after %d attempt(s) and was not retried "+
			"because the caller declared it non-idempotent: %v", e.Attempts, e.Err)
	}
	if e.Err != nil {
		return fmt.Sprintf("request failed after %d attempts: %v", e.Attempts, e.Err)
	}
	return fmt.Sprintf("request failed after %d attempts with status %d", e.Attempts, e.StatusCode)
}

// Unwrap returns the underlying error so errors.Is/As can traverse the chain.
func (e *RetriesExhaustedError) Unwrap() error { return e.Err }

// Admission stages, reported on BrokerBusyError.Stage. Do gates every request
// on two queues in sequence, and which one ran out of budget is the first thing
// an operator needs: the rate-limiter gate points at semp.request_min_interval
// (arrival rate exceeds the configured pace), the concurrency gate points at
// semp.max_concurrent_per_broker or at a broker slow enough that in-flight
// requests are holding their slots.
const (
	AdmissionStageRateLimit   = "rate_limit"
	AdmissionStageConcurrency = "concurrency"
)

// BrokerBusyError is returned by Do when a request could not be admitted within
// semp.max_queue_wait. The request was never sent — nothing reached the broker
// — so replaying it is always safe, including for a write the caller declared
// non-idempotent. That is the difference from RetriesExhaustedError, where the
// broker may already have acted.
//
// Before SOL-153442 this condition had no error at all: a caller that set no
// context deadline simply waited, and the MCP server sets no deadline of its
// own (cmd/server/main.go leaves http.Server.WriteTimeout at zero to keep
// streamable connections alive). Sustained saturation was therefore an
// indefinite hang with no signal.
type BrokerBusyError struct {
	// Stage is AdmissionStageRateLimit or AdmissionStageConcurrency — which of
	// Do's two admission gates the budget expired at.
	Stage string

	// Waited is the time spent trying to be admitted, measured from entry to
	// Do across both gates. Slightly exceeds MaxWait by scheduling jitter.
	Waited time.Duration

	// MaxWait is the configured semp.max_queue_wait that was exceeded. Upper
	// layers use it as the retry-after hint: under sustained saturation, backing
	// off by at least the bound is what keeps every shed caller from returning
	// at once.
	MaxWait time.Duration
}

func (e *BrokerBusyError) Error() string {
	return fmt.Sprintf("broker busy: request not admitted within %s at the %s gate (waited %s); "+
		"the request was not sent", e.MaxWait, e.Stage, e.Waited)
}

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
	rateLimiter   <-chan time.Time   // from the broker's shared RateLimiter; not owned here
	sem           Semaphore          // bounds in-flight requests; shared per-broker across SEMPv1+v2
	brokerURL     string             // for logging context
	retryBudget   time.Duration      // overall deadline for the whole retry chain; 0 disables
	maxQueueWait  time.Duration      // overall bound on admission (both gates); 0 disables
}

// New creates a Sender configured for a specific broker. It sets up
// retryablehttp with the retry policy from SEMPConfig and reads pacing from the
// supplied per-broker limiter. sempCfg.Retries must be non-nil.
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
//
// limiter paces requests and must be non-nil, for the same reason and with the
// same sharing requirement: a Sender-private limiter admits up to 2× the
// configured rate because each protocol client paces only itself (SOL-152401).
// Its lifetime is owned by the caller — BrokerClient.Close() stops it.
func New(httpClient *http.Client, sempCfg *config.SEMPConfig, authn auth.Authenticator, brokerURL string, sem Semaphore, limiter *RateLimiter) *Sender {
	if authn == nil {
		panic("resilience.New: authn must be non-nil; construct via semp.newAuthenticator in semp.NewBrokerClient")
	}
	if sem == nil {
		panic("resilience.New: sem must be non-nil; share one per broker via semp.NewBrokerClient")
	}
	if limiter == nil {
		panic("resilience.New: limiter must be non-nil; share one per broker via semp.NewBrokerClient")
	}
	// Guarded for the same reason as the three above, and not merely documented:
	// Retries is dereferenced here and again when sizing the retry-chain
	// deadline, so a config that skipped defaulting would surface as a bare nil
	// dereference somewhere inside construction rather than as the contract
	// violation it is.
	if sempCfg == nil || sempCfg.Retries == nil {
		panic("resilience.New: sempCfg and sempCfg.Retries must be non-nil; apply config defaults before constructing a Sender")
	}
	d := &Sender{
		authenticator: authn,
		sem:           sem,
		rateLimiter:   limiter.C(),
		// Sanitized once here rather than at each of the many log sites below —
		// brokerURL is logging-only (see the field doc), so nothing downstream
		// needs the raw form. Defense in depth: validateBrokerURL already
		// rejects credentialed URLs at config load, but this keeps that
		// guarantee from being the only thing standing between a broker URL
		// and the log stream (SOL-152979).
		brokerURL: config.SanitizeURLString(brokerURL),
	}

	// Admission bound (SOL-153442). Deliberately NOT added to the nil-guard
	// panic above, unlike Retries: a nil MaxQueueWait means "no bound", which is
	// exactly the pre-SOL-153442 behavior and a safe reading for a
	// hand-constructed config. Production always goes through config
	// applyDefaults, which fills it. Panicking instead would break every
	// direct-construction test in this package and in test/integration for no
	// safety gain — the failure mode of getting this wrong is "behaves as it did
	// before", not "silently exceeds a configured limit", which is what makes
	// the sem and limiter guards worth a panic.
	if sempCfg.MaxQueueWait != nil {
		d.maxQueueWait = *sempCfg.MaxQueueWait
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

	return d
}

// admit passes a request through both per-broker admission gates — the rate
// limiter, then the in-flight semaphore — and returns nil once a semaphore slot
// is held. The caller owns releasing that slot; admit acquires it and never
// releases it on the success path.
//
// Both gates share ONE budget of d.maxQueueWait, measured from entry rather
// than restarted per gate, so the caller-visible ceiling is the configured
// bound and not two of them (SOL-153442). Past the budget the request is shed
// with a *BrokerBusyError instead of waiting on.
//
// Bounding both gates rather than just the rate limiter is the point. The
// limiter drains at a fixed pace and only backs up while arrival rate exceeds
// it; the semaphore holds each slot for the entire retry chain, which at
// default settings runs to roughly 16 minutes (see retryBudget in New). A
// degraded broker fills every slot, and a caller with no deadline blocking on
// the semaphore is the indefinite hang this exists to prevent. Bounding the
// limiter alone would move that hang one line down, not remove it.
//
// A zero budget disables the bound: `expired` stays nil, and a receive on a nil
// channel blocks forever, so both selects fall back to waiting on the caller's
// context exactly as they did before.
func (d *Sender) admit(ctx context.Context) error {
	start := time.Now()

	// One timer for both gates, so the budget carries across rather than
	// resetting: a request that spends most of it waiting for a tick sheds on
	// the remainder at the concurrency gate, and the caller-visible ceiling
	// stays d.maxQueueWait rather than twice it.
	//
	// A timer that fires while the rate-limiter gate is not looking is still
	// delivered to the concurrency gate's select. That is the timer-channel
	// guarantee, NOT buffering — since Go 1.23 timer channels are unbuffered
	// (cap(timer.C) == 0). Do not "fix" this by draining or re-arming the
	// timer; either would silently reinstate the per-gate budget this avoids.
	var expired <-chan time.Time
	if d.maxQueueWait > 0 {
		timer := time.NewTimer(d.maxQueueWait)
		defer timer.Stop()
		expired = timer.C
	}

	// Rate limit: wait for the per-broker interval before sending a new request.
	// This does NOT apply to retries — retryablehttp handles those internally
	// with jittered backoff (RateLimitLinearJitterBackoff). During failure
	// episodes, total broker traffic can reach (1 + maxTransientRetries) times
	// the configured rate for a 429/503 episode (other retryable failures are
	// capped tighter: 401 re-auth and non-429/503 5xx retry at most once, and
	// only connection errors use the full RetryMax), but retries are spread
	// over time by the backoff and
	// jitter desynchronizes concurrent failures. The broker's Retry-After
	// header (if present on 429/503) takes priority over computed backoff.
	select {
	case <-d.rateLimiter:
		slog.Debug("rate limiter: request permitted",
			slog.String("broker", d.brokerURL))
	case <-ctx.Done():
		return ctx.Err()
	case <-expired:
		return d.shed(ctx, AdmissionStageRateLimit, start)
	}

	// In-flight cap: acquire a per-broker semaphore slot for the duration of
	// the request, including retryablehttp's internal retries (the request is
	// still in flight against the broker while retrying). Acquired after the
	// rate-limiter tick so a slot is held only while genuinely in flight.
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-expired:
		return d.shed(ctx, AdmissionStageConcurrency, start)
	}

	return nil
}

// shed builds the BrokerBusyError for an expired admission budget and logs the
// event for the operator.
//
// A caller that set its own deadline must never be told the broker is busy when
// what actually happened is that its own deadline passed. Two checks, because
// one is not enough:
//
// The Deadline() comparison comes first and is the load-bearing one. Checking
// only ctx.Err() loses this race almost every time: a timer fires straight from
// the runtime, while a context deadline has to propagate through
// cancelCtx.cancel before Err() reports it, so a caller whose deadline expires
// a few hundred microseconds BEFORE the budget still reads Err() == nil here
// and would be handed a BrokerBusyError. Measured at ~2ms wide on an idle
// machine and wider under load. Comparing the deadline against the clock has no
// such window.
//
// The ctx.Err() check then covers explicit cancellation, which has no deadline
// to read. A cancel landing within microseconds of the budget expiring can
// still surface as a BrokerBusyError; that residue is inherent to selecting on
// two simultaneously-ready channels and is harmless, since nothing was sent
// either way.
func (d *Sender) shed(ctx context.Context, stage string, start time.Time) error {
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	waited := time.Since(start)

	opID := "unknown"
	if id, ok := ctx.Value(OperationIDKey{}).(string); ok {
		opID = id
	}

	// WARN, not ERROR: shedding is the server working as configured under load,
	// and it is the signal an operator alerts on to decide whether to raise
	// semp.max_concurrent_per_broker, lower semp.request_min_interval, or add
	// broker capacity. brokerURL was sanitized once in New; stage and the
	// durations are server-generated, so nothing here is broker- or
	// caller-sourced text.
	slog.Warn("request shed: broker admission bound exceeded",
		slog.String("broker", d.brokerURL),
		slog.String("operation", opID),
		slog.String("stage", stage),
		slog.Duration("waited", waited),
		slog.Duration("max_queue_wait", d.maxQueueWait))

	return &BrokerBusyError{Stage: stage, Waited: waited, MaxWait: d.maxQueueWait}
}

// Do sends an HTTP request through the rate limiter and retryablehttp client.
// The caller is responsible for building the request and adding authentication.
// On success, returns the HTTP response (body open for the caller to read).
// On failure after retries, returns a RetriesExhaustedError or a wrapped error.
// When the request cannot be admitted within semp.max_queue_wait it returns a
// BrokerBusyError and never reaches the broker (see admit).
func (d *Sender) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if err := d.admit(ctx); err != nil {
		return nil, err
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
	// caller's idempotency markers (WithRetrySafe / WithRetryUnsafe).
	ctx = context.WithValue(ctx, retryStateKey{}, &retryState{
		method:      req.Method,
		retrySafe:   isRetrySafe(ctx),
		retryUnsafe: isRetryUnsafe(ctx),
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
		// Record that the non-idempotency guard was in force, so upper layers
		// do not tell the agent to "try again later" on a request the broker
		// may already have carried out. This is set here rather than in
		// errorHandler because a connection error leaves resp nil, so
		// errorHandler has no route back to the request context.
		if isRetryUnsafe(ctx) {
			var exhausted *RetriesExhaustedError
			if errors.As(err, &exhausted) {
				exhausted.NonIdempotent = true
			}
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
	// Capture the drained bytes rather than discarding them: the read has to
	// happen either way to release the connection, and on the non-idempotency
	// path these bytes are the broker's only account of what it did.
	var body []byte
	if resp != nil && resp.Body != nil {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, io.LimitReader(resp.Body, errorHandlerDrainLimit))
		_ = resp.Body.Close()
		body = buf.Bytes()
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
				Body:       body,
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
			Body:       body,
		}
	}

	return nil, fmt.Errorf("request failed after %d attempts", numTries)
}
