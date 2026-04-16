package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// writeTemp creates a temporary YAML file and returns its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadConfig_SingleBroker(t *testing.T) {
	t.Setenv("TEST_USERNAME", "admin")
	t.Setenv("TEST_PASSWORD", "secret")

	yaml := `
brokers:
  prod-us:
    url: "https://broker-us.example.com:1943"
    auth:
      mode: basic
      username: ${TEST_USERNAME}
      password: ${TEST_PASSWORD}
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Brokers) != 1 {
		t.Fatalf("expected 1 broker, got %d", len(cfg.Brokers))
	}

	broker := cfg.Brokers["prod-us"]
	if broker == nil {
		t.Fatal("expected broker 'prod-us' to exist")
	}
	if broker.URL != "https://broker-us.example.com:1943" {
		t.Errorf("unexpected URL: %s", broker.URL)
	}
	if broker.Auth.Mode != "basic" {
		t.Errorf("unexpected auth method: %s", broker.Auth.Mode)
	}
	if broker.Auth.Username != "admin" {
		t.Errorf("unexpected username: %s", broker.Auth.Username)
	}
	if broker.Auth.Password != "secret" {
		t.Errorf("unexpected password: %s", broker.Auth.Password)
	}
}

func TestLoadConfig_MultiBroker(t *testing.T) {
	t.Setenv("US_USER", "admin-us")
	t.Setenv("US_PASS", "secret-us")
	t.Setenv("EU_USER", "admin-eu")
	t.Setenv("EU_PASS", "secret-eu")

	yaml := `
brokers:
  prod-us:
    url: "https://broker-us.example.com:1943"
    auth:
      mode: basic
      username: ${US_USER}
      password: ${US_PASS}
  prod-eu:
    url: "https://broker-eu.example.com:1943"
    auth:
      mode: basic
      username: ${EU_USER}
      password: ${EU_PASS}
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Brokers) != 2 {
		t.Fatalf("expected 2 brokers, got %d", len(cfg.Brokers))
	}

	if cfg.Brokers["prod-us"].Auth.Username != "admin-us" {
		t.Errorf("prod-us username mismatch: %s", cfg.Brokers["prod-us"].Auth.Username)
	}
	if cfg.Brokers["prod-eu"].Auth.Username != "admin-eu" {
		t.Errorf("prod-eu username mismatch: %s", cfg.Brokers["prod-eu"].Auth.Username)
	}
}

