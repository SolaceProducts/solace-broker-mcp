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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
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
mcp_client_auth:
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

	if len(cfg.brokers) != 1 {
		t.Fatalf("expected 1 broker, got %d", len(cfg.brokers))
	}

	broker := cfg.brokers["prod-us"]
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
mcp_client_auth:
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

	if len(cfg.brokers) != 2 {
		t.Fatalf("expected 2 brokers, got %d", len(cfg.brokers))
	}

	if cfg.brokers["prod-us"].Auth.Username != "admin-us" {
		t.Errorf("prod-us username mismatch: %s", cfg.brokers["prod-us"].Auth.Username)
	}
	if cfg.brokers["prod-eu"].Auth.Username != "admin-eu" {
		t.Errorf("prod-eu username mismatch: %s", cfg.brokers["prod-eu"].Auth.Username)
	}
}

func TestLoadConfig_MissingBrokerURL(t *testing.T) {
	yaml := `
mcp_client_auth:
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
mcp_client_auth:
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

func TestLoadConfig_DisabledAuth(t *testing.T) {
	// mcp_client_auth.mode: disabled is a valid, dev-only profile: no client auth
	// is enforced and no further mcp_client_auth fields are required. The broker's
	// own auth block is still validated as usual.
	yaml := `
mcp_client_auth:
  mode: disabled
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
		t.Fatalf("unexpected error for disabled auth mode: %v", err)
	}
	if cfg.MCPClientAuth.Mode != AuthModeDisabled {
		t.Errorf("expected mcp_client_auth.mode %q, got %q", AuthModeDisabled, cfg.MCPClientAuth.Mode)
	}
	// disabled is a dev profile, not production.
	if cfg.IsProductionMode() {
		t.Errorf("disabled mode should not be production mode")
	}
	// Broker auth block is still parsed and validated normally.
	broker := cfg.brokers["dev"]
	if broker == nil {
		t.Fatal("expected broker 'dev' to be loaded")
	}
	if broker.Auth.Username != "admin" || broker.Auth.Password != "secret" {
		t.Errorf("unexpected broker credentials: %q/%q", broker.Auth.Username, broker.Auth.Password)
	}
}

func TestLoadConfig_DisabledAuth_AllowsHTTPBroker(t *testing.T) {
	// disabled is a dev-only mode, so http:// broker URLs are permitted
	yaml := `
mcp_client_auth:
  mode: disabled
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	if _, err := LoadConfig(writeTemp(t, yaml)); err != nil {
		t.Fatalf("http:// broker URL should be allowed under mode: disabled, got: %v", err)
	}
}

func TestLoadConfig_MalformedYAML(t *testing.T) {
	_, err := LoadConfig(writeTemp(t, `{{{ not yaml`))
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoadConfig_RejectsUnknownFields(t *testing.T) {
	// Unknown YAML keys are a foot-gun: a typo like `developmnet_mode` or
	// `max_concurrent_per_brokr` was previously accepted silently, leaving
	// the operator's override as a no-op. KnownFields(true) on the YAML
	// decoder forces typos to surface at config load.
	cases := []struct {
		name        string
		yaml        string
		wantInError string
	}{
		{
			name: "top-level typo",
			yaml: `
developmnet_mode: true
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`,
			wantInError: "developmnet_mode",
		},
		{
			name: "nested broker typo",
			yaml: `
brokers:
  dev:
    url: "http://localhost:8080"
    insecure_skip_verfy: true
    auth:
      mode: basic
      username: admin
      password: secret
`,
			wantInError: "insecure_skip_verfy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTemp(t, tc.yaml))
			if err == nil {
				t.Fatal("expected error for unknown YAML field")
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error should name the offending field %q, got: %v", tc.wantInError, err)
			}
		})
	}
}

func TestLoadConfig_DefaultsApplied(t *testing.T) {
	yaml := `
mcp_client_auth:
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

// TestLoadConfig_EnableWriteTools_DefaultOff pins the secure-by-default
// behavior: omitting enable_write_tools from the YAML must leave it false
// so destructive tools are not registered unless explicitly enabled.
func TestLoadConfig_EnableWriteTools_DefaultOff(t *testing.T) {
	yaml := `
mcp_client_auth:
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
	if cfg.EnableWriteTools {
		t.Error("EnableWriteTools must default to false when omitted")
	}
}

// TestLoadConfig_EnableWriteTools_ExplicitTrue verifies the flag reads
// through from YAML when set.
func TestLoadConfig_EnableWriteTools_ExplicitTrue(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
enable_write_tools: true
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
	if !cfg.EnableWriteTools {
		t.Error("EnableWriteTools must be true when explicitly set in YAML")
	}
}

func TestLoadConfig_InvalidAuthMethod(t *testing.T) {
	// "kerberos" is not in validAuthModes (basic, bearer, oauth) — the
	// validator must reject it with an "unsupported auth mode" error.
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
brokers:
  dev:
    url: "https://broker.example.com:1943"
    auth:
      mode: kerberos
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
mcp_client_auth:
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

	broker := cfg.brokers["prod"]
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
mcp_client_auth:
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
mcp_client_auth:
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
	if cfg.brokers["prod"].Auth.Token != "" {
		t.Errorf("expected token unset for commented-out line, got %q", cfg.brokers["prod"].Auth.Token)
	}
}

// Trailing inline comments must also be skipped, while ${VAR} in the live
// portion of the same line is still substituted.
func TestLoadConfig_EnvVarInInlineComment(t *testing.T) {
	t.Setenv("INLINE_PASSWORD", "live-secret")

	yaml := `
mcp_client_auth:
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
	if cfg.brokers["prod"].Auth.Password != "live-secret" {
		t.Errorf("expected password substituted to %q, got %q", "live-secret", cfg.brokers["prod"].Auth.Password)
	}
}

// A # character that appears inside a quoted string is part of the value, not
// a comment marker. ${VAR} on the same line must still be substituted and the
// # preserved in the parsed value.
func TestLoadConfig_HashInsideQuotedValue(t *testing.T) {
	t.Setenv("PWD_HEAD", "live")

	yaml := `
mcp_client_auth:
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
	if got := cfg.brokers["prod"].Auth.Password; got != "live#tail" {
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
mcp_client_auth:
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
mcp_client_auth:
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

// oauthTLSMatrixBroker is the broker block shared by the OAuth-listener-TLS
// matrix tests below. https:// is required because mode: oauth enforces TLS on
// broker URLs (validateBrokerURL).
const oauthTLSMatrixBroker = `
brokers:
  prod:
    url: "https://broker.example.com:943"
    auth:
      mode: basic
      username: admin
      password: secret
`

// OAuth mode with neither TLS certs nor the tls_terminated_upstream
// acknowledgment must fail: the listener would carry client bearer tokens in
// cleartext while validating as production. The error must name both
// remediation paths.
func TestLoadConfig_OAuthNoTLSNoAck_ReturnsError(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
` + oauthTLSMatrixBroker
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for oauth mode with no TLS and no acknowledgment")
	}
	for _, want := range []string{"tls_cert_file", "tls_key_file", "tls_terminated_upstream"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q remediation path, got: %v", want, err)
		}
	}
}

// OAuth mode with tls_terminated_upstream: true and no certs is valid — the
// operator has acknowledged TLS is terminated upstream.
func TestLoadConfig_OAuthTLSTerminatedUpstream_Valid(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false

tls_terminated_upstream: true
` + oauthTLSMatrixBroker
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.TLSTerminatedUpstream {
		t.Error("expected TLSTerminatedUpstream to be true")
	}
	if !cfg.OAuthPlaintextListenerAcknowledged() {
		t.Error("expected OAuthPlaintextListenerAcknowledged() to be true for oauth+ack+no-certs")
	}
}

// OAuth mode with TLS certs behaves as before: valid, and the plaintext-listener
// predicate is false (the server terminates TLS itself).
func TestLoadConfig_OAuthWithTLSCerts_Valid(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false

tls_cert_file: "/tmp/cert.pem"
tls_key_file: "/tmp/key.pem"
` + oauthTLSMatrixBroker
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OAuthPlaintextListenerAcknowledged() {
		t.Error("expected OAuthPlaintextListenerAcknowledged() to be false when TLS certs are set")
	}
}

// OAuth mode with BOTH TLS certs and tls_terminated_upstream: true is valid, and
// certs win: the server terminates TLS itself, so the plaintext-listener
// predicate stays false and no WARN fires. This documents the precedence for an
// operator migrating from upstream termination to direct TLS who leaves the ack
// flag set.
func TestLoadConfig_OAuthCertsAndAck_CertsWin(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false

tls_cert_file: "/tmp/cert.pem"
tls_key_file: "/tmp/key.pem"
tls_terminated_upstream: true
` + oauthTLSMatrixBroker
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.TLSTerminatedUpstream {
		t.Error("expected TLSTerminatedUpstream to be stored as true")
	}
	if cfg.OAuthPlaintextListenerAcknowledged() {
		t.Error("certs must win: OAuthPlaintextListenerAcknowledged() should be false when TLS certs are set even if the ack is also set")
	}
}

// A whitespace-only cert path (e.g. an unresolved ${VAR} or a secret mounted with
// a trailing newline) must count as "no TLS": applyDefaults trims it, so oauth
// with no acknowledgment still fails cleanly at LoadConfig rather than passing
// validation and blowing up later inside ListenAndServeTLS.
func TestLoadConfig_OAuthWhitespaceCertPath_TreatedAsNoTLS(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
tls_cert_file: "   "
tls_key_file: "   "
` + oauthTLSMatrixBroker
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error: a whitespace-only cert path must count as no TLS under oauth with no acknowledgment")
	}
	if !strings.Contains(err.Error(), "tls_terminated_upstream") {
		t.Errorf("error should name the acknowledgment remediation path, got: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil config on validation failure, got: %+v", cfg)
	}
}

// Non-OAuth modes are unaffected by the OAuth listener-TLS rule: static with no
// certs still validates, and tls_terminated_upstream is ignored outside oauth
// (the field's documented "honored only in OAuth mode" contract) — setting it
// under static neither errors nor flips the plaintext-listener predicate.
func TestLoadConfig_StaticNoTLS_UnaffectedByOAuthRule(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
tls_terminated_upstream: true
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
		t.Fatalf("static mode with no TLS must remain valid: %v", err)
	}
	if cfg.OAuthPlaintextListenerAcknowledged() {
		t.Error("OAuthPlaintextListenerAcknowledged() must be false outside oauth mode")
	}
}

