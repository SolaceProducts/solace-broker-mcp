package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	internalauth "github.com/SolaceDev/solace-broker-mcp/internal/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/tokenexchange"
)

type fakeExchanger struct {
	returnToken    *tokenexchange.Token
	returnErr      error
	mu             sync.Mutex
	calls          []tokenexchange.ExchangeInput
	invalidateCalls []tokenexchange.DeduplicationKeyInput
}

func (f *fakeExchanger) Exchange(_ context.Context, input tokenexchange.ExchangeInput) (*tokenexchange.Token, error) {
	f.mu.Lock()
	f.calls = append(f.calls, input)
	f.mu.Unlock()
	return f.returnToken, f.returnErr
}

func (f *fakeExchanger) Invalidate(_ context.Context, input tokenexchange.DeduplicationKeyInput) {
	f.mu.Lock()
	f.invalidateCalls = append(f.invalidateCalls, input)
	f.mu.Unlock()
}

// ctxWithSubjectToken runs the InjectRawSubjectToken middleware to place
// a bearer token on a context, exactly as the production middleware does.
func ctxWithSubjectToken(t *testing.T, token string) context.Context {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	var captured context.Context
	handler := internalauth.InjectRawSubjectToken(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r.Context()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return captured
}

func TestOAuthAuthenticator_AddAuth(t *testing.T) {
	exchg := &fakeExchanger{
		returnToken: &tokenexchange.Token{
			Value:     "exchanged-token-123",
			ExpiresAt: time.Now().Add(5 * time.Minute),
		},
	}
	a := NewOAuthAuthenticator(exchg, "broker-audience", "my-broker")

	ctx := ctxWithSubjectToken(t, "agent-jwt-token")
	req := newReq(t)

	if err := a.AddAuth(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := req.Header.Get("Authorization")
	if got != "Bearer exchanged-token-123" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer exchanged-token-123")
	}

	exchg.mu.Lock()
	defer exchg.mu.Unlock()
	if len(exchg.calls) != 1 {
		t.Fatalf("expected 1 Exchange call, got %d", len(exchg.calls))
	}
	call := exchg.calls[0]
	if call.SubjectToken != "agent-jwt-token" {
		t.Errorf("SubjectToken = %q, want %q", call.SubjectToken, "agent-jwt-token")
	}
	if call.BrokerAlias != "my-broker" {
		t.Errorf("BrokerAlias = %q, want %q", call.BrokerAlias, "my-broker")
	}
	if call.Audience != "broker-audience" {
		t.Errorf("Audience = %q, want %q", call.Audience, "broker-audience")
	}
}

func TestOAuthAuthenticator_AddAuth_NoSubjectToken(t *testing.T) {
	exchg := &fakeExchanger{
		returnToken: &tokenexchange.Token{Value: "should-not-reach"},
	}
	a := NewOAuthAuthenticator(exchg, "aud", "b")

	req := newReq(t)
	err := a.AddAuth(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no subject token on context")
	}
	if !strings.Contains(err.Error(), "no subject token") {
		t.Errorf("error = %v, want mention of 'no subject token'", err)
	}
	if !errors.Is(err, tokenexchange.ErrExchangeMissingSubject) {
		t.Errorf("error should wrap ErrExchangeMissingSubject, got %v", err)
	}

	exchg.mu.Lock()
	defer exchg.mu.Unlock()
	if len(exchg.calls) != 0 {
		t.Errorf("Exchange should not be called when no subject token, got %d calls", len(exchg.calls))
	}
}

func TestOAuthAuthenticator_AddAuth_ExchangeError(t *testing.T) {
	exchg := &fakeExchanger{
		returnErr: tokenexchange.ErrExchangeRejected,
	}
	a := NewOAuthAuthenticator(exchg, "aud", "b")

	ctx := ctxWithSubjectToken(t, "agent-token")
	req := newReq(t)
	err := a.AddAuth(ctx, req)
	if err == nil {
		t.Fatal("expected error when exchange fails")
	}
	if !strings.Contains(err.Error(), "token exchange failed") {
		t.Errorf("error = %v, want mention of 'token exchange failed'", err)
	}
}

func TestOAuthAuthenticator_HandleAuthFailure_NoSubjectToken(t *testing.T) {
	a := NewOAuthAuthenticator(&fakeExchanger{}, "aud", "b")
	if a.HandleAuthFailure(context.Background(), nil) {
		t.Error("expected false when no subject token on context")
	}
}

// TestOAuthAuthenticator_HandleAuthFailure_WithSubjectToken pins the
// current contract: eviction happens (Invalidate is called with the
// right key so the *next* request pulls a fresh token), but the return
// is false so the resilience layer does not retry in-flight. Retrying
// in-flight would replay the same *http.Request with the stale
// Authorization header — see the docstring on HandleAuthFailure and
// SOL-151624 for the PrepareRetry follow-up that flips this to true.
func TestOAuthAuthenticator_HandleAuthFailure_WithSubjectToken(t *testing.T) {
	exchg := &fakeExchanger{}
	a := NewOAuthAuthenticator(exchg, "aud", "my-broker")

	ctx := ctxWithSubjectToken(t, "agent-jwt")
	if a.HandleAuthFailure(ctx, nil) {
		t.Error("expected false — in-flight retry is deferred to SOL-151624")
	}

	exchg.mu.Lock()
	defer exchg.mu.Unlock()
	if len(exchg.invalidateCalls) != 1 {
		t.Fatalf("expected 1 Invalidate call, got %d", len(exchg.invalidateCalls))
	}
	if exchg.invalidateCalls[0].BrokerAlias != "my-broker" {
		t.Errorf("broker alias = %q, want %q", exchg.invalidateCalls[0].BrokerAlias, "my-broker")
	}
	if exchg.invalidateCalls[0].SubjectToken != "agent-jwt" {
		t.Errorf("subject token = %q, want %q", exchg.invalidateCalls[0].SubjectToken, "agent-jwt")
	}
}

// TestOAuthAuthenticator_ConcurrentAddAuth exercises concurrent safety:
// 100 goroutines call AddAuth with distinct contexts. The
// OAuthAuthenticator must produce correct headers on every call without
// mutating its own fields. Run with -race.
func TestOAuthAuthenticator_ConcurrentAddAuth(t *testing.T) {
	const goroutines = 100

	exchg := &fakeExchanger{
		returnToken: &tokenexchange.Token{
			Value:     "concurrent-token",
			ExpiresAt: time.Now().Add(5 * time.Minute),
		},
	}
	a := NewOAuthAuthenticator(exchg, "broker-aud", "b")

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ctx := ctxWithSubjectToken(t, "agent-token")
			req := newReq(t)
			if err := a.AddAuth(ctx, req); err != nil {
				t.Errorf("AddAuth: %v", err)
				return
			}
			if got := req.Header.Get("Authorization"); got != "Bearer concurrent-token" {
				t.Errorf("Authorization = %q, want %q", got, "Bearer concurrent-token")
			}
		}()
	}
	wg.Wait()

	exchg.mu.Lock()
	defer exchg.mu.Unlock()
	if len(exchg.calls) != goroutines {
		t.Errorf("expected %d Exchange calls, got %d", goroutines, len(exchg.calls))
	}
}