func TestLoadConfig_MissingBrokerURL(t *testing.T) {
	yaml := `
brokers:
  dev:
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing broker URL")
	}
	if !strings.Contains(err.Error(), "url is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_MissingBasicAuthCreds(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    auth:
      mode: basic
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
	if !strings.Contains(err.Error(), "username is required for basic auth") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_MalformedYAML(t *testing.T) {
	_, err := LoadConfig(writeTemp(t, `{{{ not yaml`))
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoadConfig_DefaultsApplied(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    auth:
      mode: basic
      username: admin
      password: secret
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != defaults.DefaultPort {
		t.Errorf("expected port %d, got %d", defaults.DefaultPort, cfg.Port)
	}
	if cfg.SEMP.MaxConcurrentPerBroker != defaults.DefaultMaxConcurrentPerBroker {
		t.Errorf("expected max_concurrent %d, got %d", defaults.DefaultMaxConcurrentPerBroker, cfg.SEMP.MaxConcurrentPerBroker)
	}
	if cfg.SEMP.RequestTimeoutSeconds != defaults.DefaultSEMPRequestTimeoutSeconds {
		t.Errorf("expected request_timeout %d, got %d", defaults.DefaultSEMPRequestTimeoutSeconds, cfg.SEMP.RequestTimeoutSeconds)
	}
}

func TestLoadConfig_InvalidAuthMethod(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    auth:
      mode: oauth
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for unsupported auth mode")
	}
	if !strings.Contains(err.Error(), "unsupported auth mode") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_EnvVarSubstitution(t *testing.T) {
	t.Setenv("BROKER_URL", "https://broker.example.com:1943")
	t.Setenv("BROKER_USER", "admin")
	t.Setenv("BROKER_PASS", "secret")

	yaml := `
brokers:
  prod:
    url: ${BROKER_URL}
    auth:
      mode: basic
      username: ${BROKER_USER}
      password: ${BROKER_PASS}
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	broker := cfg.Brokers["prod"]
	if broker.URL != "https://broker.example.com:1943" {
		t.Errorf("expected URL from env var, got %q", broker.URL)
	}
	if broker.Auth.Username != "admin" {
		t.Errorf("expected username from env var, got %q", broker.Auth.Username)
	}
	if broker.Auth.Password != "secret" {
		t.Errorf("expected password from env var, got %q", broker.Auth.Password)
	}
}

func TestLoadConfig_EnvVarMissing(t *testing.T) {
	yaml := `
brokers:
  prod:
    url: ${MISSING_URL}
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing env var")
	}
	if !strings.Contains(err.Error(), "MISSING_URL") {
		t.Errorf("error should mention the missing var name: %v", err)
	}
}

func TestLoadConfig_MixedHardcodedAndEnvVars(t *testing.T) {
	t.Setenv("MIXED_PASS", "env-secret")

	yaml := `
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: hardcoded-user
      password: ${MIXED_PASS}
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	broker := cfg.Brokers["dev"]
	if broker.Auth.Username != "hardcoded-user" {
		t.Errorf("expected hardcoded username, got %q", broker.Auth.Username)
	}
	if broker.Auth.Password != "env-secret" {
		t.Errorf("expected password from env var, got %q", broker.Auth.Password)
	}
}

func TestLoadConfig_PortOutOfRange(t *testing.T) {
	yaml := `
port: 99999
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for port out of range")
	}
	if !strings.Contains(err.Error(), "port must be between 1 and 65535") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_TLSOnlyCert_ReturnsError(t *testing.T) {
	yaml := `
tls_cert_file: "/tmp/cert.pem"
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error when only tls_cert_file is provided")
	}
	if !strings.Contains(err.Error(), "tls_cert_file and tls_key_file must be provided together") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_TLSBothFields_Valid(t *testing.T) {
	yaml := `
tls_cert_file: "/tmp/cert.pem"
tls_key_file: "/tmp/key.pem"
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TLSCertFile != "/tmp/cert.pem" {
		t.Errorf("expected cert /tmp/cert.pem, got %q", cfg.TLSCertFile)
	}
	if cfg.TLSKeyFile != "/tmp/key.pem" {
		t.Errorf("expected key /tmp/key.pem, got %q", cfg.TLSKeyFile)
	}
}

func TestLoadConfig_EnvOverridePort(t *testing.T) {
	t.Setenv("MCP_SERVER_PORT", "9091")

	yaml := `
port: 8080
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9091 {
		t.Errorf("expected port 9091 (from env var), got %d", cfg.Port)
	}
}

func TestLoadConfig_BearerAuth(t *testing.T) {
	t.Setenv("BEARER_TOKEN", "my-secret-token")

	yaml := `
brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth:
      mode: bearer
      token: ${BEARER_TOKEN}
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	broker := cfg.Brokers["prod"]
	if broker.Auth.Mode != "bearer" {
		t.Errorf("expected mode bearer, got %q", broker.Auth.Mode)
	}
	if broker.Auth.Token != "my-secret-token" {
		t.Errorf("expected token from env var, got %q", broker.Auth.Token)
	}
}

func TestLoadConfig_BearerAuth_MissingToken(t *testing.T) {
	yaml := `
brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth:
      mode: bearer
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing bearer token")
	}
	if !strings.Contains(err.Error(), "token is required for bearer auth") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_MissingAuthMode(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing auth mode")
	}
	if !strings.Contains(err.Error(), "auth.mode is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_AuthModeCaseInsensitive(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: BASIC
      username: admin
      password: secret
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Brokers["dev"].Auth.Mode != "basic" {
		t.Errorf("expected mode normalized to 'basic', got %q", cfg.Brokers["dev"].Auth.Mode)
	}
}

func TestLoadConfig_CollectsAllErrors(t *testing.T) {
	// Multiple issues across server-level and multiple brokers — all should
	// surface in a single error so operators fix them in one pass.
	yaml := `
port: 99999
brokers:
  broker1:
    auth:
      mode: basic
  broker2:
    url: "http://localhost:8080"
    auth:
      mode: oauth
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for multiple validation issues")
	}

	msg := err.Error()
	wantSubstrings := []string{
		"port must be between 1 and 65535",
		`broker "broker1": url is required`,
		`broker "broker1": username is required`,
		`broker "broker1": password is required`,
		`broker "broker2": unsupported auth mode "oauth"`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("combined error missing expected substring %q\nfull error:\n%s", want, msg)
		}
	}
}
