package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if cfg.SEMP.RequestTimeoutDuration != defaults.DefaultSEMPRequestTimeoutDuration {
		t.Errorf("expected request_timeout_duration %s, got %s", defaults.DefaultSEMPRequestTimeoutDuration, cfg.SEMP.RequestTimeoutDuration)
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

func TestLoadConfig_InvalidURLScheme(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "ftp://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for non-http/https URL scheme")
	}
	if !strings.Contains(err.Error(), "url scheme must be http or https") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_URLMissingHost(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "http://"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for URL with no host")
	}
	if !strings.Contains(err.Error(), "url must include a host") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_URLEmpty_ReportsRequired(t *testing.T) {
	// Empty URL is handled by the "url is required" branch, NOT the
	// structure-validation branch — verifying we don't double-report.
	yaml := `
brokers:
  dev:
    url: ""
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "url is required") {
		t.Errorf("expected 'url is required' error, got: %v", err)
	}
	if strings.Contains(err.Error(), "url scheme must be") {
		t.Errorf("empty URL should not produce 'url scheme' error — double-reporting: %v", err)
	}
}

func TestLoadConfig_LogLevel_Default(t *testing.T) {
	yaml := `
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
	if cfg.LogLevel != defaults.DefaultLogLevel {
		t.Errorf("expected default log_level %q, got %q", defaults.DefaultLogLevel, cfg.LogLevel)
	}
}

func TestLoadConfig_LogLevel_ValidValues(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			yaml := `
log_level: ` + level + `
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
				t.Fatalf("unexpected error for level %q: %v", level, err)
			}
			if cfg.LogLevel != level {
				t.Errorf("expected log_level %q, got %q", level, cfg.LogLevel)
			}
		})
	}
}

func TestLoadConfig_LogLevel_CaseInsensitive(t *testing.T) {
	yaml := `
log_level: DEBUG
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
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log_level normalized to 'debug', got %q", cfg.LogLevel)
	}
}

func TestLoadConfig_LogLevel_Invalid(t *testing.T) {
	yaml := `
log_level: verbose
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
		t.Fatal("expected error for invalid log_level")
	}
	if !strings.Contains(err.Error(), "log_level") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_RateLimit_Defaults(t *testing.T) {
	yaml := `
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
	if cfg.SEMP.RequestMinInterval == nil || *cfg.SEMP.RequestMinInterval != defaults.DefaultRequestMinInterval {
		t.Errorf("expected default request_min_interval %s, got %v", defaults.DefaultRequestMinInterval, cfg.SEMP.RequestMinInterval)
	}
	if cfg.SEMP.Retries == nil || *cfg.SEMP.Retries != defaults.DefaultRetries {
		t.Errorf("expected default retries %d, got %v", defaults.DefaultRetries, cfg.SEMP.Retries)
	}
	if cfg.SEMP.RetryMinInterval != defaults.DefaultRetryMinInterval {
		t.Errorf("expected default retry_min_interval %s, got %s", defaults.DefaultRetryMinInterval, cfg.SEMP.RetryMinInterval)
	}
	if cfg.SEMP.RetryMaxInterval != defaults.DefaultRetryMaxInterval {
		t.Errorf("expected default retry_max_interval %s, got %s", defaults.DefaultRetryMaxInterval, cfg.SEMP.RetryMaxInterval)
	}
}

func TestLoadConfig_RateLimit_ValidValues(t *testing.T) {
	yaml := `
semp:
  request_min_interval: 50ms
  retries: 5
  retry_min_interval: 1s
  retry_max_interval: 10s
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
	if cfg.SEMP.RequestMinInterval == nil || *cfg.SEMP.RequestMinInterval != 50*time.Millisecond {
		t.Errorf("expected request_min_interval 50ms, got %v", cfg.SEMP.RequestMinInterval)
	}
	if cfg.SEMP.Retries == nil || *cfg.SEMP.Retries != 5 {
		t.Errorf("expected retries 5, got %v", cfg.SEMP.Retries)
	}
	if cfg.SEMP.RetryMinInterval != time.Second {
		t.Errorf("expected retry_min_interval 1s, got %s", cfg.SEMP.RetryMinInterval)
	}
	if cfg.SEMP.RetryMaxInterval != 10*time.Second {
		t.Errorf("expected retry_max_interval 10s, got %s", cfg.SEMP.RetryMaxInterval)
	}
}

func TestLoadConfig_RateLimit_ExplicitZeroHonored(t *testing.T) {
	// Zero is a legitimate operator value for retries (no retries) and
	// request_min_interval (no rate limit per Solace Terraform provider).
	// Verify applyDefaults does NOT clobber operator-set zero with the default.
	// This is the reason both fields are pointer types -- without pointers,
	// "0 in YAML" is indistinguishable from "omitted from YAML".
	yaml := `
semp:
  request_min_interval: 0s
  retries: 0
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
	if cfg.SEMP.RequestMinInterval == nil || *cfg.SEMP.RequestMinInterval != 0 {
		t.Errorf("expected operator-set request_min_interval=0 to be honored, got %v", cfg.SEMP.RequestMinInterval)
	}
	if cfg.SEMP.Retries == nil || *cfg.SEMP.Retries != 0 {
		t.Errorf("expected operator-set retries=0 to be honored, got %v", cfg.SEMP.Retries)
	}
}

