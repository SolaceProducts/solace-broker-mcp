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

package tokenexchange

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache/cachetest"
)

// newTestExchanger builds an Exchanger pointing at the given httptest server
// with a pinned clock. The returned nowFunc can be swapped to advance time.
func newTestExchanger(t *testing.T, serverURL string) *Exchanger {
	t.Helper()
	p := validParams(t)
	p.TokenURL = serverURL
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// pinnedNow returns a fixed time for deterministic assertions.
func pinnedNow() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

// successJSON returns a valid RFC 8693 success response body.
func successJSON(accessToken string, expiresIn int64) string {
	return fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","issued_token_type":%q,"expires_in":%d}`,
		accessToken, URNTokenTypeAccessToken, expiresIn)
}

// validInput returns an ExchangeInput suitable for most tests.
func validInput() ExchangeInput {
	return ExchangeInput{
		SubjectToken: "test-subject-token",
		BrokerAlias:  "test-broker",
		Audience:     "https://broker.example.com",
	}
}

// ---------- B06: Exchange happy path ----------

func TestExchange_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-tok", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	now := pinnedNow()
	e.nowFunc = func() time.Time { return now }

	tok, err := e.Exchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok == nil {
		t.Fatal("tok = nil, want non-nil")
		return
	}
	if tok.Value != "exchanged-tok" {
		t.Errorf("tok.Value = %q, want %q", tok.Value, "exchanged-tok")
	}

	wantExpiresAt := now.Add(3600*time.Second - defaults.DefaultTokenExpirySkew)
	if !tok.ExpiresAt.Equal(wantExpiresAt) {
		t.Errorf("tok.ExpiresAt = %v, want %v", tok.ExpiresAt, wantExpiresAt)
	}
}

// ---------- B02: singleflight deduplication ----------

func TestExchange_SingleflightDeduplication(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		<-gate // block until released
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("dedup-tok", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input := validInput()
	const n = 5
	var wg sync.WaitGroup
	tokens := make([]*Token, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tokens[idx], errs[idx] = e.Exchange(context.Background(), input)
		}(i)
	}

	// Brief pause to let goroutines queue up in singleflight.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: error = %v", i, errs[i])
		}
		if tokens[i] == nil {
			t.Errorf("goroutine %d: token = nil", i)
		} else if tokens[i].Value != "dedup-tok" {
			t.Errorf("goroutine %d: tok.Value = %q, want %q", i, tokens[i].Value, "dedup-tok")
		}
	}

	if got := callCount.Load(); got != 1 {
		t.Errorf("IdP called %d times, want 1 (singleflight should collapse concurrent calls)", got)
	}
}

func TestExchange_DifferentKeysRunConcurrently(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	var receivedTokensMu sync.Mutex
	receivedTokens := map[string]bool{}
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		r.ParseForm()
		subj := r.PostFormValue("subject_token")
		receivedTokensMu.Lock()
		receivedTokens[subj] = true
		receivedTokensMu.Unlock()
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-for-"+subj, 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input1 := ExchangeInput{SubjectToken: "tok-A", BrokerAlias: "broker-A", Audience: "aud"}
	input2 := ExchangeInput{SubjectToken: "tok-B", BrokerAlias: "broker-B", Audience: "aud"}

	tokens := make([]*Token, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); tokens[0], errs[0] = e.Exchange(context.Background(), input1) }()
	go func() { defer wg.Done(); tokens[1], errs[1] = e.Exchange(context.Background(), input2) }()

	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d: unexpected error = %v", i, errs[i])
		}
	}

	if tokens[0] == nil || tokens[0].Value != "exchanged-for-tok-A" {
		t.Errorf("caller 0: token = %v, want Value=%q", tokens[0], "exchanged-for-tok-A")
	}
	if tokens[1] == nil || tokens[1].Value != "exchanged-for-tok-B" {
		t.Errorf("caller 1: token = %v, want Value=%q", tokens[1], "exchanged-for-tok-B")
	}

	if got := callCount.Load(); got != 2 {
		t.Errorf("IdP called %d times, want 2 (different keys should not be deduplicated)", got)
	}

	// Lock: the httptest handler writes receivedTokens under receivedTokensMu,
	// and wg tracks Exchange() completion, not the handler goroutine.
	receivedTokensMu.Lock()
	if !receivedTokens["tok-A"] || !receivedTokens["tok-B"] {
		t.Errorf("IdP received subject_tokens = %v, want both tok-A and tok-B", receivedTokens)
	}
	receivedTokensMu.Unlock()
}

func TestExchange_SameTokenDifferentBrokersRunConcurrently(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	var receivedTokensMu sync.Mutex
	receivedTokens := map[string]bool{}
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		r.ParseForm()
		subj := r.PostFormValue("subject_token")
		receivedTokensMu.Lock()
		receivedTokens[subj] = true
		receivedTokensMu.Unlock()
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-for-"+subj, 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input1 := ExchangeInput{SubjectToken: "same-token", BrokerAlias: "broker-A", Audience: "aud"}
	input2 := ExchangeInput{SubjectToken: "same-token", BrokerAlias: "broker-B", Audience: "aud"}

	tokens := make([]*Token, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); tokens[0], errs[0] = e.Exchange(context.Background(), input1) }()
	go func() { defer wg.Done(); tokens[1], errs[1] = e.Exchange(context.Background(), input2) }()

	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d: unexpected error = %v", i, errs[i])
		}
		if tokens[i] == nil || tokens[i].Value != "exchanged-for-same-token" {
			t.Errorf("caller %d: token = %v, want Value=%q", i, tokens[i], "exchanged-for-same-token")
		}
	}

	if got := callCount.Load(); got != 2 {
		t.Errorf("IdP called %d times, want 2 (same token + different brokers must not be deduplicated)", got)
	}

	// Lock: the httptest handler writes receivedTokens under receivedTokensMu,
	// and wg tracks Exchange() completion, not the handler goroutine.
	receivedTokensMu.Lock()
	if !receivedTokens["same-token"] {
		t.Errorf("IdP received subject_tokens = %v, want same-token", receivedTokens)
	}
	receivedTokensMu.Unlock()
}

// ---------- B04: context cancellation does not affect other callers ----------

