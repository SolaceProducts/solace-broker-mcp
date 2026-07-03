package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// stubJar satisfies CookieJarClearer for tests that need a non-nil jar
// but never exercise clearing.
type stubJar struct{}

func (stubJar) Clear() error { return nil }

func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.test/SEMP", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return req
}

func TestBasicAuthenticator_AddAuth(t *testing.T) {
	const (
		user = "admin"
		pass = "s3cret"
	)
	a := NewBasicAuthenticator(user, pass, nil)
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

func TestNewAuthenticator_DispatchesByMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.AuthConfig
		jar  CookieJarClearer
		want any
	}{
		{
			name: "basic",
			cfg:  config.AuthConfig{Mode: config.AuthModeBasic, Username: "u", Password: "p"},
			jar:  &stubJar{},
			want: (*BasicAuthenticator)(nil),
		},
		{
			name: "bearer",
			cfg:  config.AuthConfig{Mode: config.AuthModeBearer, Token: "t"},
			want: (*BearerAuthenticator)(nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := NewAuthenticator(tt.cfg, tt.jar)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			switch tt.want.(type) {
			case *BasicAuthenticator:
				if _, ok := a.(*BasicAuthenticator); !ok {
					t.Errorf("got %T, want *BasicAuthenticator", a)
				}
			case *BearerAuthenticator:
				if _, ok := a.(*BearerAuthenticator); !ok {
					t.Errorf("got %T, want *BearerAuthenticator", a)
				}
			}
		})
	}
}

func TestNewAuthenticator_BasicAuth_RequiresJar(t *testing.T) {
	cfg := config.AuthConfig{Mode: config.AuthModeBasic, Username: "u", Password: "p"}
	a, err := NewAuthenticator(cfg, nil)
	if err == nil {
		t.Fatal("expected error for basic auth with nil jar, got nil")
	}
	if a != nil {
		t.Errorf("expected nil authenticator on error, got %T", a)
	}
	if !strings.Contains(err.Error(), "cookie jar") {
		t.Errorf("error = %v, want it to mention 'cookie jar'", err)
	}
}

func TestNewAuthenticator_UnsupportedMode(t *testing.T) {
	a, err := NewAuthenticator(config.AuthConfig{Mode: "invented-mode"}, nil)
	if err == nil {
		t.Fatal("expected error for unsupported mode, got nil")
	}
	if a != nil {
		t.Errorf("expected nil authenticator on error, got %T", a)
	}
	if !strings.Contains(err.Error(), "unsupported auth mode") {
		t.Errorf("error = %v, want it to mention 'unsupported auth mode'", err)
	}
}

// TestAuthenticator_ConcurrentAddAuth_NoFieldMutation exercises Decision
// 13 Test 5: many goroutines call AddAuth concurrently, each with its
// own ctx and req. The Authenticator must produce correct headers on
// every call and must not mutate its own fields. Run with -race.
func TestAuthenticator_ConcurrentAddAuth_NoFieldMutation(t *testing.T) {
	// 16 goroutines is enough — the -race detector flags races on any
	// concurrent unsynchronized access, regardless of pressure. Higher
	// counts would be performative, not informative.
	const goroutines = 16

	t.Run("basic", func(t *testing.T) {
		a := NewBasicAuthenticator("admin", "s3cret", nil)
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

// Compile-time assertions that both types satisfy the Authenticator
// interface — protects against an accidental signature drift.
var (
	_ Authenticator = (*BasicAuthenticator)(nil)
	_ Authenticator = (*BearerAuthenticator)(nil)
)