func TestLoadConfig_EnvOverridePort(t *testing.T) {
	t.Setenv("MCP_SERVER_PORT", "9091")

	yaml := `
mcp_client_auth:
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
mcp_client_auth:
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

	broker := cfg.brokers["prod"]
	if broker.Auth.Mode != "bearer" {
		t.Errorf("expected mode bearer, got %q", broker.Auth.Mode)
	}
	if broker.Auth.Token != "my-secret-token" {
		t.Errorf("expected token from env var, got %q", broker.Auth.Token)
	}
}

func TestLoadConfig_BearerAuth_MissingToken(t *testing.T) {
	yaml := `
mcp_client_auth:
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

// TestLoadConfig_CredentialsTrimmedBeforeEmptyCheck pins the invariant that
// validateBroker rejects whitespace-only credentials at startup rather than
// letting them through to fail every request at runtime.
func TestLoadConfig_CredentialsTrimmedBeforeEmptyCheck(t *testing.T) {
	const clientAuthBlock = `
mcp_client_auth:
  mode: static
  dev_token: test
`
	cases := []struct {
		name             string
		yaml             string
		wantErrSubstring string
	}{
		{
			name: "bearer token whitespace-only",
			yaml: clientAuthBlock + `
brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth:
      mode: bearer
      token: "   "
`,
			wantErrSubstring: "token is required for bearer auth",
		},
		{
			name: "basic username whitespace-only",
			yaml: clientAuthBlock + `
brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth:
      mode: basic
      username: "   "
      password: "real-password"
`,
			wantErrSubstring: "username is required for basic auth",
		},
		{
			name: "basic password whitespace-only",
			yaml: clientAuthBlock + `
brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth:
      mode: basic
      username: "admin"
      password: "   "
`,
			wantErrSubstring: "password is required for basic auth",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTemp(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstring)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstring)
			}
		})
	}
}

// TestLoadConfig_WhitespaceTrimmedAcrossRequiredStrings pins the two
// non-obvious whitespace-trim rules that PR #110 review caught as missed
// from the original commit 1f34e6f hardening:
//
//  1. client_secret_basic / client_secret_post: the secret inside the
//     discriminated-union sub-block must be trimmed before the empty
//     check, matching the broker basic/bearer credential rule.
//  2. per-broker auth.audience (oauth mode): the field is OPTIONAL, so
//     the check uses a conditional "set-but-whitespace" pattern instead
//     of bare "trim == empty" — a future refactor that collapses it to
//     the simple form would incorrectly reject absent-audience configs.
//
// Other operator-supplied required strings (dev_token, issuer, broker.url,
// idp_token_endpoint, etc.) also have the trim rule but are not separately
// tested here — those checks reduce to "does strings.TrimSpace work on the
// stdlib", which is not our logic to test. Adding sub-tests for them would
// be regression coverage for code we don't own.
func TestLoadConfig_WhitespaceTrimmedAcrossRequiredStrings(t *testing.T) {
	const oauthHop1 = `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: aud
  resource_url: "https://mcp.example.com/mcp"
`
	const brokerOAuthHeader = `
broker_oauth:
  idp_token_endpoint: "https://idp.example.com/oauth/token"
  mcp_server_client_id: id
`
	const grantAndAud = `
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
`

	cases := []struct {
		name             string
		yaml             string
		wantErrSubstring string
	}{
		{
			name: "client_secret_basic.secret whitespace-only",
			yaml: `
mcp_client_auth:
  mode: static
  dev_token: test
` + brokerOAuthHeader + `  mcp_server_client_auth:
    client_secret_basic:
      secret: "   "
` + grantAndAud + `brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth: { mode: basic, username: u, password: p }
`,
			wantErrSubstring: "broker_oauth.mcp_server_client_auth.client_secret_basic.secret is required",
		},
		{
			name: "client_secret_post.secret whitespace-only",
			yaml: `
mcp_client_auth:
  mode: static
  dev_token: test
` + brokerOAuthHeader + `  mcp_server_client_auth:
    client_secret_post:
      secret: "   "
` + grantAndAud + `brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth: { mode: basic, username: u, password: p }
`,
			wantErrSubstring: "broker_oauth.mcp_server_client_auth.client_secret_post.secret is required",
		},
		{
			name: "per-broker auth.audience whitespace-only (optional-but-set semantics)",
			yaml: oauthHop1 + brokerOAuthHeader + `  mcp_server_client_auth:
    client_secret_basic:
      secret: s
` + grantAndAud + `brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth:
      mode: oauth
      audience: "   "
`,
			wantErrSubstring: `auth.audience is empty or whitespace-only`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTemp(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstring)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstring)
			}
		})
	}
}

func TestLoadConfig_MissingAuthMode(t *testing.T) {
	yaml := `
mcp_client_auth:
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
mcp_client_auth:
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
	if cfg.brokers["dev"].Auth.Mode != "basic" {
		t.Errorf("expected mode normalized to 'basic', got %q", cfg.brokers["dev"].Auth.Mode)
	}
}

func TestLoadConfig_InvalidURLScheme(t *testing.T) {
	yaml := `
mcp_client_auth:
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
brokers:
  prod-us:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: secret
mcp_client_auth:
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

func TestLoadConfig_RejectsInsecureSkipVerifyInProductionModeWithoutOptIn(t *testing.T) {
	// Production (oauth) mode enforces https:// but must not silently accept a
	// disabled certificate check: that exposes the broker admin credential to a
	// MITM. Without the explicit allow_insecure_broker_tls opt-in, validation
	// fails and names both the broker and the opt-in (mirrors the
	// allow_remote_unauthenticated guard).
	yaml := `
brokers:
  prod-us:
    url: "https://broker.example.com:8080"
    insecure_skip_verify: true
    auth:
      mode: basic
      username: admin
      password: secret
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "solace-mcp"
  resource_url: "https://mcp.example.com"
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for insecure_skip_verify=true in production mode without opt-in")
	}
	if !strings.Contains(err.Error(), "allow_insecure_broker_tls") {
		t.Errorf("error should name the allow_insecure_broker_tls opt-in, got: %v", err)
	}
	if !strings.Contains(err.Error(), "prod-us") {
		t.Errorf("error should identify the offending broker, got: %v", err)
	}
}

func TestLoadConfig_NormalizesModeBeforeBrokerChecks(t *testing.T) {
	// Regression: mcp_client_auth.mode is normalized to lowercase inside
	// validate(). The insecure-broker-TLS refusal keys off IsProductionMode(),
	// which compares against the lowercase "oauth" constant. If normalization
	// ran AFTER the broker loop, a non-lowercase "OAuth" would not register as
	// production and would silently bypass the refusal — a security regression.
	// This asserts a mixed-case mode is treated exactly like lowercase "oauth".
	yaml := `
brokers:
  prod-us:
    url: "https://broker.example.com:8080"
    insecure_skip_verify: true
    auth:
      mode: basic
      username: admin
      password: secret
mcp_client_auth:
  mode: OAuth
  issuer: "https://idp.example.com"
  audience: "solace-mcp"
  resource_url: "https://mcp.example.com"
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for insecure_skip_verify=true with mixed-case mode: OAuth and no opt-in")
	}
	if !strings.Contains(err.Error(), "allow_insecure_broker_tls") {
		t.Errorf("error should name the allow_insecure_broker_tls opt-in, got: %v", err)
	}
	if !strings.Contains(err.Error(), "prod-us") {
		t.Errorf("error should identify the offending broker, got: %v", err)
	}
}

