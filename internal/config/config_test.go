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

package config

import (
	"bytes"
	"log/slog"
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
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadConfig_SingleBroker(t *testing.T) {
	t.Setenv("TEST_USERNAME", "admin")
	t.Setenv("TEST_PASSWORD", "secret")

	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
		return
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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

// ${VAR} references inside YAML comments must NOT trigger env var lookups —
// the line is inert at parse time and has no effect on the loaded config.
func TestLoadConfig_EnvVarInWholeLineComment(t *testing.T) {
	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
brokers:
  prod:
    url: "https://broker.example.com:1943"
    auth:
      mode: basic
      username: admin
      password: secret
      # token: ${UNSET_BEARER_TOKEN}  # alternate bearer-auth example
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("commented-out ${UNSET_BEARER_TOKEN} must not fail load: %v", err)
	}
	if cfg.Brokers["prod"].Auth.Token != "" {
		t.Errorf("expected token unset for commented-out line, got %q", cfg.Brokers["prod"].Auth.Token)
	}
}

// Trailing inline comments must also be skipped, while ${VAR} in the live
// portion of the same line is still substituted.
func TestLoadConfig_EnvVarInInlineComment(t *testing.T) {
	t.Setenv("INLINE_PASSWORD", "live-secret")

	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
brokers:
  prod:
    url: "https://broker.example.com:1943"
    auth:
      mode: basic
      username: admin
      password: ${INLINE_PASSWORD}  # was ${UNSET_OLD_PASSWORD}
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("inline comment ${UNSET_OLD_PASSWORD} must not fail load: %v", err)
	}
	if cfg.Brokers["prod"].Auth.Password != "live-secret" {
		t.Errorf("expected password substituted to %q, got %q", "live-secret", cfg.Brokers["prod"].Auth.Password)
	}
}

// A # character that appears inside a quoted string is part of the value, not
// a comment marker. ${VAR} on the same line must still be substituted and the
// # preserved in the parsed value.
func TestLoadConfig_HashInsideQuotedValue(t *testing.T) {
	t.Setenv("PWD_HEAD", "live")

	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
brokers:
  prod:
    url: "https://broker.example.com:1943"
    auth:
      mode: basic
      username: admin
      password: "${PWD_HEAD}#tail"
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Brokers["prod"].Auth.Password; got != "live#tail" {
		t.Errorf("expected password %q, got %q", "live#tail", got)
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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

func TestLoadConfig_RejectsHTTPBrokerInProductionMode(t *testing.T) {
	yaml := `
development_mode: false
brokers:
  prod-us:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: secret
client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "solace-mcp"
  resource_url: "https://mcp.example.com"
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for http:// broker URL in production mode")
	}
	if !strings.Contains(err.Error(), "must be https") {
		t.Errorf("error should steer toward https as the fix: %v", err)
	}
	if !strings.Contains(err.Error(), "prod-us") {
		t.Errorf("error should mention the broker alias: %v", err)
	}
}

func TestLoadConfig_RejectsHTTPClientAuthInProductionMode(t *testing.T) {
	yaml := `
development_mode: false
brokers:
  prod-us:
    url: "https://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: secret
client_auth:
  mode: oauth
  issuer: "http://idp.example.com"
  audience: "solace-mcp"
  resource_url: "http://mcp.example.com"
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for http:// client_auth URLs in production mode")
	}
	if !strings.Contains(err.Error(), "client_auth.issuer") {
		t.Errorf("error should mention client_auth.issuer: %v", err)
	}
	if !strings.Contains(err.Error(), "client_auth.resource_url") {
		t.Errorf("error should mention client_auth.resource_url: %v", err)
	}
}

func TestLoadConfig_AllowsHTTPBrokerInDevelopmentMode(t *testing.T) {
	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("http:// should be allowed in development_mode: %v", err)
	}
}