// Simulates the real production scenario: three concurrent groups of
// requests that prove cancellation is scoped to the exact (user, broker)
// pair and does not leak across singleflight keys.
//
//	Group 1: User A + Broker X — cancelled → all fail
//	Group 2: User A + Broker Y — not cancelled → all succeed (same user, different broker)
//	Group 3: User C + Broker X — not cancelled → all succeed (different user, same broker)
func TestExchange_CancellationScopedToKeyBoundary(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	var receivedTokensMu sync.Mutex
	receivedTokens := map[string]bool{}
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		r.ParseForm()
		subj := r.PostFormValue("subject_token")
		receivedTokensMu.Lock()
		receivedTokens[subj] = true
		receivedTokensMu.Unlock()
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-for-"+subj, 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	userA_brokerX := ExchangeInput{SubjectToken: "user-a-jwt", BrokerAlias: "broker-x", Audience: "aud"}
	userA_brokerY := ExchangeInput{SubjectToken: "user-a-jwt", BrokerAlias: "broker-y", Audience: "aud"}
	userC_brokerX := ExchangeInput{SubjectToken: "user-c-jwt", BrokerAlias: "broker-x", Audience: "aud"}

	cancelCtx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	// Group 1: User A + Broker X — 3 requests, will be cancelled.
	g1Errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, g1Errs[idx] = e.Exchange(cancelCtx, userA_brokerX)
		}(i)
	}

	// Group 2: User A + Broker Y — 2 requests, should succeed.
	g2Tokens := make([]*Token, 2)
	g2Errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			g2Tokens[idx], g2Errs[idx] = e.Exchange(context.Background(), userA_brokerY)
		}(i)
	}

	// Group 3: User C + Broker X — 2 requests, should succeed.
	g3Tokens := make([]*Token, 2)
	g3Errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			g3Tokens[idx], g3Errs[idx] = e.Exchange(context.Background(), userC_brokerX)
		}(i)
	}

	// Let all goroutines queue up in singleflight.
	time.Sleep(50 * time.Millisecond)
	// Cancel User A + Broker X group.
	cancel()
	// Release IdP responses.
	time.Sleep(10 * time.Millisecond)
	close(gate)
	wg.Wait()

	// Group 1: all cancelled — every request belongs to the disconnected user+broker.
	for i, err := range g1Errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("group1[%d]: want context.Canceled, got %v", i, err)
		}
	}

	// Group 2: all succeed — same user, different broker = different key.
	// Singleflight collapses the 2 requests into 1 IdP call.
	for i := 0; i < 2; i++ {
		if g2Errs[i] != nil {
			t.Errorf("group2[%d]: unexpected error = %v", i, g2Errs[i])
		}
		if g2Tokens[i] == nil || g2Tokens[i].Value != "exchanged-for-user-a-jwt" {
			t.Errorf("group2[%d]: token = %v, want Value=%q", i, g2Tokens[i], "exchanged-for-user-a-jwt")
		}
	}

	// Group 3: all succeed — different user, same broker = different key.
	// Singleflight collapses the 2 requests into 1 IdP call.
	for i := 0; i < 2; i++ {
		if g3Errs[i] != nil {
			t.Errorf("group3[%d]: unexpected error = %v", i, g3Errs[i])
		}
		if g3Tokens[i] == nil || g3Tokens[i].Value != "exchanged-for-user-c-jwt" {
			t.Errorf("group3[%d]: token = %v, want Value=%q", i, g3Tokens[i], "exchanged-for-user-c-jwt")
		}
	}

	// Group 1 cancelled before the IdP responded, groups 2 and 3 each have
	// their own key. Expect 2-3 IdP calls depending on timing (group 1 may
	// or may not reach the server before cancellation).
	if got := callCount.Load(); got < 2 || got > 3 {
		t.Errorf("IdP called %d times, want 2-3", got)
	}

	// Groups 2 and 3 must have reached the IdP with their respective subject tokens.
	// Lock to establish a happens-before edge with the httptest handler's writes to
	// receivedTokens (the HTTP round-trip isn't synchronization the race detector
	// can use).
	receivedTokensMu.Lock()
	if !receivedTokens["user-a-jwt"] {
		t.Errorf("IdP never received subject_token=user-a-jwt (group 2)")
	}
	if !receivedTokens["user-c-jwt"] {
		t.Errorf("IdP never received subject_token=user-c-jwt (group 3)")
	}
	receivedTokensMu.Unlock()
}

func TestExchange_DifferentTokensSameBrokerRunConcurrently(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	var receivedTokensMu sync.Mutex
	receivedTokens := map[string]bool{}
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		r.ParseForm()
		subj := r.PostFormValue("subject_token")
		receivedTokensMu.Lock()
		receivedTokens[subj] = true
		receivedTokensMu.Unlock()
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-for-"+subj, 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input1 := ExchangeInput{SubjectToken: "user-a-jwt", BrokerAlias: "broker-x", Audience: "aud"}
	input2 := ExchangeInput{SubjectToken: "user-c-jwt", BrokerAlias: "broker-x", Audience: "aud"}

	tokens := make([]*Token, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); tokens[0], errs[0] = e.Exchange(context.Background(), input1) }()
	go func() { defer wg.Done(); tokens[1], errs[1] = e.Exchange(context.Background(), input2) }()

	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d: unexpected error = %v", i, errs[i])
		}
	}

	if tokens[0] == nil || tokens[0].Value != "exchanged-for-user-a-jwt" {
		t.Errorf("caller 0: token = %v, want Value=%q", tokens[0], "exchanged-for-user-a-jwt")
	}
	if tokens[1] == nil || tokens[1].Value != "exchanged-for-user-c-jwt" {
		t.Errorf("caller 1: token = %v, want Value=%q", tokens[1], "exchanged-for-user-c-jwt")
	}

	if got := callCount.Load(); got != 2 {
		t.Errorf("IdP called %d times, want 2 (different tokens + same broker must not be deduplicated)", got)
	}

	// Lock: the httptest handler writes receivedTokens under receivedTokensMu,
	// and wg tracks Exchange() completion, not the handler goroutine.
	receivedTokensMu.Lock()
	if !receivedTokens["user-a-jwt"] || !receivedTokens["user-c-jwt"] {
		t.Errorf("IdP received subject_tokens = %v, want both user-a-jwt and user-c-jwt", receivedTokens)
	}
	receivedTokensMu.Unlock()
}

// ---------- B04: context cancellation supersedes error ----------

// The caller's context is pre-cancelled, but the IdP call runs on a
// detached context (singleflight resilience), so the handler still
// receives and responds. The caller gets context.Canceled from the
// post-Do check, not from a failed HTTP call.
func TestExchange_ContextCancelledReturnsContextError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-token", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tok, err := e.Exchange(ctx, validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on context cancellation", tok)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; err = %v", err)
	}
}

// The caller's context deadline has already expired, but the IdP call
// runs on a detached context (singleflight resilience), so the handler
// still receives and responds. The caller gets context.DeadlineExceeded
// from the post-Do check.
func TestExchange_ContextDeadlineExceededReturnsDeadlineError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-token", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Wait on the context, not the clock. WithTimeout fires via time.AfterFunc,
	// so ctx.Err() stays nil and Done() stays open until the runtime timer
	// goroutine is actually scheduled. A sleep of 5x the deadline looks like
	// ample margin and is not a guarantee: measured on a loaded machine, the
	// context is still live 5ms after a 1ms deadline roughly 1 call in 700, and
	// the timer has been seen firing as late as 16ms. Exchange then legitimately
	// succeeds and this test legitimately fails. That is the whole of the
	// intermittent CI failure here — not a defect in Exchange.
	<-ctx.Done()
	tok, err := e.Exchange(ctx, validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on deadline exceeded", tok)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false, want true; err = %v", err)
	}
}

