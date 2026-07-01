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
		want any
	}{
		{
			name: "basic",
			cfg:  config.AuthConfig{Mode: config.AuthModeBasic, Username: "u", Password: "p"},
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
			a, err := NewAuthenticator(tt.cfg, nil, nil)
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

func TestNewAuthenticator_UnsupportedMode(t *testing.T) {
	a, err := NewAuthenticator(config.AuthConfig{Mode: "invented-mode"}, nil, nil)
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

type fakeJar struct {
	mu      sync.Mutex
	cleared bool
}

func (j *fakeJar) Clear() error {
	j.mu.Lock()
	j.cleared = true
	j.mu.Unlock()
	return nil
}

func TestBasicAuthenticator_HandleAuthFailure_ClearsJar(t *testing.T) {
	jar := &fakeJar{}
	a := NewBasicAuthenticator("admin", "secret", jar)

	retry := a.HandleAuthFailure(context.Background(), nil)
	if !retry {
		t.Error("HandleAuthFailure should return true after clearing jar")
	}
	jar.mu.Lock()
	defer jar.mu.Unlock()
	if !jar.cleared {
		t.Error("expected jar.Clear() to be called")
	}
}

func TestBasicAuthenticator_HandleAuthFailure_NilJar(t *testing.T) {
	a := NewBasicAuthenticator("admin", "secret", nil)
	if got := a.HandleAuthFailure(context.Background(), nil); got {
		t.Error("HandleAuthFailure should return false when jar is nil")
	}
}

func TestBearerAuthenticator_HandleAuthFailure(t *testing.T) {
	a := NewBearerAuthenticator("token123")
	if got := a.HandleAuthFailure(context.Background(), nil); got {
		t.Error("HandleAuthFailure should return false for bearer (static token)")
	}
}

func TestNewAuthenticator_DispatchesOAuth(t *testing.T) {
	cfg := config.AuthConfig{Mode: config.AuthModeOAuth, Audience: "aud", Scopes: []string{"s1"}}
	a, err := NewAuthenticator(cfg, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := a.(*OAuthAuthenticator); !ok {
		t.Errorf("got %T, want *OAuthAuthenticator", a)
	}
}

// Compile-time assertions that all types satisfy the Authenticator
// interface — protects against an accidental signature drift.
var (
	_ Authenticator = (*BasicAuthenticator)(nil)
	_ Authenticator = (*BearerAuthenticator)(nil)
	_ Authenticator = (*OAuthAuthenticator)(nil)
)