func TestLoadConfig_URLMissingHost(t *testing.T) {
	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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

// TestLoadConfig_RejectsBrokerURLWithCredentials_AndRedactsThemInError pins
// two related behaviours: (a) an inline user:pass URL in a broker entry is
// rejected at validation, and (b) the password never appears in the error
// message — even though the offending URL is part of the error context.
// Together they prevent the disclosure path from validation errors through
// slog.Error("failed to load config", err) at startup.
func TestLoadConfig_RejectsBrokerURLWithCredentials_AndRedactsThemInError(t *testing.T) {
	const inlinePassword = "hunter2-super-secret-must-not-leak"
	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
brokers:
  prod-us:
    url: "https://admin:` + inlinePassword + `@broker.example.com:1943"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for broker URL with embedded credentials")
	}

	// The full error string is what main() will log via slog.Error — that
	// is the disclosure surface. Assert the password is NOT present anywhere
	// in it (case-sensitive: the literal would survive %q escaping).
	if strings.Contains(err.Error(), inlinePassword) {
		t.Errorf("password leaked through validation error\nfull error: %v", err)
	}
	// Operator-friendly: the error should point at the right place to fix.
	if !strings.Contains(err.Error(), "auth block") {
		t.Errorf("error should steer operator to the auth block, got: %v", err)
	}
	if !strings.Contains(err.Error(), "prod-us") {
		t.Errorf("error should name the broker alias, got: %v", err)
	}
}

