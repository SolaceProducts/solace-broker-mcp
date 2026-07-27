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
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The method-based idempotency guard treats PUT and DELETE as replay-safe
// because RFC 9110 calls them idempotent. That reasoning holds for the SEMPv2
// config API (a repeated resource replacement converges on the same state) and
// breaks for the action API, which routes non-idempotent destructive RPC over
// PUT — doMsgVpnQueueDeleteMsgs ("Delete all spooled messages from the Queue")
// takes no message-ID range, so a replay destroys whatever arrived in the
// meantime. RFC 9110 idempotency constrains the final state, not the set of
// side effects produced along the way.
//
// WithRetryUnsafe lets the caller declare semantic non-idempotency, sourced
// from a tool's own `idempotent: false` annotation. These tests pin both
// directions: a marked request is never replayed once the broker may have
// acted on it, and an unmarked request keeps today's retry behaviour.

// abortHandler closes the connection without writing a response, which surfaces
// on the client as a transport-level *url.Error. This models the reachable
// production triggers — TCP reset, broker restart mid-request, load-balancer
// idle drop — deterministically, with no reliance on timing.
func abortHandler(counter *atomic.Int32) http.HandlerFunc {
	return func(_ http.ResponseWriter, _ *http.Request) {
		counter.Add(1)
		panic(http.ErrAbortHandler)
	}
}

func TestSender_RetryUnsafe_TransportError_NotReplayed(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, abortHandler(&requestCount), "basic", 10)
	defer server.Close()

	resp, err := sender.Do(WithRetryUnsafe(context.Background()), newMethodRequest(t, http.MethodPut, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if got := requestCount.Load(); got != 1 {
		t.Errorf("retry-unsafe PUT reached the broker %d times, want exactly 1 — "+
			"a replayed destructive action deletes messages the caller never authorized", got)
	}
}

// TestSender_RetryUnsafe_TransportError_MarksErrorNonIdempotent pins the signal
// the tools layer keys its agent-facing `retryable` flag off. Without it the
// guard stops the machine replay but the tool result still tells the agent to
// "try again later", which re-runs the very side effect that was refused.
func TestSender_RetryUnsafe_TransportError_MarksErrorNonIdempotent(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, abortHandler(&requestCount), "basic", 10)
	defer server.Close()

	resp, err := sender.Do(WithRetryUnsafe(context.Background()), newMethodRequest(t, http.MethodPut, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("want *RetriesExhaustedError, got %T: %v", err, err)
	}
	if !exhausted.NonIdempotent {
		t.Error("NonIdempotent is false; upper layers will report this failure as retryable " +
			"and invite the agent to repeat a request the broker may already have applied")
	}
	if exhausted.Err == nil {
		t.Error("underlying transport cause was dropped — returning a sentinel from checkRetry " +
			"would mask it, which is why the guard returns (false, nil)")
	}
	// The server-side log detail must not claim retries were exhausted when none
	// were taken; Attempts is 1 here.
	if msg := exhausted.Error(); strings.Contains(msg, "attempts:") {
		t.Errorf("Error() still reports exhausted retries for a request that was never "+
			"retried: %q", msg)
	}
}

// The marker must not leak onto ordinary failures, or every exhausted retry
// chain would be reported as unsafe to repeat.
func TestSender_Unmarked_TransportError_NotMarkedNonIdempotent(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, abortHandler(&requestCount), "basic", 1)
	defer server.Close()

	resp, err := sender.Do(context.Background(), newMethodRequest(t, http.MethodPut, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("want *RetriesExhaustedError, got %T: %v", err, err)
	}
	if exhausted.NonIdempotent {
		t.Error("NonIdempotent set on an unmarked request")
	}
}

func TestSender_RetryUnsafe_503_NotRetried(t *testing.T) {
	// A 503 is very probably a pre-execution rejection, but "very probably" is
	// not good enough for an irreversible purge: the operator can re-issue.
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "basic", 10)
	defer server.Close()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	resp, err := sender.Do(WithRetryUnsafe(context.Background()), newMethodRequest(t, http.MethodPut, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 passthrough or error, got status %d", resp.StatusCode)
	}
	if got := requestCount.Load(); got != 1 {
		t.Errorf("retry-unsafe PUT reached the broker %d times on 503, want exactly 1", got)
	}
	// The suppression an operator needs to see: a retry was on the table and the
	// gate took it away.
	if !strings.Contains(buf.String(), gateLogPrefix) {
		t.Errorf("gate suppressed a 503 retry without logging it:\n%s", buf.String())
	}
}

func TestSender_RetryUnsafe_Other5xx_NotRetried(t *testing.T) {
	// 504 is the sharpest case: the gateway gave up while the broker may still
	// be executing the purge, so a replay is the same hazard as a transport error.
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusGatewayTimeout)
	}, "basic", 10)
	defer server.Close()

	resp, err := sender.Do(WithRetryUnsafe(context.Background()), newMethodRequest(t, http.MethodPut, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil && resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 passthrough or error, got status %d", resp.StatusCode)
	}
	if got := requestCount.Load(); got != 1 {
		t.Errorf("retry-unsafe PUT reached the broker %d times on 504, want exactly 1", got)
	}
}