// TestExchange_ExpiredContextIsHonoredOnEveryPath pins the guarantee the
// deadline test above depends on, across both paths that could return a token
// without consulting the caller.
//
// The cache hit never looked at ctx: TokenCache.Get is not required to honor
// it and the in-memory implementation shipped today ignores it outright, so a
// cancelled caller was handed a live token. The singleflight select is a
// second, much narrower instance — ctx.Done() and the result channel are
// sibling cases, which the Go spec resolves "via a uniform pseudo-random
// selection" when both are ready — now closed by a re-check on the success
// branch.
//
// Only the warm-cache subtest couples to the fix. A cache hit needs no
// scheduling accident, so it returns a token on every run without the guard.
// The cold-cache subtest documents the same contract for the singleflight path
// but passes either way: making both select cases ready on demand requires the
// caller to be off-CPU for an entire IdP round-trip, which a test cannot force.
// It is a statement of intent, not proof.
//
// Neither of these is the cause of the intermittent CI failure in
// TestExchange_ContextDeadlineExceededReturnsDeadlineError. That was the test's
// own premise; see the note there.
func TestExchange_ExpiredContextIsHonoredOnEveryPath(t *testing.T) {
	t.Parallel()

	newExchanger := func(t *testing.T) *Exchanger {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("exchanged-token", 3600))
		}))
		t.Cleanup(srv.Close)
		e := newTestExchanger(t, srv.URL)
		e.nowFunc = func() time.Time { return pinnedNow() }
		return e
	}

	expired := func(t *testing.T) context.Context {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	t.Run("cold cache", func(t *testing.T) {
		t.Parallel()
		e := newExchanger(t)

		tok, err := e.Exchange(expired(t), validInput())
		if tok != nil {
			t.Errorf("tok = %v, want nil for an already-cancelled caller", tok)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(err, context.Canceled) = false, want true; err = %v", err)
		}
	})

	t.Run("warm cache", func(t *testing.T) {
		t.Parallel()
		// Deliberately NOT pinning nowFunc here. The cache expires entries
		// against the wall clock, so a token minted at the pinned 2026-01-01
		// is already stale and every Get is a miss — which would quietly turn
		// this into a second cold-cache test.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, successJSON("exchanged-token", 3600))
		}))
		t.Cleanup(srv.Close)
		e := newTestExchanger(t, srv.URL)

		// Populate the cache through a live call, so the second call takes the
		// hit path rather than the singleflight path.
		if _, err := e.Exchange(context.Background(), validInput()); err != nil {
			t.Fatalf("priming exchange: %v", err)
		}
		// Confirm the fixture actually achieves a hit; otherwise the assertion
		// below would pass for the wrong reason.
		// cache.GetHit is the zero value of GetStatus, so a `!= GetHit` check
		// would also pass on a zero-valued result. Assert the error first, then
		// the status, so this cannot succeed for the wrong reason.
		gr, err := e.cache.Get(context.Background(), computeDeduplicationKey(DeduplicationKeyInput{
			SubjectToken: validInput().SubjectToken,
			BrokerAlias:  validInput().BrokerAlias,
		}))
		if err != nil {
			t.Fatalf("priming the cache: %v", err)
		}
		if gr.Status != cache.GetHit || gr.Entry.Value == "" {
			t.Fatalf("fixture did not warm the cache: status=%v value_empty=%v", gr.Status, gr.Entry.Value == "")
		}

		tok, err := e.Exchange(expired(t), validInput())
		if tok != nil {
			t.Errorf("tok = %v, want nil: a cache hit must not outrank the caller's cancellation", tok)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(err, context.Canceled) = false, want true; err = %v", err)
		}
	})
}

// A follower that shares an in-flight exchange must bail out the moment its
// own context is cancelled — it must NOT block until the leader's IdP call
// completes. The IdP handler is held on a gate so the shared call stays in
// flight; the follower is cancelled and must return context.Canceled while
// the gate is still closed (leader not yet done). Under the old blocking
// group.Do this test would hang until the timeout fires.
//
// callCount == 1 at the end proves the follower actually deduped into the
// leader's call rather than starting its own; the leader-token assertion
// proves the follower's cancellation did not corrupt the shared result.
func TestExchange_CancelledFollowerBailsBeforeLeaderCompletes(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	entered := make(chan struct{}) // signals the IdP handler is mid-flight
	gate := make(chan struct{})    // releases the IdP response
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		once.Do(func() { close(entered) })
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-token", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input := validInput()

	// Leader on a background context keeps the shared IdP call in flight.
	type result struct {
		tok *Token
		err error
	}
	leaderRes := make(chan result, 1)
	go func() {
		tok, err := e.Exchange(context.Background(), input)
		leaderRes <- result{tok, err}
	}()

	// Wait until the IdP handler is actually blocked on the gate — the
	// singleflight key is now registered, so a same-key caller will attach.
	<-entered

	// Follower with the same key. followerStarted ensures its goroutine is
	// actually running before we cancel, so the cancel cannot land before the
	// follower has begun (which would exercise nothing). Dedup determinism
	// does NOT rely on timing: the leader is gated, so the key stays in flight
	// and the follower attaches regardless of where the cancel lands — that is
	// what the callCount == 1 assertion below verifies.
	cancelCtx, cancel := context.WithCancel(context.Background())
	followerStarted := make(chan struct{})
	followerErr := make(chan error, 1)
	go func() {
		close(followerStarted)
		_, err := e.Exchange(cancelCtx, input)
		followerErr <- err
	}()
	<-followerStarted
	cancel()

	// The follower must return promptly with context.Canceled, well before
	// the gate is released.
	select {
	case err := <-followerErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("follower err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not bail out early; still blocked on the leader")
	}

	// Prove the follower bailed while the leader was still in flight: the gate
	// is still closed, so the leader must not have completed.
	select {
	case <-leaderRes:
		t.Fatal("leader completed before gate released; cannot prove early bail-out")
	default:
	}

	// Release the leader and confirm it still succeeds with the shared result.
	close(gate)
	got := <-leaderRes
	if got.err != nil {
		t.Fatalf("leader err = %v, want nil", got.err)
	}
	if got.tok == nil || got.tok.Value != "exchanged-token" {
		t.Fatalf("leader token = %v, want Value=%q", got.tok, "exchanged-token")
	}

	// Exactly one IdP call: the follower deduped into the leader's in-flight
	// call rather than starting a second round-trip.
	if n := callCount.Load(); n != 1 {
		t.Fatalf("IdP called %d times, want 1 (follower must dedupe into the leader)", n)
	}
}