func TestLoadConfig_AllowsInsecureSkipVerifyInProductionModeWithOptIn(t *testing.T) {
	// With the explicit opt-in the operator has accepted the risk; validation
	// passes but the startup WARN still fires so the insecure setting stays
	// visible in triage logs.
	buf := captureSlog(t)
	yaml := `
brokers:
  prod-us:
    url: "https://broker.example.com:8080"
    insecure_skip_verify: true
    auth:
      mode: basic
      username: admin
      password: secret
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "solace-mcp"
  resource_url: "https://mcp.example.com"
  tool_authorization:
    enabled: false

allow_insecure_broker_tls: true
tls_terminated_upstream: true
`
	if _, err := LoadConfig(writeTemp(t, yaml)); err != nil {
		t.Fatalf("insecure_skip_verify with allow_insecure_broker_tls should pass: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "INSECURE: TLS verification disabled for broker") {
		t.Errorf("expected INSECURE TLS WARN when opt-in accepted, got: %s", out)
	}
	if !strings.Contains(out, "broker=prod-us") {
		t.Errorf("expected WARN to identify broker via broker=<alias>, got: %s", out)
	}
}

func TestLoadConfig_NoWarnOnInsecureSkipVerifyInDevelopmentMode(t *testing.T) {
	// In development_mode the insecure flag is the expected default for
	// local broker setups, so no startup WARN should fire.
	buf := captureSlog(t)
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
brokers:
  dev:
    url: "https://broker.example.com:8080"
    insecure_skip_verify: true
    auth:
      mode: basic
      username: admin
      password: secret
`
	if _, err := LoadConfig(writeTemp(t, yaml)); err != nil {
		t.Errorf("insecure_skip_verify: true should be allowed in development_mode: %v", err)
	}
	if strings.Contains(buf.String(), "INSECURE: TLS verification disabled for broker") {
		t.Errorf("did not expect INSECURE TLS WARN in dev mode, got: %s", buf.String())
	}
}

func TestLoadConfig_RejectsHTTPClientAuthInProductionMode(t *testing.T) {
	yaml := `
brokers:
  prod-us:
    url: "https://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: secret
mcp_client_auth:
  mode: oauth
  issuer: "http://idp.example.com"
  audience: "solace-mcp"
  resource_url: "http://mcp.example.com"
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for http:// mcp_client_auth URLs in production mode")
	}
	if !strings.Contains(err.Error(), "mcp_client_auth.issuer") {
		t.Errorf("error should mention mcp_client_auth.issuer: %v", err)
	}
	if !strings.Contains(err.Error(), "mcp_client_auth.resource_url") {
		t.Errorf("error should mention mcp_client_auth.resource_url: %v", err)
	}
}

func TestLoadConfig_AllowsHTTPBrokerInStaticMode(t *testing.T) {
	yaml := `
mcp_client_auth:
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
		t.Fatalf("http:// should be allowed under mcp_client_auth.mode: static: %v", err)
	}
}

func TestLoadConfig_URLMissingHost(t *testing.T) {
	yaml := `
mcp_client_auth:
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
mcp_client_auth:
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
mcp_client_auth:
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

// TestSanitizeURLString pins the exported contract: userinfo stripped for
// any valid http/https URL, everything else replaced with
// "<unparseable url>" rather than echoed back — including the schemeless
// "user:pass@host:port" form url.Parse treats as opaque, which a naive
// User != nil check alone would miss.
func TestSanitizeURLString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no userinfo", "https://broker.example.com:1943", "https://broker.example.com:1943"},
		{"username only", "https://admin@broker.example.com", "https://broker.example.com"},
		{"username and password", "https://admin:hunter2@broker.example.com:1943", "https://broker.example.com:1943"},
		{"schemeless is opaque, rejected by the scheme/host check", "admin:hunter2@broker.example.com:943", "<unparseable url>"},
		{"scheme-relative", "//admin:hunter2@broker.example.com", "<unparseable url>"},
		{"non-http(s) scheme", "ftp://admin:hunter2@broker.example.com", "<unparseable url>"},
		{"no host", "https://", "<unparseable url>"},
		{"unparseable", "https://admin:hunter2@[::1", "<unparseable url>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeURLString(tc.in); got != tc.want {
				t.Errorf("SanitizeURLString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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

// TestMCPClientAuthConfig_LogValue_RedactsURLCredentials applies the same
// LogValue-level defense in depth to MCPClientAuthConfig. Issuer and
// ResourceURL both go through validateBrokerURL, but LogValue should not
// rely on that ordering.
func TestMCPClientAuthConfig_LogValue_RedactsURLCredentials(t *testing.T) {
	const inlinePassword = "hunter2-issuer-secret-must-not-leak"
	c := MCPClientAuthConfig{
		Mode:        AuthModeOAuth,
		Issuer:      "https://admin:" + inlinePassword + "@idp.example.com/realms/main",
		Audience:    "mcp",
		ResourceURL: "https://admin:" + inlinePassword + "@mcp.example.com/mcp",
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("client auth config", slog.Any("mcp_client_auth", c))

	out := buf.String()
	if strings.Contains(out, inlinePassword) {
		t.Errorf("password leaked through MCPClientAuthConfig.LogValue output:\n%s", out)
	}
	if !strings.Contains(out, "idp.example.com") {
		t.Errorf("issuer host stripped from LogValue output (sanitization too aggressive):\n%s", out)
	}
	if !strings.Contains(out, "mcp.example.com") {
		t.Errorf("resource_url host stripped from LogValue output (sanitization too aggressive):\n%s", out)
	}
	if !strings.Contains(out, `"mode":"oauth"`) {
		t.Errorf("LogValue output should include mode key, got: %s", out)
	}
}

// TestAuthConfig_LogValue_RedactsCredentials pins that AuthConfig.LogValue
// exposes only mode — username, password, and token must never appear in
// rendered slog output. Uses sentinel strings so a leak is a deterministic
// substring hit rather than a guess at what "a secret looks like". SOL-150757.
func TestAuthConfig_LogValue_RedactsCredentials(t *testing.T) {
	const (
		sentUser = "SENTINEL_USERNAME_MUST_NOT_LEAK"
		sentPass = "SENTINEL_PASSWORD_MUST_NOT_LEAK"
		sentTok  = "SENTINEL_TOKEN_MUST_NOT_LEAK"
	)

	cases := []struct {
		name string
		mode string
	}{
		{"basic", "basic"},
		{"bearer", "bearer"},
		{"oauth", "oauth"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := AuthConfig{
				Mode:     tc.mode,
				Username: sentUser,
				Password: sentPass,
				Token:    sentTok,
			}
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			logger.Info("auth config", slog.Any("auth", a))

			out := buf.String()
			for _, s := range []string{sentUser, sentPass, sentTok} {
				if strings.Contains(out, s) {
					t.Errorf("credential sentinel %q leaked through AuthConfig.LogValue:\n%s", s, out)
				}
			}
			if !strings.Contains(out, `"mode":"`+tc.mode+`"`) {
				t.Errorf("LogValue output should include mode=%q, got: %s", tc.mode, out)
			}
		})
	}
}

func TestLoadConfig_LogLevel_Default(t *testing.T) {
	yaml := `
mcp_client_auth:
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
mcp_client_auth:
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
mcp_client_auth:
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
mcp_client_auth:
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
mcp_client_auth:
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
mcp_client_auth:
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

func TestLoadConfig_MaxConcurrentPerBroker_OutOfRange(t *testing.T) {
	// Per-broker semaphore size drives both the in-memory semaphore and the
	// HTTP transport's idle-connection pool (MaxIdleConnsPerHost +
	// MaxIdleConns × 2). Unbounded above lets a misconfig or compromised
	// config over-allocate and OOM the process.
	cases := []struct {
		name        string
		value       string
		wantInError string
	}{
		{
			name:        "negative",
			value:       "-1",
			wantInError: "semp.max_concurrent_per_broker",
		},
		{
			name:        "above ceiling",
			value:       "1000000",
			wantInError: "semp.max_concurrent_per_broker",
		},
		{
			name:        "just above ceiling",
			value:       "1025",
			wantInError: "semp.max_concurrent_per_broker",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
development_mode: true
mcp_client_auth:
  mode: static
  dev_token: test
semp:
  max_concurrent_per_broker: ` + tc.value + `
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
				t.Fatalf("expected error for max_concurrent_per_broker=%s", tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error should name the offending field, got: %v", err)
			}
		})
	}

	// Boundary: 1024 must pass (inclusive upper bound). Locks in the
	// inclusive semantic against future drift.
	t.Run("at ceiling passes", func(t *testing.T) {
		yaml := `
development_mode: true
mcp_client_auth:
  mode: static
  dev_token: test
semp:
  max_concurrent_per_broker: 1024
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
		if _, err := LoadConfig(writeTemp(t, yaml)); err != nil {
			t.Errorf("max_concurrent_per_broker: 1024 should pass (inclusive upper bound), got: %v", err)
		}
	})
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
      mode: kerberos
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
		`broker "broker2": unsupported auth mode "kerberos"`,
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
mcp_client_auth:
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
mcp_client_auth:
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
	if cfg.brokers["dev"] == nil {
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

func TestLoadConfig_AuthMode_Missing(t *testing.T) {
	// mcp_client_auth.mode is required — omitting it must fail validation with a
	// specific, actionable error. This is the central anti-confusion guarantee
	// of the refactor: there is no value of "I didn't write anything" that
	// resolves to no-auth.
	yaml := `
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
		t.Fatal("expected error when mcp_client_auth.mode is missing")
	}
	if !strings.Contains(err.Error(), "mcp_client_auth.mode is required") {
		t.Errorf("error should state mcp_client_auth.mode is required, got: %v", err)
	}
	for _, m := range []string{"disabled", "static", "oauth"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error should list valid mode %q, got: %v", m, err)
		}
	}
}

func TestLoadConfig_AuthMode_Invalid(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: production
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
		t.Fatal("expected error for unknown mcp_client_auth.mode value")
	}
	if !strings.Contains(err.Error(), `mcp_client_auth.mode "production" is invalid`) {
		t.Errorf("error should quote the bad value, got: %v", err)
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
			cfg := &ServerConfig{MCPClientAuth: MCPClientAuthConfig{Mode: tt.mode}}
			if got := cfg.IsProductionMode(); got != tt.want {
				t.Errorf("IsProductionMode() for mode=%q: got %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestLoadConfig_AuthMode_CaseInsensitive(t *testing.T) {
	// Mode value normalization matches what validate() does for log_level
	// and broker auth.mode — operators get a forgiving config experience.
	cases := []string{"DISABLED", "Static", "OAuth"}
	for _, mode := range cases {
		t.Run(mode, func(t *testing.T) {
			extra := ""
			switch strings.ToLower(mode) {
			case "static":
				extra = "  dev_token: test"
			case "oauth":
				extra = `  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false

tls_terminated_upstream: true`
			}
			yaml := `
mcp_client_auth:
  mode: ` + mode + `
` + extra + `
brokers:
  dev:
    url: "https://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
			cfg, err := LoadConfig(writeTemp(t, yaml))
			if err != nil {
				t.Fatalf("expected case-insensitive accept for mode=%q, got: %v", mode, err)
			}
			if cfg.MCPClientAuth.Mode != strings.ToLower(mode) {
				t.Errorf("expected normalized mode %q, got %q", strings.ToLower(mode), cfg.MCPClientAuth.Mode)
			}
		})
	}
}

