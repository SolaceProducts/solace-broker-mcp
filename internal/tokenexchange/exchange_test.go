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
)

// newTestExchanger builds an Exchanger pointing at the given httptest server
// with a pinned clock. The returned nowFunc can be swapped to advance time.
func newTestExchanger(t *testing.T, serverURL string) *Exchanger {
	t.Helper()
	p := validParams()
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
	gate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("tok", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	input1 := ExchangeInput{SubjectToken: "tok-A", BrokerAlias: "broker-A", Audience: "aud"}
	input2 := ExchangeInput{SubjectToken: "tok-B", BrokerAlias: "broker-B", Audience: "aud"}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.Exchange(context.Background(), input1) }()
	go func() { defer wg.Done(); e.Exchange(context.Background(), input2) }()

	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := callCount.Load(); got != 2 {
		t.Errorf("IdP called %d times, want 2 (different keys should not be deduplicated)", got)
	}
}

// ---------- B04: context cancellation supersedes error ----------

func TestExchange_ContextCancelledReturnsContextError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the HTTP call fails with context.Canceled.
	cancel()

	tok, err := e.Exchange(ctx, validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on context cancellation", tok)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; err = %v", err)
	}
}

func TestExchange_ContextDeadlineExceededReturnsDeadlineError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	time.Sleep(5 * time.Millisecond) // ensure deadline passes
	tok, err := e.Exchange(ctx, validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on deadline exceeded", tok)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false, want true; err = %v", err)
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

// ---------- B07: doExchange wraps buildIdPRequest errors as transport ----------

func TestDoExchange_BuildRequestFailureWrapsAsTransport(t *testing.T) {
	t.Parallel()

	e := &Exchanger{
		tokenURL:         "://malformed-url",
		clientID:         "cid",
		clientAuthMethod: ClientSecretPost,
		clientSecret:     "sec",
		grantType:        GrantTypeTokenExchange,
		audienceParam:    AudienceParamAudience,
		httpClient:       &http.Client{},
		nowFunc:          func() time.Time { return pinnedNow() },
	}

	tok, err := e.doExchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil on build failure", tok)
	}
	if !errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
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

// ---------- B11–B17: logExchangeError ----------

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

// B11: structured attributes always included
func TestLogExchangeError_StructuredAttrsAlwaysPresent(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e := &Exchanger{
		tokenURL: "https://idp.example.com/token",
	}
	input := ExchangeInput{
		BrokerAlias: "my-broker",
		Audience:    "https://aud.example.com",
	}
	elapsed := 150 * time.Millisecond
	testErr := fmt.Errorf("%w: test", ErrExchangeRejected)

	e.logExchangeError(testErr, input, elapsed)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	rec := recs[0]

	wantAttrs := map[string]string{
		"broker_alias":   "my-broker",
		"audience":       "https://aud.example.com",
		"token_endpoint": "https://idp.example.com/token",
	}
	for key, want := range wantAttrs {
		if got, ok := rec.Attrs[key]; !ok {
			t.Errorf("attribute %q missing from log record", key)
		} else if got != want {
			t.Errorf("attribute %q = %q, want %q", key, got, want)
		}
	}

	if _, ok := rec.Attrs["elapsed"]; !ok {
		t.Errorf("attribute \"elapsed\" missing from log record")
	}
	if _, ok := rec.Attrs["error"]; !ok {
		t.Errorf("attribute \"error\" missing from log record")
	}
}

// B11: SubjectToken and ClientSecret are NOT logged
func TestLogExchangeError_SensitiveFieldsNotLogged(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e := &Exchanger{
		tokenURL:     "https://idp.example.com/token",
		clientSecret: "super-secret-value",
	}
	input := ExchangeInput{
		SubjectToken: "sensitive-token-value",
		BrokerAlias:  "b",
		Audience:     "a",
	}

	e.logExchangeError(ErrExchangeTransport, input, time.Second)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}

	for key, val := range recs[0].Attrs {
		if strings.Contains(val, "sensitive-token-value") {
			t.Errorf("attribute %q = %q leaks SubjectToken", key, val)
		}
		if strings.Contains(val, "super-secret-value") {
			t.Errorf("attribute %q = %q leaks clientSecret", key, val)
		}
	}
}

// B12: ErrExchangeRejected → WARN
func TestLogExchangeError_RejectedLogsAtWarn(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e := &Exchanger{tokenURL: "https://idp.example.com/token"}
	e.logExchangeError(
		fmt.Errorf("%w: invalid_grant", ErrExchangeRejected),
		ExchangeInput{BrokerAlias: "b", Audience: "a"},
		time.Second,
	)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	if recs[0].Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN for ErrExchangeRejected", recs[0].Level)
	}
	if !strings.Contains(recs[0].Message, "rejected by IdP") {
		t.Errorf("message = %q, want to contain \"rejected by IdP\"", recs[0].Message)
	}
}