// The shared IdP call runs on singleflight's own goroutine, detached from any
// caller's context, so it must run to completion and warm the cache even when
// the ONLY caller cancels mid-flight. This pins the payoff the DoChan design
// claims: a subsequent caller gets a cache hit instead of re-hitting the IdP.
func TestExchange_DetachedCallWarmsCacheAfterCallerBails(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		once.Do(func() { close(entered) })
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("warmed-token", 3600))
	}))
	defer srv.Close()

	// Real clock: Otter compares ExpiresAt to time.Now(); a pinned past clock
	// would make the Put look expired and be dropped (see CacheHitShortCircuitsIdP).
	e := newTestExchanger(t, srv.URL)
	input := validInput()

	// One caller starts the exchange, then abandons it while the IdP call is gated.
	cancelCtx, cancel := context.WithCancel(context.Background())
	bailed := make(chan error, 1)
	go func() {
		_, err := e.Exchange(cancelCtx, input)
		bailed <- err
	}()

	<-entered // the detached IdP call is in flight
	cancel()  // the only caller bails

	if err := <-bailed; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller err = %v, want context.Canceled", err)
	}

	// Release the detached call. With no caller waiting, it must still complete
	// and Put the token into the cache.
	close(gate)

	// The Put happens asynchronously on the detached goroutine after the gate.
	// Poll the cache directly (not via Exchange, which would trigger a fetch on
	// a miss and skew callCount) until the entry lands.
	key := computeDeduplicationKey(DeduplicationKeyInput{
		SubjectToken: input.SubjectToken,
		BrokerAlias:  input.BrokerAlias,
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		gr, err := e.cache.Get(context.Background(), key)
		if err == nil && gr.Status == cache.GetHit {
			if gr.Entry.Value != "warmed-token" {
				t.Fatalf("cached token = %q, want %q", gr.Entry.Value, "warmed-token")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache never warmed: the detached call's Put did not land within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Exactly one IdP call — the detached call, not a re-fetch.
	if n := callCount.Load(); n != 1 {
		t.Fatalf("IdP called %d times, want 1", n)
	}
}

// ---------- B05: non-context errors are returned ----------

func TestExchange_TransportErrorReturned(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "internal error")
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	tok, err := e.Exchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on 500", tok)
	}
	if !errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
	}
}

func TestExchange_RejectedErrorReturned(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	tok, err := e.Exchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on 400", tok)
	}
	if !errors.Is(err, ErrExchangeRejected) {
		t.Errorf("errors.Is(err, ErrExchangeRejected) = false, want true; err = %v", err)
	}
}

// ---------- B07: doExchange classifies buildIdPRequest failures as non-retryable ----------

// A request-construction failure is a deterministic config or code defect
// (unparseable URL, invalid method), not a transient network condition.
// It must classify as ErrExchangeRequestBuild — non-retryable — so the
// server-side retry policy does not loop on it, and so the classifier
// does not mislead the agent into retrying.
func TestDoExchange_BuildRequestFailureClassifiesAsRequestBuild(t *testing.T) {
	t.Parallel()

	e := &Exchanger{
		tokenURL:         "://malformed-url",
		clientID:         "cid",
		clientAuthMethod: ClientSecretPost,
		clientSecret:     "sec",
		grantType:        GrantTypeTokenExchange,
		audienceParam:    AudienceParamAudience,
		httpClient:       &http.Client{},
		cache:            cachetest.Default(t),
		nowFunc:          func() time.Time { return pinnedNow() },
	}

	tok, err := e.doExchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on build failure", tok)
	}
	if !errors.Is(err, ErrExchangeRequestBuild) {
		t.Errorf("errors.Is(err, ErrExchangeRequestBuild) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = true, want false — a request-build failure must NOT be a transport failure")
	}
}

// ---------- B08, B09: doExchange HTTP failure paths ----------

func TestDoExchange_HTTPFailureWithCancelledContextReturnsContextError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tok, err := e.doExchange(ctx, validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil", tok)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = true, want false — context errors bypass transport sentinel")
	}
}

func TestDoExchange_HTTPFailureWithoutContextErrorReturnsTransport(t *testing.T) {
	t.Parallel()

	// Use a server that closes the connection immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	tok, err := e.doExchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil", tok)
	}
	if !errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
	}
	if !strings.Contains(err.Error(), "IdP request failed") {
		t.Errorf("err.Error() = %q, want it to contain \"IdP request failed\"", err.Error())
	}
}

// ---------- B10: doExchange passes nowFunc to parseIdPResponse ----------

func TestDoExchange_PassesNowFuncToParseIdPResponse(t *testing.T) {
	t.Parallel()

	pinned := time.Date(2030, 6, 15, 12, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("tok", 7200))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinned }

	tok, err := e.doExchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("doExchange: %v", err)
	}

	wantExpiresAt := pinned.Add(7200*time.Second - defaults.DefaultTokenExpirySkew)
	if !tok.ExpiresAt.Equal(wantExpiresAt) {
		t.Errorf("tok.ExpiresAt = %v, want %v (based on pinned nowFunc)", tok.ExpiresAt, wantExpiresAt)
	}
}

// ---------- B03: elapsed time uses nowFunc ----------

