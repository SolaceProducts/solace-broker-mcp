package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://example.test/SEMP", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return req
}

func TestAddAuth_Basic(t *testing.T) {
	req := newReq(t)
	cfg := config.AuthConfig{
		Mode:     config.AuthModeBasic,
		Username: "admin",
		Password: "s3cret",
	}
	if err := AddAuth(context.Background(), req, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Basic ") {
		t.Errorf("Authorization header = %q, want Basic-prefixed", got)
	}
	// Sanity: SetBasicAuth base64-encodes "user:pass". We don't recompute
	// the encoding here — just confirm the credentials are not in the
	// header verbatim.
	if strings.Contains(got, "admin") || strings.Contains(got, "s3cret") {
		t.Errorf("Authorization header leaks raw credentials: %q", got)
	}
}

func TestAddAuth_Bearer(t *testing.T) {
	req := newReq(t)
	cfg := config.AuthConfig{
		Mode:  config.AuthModeBearer,
		Token: "abc123",
	}
	if err := AddAuth(context.Background(), req, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req.Header.Get("Authorization")
	if got != "Bearer abc123" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer abc123")
	}
}

func TestAddAuth_UnsupportedMode(t *testing.T) {
	req := newReq(t)
	cfg := config.AuthConfig{Mode: "invented-mode"}

	err := AddAuth(context.Background(), req, cfg)
	if err == nil {
		t.Fatal("expected error for unsupported mode, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported auth mode") {
		t.Errorf("error = %v, want it to mention 'unsupported auth mode'", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header should be unset on unsupported mode; got %q", got)
	}
}