func TestLoadConfig_AuthMode_Static_NoToken(t *testing.T) {
	// mode: static without a dev_token is exactly the SOL-149921 vulnerability
	// in its new form — must be rejected with a specific error.
	yaml := `
mcp_client_auth:
  mode: static
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
		t.Fatal("expected error when mcp_client_auth.mode is static and dev_token is empty")
	}
	if !strings.Contains(err.Error(), "mcp_client_auth.dev_token is required") {
		t.Errorf("error should name dev_token, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"static"`) {
		t.Errorf("error should quote mode value, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_MissingIssuer(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
brokers:
  dev:
    url: "https://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "mcp_client_auth.issuer is required") {
		t.Fatalf("expected mcp_client_auth.issuer required error, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_MissingAudience(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  resource_url: "https://mcp.example.com/mcp"
brokers:
  dev:
    url: "https://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "mcp_client_auth.audience is required") {
		t.Fatalf("expected mcp_client_auth.audience required error, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_MissingResourceURL(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
brokers:
  dev:
    url: "https://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "mcp_client_auth.resource_url is required") {
		t.Fatalf("expected mcp_client_auth.resource_url required error, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_HTTPIssuer(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "http://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
brokers:
  dev:
    url: "https://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "scheme must be https") {
		t.Fatalf("expected https-required error for issuer under mode: oauth, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_HTTPBroker(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
brokers:
  dev:
    url: "http://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "scheme must be https") {
		t.Fatalf("expected https-required error for broker URL under mode: oauth, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_Static_HTTPBroker(t *testing.T) {
	// http:// broker URLs are explicitly allowed under mode: static (dev profile).
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
brokers:
  dev:
    url: "http://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("http:// broker URL should be allowed under mode: static, got: %v", err)
	}
}

func TestLoadConfig_DevelopmentModeDeprecationWarning(t *testing.T) {
	// Legacy development_mode YAML field must still parse so operators with
	// old configs reach the helpful mcp_client_auth.mode error — not a generic
	// "unknown field" YAML error. But its presence must emit a deprecation
	// warning so operators clean up.
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	yaml := `
development_mode: true
mcp_client_auth:
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
	if _, err := LoadConfig(writeTemp(t, yaml)); err != nil {
		t.Fatalf("config should parse and validate, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "development_mode is deprecated") {
		t.Errorf("expected deprecation warning in slog output, got: %s", out)
	}
	if !strings.Contains(out, "mcp_client_auth.mode") {
		t.Errorf("warning should point operator at the new field, got: %s", out)
	}
}

// TestLoadConfig_StaticMode_IgnoresOffModeOAuthFields guards the validator
// against the missing-vs-malformed asymmetry called out in the PR #62 review:
// under mode: static, OAuth-only fields (issuer, resource_url) are ignored if
// missing — they must also be ignored if malformed. Validating them under
// mode: static contradicts the spec ("off-mode fields are ignored").
func TestLoadConfig_StaticMode_IgnoresOffModeOAuthFields(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
  issuer: "not a valid url"
  resource_url: ":::also bad"
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
		t.Fatalf("malformed issuer/resource_url should be ignored under mode: static, got: %v", err)
	}
}

// TestLoadConfig_OAuthMode_RejectsMalformedIssuer is the symmetric companion
// to TestLoadConfig_StaticMode_IgnoresOffModeOAuthFields. Under mode: oauth,
// a structurally malformed issuer must still be rejected — guarding against
// a future refactor accidentally weakening the validateBrokerURL call inside
// the AuthModeOAuth case.
func TestLoadConfig_OAuthMode_RejectsMalformedIssuer(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: ":::not a url"
  audience: "mcp"
  resource_url: "https://mcp.example.com/mcp"
brokers:
  dev:
    url: "https://broker.example.com"
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for malformed issuer under mode: oauth")
	}
	if !strings.Contains(err.Error(), "mcp_client_auth.issuer") {
		t.Errorf("error should mention mcp_client_auth.issuer, got: %v", err)
	}
}

// --- Broker alias contract tests (SOL-149789) ---

// Layer 1: pure-helper tests — contract predicate and canonicalization.

func TestIsValidAlias(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Valid
		{"single char", "a", true},
		{"single digit", "1", true},
		{"lowercase word", "prod", true},
		{"mixed case", "ProdEast", true},
		{"with hyphen", "prod-east-1", true},
		{"length 63 boundary", strings.Repeat("a", 63), true},

		// Invalid
		{"empty", "", false},
		{"whitespace only", "  ", false},
		{"leading hyphen", "-prod", false},
		{"trailing hyphen", "prod-", false},
		{"embedded space", "prod east", false},
		{"underscore", "prod_east", false},
		{"dot", "prod.east", false},
		{"length 64 overflow", strings.Repeat("a", 64), false},
		{"leading whitespace", " prod", false},
		{"unicode", "prodé", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidAlias(tc.in); got != tc.want {
				t.Errorf("isValidAlias(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateAndCanonicalizeBrokers(t *testing.T) {
	mk := func(aliases ...string) map[string]*BrokerConfig {
		m := make(map[string]*BrokerConfig, len(aliases))
		for _, a := range aliases {
			m[a] = &BrokerConfig{URL: "https://example", Auth: AuthConfig{Mode: "basic", Username: "u", Password: "p"}}
		}
		return m
	}

	t.Run("happy path lowercase keys", func(t *testing.T) {
		canonical, errs := validateAndCanonicalizeBrokers(mk("prod-us", "dev"))
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if _, ok := canonical["prod-us"]; !ok {
			t.Errorf("canonical map missing prod-us")
		}
		if got := canonical["dev"].displayName; got != "dev" {
			t.Errorf("displayName = %q, want dev", got)
		}
	})

	t.Run("mixed case canonicalizes", func(t *testing.T) {
		canonical, errs := validateAndCanonicalizeBrokers(mk("ProdEast"))
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		b, ok := canonical["prodeast"]
		if !ok {
			t.Fatal("canonical map missing prodeast")
		}
		if b.displayName != "ProdEast" {
			t.Errorf("displayName = %q, want ProdEast", b.displayName)
		}
	})

	t.Run("case-only collision rejected", func(t *testing.T) {
		canonical, errs := validateAndCanonicalizeBrokers(mk("Prod", "prod"))
		if len(errs) == 0 {
			t.Fatal("expected collision error")
		}
		msg := errors.Join(errs...).Error()
		if !strings.Contains(msg, `"Prod"`) || !strings.Contains(msg, `"prod"`) {
			t.Errorf("collision error should quote both originals, got: %s", msg)
		}
		if !strings.Contains(msg, "case-insensitively") {
			t.Errorf("collision error should mention case-insensitive comparison, got: %s", msg)
		}
		if _, ok := canonical["prod"]; ok {
			t.Errorf("expected canonical map to exclude collision-loser; got entry for %q", "prod")
		}
	})

	t.Run("3-way collision lists all originals", func(t *testing.T) {
		_, errs := validateAndCanonicalizeBrokers(mk("Prod", "PROD", "prod"))
		if len(errs) == 0 {
			t.Fatal("expected collision error")
		}
		msg := errors.Join(errs...).Error()
		for _, original := range []string{`"Prod"`, `"PROD"`, `"prod"`} {
			if !strings.Contains(msg, original) {
				t.Errorf("collision error should quote %s, got: %s", original, msg)
			}
		}
	})

	t.Run("two separate collision groups reported independently", func(t *testing.T) {
		_, errs := validateAndCanonicalizeBrokers(mk("Prod", "prod", "Dev", "DEV"))
		if len(errs) == 0 {
			t.Fatal("expected collision errors")
		}
		msg := errors.Join(errs...).Error()
		// All four originals appear somewhere in the joined error output.
		for _, original := range []string{"Prod", "prod", "Dev", "DEV"} {
			if !strings.Contains(msg, `"`+original+`"`) {
				t.Errorf("expected error to mention %q, got: %s", original, msg)
			}
		}
		// At least two separate collision messages.
		if count := strings.Count(msg, "collide"); count < 2 {
			t.Errorf("expected at least 2 collision messages, got %d in: %s", count, msg)
		}
	})

	t.Run("invalid alias reported", func(t *testing.T) {
		_, errs := validateAndCanonicalizeBrokers(mk("prod east"))
		if len(errs) == 0 {
			t.Fatal("expected error for invalid alias")
		}
		msg := errors.Join(errs...).Error()
		if !strings.Contains(msg, `"prod east"`) {
			t.Errorf("error should quote the invalid alias, got: %s", msg)
		}
		if !strings.Contains(msg, "1-63") {
			t.Errorf("error should describe the contract, got: %s", msg)
		}
	})

	t.Run("mixed valid and invalid", func(t *testing.T) {
		canonical, errs := validateAndCanonicalizeBrokers(mk("dev", "prod east"))
		if len(errs) == 0 {
			t.Fatal("expected error for invalid alias")
		}
		if _, ok := canonical["dev"]; !ok {
			t.Errorf("valid alias dev should be canonicalized despite sibling invalid")
		}
	})

	t.Run("empty map returns empty canonical no errors", func(t *testing.T) {
		canonical, errs := validateAndCanonicalizeBrokers(map[string]*BrokerConfig{})
		if len(errs) != 0 {
			t.Errorf("unexpected errors: %v", errs)
		}
		if len(canonical) != 0 {
			t.Errorf("canonical map should be empty, got %d entries", len(canonical))
		}
	})
}

// Layer 2: accessor contract tests — public API behavior.

func loadAliasContractTestConfig(t *testing.T, aliases ...string) *ServerConfig {
	t.Helper()
	var b strings.Builder
	b.WriteString("mcp_client_auth:\n  mode: disabled\nbrokers:\n")
	for _, a := range aliases {
		fmt.Fprintf(&b, "  %s:\n    url: https://broker.example.com\n    auth:\n      mode: basic\n      username: admin\n      password: secret\n", a)
	}
	cfg, err := LoadConfig(writeTemp(t, b.String()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestBrokerLookupIsCaseInsensitive(t *testing.T) {
	cfg := loadAliasContractTestConfig(t, "ProdEast")
	want, ok := cfg.Broker("ProdEast")
	if !ok {
		t.Fatal("Broker(ProdEast) returned !ok")
	}
	for _, alias := range []string{"PRODEAST", "prodeast", "pRoDeAsT"} {
		got, ok := cfg.Broker(alias)
		if !ok {
			t.Errorf("Broker(%q) returned !ok", alias)
			continue
		}
		if got != want {
			t.Errorf("Broker(%q) returned different pointer than Broker(%q)", alias, "ProdEast")
		}
	}
}

func TestBrokerDisplayNamePreservation(t *testing.T) {
	cfg := loadAliasContractTestConfig(t, "ProdEast")
	b, ok := cfg.Broker("prodeast")
	if !ok {
		t.Fatal("Broker(prodeast) returned !ok")
	}
	if b.DisplayName() != "ProdEast" {
		t.Errorf("DisplayName() = %q, want ProdEast", b.DisplayName())
	}
}

func TestBrokerAliasesReturnsDisplayForms(t *testing.T) {
	cfg := loadAliasContractTestConfig(t, "ProdEast", "DevWest")
	got := cfg.BrokerAliases()
	want := []string{"DevWest", "ProdEast"}
	if len(got) != len(want) {
		t.Fatalf("BrokerAliases() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BrokerAliases()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBrokerLookupUnknownReturnsFalse(t *testing.T) {
	cfg := loadAliasContractTestConfig(t, "prod")
	if b, ok := cfg.Broker("nonexistent"); ok || b != nil {
		t.Errorf("Broker(nonexistent) = (%v, %v), want (nil, false)", b, ok)
	}
}

// Layer 3: end-to-end loader tests.

func TestLoadConfigRejectsInvalidAlias(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: disabled
brokers:
  "prod east":
    url: https://broker.example.com
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid alias")
	}
	if !strings.Contains(err.Error(), `"prod east"`) {
		t.Errorf("error should quote the invalid alias, got: %v", err)
	}
	if !strings.Contains(err.Error(), "1-63") {
		t.Errorf("error should describe the contract, got: %v", err)
	}
}

func TestLoadConfigRejectsCaseCollision(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: disabled
brokers:
  Prod:
    url: https://broker.example.com
    auth:
      mode: basic
      username: admin
      password: secret
  prod:
    url: https://broker.example.com
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for case collision")
	}
	if !strings.Contains(err.Error(), `"Prod"`) || !strings.Contains(err.Error(), `"prod"`) {
		t.Errorf("error should quote both originals, got: %v", err)
	}
}

func TestLoadConfigUsesDisplayNameInValidationErrors(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: disabled
brokers:
  ProdEast:
    url: ""
    auth:
      mode: basic
      username: admin
      password: secret
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing broker URL")
	}
	if !strings.Contains(err.Error(), "ProdEast") {
		t.Errorf("per-broker validation error should preserve original casing %q, got: %v", "ProdEast", err)
	}
}

// TestLoadConfigRejectsNilBrokerEntry pins that a YAML broker entry with no
// body (e.g. `prod:` with nothing under it) is reported as a user-facing
// error rather than causing a nil-pointer panic during validation.
func TestLoadConfigRejectsNilBrokerEntry(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: disabled
brokers:
  prod:
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for nil broker entry")
	}
	if !strings.Contains(err.Error(), `"prod"`) {
		t.Errorf("error should quote the offending alias, got: %v", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should explain the entry is empty, got: %v", err)
	}
}
func TestLoadConfig_NoBrokerOAuthBlock_BackwardsCompat(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
brokers:
  legacy:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: secret
  legacy-bearer:
    url: "http://broker2.example.com:8080"
    auth:
      mode: bearer
      token: abc123
`
	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("backwards-compat YAML must load cleanly, got error: %v", err)
	}
	if cfg.BrokerOAuth != nil {
		t.Errorf("expected BrokerOAuth to be nil when block absent, got %+v", cfg.BrokerOAuth)
	}
}

// TestLoadConfig_BrokerOAuth covers the validator surface for the
// broker_oauth block. The client-auth method is chosen by the populated
// sub-block under mcp_server_client_auth (discriminated union, no separate
// discriminator field), and the schema requires grant_type and
// audience_parameter_name explicitly — no defaults.
func TestLoadConfig_BrokerOAuth(t *testing.T) {
	// All cases run with mcp_client_auth.mode: static (so the deployment is in
	// dev mode and http:// broker URLs are accepted). Production-mode
	// behavior (https:// enforcement on token_url) is exercised in a
	// separate test below.
	const clientAuthBlock = `
mcp_client_auth:
  mode: static
  dev_token: test
`
	// A complete, validator-passing broker_oauth block. Cases that exercise a
	// specific failure mode start from this canonical shape and remove or
	// tweak the offending field, so it's clear what the case is testing.
	const validBrokerOAuth = `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
`

	cases := []struct {
		name             string
		yaml             string
		wantErr          bool
		wantErrSubstring string
	}{
		{
			// mcp_client_auth.mode: static + an oauth-mode broker violates the
			// Hop1/Hop2 alignment invariant (see validateHop1Hop2Alignment) —
			// the schema itself is valid.
			name: "schema-valid oauth broker config without Hop1 oauth — rejected by alignment invariant",
			yaml: clientAuthBlock + validBrokerOAuth + `
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "mcp_client_auth.mode must be oauth",
		},
		{
			name: "broker uses oauth mode but no broker_oauth block — block-required error",
			yaml: clientAuthBlock + `
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth block is required when any broker uses auth.mode",
		},
		{
			name: "broker_oauth.idp_token_endpoint missing",
			yaml: clientAuthBlock + `
broker_oauth:
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.idp_token_endpoint is required",
		},
		{
			name: "broker_oauth.mcp_server_client_id missing",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.mcp_server_client_id is required",
		},
		{
			// Discriminated union: no sub-block under client_auth at all.
			name: "broker_oauth.mcp_server_client_auth empty (no sub-block)",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth: {}
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.mcp_server_client_auth: at least one method sub-block is required",
		},
		{
			// Discriminated union: multiple sub-blocks populated.
			name: "broker_oauth.mcp_server_client_auth has two methods populated",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
    client_secret_post:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.mcp_server_client_auth: only one method sub-block may be configured",
		},
		{
			name: "client_secret_basic.secret missing",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic: {}
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.mcp_server_client_auth.client_secret_basic.secret is required",
		},
		{
			name: "client_secret_post.secret missing",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_post: {}
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.mcp_server_client_auth.client_secret_post.secret is required",
		},
		{
			name: "broker_oauth.idp_token_endpoint is not a valid URL",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: ":::not-a-url"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.idp_token_endpoint",
		},
		{
			// Audience is optional — the broker's OAuth profile may have
			// audience validation disabled. The schema accepts the omission,
			// and with Hop1 also set to oauth the config loads cleanly.
			name: "per-broker audience omitted when mode is oauth — accepted",
			yaml: `
tls_terminated_upstream: true
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp-server"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
broker_oauth:
  idp_token_endpoint: "https://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "https://broker.example.com:8080"
    auth:
      mode: oauth
`,
			wantErr: false,
		},
		{
			// The removed per-broker auth.scopes field must stay removed:
			// strict YAML decoding (KnownFields(true)) rejects any config
			// that still declares it, so operators who upgrade past the
			// removal see a loud error at startup rather than silently
			// losing behavior. See dedup_key.go for why per-user scopes
			// (if ever reintroduced) must join the dedup key.
			name: "per-broker auth.scopes is a removed field — strict decoder rejects it",
			yaml: clientAuthBlock + validBrokerOAuth + `
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
      scopes:
        - "semp:read"
`,
			wantErr:          true,
			wantErrSubstring: `field scopes not found in type config.AuthConfig`,
		},
		{
			name: "broker_oauth.grant_type missing",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.grant_type is required",
		},
		{
			name: "broker_oauth.grant_type invalid value",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.grant_type",
		},
		{
			name: "broker_oauth.audience_parameter_name missing",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.audience_parameter_name is required",
		},
		{
			name: "broker_oauth.audience_parameter_name invalid value",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: gibberish
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: "broker_oauth.audience_parameter_name",
		},
		{
			// scope (Entra OBO style) and resource (RFC 8707) are recognized
			// concepts but not yet implemented at the wire-construction layer
			// (internal/tokenexchange.resolveAudienceParam). Rejecting them
			// here, at config load, means the failure is joined with every
			// other broker_oauth error instead of only surfacing once the
			// Hop-2 runtime is actually constructed at server startup.
			name: "broker_oauth.audience_parameter_name scope — not yet implemented",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: scope
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: `broker_oauth.audience_parameter_name "scope" is not supported in this version`,
		},
		{
			name: "broker_oauth.audience_parameter_name resource — not yet implemented",
			yaml: clientAuthBlock + `
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: resource
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: oauth
      audience: solace-broker-prod
`,
			wantErr:          true,
			wantErrSubstring: `broker_oauth.audience_parameter_name "resource" is not supported in this version`,
		},
		{
			// Backwards-compatibility: a config without broker_oauth and no
			// broker using oauth mode loads cleanly. This is the path every
			// basic/bearer-only deployment is on today.
			name: "backwards-compat — no broker_oauth, no oauth-mode broker — loads cleanly",
			yaml: clientAuthBlock + `
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: shhh
`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTemp(t, tc.yaml))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstring)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstring) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstring)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestLoadConfig_BrokerOAuth_MethodResolution verifies that a config with
// exactly one client_auth sub-block populated parses correctly, with the
// expected sub-block populated and the others nil. Uses a basic-mode broker
// since this test is about the discriminated-union parsing of broker_oauth,
// not per-broker auth modes.
func TestLoadConfig_BrokerOAuth_MethodResolution(t *testing.T) {
	t.Run("client_secret_basic sub-block parses correctly", func(t *testing.T) {
		yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: shhh
`
		// broker_oauth is set but no broker uses oauth mode. validateBrokerOAuth
		// emits a WARN log; silence it so the test output is clean.
		var buf bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
		defer slog.SetDefault(old)

		cfg, err := LoadConfig(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.BrokerOAuth == nil {
			t.Fatal("BrokerOAuth is nil")
		}
		// Exactly the basic sub-block must be populated.
		if cfg.BrokerOAuth.ClientAuth.ClientSecretBasic == nil {
			t.Fatal("expected ClientSecretBasic sub-block to be populated")
		}
		if cfg.BrokerOAuth.ClientAuth.ClientSecretPost != nil {
			t.Error("expected ClientSecretPost sub-block to be nil when basic is configured")
		}
		if got, want := cfg.BrokerOAuth.ClientAuth.ClientSecretBasic.Secret, "shhh"; got != want {
			t.Errorf("ClientSecretBasic.Secret = %q, want %q", got, want)
		}
	})

	t.Run("client_secret_post sub-block parses correctly", func(t *testing.T) {
		yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_post:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: shhh
`
		var buf bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
		defer slog.SetDefault(old)

		cfg, err := LoadConfig(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Exactly the post sub-block must be populated.
		if cfg.BrokerOAuth.ClientAuth.ClientSecretPost == nil {
			t.Fatal("expected ClientSecretPost sub-block to be populated")
		}
		if cfg.BrokerOAuth.ClientAuth.ClientSecretBasic != nil {
			t.Error("expected ClientSecretBasic sub-block to be nil when post is configured")
		}
		if got, want := cfg.BrokerOAuth.ClientAuth.ClientSecretPost.Secret, "shhh"; got != want {
			t.Errorf("ClientSecretPost.Secret = %q, want %q", got, want)
		}
	})
}

// TestLoadConfig_BrokerOAuth_ProductionMode_RequiresHTTPS verifies that when
// the deployment is in production mode (mcp_client_auth.mode: oauth), the
// broker_oauth.idp_token_endpoint must use https://. Uses a bearer-mode
// broker since this test is about the production-mode URL rule, not
// per-broker auth modes.
func TestLoadConfig_BrokerOAuth_ProductionMode_RequiresHTTPS(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp-server"
  resource_url: "https://mcp.example.com/mcp"
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "https://broker.example.com:943"
    auth:
      mode: bearer
      token: shhh
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for http:// idp_token_endpoint in production mode")
	}
	if !strings.Contains(err.Error(), "broker_oauth.idp_token_endpoint") || !strings.Contains(err.Error(), "https") {
		t.Errorf("expected https-required error on broker_oauth.idp_token_endpoint, got: %v", err)
	}
}

// TestLoadConfig_BrokerOAuth_ClientSecretFromEnvVar verifies that ${VAR}
// substitution works for the secret field nested inside the client_auth
// sub-block (the risk flagged in the ticket). Uses a basic-mode broker
// since this test is about env-var substitution, not per-broker auth modes.
func TestLoadConfig_BrokerOAuth_ClientSecretFromEnvVar(t *testing.T) {
	t.Setenv("MCP_TEST_OAUTH_CLIENT_SECRET", "the-real-secret")

	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: "${MCP_TEST_OAUTH_CLIENT_SECRET}"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: shhh
`
	// Silence the "broker_oauth provided but no broker uses oauth mode" WARN
	// that fires when broker_oauth is set with no oauth broker.
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(old)

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BrokerOAuth == nil {
		t.Fatal("BrokerOAuth is nil")
	}
	if cfg.BrokerOAuth.ClientAuth.ClientSecretBasic == nil {
		t.Fatal("ClientSecretBasic sub-block is nil")
	}
	if got, want := cfg.BrokerOAuth.ClientAuth.ClientSecretBasic.Secret, "the-real-secret"; got != want {
		t.Errorf("mcp_server_client_auth.client_secret_basic.secret = %q, want %q", got, want)
	}
}

// TestLoadConfig_BrokerOAuth_ClientSecretEmptyAfterSubstitution verifies that
// when ${VAR} substitution resolves to an empty value, the validator rejects
// the result rather than silently accepting an empty secret.
func TestLoadConfig_BrokerOAuth_ClientSecretEmptyAfterSubstitution(t *testing.T) {
	t.Setenv("MCP_TEST_OAUTH_EMPTY_SECRET", "")

	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: "${MCP_TEST_OAUTH_EMPTY_SECRET}"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: shhh
`
	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for empty secret after substitution")
	}
	if !strings.Contains(err.Error(), "broker_oauth.mcp_server_client_auth.client_secret_basic.secret is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestLoadConfig_BrokerOAuth_WarnsWhenUnused verifies the WARN log fires when
// broker_oauth is configured but no broker uses oauth mode. This is operator
// noise, not a fatal error — the config is valid.
func TestLoadConfig_BrokerOAuth_WarnsWhenUnused(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: test
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  legacy:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	_, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "broker_oauth provided but no broker uses oauth mode") {
		t.Errorf("expected WARN about unused broker_oauth, got log output:\n%s", buf.String())
	}
}

// TestValidateHop1Hop2Alignment_Direct exercises the alignment validator in
// isolation, with crafted configs that don't need a full LoadConfig round
// trip. The same invariant is also exercised end-to-end via LoadConfig in
// TestLoadConfig_Hop1Hop2Alignment.
func TestValidateHop1Hop2Alignment_Direct(t *testing.T) {
	hop2Broker := func() *BrokerConfig {
		return &BrokerConfig{Auth: AuthConfig{Mode: AuthModeOAuth}}
	}
	basicBroker := func() *BrokerConfig {
		return &BrokerConfig{Auth: AuthConfig{Mode: AuthModeBasic, Username: "u", Password: "p"}}
	}

	t.Run("hop1 oauth + hop2 oauth — valid (no error)", func(t *testing.T) {
		cfg := &ServerConfig{
			MCPClientAuth: MCPClientAuthConfig{Mode: AuthModeOAuth},
			brokers:       map[string]*BrokerConfig{"a": hop2Broker()},
		}
		if err := validateHop1Hop2Alignment(cfg); err != nil {
			t.Fatalf("expected nil, got: %v", err)
		}
	})

	t.Run("hop1 oauth + no hop2 — valid (no error)", func(t *testing.T) {
		cfg := &ServerConfig{
			MCPClientAuth: MCPClientAuthConfig{Mode: AuthModeOAuth},
			brokers:       map[string]*BrokerConfig{"a": basicBroker()},
		}
		if err := validateHop1Hop2Alignment(cfg); err != nil {
			t.Fatalf("expected nil, got: %v", err)
		}
	})

	t.Run("hop1 static + no hop2 — valid (no error)", func(t *testing.T) {
		cfg := &ServerConfig{
			MCPClientAuth: MCPClientAuthConfig{Mode: AuthModeStatic, DevToken: "t"},
			brokers:       map[string]*BrokerConfig{"a": basicBroker()},
		}
		if err := validateHop1Hop2Alignment(cfg); err != nil {
			t.Fatalf("expected nil, got: %v", err)
		}
	})

	t.Run("hop1 static + hop2 oauth — invariant violated", func(t *testing.T) {
		cfg := &ServerConfig{
			MCPClientAuth: MCPClientAuthConfig{Mode: AuthModeStatic, DevToken: "t"},
			brokers:       map[string]*BrokerConfig{"a": hop2Broker()},
		}
		err := validateHop1Hop2Alignment(cfg)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		msg := err.Error()
		// Operator-facing error must name (a) the offending Hop 1 mode,
		// (b) the broker count, and (c) the corrective action — without
		// leaking internal vocabulary (Hop 1/2, RFC numbers, subject_token).
		for _, want := range []string{`"static"`, "1 broker", "auth.mode: oauth", "mcp_client_auth.mode must be oauth"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error missing %q in:\n%s", want, msg)
			}
		}
		for _, unwanted := range []string{"Hop 1", "Hop 2", "RFC", "subject_token"} {
			if strings.Contains(msg, unwanted) {
				t.Errorf("error contains internal vocabulary %q in:\n%s", unwanted, msg)
			}
		}
	})

	t.Run("hop1 disabled + hop2 oauth — invariant violated", func(t *testing.T) {
		cfg := &ServerConfig{
			MCPClientAuth: MCPClientAuthConfig{Mode: AuthModeDisabled},
			brokers:       map[string]*BrokerConfig{"a": hop2Broker()},
		}
		err := validateHop1Hop2Alignment(cfg)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), `"disabled"`) {
			t.Errorf("expected error to name hop1 mode 'disabled', got: %v", err)
		}
	})

	t.Run("hop1 static + 3 hop2 brokers — plural subject", func(t *testing.T) {
		cfg := &ServerConfig{
			MCPClientAuth: MCPClientAuthConfig{Mode: AuthModeStatic, DevToken: "t"},
			brokers: map[string]*BrokerConfig{
				"a": hop2Broker(), "b": hop2Broker(), "c": hop2Broker(),
			},
		}
		err := validateHop1Hop2Alignment(cfg)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "3 brokers have auth.mode: oauth") {
			t.Errorf("expected plural subject '3 brokers have' in error, got: %v", err)
		}
	})
}

// TestLoadConfig_Hop1Hop2Alignment verifies that LoadConfig surfaces both
// the alignment validation error AND its banner when a broker uses
// auth.mode: oauth but mcp_client_auth.mode does not.
func TestLoadConfig_Hop1Hop2Alignment(t *testing.T) {
	yaml := `
mcp_client_auth:
  mode: static
  dev_token: t
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  staging:
    url: "https://broker.example.com:943"
    auth:
      mode: oauth
      audience: "solace-broker-staging"
`
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(old)

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	out := buf.String()

	if !strings.Contains(msg, "mcp_client_auth.mode must be oauth") {
		t.Errorf("expected Hop1/Hop2 alignment error, got: %s", msg)
	}
	if !strings.Contains(out, "OAuth broker authentication requires OAuth client") {
		t.Errorf("expected Hop1/Hop2 alignment banner to fire, got:\n%s", out)
	}
}

// TestServerConfig_Hop2OAuthActive pins the two cases that define the
// method's contract: both preconditions true → returns true; and each
// precondition individually flipped false → returns false. Together these
// three assertions prove each precondition is load-bearing.
func TestServerConfig_Hop2OAuthActive(t *testing.T) {
	// Minimal valid broker_oauth block. Not validated here — Hop2OAuthActive
	// only checks for non-nil, so field contents are irrelevant.
	validBrokerOAuth := func() *BrokerOAuthConfig {
		return &BrokerOAuthConfig{
			TokenURL: "https://idp.example.com/token",
			ClientID: "mcp-server",
		}
	}
	oauthBroker := func() *BrokerConfig {
		return &BrokerConfig{Auth: AuthConfig{Mode: AuthModeOAuth}}
	}
	basicBroker := func() *BrokerConfig {
		return &BrokerConfig{Auth: AuthConfig{Mode: AuthModeBasic, Username: "u", Password: "p"}}
	}

	t.Run("both preconditions true → active", func(t *testing.T) {
		cfg := &ServerConfig{
			BrokerOAuth: validBrokerOAuth(),
			brokers:     map[string]*BrokerConfig{"a": oauthBroker()},
		}
		if !cfg.Hop2OAuthActive() {
			t.Fatal("expected Hop2OAuthActive to return true when broker_oauth is set and at least one broker uses oauth mode")
		}
	})

	t.Run("broker_oauth: nil → inactive (even with an oauth broker)", func(t *testing.T) {
		cfg := &ServerConfig{
			BrokerOAuth: nil,
			brokers:     map[string]*BrokerConfig{"a": oauthBroker()},
		}
		if cfg.Hop2OAuthActive() {
			t.Fatal("expected Hop2OAuthActive to return false when broker_oauth: is nil — the global block must be load-bearing")
		}
	})

	t.Run("no broker uses oauth mode → inactive (even with broker_oauth set)", func(t *testing.T) {
		cfg := &ServerConfig{
			BrokerOAuth: validBrokerOAuth(),
			brokers:     map[string]*BrokerConfig{"a": basicBroker()},
		}
		if cfg.Hop2OAuthActive() {
			t.Fatal("expected Hop2OAuthActive to return false when no broker uses auth.mode: oauth — at-least-one-broker precondition must be load-bearing")
		}
	})
}

func TestBrokerOAuthConfig_LogValue(t *testing.T) {
	const (
		secretClientID     = "mcp-client-id-VALUE"
		secretClientSecret = "SECRET_CLIENT_SECRET_VAL"
	)

	cfg := BrokerOAuthConfig{
		TokenURL: "https://idp.example.com/token",
		ClientID: secretClientID,
		ClientAuth: BrokerClientAuth{
			ClientSecretBasic: &ClientSecretAuth{Secret: secretClientSecret},
		},
		GrantType:     GrantTypeTokenExchange,
		AudienceParam: AudienceParamAudience,
	}

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	slog.Info("broker_oauth", slog.Any("cfg", cfg))

	out := buf.String()
	if strings.Contains(out, secretClientSecret) {
		t.Errorf("client_secret leaked into log output: %s", out)
	}
	if !strings.Contains(out, "idp_token_endpoint") || !strings.Contains(out, secretClientID) {
		t.Errorf("expected idp_token_endpoint and mcp_server_client_id in log output: %s", out)
	}
}

// listenAddressYAML assembles a config exercising listen_address against a given
// client-auth mode. oauth (production) requires https everywhere, so the broker
// URL is https for every case to keep the focus on listen_address behavior.
func listenAddressYAML(mode, listenLine, overrideLine string) string {
	var authBlock string
	switch mode {
	case "static":
		authBlock = "mcp_client_auth:\n  mode: static\n  dev_token: test\n"
	case "oauth":
		authBlock = "mcp_client_auth:\n  mode: oauth\n  issuer: \"https://idp.example.com\"\n  audience: \"mcp\"\n  resource_url: \"https://mcp.example.com/mcp\"\n  tool_authorization:\n    enabled: false\ntls_terminated_upstream: true\n"
	default:
		authBlock = "mcp_client_auth:\n  mode: " + mode + "\n"
	}
	return authBlock + listenLine + overrideLine + `brokers:
  dev:
    url: "https://broker.example.com:1943"
    auth:
      mode: basic
      username: admin
      password: secret
`
}

func TestLoadConfig_ListenAddress_DefaultResolution(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		listenLine string
		wantAddr   string
	}{
		{"disabled defaults loopback", "disabled", "", "127.0.0.1"},
		{"static defaults loopback", "static", "", "127.0.0.1"},
		{"oauth defaults all interfaces", "oauth", "", ""},
		{"explicit value preserved under disabled", "disabled", "listen_address: \"127.0.0.1\"\n", "127.0.0.1"},
		{"explicit value preserved under oauth", "oauth", "listen_address: \"0.0.0.0\"\n", "0.0.0.0"},
		{"mixed-case mode still defaults loopback", "Disabled", "", "127.0.0.1"},
		{"surrounding whitespace is trimmed", "oauth", "listen_address: \" 0.0.0.0 \"\n", "0.0.0.0"},
		{"whitespace-only resolves to loopback default", "disabled", "listen_address: \"   \"\n", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeTemp(t, listenAddressYAML(tc.mode, tc.listenLine, "")))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ListenAddress != tc.wantAddr {
				t.Errorf("ListenAddress = %q, want %q", cfg.ListenAddress, tc.wantAddr)
			}
		})
	}
}

func TestLoadConfig_ListenAddress_Validation(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		listenLine   string
		overrideLine string
		wantErr      string // substring; "" means expect success
	}{
		{
			name:       "disabled non-loopback without override is refused",
			mode:       "disabled",
			listenLine: "listen_address: \"0.0.0.0\"\n",
			wantErr:    "allow_remote_unauthenticated",
		},
		{
			name:         "disabled non-loopback with override is allowed",
			mode:         "disabled",
			listenLine:   "listen_address: \"0.0.0.0\"\n",
			overrideLine: "allow_remote_unauthenticated: true\n",
		},
		{
			name:       "disabled explicit loopback is allowed",
			mode:       "disabled",
			listenLine: "listen_address: \"127.0.0.1\"\n",
		},
		{
			name:       "disabled localhost is allowed",
			mode:       "disabled",
			listenLine: "listen_address: \"localhost\"\n",
		},
		{
			name:       "disabled IPv6 loopback is allowed",
			mode:       "disabled",
			listenLine: "listen_address: \"::1\"\n",
		},
		{
			name:       "disabled uppercase LOCALHOST is allowed (case-insensitive)",
			mode:       "disabled",
			listenLine: "listen_address: \"LOCALHOST\"\n",
		},
		{
			name:       "disabled IPv6 unspecified without override is refused",
			mode:       "disabled",
			listenLine: "listen_address: \"::\"\n",
			wantErr:    "allow_remote_unauthenticated",
		},
		{
			name:       "static non-loopback is allowed without override",
			mode:       "static",
			listenLine: "listen_address: \"0.0.0.0\"\n",
		},
		{
			name:       "oauth non-loopback is allowed",
			mode:       "oauth",
			listenLine: "listen_address: \"0.0.0.0\"\n",
		},
		{
			name:       "malformed listen_address is rejected",
			mode:       "oauth",
			listenLine: "listen_address: \"not an ip\"\n",
			wantErr:    "must be an IP address",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTemp(t, listenAddressYAML(tc.mode, tc.listenLine, tc.overrideLine)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestServerConfig_BindAddress(t *testing.T) {
	cases := []struct {
		addr string
		port int
		want string
	}{
		{"127.0.0.1", 9090, "127.0.0.1:9090"},
		{"", 9090, ":9090"},
		{"0.0.0.0", 8080, "0.0.0.0:8080"},
		{"::1", 9090, "[::1]:9090"}, // IPv6 host must be bracketed for net.Listen
	}
	for _, tc := range cases {
		cfg := &ServerConfig{ListenAddress: tc.addr, Port: tc.port}
		if got := cfg.BindAddress(); got != tc.want {
			t.Errorf("BindAddress() with addr=%q port=%d = %q, want %q", tc.addr, tc.port, got, tc.want)
		}
	}
}

func TestServerConfig_StaticTokenExposedCleartext(t *testing.T) {
	cases := []struct {
		name string
		mode string
		addr string
		cert string
		want bool
	}{
		{"static non-loopback no TLS -> exposed", AuthModeStatic, "0.0.0.0", "", true},
		{"static non-loopback with TLS -> safe", AuthModeStatic, "0.0.0.0", "/tmp/cert.pem", false},
		{"static loopback -> safe", AuthModeStatic, "127.0.0.1", "", false},
		{"static localhost -> safe", AuthModeStatic, "localhost", "", false},
		{"disabled non-loopback -> not static, no warning", AuthModeDisabled, "0.0.0.0", "", false},
		{"oauth non-loopback -> not static, no warning", AuthModeOAuth, "0.0.0.0", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ServerConfig{
				ListenAddress: tc.addr,
				TLSCertFile:   tc.cert,
				MCPClientAuth: MCPClientAuthConfig{Mode: tc.mode},
			}
			if got := cfg.StaticTokenExposedCleartext(); got != tc.want {
				t.Errorf("StaticTokenExposedCleartext() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadResolvedConfigFile_SubstitutesEnvVars pins the contract the --health
// probe depends on: the resolved bytes must agree with what LoadConfig would
// parse, ${VAR_NAME} references included.
func TestReadResolvedConfigFile_SubstitutesEnvVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("tls_cert_file: \"${TEST_CERT_PATH}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_CERT_PATH", "/etc/certs/server.pem")

	got, err := ReadResolvedConfigFile(path)
	if err != nil {
		t.Fatalf("ReadResolvedConfigFile: unexpected error: %v", err)
	}
	if !strings.Contains(string(got), "/etc/certs/server.pem") {
		t.Errorf("resolved bytes = %q, want the substituted path", string(got))
	}
	if strings.Contains(string(got), "${TEST_CERT_PATH}") {
		t.Error("resolved bytes still contain the unsubstituted reference")
	}
}

func TestReadResolvedConfigFile_ErrorsOnMissingFile(t *testing.T) {
	if _, err := ReadResolvedConfigFile(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("ReadResolvedConfigFile(missing file) = nil error, want an error")
	}
}

// TestReadResolvedConfigFile_ErrorsOnUnsetEnvVar matches LoadConfig, which refuses
// a config referencing an env var that is not set rather than substituting empty.
func TestReadResolvedConfigFile_ErrorsOnUnsetEnvVar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("tls_cert_file: \"${TEST_DEFINITELY_UNSET_VAR}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadResolvedConfigFile(path); err == nil {
		t.Fatal("ReadResolvedConfigFile(unset var) = nil error, want an error")
	}
}

// TestLoadConfig_ResolvesVarsFromEnvFile pins step 2 of the processing order as a
// step LoadConfig actually performs, not merely one loadEnvFile implements. The
// variable is deliberately absent from the process environment, so the only way
// substitution can succeed is if the .env file beside the config was loaded first.
// Deleting the loadEnvFile call from ReadResolvedConfigFile turns this red.
func TestLoadConfig_ResolvesVarsFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := `port: 9090
mcp_client_auth:
  mode: disabled
brokers:
  b1:
    url: "https://broker:1943"
    auth:
      mode: basic
      username: admin
      password: "${TEST_ENVFILE_ONLY_PASSWORD}"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TEST_ENVFILE_ONLY_PASSWORD=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("TEST_ENVFILE_ONLY_PASSWORD")                       //nolint:errcheck // only the .env file may supply it
	t.Cleanup(func() { os.Unsetenv("TEST_ENVFILE_ONLY_PASSWORD") }) //nolint:errcheck // loadEnvFile sets it process-wide

	got, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error: %v", err)
	}
	b, ok := got.Broker("b1")
	if !ok {
		t.Fatal(`Broker("b1") not found`)
	}
	if b.Auth.Password != "from-env-file" {
		t.Errorf("password = %q, want %q — the .env file beside the config was not loaded before substitution", b.Auth.Password, "from-env-file")
	}
}

// TestReadResolvedConfigFile_ResolvesVarsFromEnvFile pins the same step for the
// partial readers that share this preprocessing, currently the --health probe.
func TestReadResolvedConfigFile_ResolvesVarsFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("tls_cert_file: \"${TEST_ENVFILE_ONLY_CERT}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TEST_ENVFILE_ONLY_CERT=/etc/certs/from-env-file.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("TEST_ENVFILE_ONLY_CERT")                       //nolint:errcheck // only the .env file may supply it
	t.Cleanup(func() { os.Unsetenv("TEST_ENVFILE_ONLY_CERT") }) //nolint:errcheck // loadEnvFile sets it process-wide

	got, err := ReadResolvedConfigFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadResolvedConfigFile: unexpected error: %v", err)
	}
	if !strings.Contains(string(got), "/etc/certs/from-env-file.pem") {
		t.Errorf("resolved bytes = %q, want the value from the .env file", string(got))
	}
}