func TestExchange_ElapsedTimeMeasuredWithNowFunc(t *testing.T) {
	t.Parallel()

	// Track nowFunc calls to verify start/end capture.
	var calls atomic.Int32
	fakeNow := pinnedNow()
	mu := sync.Mutex{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Advance the clock while the "request" is in flight.
		mu.Lock()
		fakeNow = fakeNow.Add(500 * time.Millisecond)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("tok", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time {
		calls.Add(1)
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}

	_, err := e.Exchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// nowFunc is called at least twice: once for start, once for elapsed.
	if got := calls.Load(); got < 2 {
		t.Errorf("nowFunc called %d times, want at least 2 (start + elapsed)", got)
	}
}

// logRecord captures a single slog record for test assertions.
type logRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// captureLogs installs a custom slog handler that captures records, and
// returns a function to retrieve them. Caller must defer the returned
// restore function to reset the default logger.
func captureLogs(t *testing.T) (records func() []logRecord, restore func()) {
	t.Helper()

	var mu sync.Mutex
	var captured []logRecord

	handler := &captureHandler{
		mu:       &mu,
		captured: &captured,
	}

	prev := slog.Default()
	slog.SetDefault(slog.New(handler))

	return func() []logRecord {
			mu.Lock()
			defer mu.Unlock()
			cp := make([]logRecord, len(captured))
			copy(cp, captured)
			return cp
		}, func() {
			slog.SetDefault(prev)
		}
}

type captureHandler struct {
	mu       *sync.Mutex
	captured *[]logRecord
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{
		Level:   r.Level,
		Message: r.Message,
		Attrs:   make(map[string]string),
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	*h.captured = append(*h.captured, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func TestExchange_SuccessLogsDebugWithBrokerAndElapsed(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-token", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	tok, err := e.Exchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == nil {
		t.Fatal("expected non-nil token")
	}

	recs := records()
	var found bool
	for _, rec := range recs {
		if rec.Message == "token exchange succeeded" {
			found = true
			if rec.Level != slog.LevelDebug {
				t.Errorf("want Debug level, got %v", rec.Level)
			}
			if _, ok := rec.Attrs["broker"]; !ok {
				t.Error("missing broker attr")
			}
			if _, ok := rec.Attrs["exchange_elapsed"]; !ok {
				t.Error("missing exchange_elapsed attr")
			}
		}
	}
	if !found {
		t.Error("success-path debug log not emitted")
	}
}

// B04 (log aspect): a cancelled caller gets no success/error OUTCOME log.
// The caller's context is pre-cancelled, but the IdP call runs on a detached
// context (singleflight resilience), so the handler still receives and responds
// to the request. The caller returns context.Canceled from the select's
// ctx.Done branch, not from a failed HTTP call — and must not have "token
// exchange succeeded" or an error attributed to it. A debug "token exchange
// abandoned by caller" breadcrumb IS emitted on that branch (it makes a
// cancellation storm visible now that the shared call runs detached) and is
// the one allowed "token exchange" line.
func TestExchange_ContextErrorNotLogged(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-token", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.Exchange(ctx, validInput())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	recs := records()
	for _, rec := range recs {
		// The intentional cancel-branch breadcrumb is allowed; nothing else
		// with "token exchange" (a success or error outcome) may be attributed
		// to the cancelled caller.
		if rec.Message == "token exchange abandoned by caller" {
			continue
		}
		if strings.Contains(rec.Message, "token exchange") {
			t.Errorf("context cancellation should not produce a token exchange outcome log, got: %q", rec.Message)
		}
	}
}

// ---------- Integration: Exchange end-to-end with various IdP responses ----------

func TestExchange_InvalidResponseError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Missing access_token.
		fmt.Fprint(w, `{"token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`)
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	tok, err := e.Exchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on invalid response", tok)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = false, want true; err = %v", err)
	}
}

// Verify that the request body sent to the IdP contains expected form fields.
func TestExchange_RequestBodyContainsExpectedFields(t *testing.T) {
	t.Parallel()

	var capturedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		capturedForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("tok", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input := ExchangeInput{
		SubjectToken: "my-jwt",
		BrokerAlias:  "my-broker",
		Audience:     "https://broker.example.com",
	}

	_, err := e.Exchange(context.Background(), input)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	checks := map[string]string{
		"grant_type":         URNGrantTypeTokenExchange,
		"subject_token":      "my-jwt",
		"subject_token_type": URNTokenTypeAccessToken,
		"audience":           "https://broker.example.com",
	}
	for key, want := range checks {
		if got := capturedForm.Get(key); got != want {
			t.Errorf("form[%q] = %q, want %q", key, got, want)
		}
	}

	// scope must NOT appear — the exchange request omits it so the IdP
	// applies its per-client / per-user default scopes.
	if _, present := capturedForm["scope"]; present {
		t.Errorf("form[%q] present (value=%q); want absent", "scope", capturedForm.Get("scope"))
	}

	// BrokerAlias must NOT appear in the request.
	for _, key := range []string{"broker_alias", "broker", "alias"} {
		if capturedForm.Get(key) != "" {
			t.Errorf("form[%q] = %q, BrokerAlias must not appear in IdP request", key, capturedForm.Get(key))
		}
	}
}

// ---------- B07 variant: unknown grant type through Exchange ----------

// An unknown grant type fails at request-build time (before any HTTP call).
// It must classify as ErrExchangeRequestBuild — a deterministic config
// defect — not ErrExchangeTransport.
func TestExchange_UnknownGrantTypeReturnsRequestBuildError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("IdP should not be called for unknown grant type")
	}))
	defer srv.Close()

	e := &Exchanger{
		tokenURL:         srv.URL,
		clientID:         "cid",
		clientAuthMethod: ClientSecretPost,
		clientSecret:     "sec",
		grantType:        GrantType(99),
		audienceParam:    AudienceParamAudience,
		httpClient:       &http.Client{},
		cache:            cachetest.Default(t),
		nowFunc:          func() time.Time { return pinnedNow() },
	}

	tok, err := e.Exchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil for unknown grant type", tok)
	}
	if !errors.Is(err, ErrExchangeRequestBuild) {
		t.Errorf("errors.Is(err, ErrExchangeRequestBuild) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = true, want false — request-build failure must NOT be a transport failure")
	}
}

// ---------- Verify singleflight shares errors too ----------

func TestExchange_SingleflightSharesErrors(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		<-gate
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "server error")
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input := validInput()
	const n = 3
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = e.Exchange(context.Background(), input)
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := 0; i < n; i++ {
		if !errors.Is(errs[i], ErrExchangeTransport) {
			t.Errorf("goroutine %d: errors.Is(err, ErrExchangeTransport) = false; err = %v", i, errs[i])
		}
	}

	if got := callCount.Load(); got != 1 {
		t.Errorf("IdP called %d times, want 1 (singleflight shares errors too)", got)
	}
}

// ---------- Verify JSON response fields passed through correctly ----------

func TestExchange_ResponseFieldsPreserved(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name        string
		respJSON    string
		wantValue   string
		wantExpires time.Time
	}

	now := pinnedNow()
	tests := []testCase{
		{
			name:        "standard token",
			respJSON:    successJSON("standard-tok", 1800),
			wantValue:   "standard-tok",
			wantExpires: now.Add(1800*time.Second - defaults.DefaultTokenExpirySkew),
		},
		{
			name: "extra fields ignored",
			respJSON: `{"access_token":"extra-tok","token_type":"Bearer","issued_token_type":"` +
				URNTokenTypeAccessToken + `","expires_in":600,"refresh_token":"ignored","scope":"openid"}`,
			wantValue:   "extra-tok",
			wantExpires: now.Add(600*time.Second - defaults.DefaultTokenExpirySkew),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.respJSON)
			}))
			defer srv.Close()

			e := newTestExchanger(t, srv.URL)
			e.nowFunc = func() time.Time { return now }

			tok, err := e.Exchange(context.Background(), validInput())
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if tok.Value != tc.wantValue {
				t.Errorf("tok.Value = %q, want %q", tok.Value, tc.wantValue)
			}
			if !tok.ExpiresAt.Equal(tc.wantExpires) {
				t.Errorf("tok.ExpiresAt = %v, want %v", tok.ExpiresAt, tc.wantExpires)
			}
		})
	}
}

// ---------- Verify JSON error response integration ----------