// TestSender_RetryUnsafe_401_StillReAuths is the guard against over-correcting.
// A 401 is an authentication rejection the broker issues BEFORE executing the
// request, so it is the one path a non-idempotent request can still safely
// retry. Losing it would turn a token expiring mid-purge into a hard failure.
func TestSender_RetryUnsafe_401_StillReAuths(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		jsonOK(w)
	}, "basic", 10)
	defer server.Close()

	resp, err := sender.Do(WithRetryUnsafe(context.Background()), newMethodRequest(t, http.MethodPut, server.URL))
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after 401 re-auth retry", resp.StatusCode)
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("expected 2 requests (original + 401 re-auth retry), got %d", got)
	}
}

// --- Controls: these fail if the guard is written too broadly ---

// An UNMARKED PUT must keep today's retry behaviour. This proves the new marker
// is what changed the outcome, not the HTTP method, and that the SEMPv2 config
// API keeps its resilience.
func TestSender_UnmarkedPUT_TransportError_StillRetried(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, abortHandler(&requestCount), "basic", 3)
	defer server.Close()

	resp, err := sender.Do(context.Background(), newMethodRequest(t, http.MethodPut, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if got := requestCount.Load(); got != 4 {
		t.Errorf("unmarked PUT reached the broker %d times, want 4 (original + 3 retries) — "+
			"config-API PUT must keep its retry resilience", got)
	}
}

func TestSender_UnmarkedPUT_503_StillRetried(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "basic", 10)
	defer server.Close()

	resp, err := sender.Do(context.Background(), newMethodRequest(t, http.MethodPut, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 passthrough or error, got status %d", resp.StatusCode)
	}
	// maxTransientRetries caps 429/503 at 3 retries (SOL-152209), so 1 + 3.
	if got := requestCount.Load(); got != maxTransientRetries+1 {
		t.Errorf("unmarked PUT reached the broker %d times on 503, want %d", got, maxTransientRetries+1)
	}
}

// A GET is safe by method and carries no marker, so it must be untouched.
func TestSender_UnmarkedGET_TransportError_StillRetried(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, abortHandler(&requestCount), "basic", 2)
	defer server.Close()

	resp, err := sender.Do(context.Background(), newGetRequest(t, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if got := requestCount.Load(); got != 3 {
		t.Errorf("GET reached the broker %d times, want 3 (original + 2 retries)", got)
	}
}

// retryablehttp calls CheckRetry on every response, success included. The
// non-idempotency gate therefore has to be scoped to statuses that were
// genuinely retry candidates, or every successful action tool call emits a
// "not retrying" line describing suppression that never happened — the noise
// lands hardest on delete-queue-messages, the operation an operator is most
// likely to be reading the logs for.
func TestSender_RetryUnsafe_SuccessAndClientError_DoNotLogSuppression(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"200 OK", http.StatusOK},
		{"400 Bad Request", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}, "basic", 10)
			defer server.Close()

			resp, err := sender.Do(WithRetryUnsafe(context.Background()), newMethodRequest(t, http.MethodPut, server.URL))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			resp.Body.Close()

			if strings.Contains(buf.String(), gateLogPrefix) {
				t.Errorf("gate logged suppression on a %d, which was never a retry candidate:\n%s", tc.status, buf.String())
			}
		})
	}
}

