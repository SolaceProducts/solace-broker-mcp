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
	t.Setenv("TEST_SINGLE_USERNAME", "admin")
	t.Setenv("TEST_SINGLE_PASSWORD", "secret")

	yaml := `
brokers:
  prod-us:
    url: "https://broker-us.example.com:1943"
    env_prefix: "TEST_SINGLE"
    auth:
      method: basic
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
	if broker.Auth.Method != "basic" {
		t.Errorf("unexpected auth method: %s", broker.Auth.Method)
	}
	if broker.Auth.Username != "admin" {
		t.Errorf("unexpected username: %s", broker.Auth.Username)
	}
	if broker.Auth.Password != "secret" {
		t.Errorf("unexpected password: %s", broker.Auth.Password)
	}
}

func TestLoadConfig_MultiBroker(t *testing.T) {
	t.Setenv("TEST_US_USERNAME", "admin-us")
	t.Setenv("TEST_US_PASSWORD", "secret-us")
	t.Setenv("TEST_EU_USERNAME", "admin-eu")
	t.Setenv("TEST_EU_PASSWORD", "secret-eu")

	yaml := `
brokers:
  prod-us:
    url: "https://broker-us.example.com:1943"
    env_prefix: "TEST_US"
    auth:
      method: basic
  prod-eu:
    url: "https://broker-eu.example.com:1943"
    env_prefix: "TEST_EU"
    auth:
      method: basic
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
    env_prefix: "TEST_DEV"
    auth:
      method: basic
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
	// env_prefix set but env vars not set — should error about missing env var
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    env_prefix: "TEST_NOCREDS"
    auth:
      method: basic
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing credentials env vars")
	}
	if !strings.Contains(err.Error(), "TEST_NOCREDS_USERNAME") {
		t.Errorf("error should mention the missing env var: %v", err)
	}
}

func TestLoadConfig_MalformedYAML(t *testing.T) {
	_, err := LoadConfig(writeTemp(t, `{{{ not yaml`))
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoadConfig_DefaultsApplied(t *testing.T) {
	t.Setenv("TEST_DEF_USERNAME", "admin")
	t.Setenv("TEST_DEF_PASSWORD", "secret")

	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    env_prefix: "TEST_DEF"
    auth:
      method: basic
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
	t.Setenv("TEST_OAUTH_USERNAME", "admin")
	t.Setenv("TEST_OAUTH_PASSWORD", "secret")

	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    env_prefix: "TEST_OAUTH"
    auth:
      method: oauth
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for unsupported auth method")
	}
	if !strings.Contains(err.Error(), "unsupported auth method") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_EnvPrefixMissing(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    auth:
      method: basic
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing env_prefix")
	}
	if !strings.Contains(err.Error(), "env_prefix is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_EnvPrefixInvalidChars(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    env_prefix: "prod-us"
    auth:
      method: basic
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid env_prefix characters")
	}
	if !strings.Contains(err.Error(), "env_prefix must contain only uppercase letters") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_EnvPrefixValid(t *testing.T) {
	t.Setenv("PROD_US_USERNAME", "prod-admin")
	t.Setenv("PROD_US_PASSWORD", "prod-secret")

	yaml := `
brokers:
  prod-us:
    url: "https://broker.example.com:1943"
    env_prefix: "PROD_US"
    auth:
      method: basic
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	broker := cfg.Brokers["prod-us"]
	if broker.Auth.Username != "prod-admin" {
		t.Errorf("expected username 'prod-admin', got %q", broker.Auth.Username)
	}
	if broker.Auth.Password != "prod-secret" {
		t.Errorf("expected password 'prod-secret', got %q", broker.Auth.Password)
	}
}

func TestLoadConfig_EnvPrefixUsernameNotSet(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    env_prefix: "MISSING"
    auth:
      method: basic
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing username env var")
	}
	if !strings.Contains(err.Error(), "MISSING_USERNAME") {
		t.Errorf("error should mention the env var name: %v", err)
	}
}

func TestLoadConfig_EnvPrefixPasswordNotSet(t *testing.T) {
	t.Setenv("PARTIAL_USERNAME", "admin")

	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    env_prefix: "PARTIAL"
    auth:
      method: basic
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing password env var")
	}
	if !strings.Contains(err.Error(), "PARTIAL_PASSWORD") {
		t.Errorf("error should mention the env var name: %v", err)
	}
}

func TestLoadConfig_CredentialsNotInYAML(t *testing.T) {
	t.Setenv("TEST_CRED_USERNAME", "env-user")
	t.Setenv("TEST_CRED_PASSWORD", "env-pass")

	// YAML includes username/password but they should be ignored — env vars win
	yaml := `
brokers:
  dev:
    url: "https://broker.example.com:1943"
    env_prefix: "TEST_CRED"
    auth:
      method: basic
      username: yaml-user
      password: yaml-pass
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	broker := cfg.Brokers["dev"]
	if broker.Auth.Username != "env-user" {
		t.Errorf("expected username from env var 'env-user', got %q", broker.Auth.Username)
	}
	if broker.Auth.Password != "env-pass" {
		t.Errorf("expected password from env var 'env-pass', got %q", broker.Auth.Password)
	}
}

func TestLoadConfig_PortOutOfRange(t *testing.T) {
	t.Setenv("TEST_PORT_USERNAME", "admin")
	t.Setenv("TEST_PORT_PASSWORD", "secret")

	yaml := `
port: 99999
brokers:
  dev:
    url: "http://localhost:8080"
    env_prefix: "TEST_PORT"
    auth:
      method: basic
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
	t.Setenv("TEST_TLS_USERNAME", "admin")
	t.Setenv("TEST_TLS_PASSWORD", "secret")

	yaml := `
tls_cert_file: "/tmp/cert.pem"
brokers:
  dev:
    url: "http://localhost:8080"
    env_prefix: "TEST_TLS"
    auth:
      method: basic
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
	t.Setenv("TEST_TLSOK_USERNAME", "admin")
	t.Setenv("TEST_TLSOK_PASSWORD", "secret")

	yaml := `
tls_cert_file: "/tmp/cert.pem"
tls_key_file: "/tmp/key.pem"
brokers:
  dev:
    url: "http://localhost:8080"
    env_prefix: "TEST_TLSOK"
    auth:
      method: basic
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
	t.Setenv("TEST_ENVPORT_USERNAME", "admin")
	t.Setenv("TEST_ENVPORT_PASSWORD", "secret")
	t.Setenv("MCP_SERVER_PORT", "9091")

	yaml := `
port: 8080
brokers:
  dev:
    url: "http://localhost:8080"
    env_prefix: "TEST_ENVPORT"
    auth:
      method: basic
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9091 {
		t.Errorf("expected port 9091 (from env var), got %d", cfg.Port)
	}
}