func TestExchange_FourxxWithOAuthError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		status       int
		body         string
		wantSentinel error
	}{
		{"401 invalid_token", 401, `{"error":"invalid_token"}`, ErrExchangeRejected},
		{"400 invalid_grant", 400, `{"error":"invalid_grant"}`, ErrExchangeRejected},
		{"403 WAF HTML", 403, `<html>Denied</html>`, ErrInvalidResponse},
		{"500 server error", 500, `Internal Server Error`, ErrExchangeTransport},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			e := newTestExchanger(t, srv.URL)
			e.nowFunc = func() time.Time { return pinnedNow() }

			tok, err := e.Exchange(context.Background(), validInput())

			if tok != nil {
				t.Errorf("tok = %v, want nil", tok)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Errorf("errors.Is(err, %v) = false, want true; err = %v", tc.wantSentinel, err)
			}
		})
	}
}

// ---------- cache integration: Exchanger-cache contract ----------

// countingIdP returns an httptest server that increments callCount on each
// request and hands out a distinct access token per call ("tok-1", "tok-2", …).
// Distinct tokens per call make it trivial to prove which response a caller
// received without threading assertions through the handler.
func countingIdP(callCount *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON(fmt.Sprintf("tok-%d", n), 3600))
	}))
}

// TestExchange_CacheHitShortCircuitsIdP pins the core cache-hit contract:
// once a token has been exchanged for a given (subjectToken, brokerAlias),
// the next call with the same key returns from cache without touching the IdP.
// Silent regression here means every request hits the IdP — cache is a no-op.
func TestExchange_CacheHitShortCircuitsIdP(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := countingIdP(&callCount)
	defer srv.Close()

	// Use the real clock: Otter compares CachedCredential.ExpiresAt against
	// time.Now(), not e.nowFunc. Pinning nowFunc to a past instant would make
	// every Put appear expired and be silently dropped as PutDroppedTTL.
	e := newTestExchanger(t, srv.URL)

	first, err := e.Exchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	second, err := e.Exchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("second Exchange: %v", err)
	}

	if got := callCount.Load(); got != 1 {
		t.Errorf("IdP called %d times, want 1 (second call must be served from cache)", got)
	}
	if first.Value != second.Value {
		t.Errorf("token values differ: first=%q second=%q; cache hit must return the stored token", first.Value, second.Value)
	}
	if !first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Errorf("ExpiresAt differs: first=%v second=%v", first.ExpiresAt, second.ExpiresAt)
	}
}

// TestExchange_CacheMissStoresResult verifies the write half of the cache
// contract: a successful IdP exchange lands in the cache under the same key
// the next Get will use. If Put breaks silently, the cache is a no-op and
// every request re-hits the IdP.
func TestExchange_CacheMissStoresResult(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := countingIdP(&callCount)
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)

	input := validInput()
	tok, err := e.Exchange(context.Background(), input)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	key := computeDeduplicationKey(DeduplicationKeyInput{
		SubjectToken: input.SubjectToken,
		BrokerAlias:  input.BrokerAlias,
	})
	gr, err := e.cache.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("cache.Get: %v", err)
	}
	if gr.Status != cache.GetHit {
		t.Fatalf("cache status = %v, want GetHit (Exchange should have stored the result)", gr.Status)
	}
	if gr.Entry.Value != tok.Value {
		t.Errorf("cached value = %q, want %q (must match returned token bytes)", gr.Entry.Value, tok.Value)
	}
	if !gr.Entry.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("cached ExpiresAt = %v, want %v", gr.Entry.ExpiresAt, tok.ExpiresAt)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("IdP called %d times during setup, want 1", got)
	}
}

// TestExchange_InvalidateForcesRefetch pins the invalidation contract used
// by OAuthAuthenticator.HandleAuthFailure. After Invalidate for a key, the
// next Exchange must miss the cache and re-fetch from the IdP. If this
// regresses, the broker's 401-retry loop keeps re-serving the stale token.
func TestExchange_InvalidateForcesRefetch(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := countingIdP(&callCount)
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)

	input := validInput()
	first, err := e.Exchange(context.Background(), input)
	if err != nil {
		t.Fatalf("first Exchange: %v", err)
	}

	e.Invalidate(context.Background(), DeduplicationKeyInput{
		SubjectToken: input.SubjectToken,
		BrokerAlias:  input.BrokerAlias,
	})

	second, err := e.Exchange(context.Background(), input)
	if err != nil {
		t.Fatalf("second Exchange: %v", err)
	}

	if got := callCount.Load(); got != 2 {
		t.Errorf("IdP called %d times, want 2 (Invalidate should have forced a re-fetch)", got)
	}
	if first.Value == second.Value {
		t.Errorf("token values equal (%q) — second Exchange should have fetched a fresh token, not returned a cached one", first.Value)
	}
}

// TestExchange_ConcurrentMissesCollapse verifies the interplay between
// singleflight and the cache: many concurrent misses for the same key must
// collapse into ONE IdP call and ONE cache write. Without this guarantee,
// a burst of first-time requests would hammer the IdP and race the cache.
func TestExchange_ConcurrentMissesCollapse(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("burst-tok", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)

	input := validInput()
	const n = 100
	var wg sync.WaitGroup
	tokens := make([]*Token, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tokens[idx], errs[idx] = e.Exchange(context.Background(), input)
		}(i)
	}
	// Let goroutines queue up on singleflight before releasing the IdP.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: error = %v", i, errs[i])
			continue
		}
		if tokens[i] == nil || tokens[i].Value != "burst-tok" {
			t.Errorf("goroutine %d: token = %+v, want burst-tok", i, tokens[i])
		}
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("IdP called %d times, want 1 (singleflight must collapse concurrent misses)", got)
	}

	// The winning singleflight caller must have written the result to the
	// cache; a follow-up lookup for the same key must be a hit.
	key := computeDeduplicationKey(DeduplicationKeyInput{
		SubjectToken: input.SubjectToken,
		BrokerAlias:  input.BrokerAlias,
	})
	gr, err := e.cache.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("post-burst cache.Get: %v", err)
	}
	if gr.Status != cache.GetHit {
		t.Errorf("cache status after burst = %v, want GetHit (singleflight winner should have stored the token)", gr.Status)
	}
}

// countingCache wraps a real TokenCache and counts Put calls. Used to prove
// that the singleflight-winner gating actually collapses N concurrent Put
// attempts into 1, not just N Puts that Otter happens to deduplicate.
type countingCache struct {
	inner cache.TokenCache
	puts  atomic.Int32
}

func (c *countingCache) Get(ctx context.Context, key string) (cache.GetResult, error) {
	return c.inner.Get(ctx, key)
}
func (c *countingCache) Put(ctx context.Context, key string, entry cache.CachedCredential) (cache.PutResult, error) {
	c.puts.Add(1)
	return c.inner.Put(ctx, key, entry)
}
func (c *countingCache) Delete(ctx context.Context, key string) (cache.DeleteResult, error) {
	return c.inner.Delete(ctx, key)
}
func (c *countingCache) Close() error {
	return c.inner.Close()
}