func TestLoadConfig_RateLimit_NegativeRetries(t *testing.T) {
	yaml := `
semp:
  retries: -1
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
		t.Fatal("expected error for negative retries")
	}
	if !strings.Contains(err.Error(), "semp.retries must be >= 0") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_RateLimit_NegativeRequestMinInterval(t *testing.T) {
	yaml := `
semp:
  request_min_interval: -10ms
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
		t.Fatal("expected error for negative request_min_interval")
	}
	if !strings.Contains(err.Error(), "semp.request_min_interval must be >= 0") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_RateLimit_MaxSmallerThanMin(t *testing.T) {
	yaml := `
semp:
  retry_min_interval: 30s
  retry_max_interval: 3s
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
		t.Fatal("expected error when retry_max_interval < retry_min_interval")
	}
	if !strings.Contains(err.Error(), "retry_max_interval") || !strings.Contains(err.Error(), "must be >= semp.retry_min_interval") {
		t.Errorf("unexpected error: %v", err)
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

func TestLoad_UsesConfigFileEnv(t *testing.T) {
	yaml := `
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	path := writeTemp(t, yaml)
	t.Setenv("CONFIG_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Brokers["dev"] == nil {
		t.Error("expected broker 'dev' to be loaded")
	}
}

func TestLoad_ConfigFileEnvMissing_ReturnsError(t *testing.T) {
	// When CONFIG_FILE points at a non-existent file, Load MUST NOT silently
	// fall back to the system/local paths. The operator explicitly pointed
	// at THIS file; a silent fallback would mask the mistake.
	t.Setenv("CONFIG_FILE", "/does/not/exist.yaml")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when CONFIG_FILE points to missing file")
	}
	// The error should reference the file we tried -- not a "no config found"
	// error that implies fallback happened.
	if !strings.Contains(err.Error(), "reading config file") {
		t.Errorf("expected 'reading config file' error from strict CONFIG_FILE handling, got: %v", err)
	}
}

func TestLoad_ConfigFileEnvCorrupt_ReturnsError(t *testing.T) {
	// CONFIG_FILE points to a file that exists but has broken YAML. Strict:
	// error bubbles up, no fallback.
	path := writeTemp(t, `{{{ not yaml`)
	t.Setenv("CONFIG_FILE", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for malformed YAML in CONFIG_FILE path")
	}
	if !strings.Contains(err.Error(), "parsing config YAML") {
		t.Errorf("expected YAML parse error, got: %v", err)
	}
}

func TestLoad_NoConfigFileEnv_NoSystemOrLocal_ReturnsError(t *testing.T) {
	// No CONFIG_FILE set, no /etc/mcp-server/config.yaml (unlikely on dev),
	// and we chdir to an empty temp directory so broker-config.yaml doesn't
	// exist in CWD. Load should error with a message listing what was tried.
	t.Setenv("CONFIG_FILE", "")

	// Switch CWD to an empty temp dir so the local fallback path has nothing
	// to find. Restore at the end.
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	emptyDir := t.TempDir()
	if err := os.Chdir(emptyDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWD)
	})

	_, err = Load()
	if err == nil {
		t.Skip("Load succeeded unexpectedly -- likely /etc/mcp-server/config.yaml exists on this machine; skipping")
	}
	if !strings.Contains(err.Error(), "no config file found") {
		t.Errorf("expected 'no config file found' error, got: %v", err)
	}
}