// gateLogPrefix is the non-idempotency gate's message, matched in full rather
// than on the word "non-idempotent" alone: RetriesExhaustedError.Error() also
// carries that word, so a bare substring match could pass for the wrong reason
// if that error ever starts being logged on this path.
const gateLogPrefix = "not retrying: caller declared the request non-idempotent"

// TestCheckRetry_RetryUnsafe_NeverRetriesAnyStatus is the invariant behind
// SOL-152400, and it is load-bearing in a way the per-status tests are not.
//
// The gate is scoped by retryableStatus, which restates the switch's
// classification in a second place. That coupling fails OPEN: make the helper
// broader than the switch and you get a stray log line, make it narrower — or
// add a retryable arm to the switch and forget the helper — and the gate stops
// firing, which replays a purge. A comment is not enough to hold that.
//
// Sweeping every status closes it at CI time, and doubles as the proof that
// scoping the gate preserved behaviour for all of them.
func TestCheckRetry_RetryUnsafe_NeverRetriesAnyStatus(t *testing.T) {
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, "basic", 10)
	defer server.Close()

	// Swept across methods, not just PUT. The POST/PATCH guard returns before the
	// status gate, so a PUT-only sweep would read exhaustive while leaving the
	// guarantee method-conditional: correct for today's action tools, which are
	// all PUT, and silently absent for the first `idempotent: false` tool built
	// on a POST config operation.
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		for code := 100; code <= 599; code++ {
			// 401 is the one deliberate exception: authentication is rejected
			// before the broker acts, so re-auth cannot duplicate a side effect.
			// TestSender_RetryUnsafe_401_StillReAuths pins that path.
			if code == http.StatusUnauthorized {
				continue
			}
			state := &retryState{method: method, retryUnsafe: true}
			ctx := context.WithValue(context.Background(), retryStateKey{}, state)

			retry, err := sender.checkRetry(ctx, &http.Response{StatusCode: code}, nil)
			if retry {
				t.Errorf("%s %d: got retry=true — a declared non-idempotent request must never be replayed", method, code)
			}
			// An error is allowed, and on a retryable status it is required: it
			// routes the request through errorHandler into a NonIdempotent
			// *RetriesExhaustedError instead of handing the agent a raw 503 that
			// the tools layer reports as retryable. Any other error is a bug.
			if err != nil && !errors.Is(err, errNonIdempotentNotRetried) {
				t.Errorf("%s %d: got unexpected err=%v, want nil or errNonIdempotentNotRetried", method, code, err)
			}
			if retryableStatus(code) && !errors.Is(err, errNonIdempotentNotRetried) {
				t.Errorf("%s %d: gate returned err=%v; without errNonIdempotentNotRetried the raw status "+
					"reaches the tools layer and is reported retryable", method, code, err)
			}
			// The retry counters gate errTransientCapReached and the once-only
			// 5xx replay. A marked request must never touch them, or a later
			// attempt inherits a budget it never spent.
			if state.transientRetried != 0 || state.other5xxRetried {
				t.Errorf("%s %d: gate mutated retry counters (transientRetried=%d, other5xxRetried=%v)",
					method, code, state.transientRetried, state.other5xxRetried)
			}
		}
	}
}