// TestExchange_SingleflightWinnerOnlyWritesCache pins the C1/C2 contract:
// when N goroutines burst-miss the same key, only the singleflight winner
// runs cache.Put. Waiters that received a shared result must NOT re-Put
// (that would produce N redundant writes and N log lines per burst).
func TestExchange_SingleflightWinnerOnlyWritesCache(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("winner-tok", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	// Wrap the real cache with a counter so we can observe the number of
	// Put calls the exchanger actually makes.
	counter := &countingCache{inner: e.cache}
	e.cache = counter

	input := validInput()
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.Exchange(context.Background(), input)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := callCount.Load(); got != 1 {
		t.Errorf("IdP called %d times, want 1 (singleflight collapses concurrent misses)", got)
	}
	if got := counter.puts.Load(); got != 1 {
		t.Errorf("cache.Put called %d times, want 1 (only the singleflight winner should Put)", got)
	}
}

// ---------- SOL-151520: retries + exhaustion sentinel ----------

// newRetryingTestExchanger builds an Exchanger backed by the production
// NewRetryingHTTPClient so the retry loop is exercised end-to-end.
// Per-attempt timeout is shortened to 500ms to keep connection-error
// paths fast; retry policy and the chain deadline both use the shipped
// package defaults, so behavior otherwise matches production. Backoff
// waits (1-2s) dominate wall-clock for tests that hit RetryMax.
//
// For tests that specifically need to exercise the chain-deadline fence
// mid-retry, use newRetryingTestExchangerWithChain to inject a shorter
// chainDeadline via Params.ChainDeadline.
func newRetryingTestExchanger(t *testing.T, serverURL string) *Exchanger {
	t.Helper()
	return buildRetryingTestExchanger(t, serverURL, 0)
}

// newRetryingTestExchangerWithChain is like newRetryingTestExchanger but
// takes a chainDeadline override so a test can force the deadline fence
// to fire mid-retry without waiting out the production ~19s budget.
// The override threads through Params.ChainDeadline into
// ComputeChainDeadline.
func newRetryingTestExchangerWithChain(t *testing.T, serverURL string, chainDeadline time.Duration) *Exchanger {
	t.Helper()
	return buildRetryingTestExchanger(t, serverURL, chainDeadline)
}

// buildRetryingTestExchanger centralizes the common construction path.
// A chainDeadline of 0 means "use the formula" (production behavior);
// a positive value pins the exchanger's chain deadline to that value.
func buildRetryingTestExchanger(t *testing.T, serverURL string, chainDeadline time.Duration) *Exchanger {
	t.Helper()
	client, err := idpclient.NewRetryingHTTPClient(
		idpclient.RetryOptions{
			MaxRetries:   DefaultMaxRetries,
			RetryWaitMin: DefaultRetryWaitMin,
			RetryWaitMax: DefaultRetryWaitMax,
		},
		idpclient.WithTimeout(500*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}
	p := validParams(t)
	p.TokenURL = serverURL
	p.HTTPClient = client
	p.ChainDeadline = chainDeadline
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestExchange_RetriesExhaustedOn5xx asserts the exhaustion rewrap fires
// when the retry loop gives up on repeated 5xx: sentinel switches from
// ErrExchangeTransport (what parseIdPResponse produced) to
// ErrExchangeRetriesExhausted (what the classifier produced).
func TestExchange_RetriesExhaustedOn5xx(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := newRetryingTestExchanger(t, srv.URL)
	_, err := e.Exchange(context.Background(), validInput())

	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Errorf("errors.Is(err, ErrExchangeRetriesExhausted) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = true, want false (should be rewrapped, not the raw transport sentinel)")
	}
	var exchErr *ExchangeError
	if !errors.As(err, &exchErr) {
		t.Fatalf("errors.As(err, *ExchangeError) = false; err = %v", err)
	}
	if exchErr.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("HTTPStatus = %d, want 500 (last-attempt status must survive rewrap)", exchErr.HTTPStatus)
	}
	// FailureClass must survive the rewrap even though the sentinel was
	// replaced. This is the signal the circuit breaker reads to decide an
	// exhausted 5xx counts as a failure — losing it here is exactly the
	// sever bug this assertion guards against.
	if exchErr.FailureClass != FailureClassUpstream5xx {
		t.Errorf("FailureClass = %v, want Upstream5xx (must survive the exhaustion rewrap)", exchErr.FailureClass)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits = %d, want 3 (retryablehttp should have tried all attempts)", got)
	}
}

// TestExchange_RetriesExhaustedOn429 mirrors the 5xx exhaustion test for
// the rate-limit path: parseIdPResponse now maps 429 → ErrExchangeTransport
// so classifyRetryOutcome rewraps it as ErrExchangeRetriesExhausted after
// the retry loop gives up. Guards the taxonomy alignment: a persistent
// 429 is a "we tried, IdP kept refusing" story, same shape as a persistent
// 5xx, and must NOT surface as ErrInvalidResponse (which was the pre-
// SOL-151520 classification and would have implied a malformed response).
func TestExchange_RetriesExhaustedOn429(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	e := newRetryingTestExchanger(t, srv.URL)
	_, err := e.Exchange(context.Background(), validInput())

	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Errorf("errors.Is(err, ErrExchangeRetriesExhausted) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrInvalidResponse) {
		t.Errorf("errors.Is(err, ErrInvalidResponse) = true, want false (429 must NOT fall into the pre-retry non-OAuth-body branch)")
	}
	var exchErr *ExchangeError
	if !errors.As(err, &exchErr) {
		t.Fatalf("errors.As(err, *ExchangeError) = false; err = %v", err)
	}
	if exchErr.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want 429 (last-attempt status must survive rewrap)", exchErr.HTTPStatus)
	}
	// FailureClass must survive the rewrap and remain RateLimited — this
	// is what lets the circuit breaker EXCLUDE an exhausted 429 from the
	// failure percentage rather than counting it as an availability failure.
	if exchErr.FailureClass != FailureClassRateLimited {
		t.Errorf("FailureClass = %v, want RateLimited (must survive rewrap so the breaker can exclude it)", exchErr.FailureClass)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits = %d, want 3 (retry loop should have tried all attempts on 429)", got)
	}
}

// TestExchange_RetryRecoversNoExhaustion covers the middle ground: two
// 500s followed by a 200. attempts > 1, but the eventual success means
// no error, so no rewrap. Guards against a bug where the counter's
// non-zero value would spuriously trigger exhaustion on the success
// path.
func TestExchange_RetryRecoversNoExhaustion(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("recovered-tok", 3600))
	}))
	defer srv.Close()

	e := newRetryingTestExchanger(t, srv.URL)
	tok, err := e.Exchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok == nil || tok.Value != "recovered-tok" {
		t.Errorf("tok = %+v, want value=recovered-tok", tok)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits = %d, want 3", got)
	}
}

