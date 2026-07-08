package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.test/SEMP", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return req
}

// stubJar is a no-op CookieJarClearer for tests that construct a
// BasicAuthenticator but don't exercise the jar-clearing path. Tests
// that need to observe Clear() calls or error paths should use their
// own recording stub.
type stubJar struct{}

func (stubJar) Clear() error { return nil }

func TestBasicAuthenticator_AddAuth(t *testing.T) {
	const (
		user = "admin"
		pass = "s3cret"
	)
	a := NewBasicAuthenticator(user, pass, stubJar{})
	req := newReq(t)

	if err := a.AddAuth(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req.Header.Get("Authorization")
	encoded, ok := strings.CutPrefix(got, "Basic ")
	if !ok {
		t.Fatalf("Authorization header = %q, want Basic-prefixed", got)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("Basic credential is not valid base64: %v", err)
	}
	if want := user + ":" + pass; string(decoded) != want {
		t.Errorf("decoded Basic credential = %q, want %q", decoded, want)
	}
}

func TestBearerAuthenticator_AddAuth(t *testing.T) {
	a := NewBearerAuthenticator("abc123")
	req := newReq(t)

	if err := a.AddAuth(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer abc123" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer abc123")
	}
}

// TestAuthenticator_ConcurrentAddAuth_NoFieldMutation exercises concurrent
// safety: many goroutines call AddAuth concurrently, each with its own
// ctx and req. The Authenticator must produce correct headers on every
// call and must not mutate its own fields. Run with -race.
func TestAuthenticator_ConcurrentAddAuth_NoFieldMutation(t *testing.T) {
	const goroutines = 16

	t.Run("basic", func(t *testing.T) {
		a := NewBasicAuthenticator("admin", "s3cret", stubJar{})
		wantUser, wantPass := a.username, a.password
		wantCreds := wantUser + ":" + wantPass

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req := newReq(t)
				if err := a.AddAuth(ctx, req); err != nil {
					t.Errorf("AddAuth: %v", err)
					return
				}
				encoded, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Basic ")
				if !ok {
					t.Errorf("missing Basic-prefixed Authorization header")
					return
				}
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || string(decoded) != wantCreds {
					t.Errorf("decoded credential = %q (err=%v), want %q", decoded, err, wantCreds)
				}
			}()
		}
		wg.Wait()

		if a.username != wantUser || a.password != wantPass {
			t.Errorf("Authenticator fields mutated: username=%q->%q, password=%q->%q",
				wantUser, a.username, wantPass, a.password)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		a := NewBearerAuthenticator("abc123")
		wantToken := a.token

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req := newReq(t)
				if err := a.AddAuth(ctx, req); err != nil {
					t.Errorf("AddAuth: %v", err)
					return
				}
				if got := req.Header.Get("Authorization"); got != "Bearer abc123" {
					t.Errorf("Authorization = %q, want %q", got, "Bearer abc123")
				}
			}()
		}
		wg.Wait()

		if a.token != wantToken {
			t.Errorf("Authenticator field mutated: token=%q->%q", wantToken, a.token)
		}
	})
}

// Compile-time assertions that all types satisfy the Authenticator
// interface — protects against an accidental signature drift.
var (
	_ Authenticator = (*BasicAuthenticator)(nil)
	_ Authenticator = (*BearerAuthenticator)(nil)
	_ Authenticator = (*OAuthAuthenticator)(nil)
)