// TestBrokerConfig_LogValue_RedactsURLCredentials pins that the LogValuer
// itself strips userinfo from the URL — independent of whether validation
// has run. validateBrokerURL already rejects credentialed URLs, but
// LogValue should not assume that: any future code path that logs a
// BrokerConfig before validation (e.g., a debug line in LoadConfig itself)
// would otherwise echo the credential to slog.
func TestBrokerConfig_LogValue_RedactsURLCredentials(t *testing.T) {
	const inlinePassword = "hunter2-super-secret-must-not-leak"
	b := BrokerConfig{
		URL: "https://admin:" + inlinePassword + "@broker.example.com:1943",
		Auth: AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "irrelevant-to-this-test",
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("broker config", slog.Any("broker", b))

	out := buf.String()
	if strings.Contains(out, inlinePassword) {
		t.Errorf("password leaked through BrokerConfig.LogValue output:\n%s", out)
	}
	// Sanity: the host must still be present so logs remain useful.
	if !strings.Contains(out, "broker.example.com") {
		t.Errorf("host stripped from LogValue output (sanitization too aggressive):\n%s", out)
	}
}

// TestClientAuthConfig_LogValue_RedactsURLCredentials applies the same
// LogValue-level defense in depth to ClientAuthConfig. Issuer and
// ResourceURL both go through validateBrokerURL, but LogValue should not
// rely on that ordering.
func TestClientAuthConfig_LogValue_RedactsURLCredentials(t *testing.T) {
	const inlinePassword = "hunter2-issuer-secret-must-not-leak"
	c := ClientAuthConfig{
		Issuer:      "https://admin:" + inlinePassword + "@idp.example.com/realms/main",
		Audience:    "mcp",
		ResourceURL: "https://admin:" + inlinePassword + "@mcp.example.com/mcp",
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("client auth config", slog.Any("client_auth", c))

	out := buf.String()
	if strings.Contains(out, inlinePassword) {
		t.Errorf("password leaked through ClientAuthConfig.LogValue output:\n%s", out)
	}
	if !strings.Contains(out, "idp.example.com") {
		t.Errorf("issuer host stripped from LogValue output (sanitization too aggressive):\n%s", out)
	}
	if !strings.Contains(out, "mcp.example.com") {
		t.Errorf("resource_url host stripped from LogValue output (sanitization too aggressive):\n%s", out)
	}
}

func TestLoadConfig_LogLevel_Default(t *testing.T) {
	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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
development_mode: true
client_auth:
  mode: static
  dev_token: test
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

func TestValidate_BrokerErrorsAreSorted(t *testing.T) {
	// Broker map iteration is non-deterministic; sorted aliases ensure the
	// joined error string always lists errors in alphabetical alias order.
	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
brokers:
  zebra:
    auth:
      mode: basic
  alpha:
    auth:
      mode: basic
  monkey:
    auth:
      mode: basic
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	msg := err.Error()
	alphaIdx := strings.Index(msg, `"alpha"`)
	monkeyIdx := strings.Index(msg, `"monkey"`)
	zebraIdx := strings.Index(msg, `"zebra"`)
	if alphaIdx < 0 || monkeyIdx < 0 || zebraIdx < 0 {
		t.Fatalf("expected all three aliases in error, got: %s", msg)
	}
	if alphaIdx >= monkeyIdx || monkeyIdx >= zebraIdx {
		t.Errorf("expected errors in alphabetical order (alpha < monkey < zebra), got positions %d %d %d\nfull error:\n%s",
			alphaIdx, monkeyIdx, zebraIdx, msg)
	}
}

func TestLoad_UsesConfigFileEnv(t *testing.T) {
	yaml := `
development_mode: true
client_auth:
  mode: static
  dev_token: test
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

// captureSlog redirects the default slog logger to a buffer for the duration
// of the test and returns the buffer. The original logger is restored via
// t.Cleanup.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	orig := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func TestLoadEnvFile_StripsMatchedQuotes(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := `
DOUBLE_QUOTED="bar"
SINGLE_QUOTED='qux'
UNBALANCED="unbalanced'
UNQUOTED=plain
`
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatalf("writing .env: %v", err)
	}
	t.Setenv("ENV_FILE", envFile)
	// Unset keys so loadEnvFile sets them.
	for _, k := range []string{"DOUBLE_QUOTED", "SINGLE_QUOTED", "UNBALANCED", "UNQUOTED"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	loadEnvFile(filepath.Join(dir, "config.yaml"))

	cases := []struct{ key, want string }{
		{"DOUBLE_QUOTED", "bar"},
		{"SINGLE_QUOTED", "qux"},
		{"UNBALANCED", "\"unbalanced'"},
		{"UNQUOTED", "plain"},
	}
	for _, c := range cases {
		if got := os.Getenv(c.key); got != c.want {
			t.Errorf("%s: got %q, want %q", c.key, got, c.want)
		}
	}
}

func TestLoadEnvFile_LogsWarningOnUnreadable(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("FOO=bar\n"), 0o600); err != nil {
		t.Fatalf("writing .env: %v", err)
	}
	if err := os.Chmod(envFile, 0o000); err != nil {
		t.Fatalf("chmod .env: %v", err)
	}
	t.Cleanup(func() { os.Chmod(envFile, 0o600) }) //nolint:errcheck

	t.Setenv("ENV_FILE", envFile)
	buf := captureSlog(t)

	loadEnvFile(filepath.Join(dir, "config.yaml"))

	logged := buf.String()
	if !strings.Contains(logged, "WARN") || !strings.Contains(logged, "env file unreadable") {
		t.Errorf("expected WARN 'env file unreadable' in log output, got: %s", logged)
	}
}

func TestLoadEnvFile_MissingFileIsSilent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENV_FILE", filepath.Join(dir, "nonexistent.env"))
	buf := captureSlog(t)

	loadEnvFile(filepath.Join(dir, "config.yaml"))

	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("expected no WARN for missing .env, got: %s", buf.String())
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

func TestIsProductionMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{AuthModeDisabled, false},
		{AuthModeStatic, false},
		{AuthModeOAuth, true},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := &ServerConfig{ClientAuth: ClientAuthConfig{Mode: tt.mode}}
			if got := cfg.IsProductionMode(); got != tt.want {
				t.Errorf("IsProductionMode() for mode=%q: got %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}