// B13: ErrExchangeTransport → ERROR
func TestLogExchangeError_TransportLogsAtError(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e := &Exchanger{tokenURL: "https://idp.example.com/token"}
	e.logExchangeError(
		fmt.Errorf("%w: connection refused", ErrExchangeTransport),
		ExchangeInput{BrokerAlias: "b", Audience: "a"},
		time.Second,
	)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	if recs[0].Level != slog.LevelError {
		t.Errorf("level = %v, want ERROR for ErrExchangeTransport", recs[0].Level)
	}
	if !strings.Contains(recs[0].Message, "transport failure") {
		t.Errorf("message = %q, want to contain \"transport failure\"", recs[0].Message)
	}
}

// B14: ErrInvalidResponse → ERROR
func TestLogExchangeError_InvalidResponseLogsAtError(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e := &Exchanger{tokenURL: "https://idp.example.com/token"}
	e.logExchangeError(
		fmt.Errorf("%w: bad body", ErrInvalidResponse),
		ExchangeInput{BrokerAlias: "b", Audience: "a"},
		time.Second,
	)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	if recs[0].Level != slog.LevelError {
		t.Errorf("level = %v, want ERROR for ErrInvalidResponse", recs[0].Level)
	}
	if !strings.Contains(recs[0].Message, "invalid IdP response") {
		t.Errorf("message = %q, want to contain \"invalid IdP response\"", recs[0].Message)
	}
}

// B15: unknown error → ERROR "unexpected error"
func TestLogExchangeError_UnknownErrorLogsAtError(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e := &Exchanger{tokenURL: "https://idp.example.com/token"}
	e.logExchangeError(
		errors.New("something completely unknown"),
		ExchangeInput{BrokerAlias: "b", Audience: "a"},
		time.Second,
	)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	if recs[0].Level != slog.LevelError {
		t.Errorf("level = %v, want ERROR for unknown error", recs[0].Level)
	}
	if !strings.Contains(recs[0].Message, "unexpected error") {
		t.Errorf("message = %q, want to contain \"unexpected error\"", recs[0].Message)
	}
}

// B04 (log aspect): context errors are NOT logged
func TestExchange_ContextErrorNotLogged(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	e.nowFunc = func() time.Time { return pinnedNow() }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e.Exchange(ctx, validInput())

	recs := records()
	for _, rec := range recs {
		if strings.Contains(rec.Message, "token exchange") {
			t.Errorf("context cancellation should not produce token exchange log, got: %q", rec.Message)
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
		Scopes:       []string{"read", "write"},
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
		"scope":              "read write",
	}
	for key, want := range checks {
		if got := capturedForm.Get(key); got != want {
			t.Errorf("form[%q] = %q, want %q", key, got, want)
		}
	}

	// BrokerAlias must NOT appear in the request.
	for _, key := range []string{"broker_alias", "broker", "alias"} {
		if capturedForm.Get(key) != "" {
			t.Errorf("form[%q] = %q, BrokerAlias must not appear in IdP request", key, capturedForm.Get(key))
		}
	}
}

// ---------- B07 variant: unknown grant type through Exchange ----------

func TestExchange_UnknownGrantTypeReturnsTransportError(t *testing.T) {
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
		nowFunc:          func() time.Time { return pinnedNow() },
	}

	tok, err := e.Exchange(context.Background(), validInput())

	if tok != nil {
		t.Errorf("tok = %v, want nil for unknown grant type", tok)
	}
	if !errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
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

// ---------- Verify logExchangeError marshals the error string ----------

func TestLogExchangeError_ErrorAttrContainsErrorMessage(t *testing.T) {
	records, restore := captureLogs(t)
	defer restore()

	e := &Exchanger{tokenURL: "https://idp.example.com/token"}
	testErr := fmt.Errorf("%w: specific failure detail", ErrExchangeTransport)

	e.logExchangeError(testErr, ExchangeInput{BrokerAlias: "b", Audience: "a"}, time.Second)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d log records, want 1", len(recs))
	}
	errorAttr := recs[0].Attrs["error"]
	if !strings.Contains(errorAttr, "specific failure detail") {
		t.Errorf("error attr = %q, want it to contain \"specific failure detail\"", errorAttr)
	}
}

// ---------- Verify JSON error response integration ----------

func TestExchange_FourxxWithOAuthError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		wantSentinel error
	}{
		{"401 invalid_token", 401, `{"error":"invalid_token"}`, ErrExchangeRejected},
		{"400 invalid_grant", 400, `{"error":"invalid_grant"}`, ErrExchangeRejected},
		{"403 WAF HTML", 403, `<html>Denied</html>`, ErrExchangeTransport},
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