// TestExchange_ConnectionErrorRetriesExhausted covers the retry-loop
// path where every attempt fails at the network layer (server closed
// before any request is dispatched). parseIdPResponse never runs, so
// doExchange produces ErrExchangeTransport from the httpClient.Do error
// branch; the classifier still rewraps. HTTPStatus stays 0 because the
// last attempt never saw a response.
func TestExchange_ConnectionErrorRetriesExhausted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	e := newRetryingTestExchanger(t, url)
	_, err := e.Exchange(context.Background(), validInput())

	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Errorf("errors.Is(err, ErrExchangeRetriesExhausted) = false, want true; err = %v", err)
	}
	var exchErr *ExchangeError
	if !errors.As(err, &exchErr) {
		t.Fatalf("errors.As(err, *ExchangeError) = false; err = %v", err)
	}
	if exchErr.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 (no HTTP response was received)", exchErr.HTTPStatus)
	}
	// HTTPStatus == 0 alone is ambiguous (shared with the body-read and
	// deadline paths); FailureClass is what disambiguates an exhausted
	// network failure so the circuit breaker can count it. Assert it
	// survives the rewrap.
	if exchErr.FailureClass != FailureClassNetwork {
		t.Errorf("FailureClass = %v, want Network (must survive rewrap; disambiguates the HTTPStatus==0 case)", exchErr.FailureClass)
	}
}

// TestExchange_RejectedNotRewrapped is the negative case: a 4xx OAuth
// error is not retried by the retry loop (checkRetry declines), so
// attempts stays at 1. The classifier's attempts >= 1 guard is TRUE,
// but the underlying sentinel is ErrExchangeRejected — the rewrap
// filter (ErrExchangeTransport OR DeadlineExceeded) excludes it,
// and the raw rejection sentinel propagates unchanged.
func TestExchange_RejectedNotRewrapped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()

	e := newRetryingTestExchanger(t, srv.URL)
	_, err := e.Exchange(context.Background(), validInput())

	if !errors.Is(err, ErrExchangeRejected) {
		t.Errorf("errors.Is(err, ErrExchangeRejected) = false, want true; err = %v", err)
	}
	if errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Errorf("errors.Is(err, ErrExchangeRetriesExhausted) = true, want false (4xx is not retryable, should not be rewrapped)")
	}
}

// TestClassifyRetryOutcome_ZeroAttemptsPassthrough asserts the
// attempts >= 1 invariant: a raw context.DeadlineExceeded (or any
// error) with attempts == 0 is passed through untouched. This is the
// pre-dispatch-deadline case from error-type.md — nothing was tried,
// so the exhaustion sentinel would be a lie.
func TestClassifyRetryOutcome_ZeroAttemptsPassthrough(t *testing.T) {
	t.Parallel()
	e := newTestExchanger(t, "http://unused")

	underlying := context.DeadlineExceeded
	got := e.classifyRetryOutcome(context.Background(), underlying, 0, "test-broker")
	if got != underlying {
		t.Errorf("classifyRetryOutcome with attempts=0: got %v, want raw underlying (%v)", got, underlying)
	}
	if errors.Is(got, ErrExchangeRetriesExhausted) {
		t.Errorf("attempts=0 must not rewrap as ErrExchangeRetriesExhausted; got %v", got)
	}
}

// TestClassifyRetryOutcome_DeadlineMidRetryRewrapped covers the
// spec-required rewrap of a mid-retry chain-deadline expiry: when
// attempts >= 1 and the underlying is context.DeadlineExceeded, the
// classifier stamps the exhaustion sentinel with the deadline error
// embedded in the message (HTTPStatus stays 0 because no *ExchangeError
// is available on that path).
func TestClassifyRetryOutcome_DeadlineMidRetryRewrapped(t *testing.T) {
	t.Parallel()
	e := newTestExchanger(t, "http://unused")

	got := e.classifyRetryOutcome(context.Background(), context.DeadlineExceeded, 2, "test-broker")
	if got == context.DeadlineExceeded {
		t.Fatalf("classifier returned the raw underlying error; want a rewrapped *ExchangeError")
	}
	if !errors.Is(got, ErrExchangeRetriesExhausted) {
		t.Errorf("errors.Is = false, want true (mid-retry deadline should rewrap); got %v", got)
	}
	// Deliberate: ExchangeError.Unwrap returns only the Sentinel, so the
	// deadline error is intentionally NOT reachable via errors.Is. This
	// matches the pattern of the other sentinels in the family and keeps
	// the classifier surface narrow. If a future change decides to chain
	// the underlying, this assertion becomes the reminder to update it.
	if errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("underlying DeadlineExceeded is unexpectedly reachable via errors.Is — ExchangeError.Unwrap chained the cause; update this test and AgentMessage accordingly")
	}
	var exchErr *ExchangeError
	if !errors.As(got, &exchErr) {
		t.Fatalf("errors.As failed for %v", got)
	}
	if exchErr.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 (no last-attempt status on deadline path)", exchErr.HTTPStatus)
	}
	if !strings.Contains(exchErr.Message, "2 attempts") {
		t.Errorf("Message = %q, want it to mention the attempt count", exchErr.Message)
	}
}

// TestExchange_ChainDeadlineFiresMidRetry exercises the chain-deadline
// fence end-to-end. The direct-unit variant above pins classifyRetryOutcome's
// rewrap behavior, but never proves that the deadline actually fires
// mid-retry through the real retry loop. This test does — it pins the
// chain deadline to 1500ms via Params.ChainDeadline while the retry
// backoff is 1-2s, so the deadline will fire during backoff between
// attempts. Requires Params.ChainDeadline to make the test tractable
// (otherwise we'd wait ~19s per run).
//
// The server returns 500 forever; the retry loop dispatches at least
// one attempt, then the chain deadline expires during the first backoff.
// Expected: sentinel = ErrExchangeRetriesExhausted (mid-retry deadline
// path), attempts >= 1 (something was dispatched before the fence).
func TestExchange_ChainDeadlineFiresMidRetry(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// 1500ms deadline < 1000ms min-backoff after the first attempt's
	// ~fast 500 response means the deadline will fire during backoff.
	e := newRetryingTestExchangerWithChain(t, srv.URL, 1500*time.Millisecond)

	start := time.Now()
	_, err := e.Exchange(context.Background(), validInput())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		t.Errorf("errors.Is(err, ErrExchangeRetriesExhausted) = false, want true; err = %v", err)
	}
	// At least one attempt must have dispatched before the deadline fired —
	// otherwise the "attempts >= 1" invariant in classifyRetryOutcome would
	// short-circuit and we'd get raw context.DeadlineExceeded, not the
	// exhaustion sentinel.
	if got := hits.Load(); got < 1 {
		t.Errorf("server hits = %d, want >= 1 (retry loop must dispatch at least once before deadline)", got)
	}
	// Wall-clock bound: the chain deadline is 1500ms; add generous slack
	// for jitter, CI, and the classifier's post-loop work. If this fails,
	// the deadline fence isn't firing — retry loop is running to completion.
	if elapsed > 3*time.Second {
		t.Errorf("elapsed = %v, want < 3s (chain deadline should have fenced the loop)", elapsed)
	}
}
