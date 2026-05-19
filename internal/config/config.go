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

// Package config loads and validates the Solace Broker MCP Server configuration
// from a YAML file. It supports ${VAR_NAME} env var substitution in any YAML
// field, multiple brokers with independent credentials, and applies defaults
// from the defaults package for optional fields.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"gopkg.in/yaml.v3"
)

// ServerConfig holds the complete MCP server configuration, including all
// configured brokers and SEMP client settings.
type ServerConfig struct {
	Brokers         map[string]*BrokerConfig // broker alias → config
	SEMP            SEMPConfig               // SEMP client settings
	Port            int                      // HTTP port the MCP server listens on
	LogLevel        string                   // slog level name: "debug", "info", "warn", "error"
	DevelopmentMode bool                     // use static dev token if true
	ClientAuth      ClientAuthConfig         // authentication config for mcp client to server interactions
	TLSCertFile     string                   // path to TLS certificate file (optional, enables HTTPS)
	TLSKeyFile      string                   // path to TLS private key file (optional, requires TLSCertFile)
}

type ClientAuthConfig struct {
	Issuer      string `yaml:"issuer"`       // IdP issuer URL - required when development_mode: false
	Audience    string `yaml:"audience"`     // Expected 'aud' claim value — required when development_mode: false
	DevToken    string `yaml:"dev_token"`    // Static token for dev — only used when development_mode: true
	ResourceURL string `yaml:"resource_url"` // OAuth resource URL (e.g., "https://mcp.example.com/mcp") - defaults to localhost if not set
}

// validLogLevels is the allowlist of slog levels operators may configure.
// Matches the story spec: strict exactly these four values. Excludes slog's
// offset syntax (e.g., "INFO+3") which UnmarshalText would otherwise accept.
var validLogLevels = []string{"debug", "info", "warn", "error"}

// BrokerConfig holds the connection and authentication configuration for a
// single Solace broker.
type BrokerConfig struct {
	URL                string     `yaml:"url"`                  // SEMP API base URL (e.g., "https://broker:1943")
	InsecureSkipVerify bool       `yaml:"insecure_skip_verify"` // skip TLS cert verification (dev only, self-signed certs)
	Auth               AuthConfig `yaml:"auth"`                 // authentication config
}

// Auth mode constants for broker authentication (Hop 2: MCP Server → Broker).
const (
	AuthModeBasic  = "basic"
	AuthModeBearer = "bearer"
)

// validAuthModes is the allowlist of supported auth modes for broker connections.
// Add new modes (e.g., "oauth") here — validate() and error messages derive from this slice.
var validAuthModes = []string{AuthModeBasic, AuthModeBearer}

// AuthConfig holds the authentication credentials for a broker connection.
type AuthConfig struct {
	Mode     string `yaml:"mode"`     // "basic" or "bearer"
	Username string `yaml:"username"` // basic auth username (use ${VAR_NAME} for env var)
	Password string `yaml:"password"` // basic auth password (use ${VAR_NAME} for env var)
	Token    string `yaml:"token"`    // bearer token (use ${VAR_NAME} for env var)
}

// LogValue implements slog.LogValuer for AuthConfig. It exposes only the auth
// mode — username, password, and token are deliberately excluded to prevent
// credential leaks in log output. See docs/secure-logging-rules.md Rule 2.
func (a AuthConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("mode", a.Mode),
	)
}

// LogValue implements slog.LogValuer for BrokerConfig. It exposes connection
// metadata (URL, TLS settings, auth method) but excludes credentials.
// See docs/secure-logging-rules.md Rule 2.
func (b BrokerConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", b.URL),
		slog.Bool("insecure_skip_verify", b.InsecureSkipVerify),
		slog.String("auth_mode", b.Auth.Mode),
	)
}

// LogValue implements slog.LogValuer for ClientAuthConfig. It exposes OAuth
// configuration (issuer, audience, resource URL) but excludes DevToken
// to prevent credential leaks in log output. See docs/secure-logging-rules.md Rule 2.
func (c ClientAuthConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("issuer", c.Issuer),
		slog.String("audience", c.Audience),
		slog.String("resource_url", c.ResourceURL),
	)
}

// SEMPConfig holds settings that control how the MCP server communicates with
// brokers over the SEMP API. The rate-limit and retry fields below are
// defined here but not yet consumed -- they will be wired up in Story 5 when
// the rate limiter and retry policy are implemented.
//
// Field names match the Solace Terraform provider's naming convention so
// operators familiar with terraform-provider-solacebroker get a consistent
// experience across tools.
//
// RequestMinInterval and Retries use pointer types so we can distinguish
// "field omitted in YAML" (nil) from "field set to 0" (non-nil pointer to
// zero). Zero is a legitimate operator choice for both -- the Terraform
// provider documents "set request_min_interval to 0 for no rate limit", and
// retries=0 means no retries by analogy. Plain int/time.Duration cannot tell
// those two cases apart, because both produce the zero value. After
// applyDefaults runs, these fields are guaranteed non-nil so downstream code
// can dereference safely.
type SEMPConfig struct {
	MaxConcurrentPerBroker int            `yaml:"max_concurrent_per_broker"` // semaphore size per broker
	RequestTimeoutDuration time.Duration  `yaml:"request_timeout_duration"`  // HTTP request timeout for SEMP calls (e.g., "30s")
	RequestMinInterval     *time.Duration `yaml:"request_min_interval"`      // minimum spacing between SEMP requests; 0 = no throttle
	Retries                *int           `yaml:"retries"`                   // max retry attempts for a failed SEMP call; 0 = no retries
	RetryMinInterval       time.Duration  `yaml:"retry_min_interval"`        // starting backoff before the first retry (must be > 0)
	RetryMaxInterval       time.Duration  `yaml:"retry_max_interval"`        // cap on retry backoff, must be >= RetryMinInterval
}

// yamlConfig is the intermediate representation used for YAML unmarshalling.
// It mirrors the YAML file structure before being transformed into ServerConfig.
type yamlConfig struct {
	Brokers         map[string]*BrokerConfig `yaml:"brokers"`
	SEMP            SEMPConfig               `yaml:"semp"`
	Port            int                      `yaml:"port"`
	LogLevel        string                   `yaml:"log_level"`
	DevelopmentMode bool                     `yaml:"development_mode"`
	ClientAuth      ClientAuthConfig         `yaml:"client_auth"`
	TLSCertFile     string                   `yaml:"tls_cert_file"`
	TLSKeyFile      string                   `yaml:"tls_key_file"`
}

// Load locates the server configuration file, loads it, and returns a ready
// ServerConfig. It checks the following in order:
//
//  1. CONFIG_FILE env var — explicit operator override. Strict: any error
//     loading this file is fatal. We do NOT silently fall through to the
//     other paths, because if the operator explicitly pointed at a file,
//     they meant THAT file.
//  2. defaults.DefaultConfigPathSystem (/etc/mcp-server/config.yaml) —
//     production-install location.
//  3. defaults.DefaultConfigPathLocal (broker-config.yaml in CWD) —
//     developer-convenience fallback for running out of the repo.
//
// Only "file does not exist" errors from step 2 or 3 trigger fallback to the
// next path. Parse errors, permission errors, validation errors, or any other
// failure from an existing file are always fatal — never silently masked by
// trying another path. If nothing is found, returns an error listing all
// paths tried.
func Load() (*ServerConfig, error) {
	// Step 1: CONFIG_FILE env var (explicit, strict)
	if envPath := os.Getenv("CONFIG_FILE"); envPath != "" {
		return LoadConfig(envPath)
	}

	// Step 2: system path
	if _, err := os.Stat(defaults.DefaultConfigPathSystem); err == nil {
		return LoadConfig(defaults.DefaultConfigPathSystem)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", defaults.DefaultConfigPathSystem, err)
	}

	// Step 3: local path
	if _, err := os.Stat(defaults.DefaultConfigPathLocal); err == nil {
		return LoadConfig(defaults.DefaultConfigPathLocal)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", defaults.DefaultConfigPathLocal, err)
	}

	// Nothing found
	return nil, fmt.Errorf(
		"no config file found; set CONFIG_FILE env var or place config at one of: %s, %s",
		defaults.DefaultConfigPathSystem,
		defaults.DefaultConfigPathLocal,
	)
}

// LoadConfig reads a YAML configuration file from path, substitutes ${VAR_NAME}
// env var references, parses YAML, applies defaults, validates, and returns a
// ServerConfig ready for use.
//
// Processing order:
//  1. Read YAML file (raw bytes)
//  2. Load .env file (so env vars are available for substitution)
//  3. Substitute ${VAR_NAME} in raw bytes
//  4. Parse YAML
//  5. Apply defaults (fill missing optional fields)
//  6. Apply env var overrides (MCP_SERVER_PORT — runtime override)
//  7. Validate (required fields, value ranges, TLS pairing)
func LoadConfig(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304/G703 — path is the operator-provided config file location (CONFIG_FILE env var, /etc/mcp-server/config.yaml, or ./broker-config.yaml), not untrusted external input
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	loadEnvFile(path)

	data, err = substituteEnvVars(data)
	if err != nil {
		return nil, fmt.Errorf("substituting env vars: %w", err)
	}

	var raw yamlConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	cfg := &ServerConfig{
		Brokers:         raw.Brokers,
		SEMP:            raw.SEMP,
		Port:            raw.Port,
		LogLevel:        raw.LogLevel,
		DevelopmentMode: raw.DevelopmentMode,
		ClientAuth:      raw.ClientAuth,
		TLSCertFile:     raw.TLSCertFile,
		TLSKeyFile:      raw.TLSKeyFile,
	}

	applyDefaults(cfg)

	if err := applyEnvOverrides(cfg); err != nil {
		return nil, fmt.Errorf("applying env overrides: %w", err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// applyDefaults fills in missing optional fields from the defaults package.
func applyDefaults(cfg *ServerConfig) {
	if cfg.Port == 0 {
		cfg.Port = defaults.DefaultPort
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = defaults.DefaultLogLevel
	}
	if cfg.SEMP.MaxConcurrentPerBroker == 0 {
		cfg.SEMP.MaxConcurrentPerBroker = defaults.DefaultMaxConcurrentPerBroker
	}
	if cfg.SEMP.RequestTimeoutDuration == 0 {
		cfg.SEMP.RequestTimeoutDuration = defaults.DefaultSEMPRequestTimeoutDuration
	}
	// RequestMinInterval and Retries are pointers so we can distinguish
	// "operator omitted the field" (nil) from "operator set it to 0" (non-nil
	// pointer to zero). Apply the default only when nil. See the SEMPConfig
	// struct doc for the full rationale.
	if cfg.SEMP.RequestMinInterval == nil {
		def := defaults.DefaultRequestMinInterval
		cfg.SEMP.RequestMinInterval = &def
	}
	if cfg.SEMP.Retries == nil {
		def := defaults.DefaultRetries
		cfg.SEMP.Retries = &def
	}
	if cfg.SEMP.RetryMinInterval == 0 {
		cfg.SEMP.RetryMinInterval = defaults.DefaultRetryMinInterval
	}
	if cfg.SEMP.RetryMaxInterval == 0 {
		cfg.SEMP.RetryMaxInterval = defaults.DefaultRetryMaxInterval
	}
	// InsecureSkipVerify is not defaulted here — Go's zero value for bool
	// (false) already matches the intended default (verify TLS certificates).
}

// loadEnvFile loads a .env file into the process environment. It checks two
// locations in order: the ENV_FILE environment variable (explicit path), then
// a .env file in the same directory as the config file (convention). If the
// file does not exist it is silently skipped — .env files are optional. Any
// other read error (e.g. permission denied) is logged at WARN so operators
// get a pointer to the real cause rather than a downstream "env var not set"
// error. Variables already set in the environment are not overwritten, so
// real env vars (e.g., from CI/CD) take precedence over .env values.
func loadEnvFile(configPath string) {
	envPath := os.Getenv("ENV_FILE")
	if envPath == "" {
		envPath = filepath.Join(filepath.Dir(configPath), ".env")
	}
	data, err := os.ReadFile(envPath) //nolint:gosec // G703 — path from trusted config/env, not external user input
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("env file unreadable", slog.String("path", envPath))
		}
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = stripMatchedQuotes(strings.TrimSpace(value))
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value) //nolint:gosec // G104 — os.Setenv only fails on Plan 9; safe to ignore
		}
	}

	slog.Info("loaded .env file",
		slog.String("path", envPath))
}

// stripMatchedQuotes removes one matching pair of surrounding double or single
// quotes from a .env value — e.g. "secret" → secret, 'pw' → pw. Unbalanced
// or unquoted values are returned unchanged.
func stripMatchedQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// envVarPattern matches ${VAR_NAME} in raw YAML bytes. VAR_NAME must be
// uppercase letters, numbers, and underscores.
var envVarPattern = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// substituteEnvVars replaces all ${VAR_NAME} occurrences in raw YAML bytes with
// the corresponding environment variable values. Returns an error if any
// referenced env var is not set. This runs before YAML parsing so any field
// can reference env vars.
func substituteEnvVars(data []byte) ([]byte, error) {
	var missing []string

	result := envVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		// Extract var name from ${VAR_NAME}
		varName := string(envVarPattern.FindSubmatch(match)[1])
		value, exists := os.LookupEnv(varName)
		if !exists {
			missing = append(missing, varName)
			return match // leave as-is, error reported after
		}
		return []byte(value)
	})

	if len(missing) > 0 {
		return nil, fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
	}

	return result, nil
}

// validate checks that the config has all required fields and that values are
// within acceptable ranges. All validation errors are collected and returned as
// a single joined error so operators see every issue in one run instead of
// fixing them one-by-one across multiple reloads.
func validate(cfg *ServerConfig) error {
	var errs []error

	if len(cfg.Brokers) == 0 {
		errs = append(errs, fmt.Errorf("at least one broker must be configured"))
	}

	for _, alias := range slices.Sorted(maps.Keys(cfg.Brokers)) {
		errs = append(errs, validateBroker(alias, cfg.Brokers[alias], !cfg.DevelopmentMode)...)
	}

	if err := ValidatePort(cfg.Port); err != nil {
		errs = append(errs, err)
	}

	// Normalize log_level and check against the allowlist.
	cfg.LogLevel = strings.ToLower(cfg.LogLevel)
	if !slices.Contains(validLogLevels, cfg.LogLevel) {
		errs = append(errs, fmt.Errorf("log_level %q is invalid (must be one of %v)", cfg.LogLevel, validLogLevels))
	}

	if n := cfg.SEMP.MaxConcurrentPerBroker; n < 1 || n > defaults.MaxConcurrentPerBrokerCeiling {
		errs = append(errs, fmt.Errorf("semp.max_concurrent_per_broker must be between 1 and %d, got %d", defaults.MaxConcurrentPerBrokerCeiling, n))
	}

	if cfg.SEMP.RequestTimeoutDuration <= 0 {
		errs = append(errs, fmt.Errorf("semp.request_timeout_duration must be > 0, got %s", cfg.SEMP.RequestTimeoutDuration))
	}

	// RequestMinInterval and Retries are guaranteed non-nil by applyDefaults,
	// so dereferencing is safe here. Story rule: non-negative for both; zero is
	// explicitly allowed (0 = no rate limit / no retries).
	if *cfg.SEMP.RequestMinInterval < 0 {
		errs = append(errs, fmt.Errorf("semp.request_min_interval must be >= 0, got %s", *cfg.SEMP.RequestMinInterval))
	}

	if *cfg.SEMP.Retries < 0 {
		errs = append(errs, fmt.Errorf("semp.retries must be >= 0, got %d", *cfg.SEMP.Retries))
	}

	if cfg.SEMP.RetryMinInterval <= 0 {
		errs = append(errs, fmt.Errorf("semp.retry_min_interval must be > 0, got %s", cfg.SEMP.RetryMinInterval))
	}

	if cfg.SEMP.RetryMaxInterval <= 0 {
		errs = append(errs, fmt.Errorf("semp.retry_max_interval must be > 0, got %s", cfg.SEMP.RetryMaxInterval))
	} else if cfg.SEMP.RetryMaxInterval < cfg.SEMP.RetryMinInterval {
		errs = append(errs, fmt.Errorf("semp.retry_max_interval (%s) must be >= semp.retry_min_interval (%s)", cfg.SEMP.RetryMaxInterval, cfg.SEMP.RetryMinInterval))
	}

	// Validate client authentication configuration based on development mode.
	// Note that no validation is needed when development mode is enabled, as DevToken
	// can be set or empty. When DevToken is empty, all requests pass through
	if !cfg.DevelopmentMode {
		// Production mode: require JWT validation fields
		if cfg.ClientAuth.Issuer == "" {
			errs = append(errs, fmt.Errorf("client_auth.issuer is required when development_mode is false"))
		}
		if cfg.ClientAuth.Audience == "" {
			errs = append(errs, fmt.Errorf("client_auth.audience is required when development_mode is false"))
		}
		if cfg.ClientAuth.ResourceURL == "" {
			errs = append(errs, fmt.Errorf("client_auth.resource_url is required when development_mode is false"))
		}
	}

	// Validate issuer structure if set
	if cfg.ClientAuth.Issuer != "" {
		if err := validateBrokerURL(cfg.ClientAuth.Issuer, !cfg.DevelopmentMode); err != nil {
			errs = append(errs, fmt.Errorf("client_auth.issuer: %w", err))
		}
	}

	// Validate resource_url structure if set
	if cfg.ClientAuth.ResourceURL != "" {
		if err := validateBrokerURL(cfg.ClientAuth.ResourceURL, !cfg.DevelopmentMode); err != nil {
			errs = append(errs, fmt.Errorf("client_auth.resource_url: %w", err))
		}
	}

	// TLS: both cert and key must be provided together, or neither.
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		errs = append(errs, fmt.Errorf("both tls_cert_file and tls_key_file must be provided together; got cert=%q, key=%q", cfg.TLSCertFile, cfg.TLSKeyFile))
	}

	return errors.Join(errs...)
}

// validateBroker returns all validation errors for a single broker. Credential
// checks are skipped when auth.mode is missing or invalid because there are no
// meaningful credential rules to apply without a known mode.
func validateBroker(alias string, broker *BrokerConfig, productionMode bool) []error {
	var errs []error

	if broker.URL == "" {
		errs = append(errs, fmt.Errorf("broker %q: url is required", alias))
	} else if err := validateBrokerURL(broker.URL, productionMode); err != nil {
		errs = append(errs, fmt.Errorf("broker %q: %w", alias, err))
	}

	// Normalize auth mode (case-insensitive per story spec).
	broker.Auth.Mode = strings.ToLower(broker.Auth.Mode)

	if broker.Auth.Mode == "" {
		errs = append(errs, fmt.Errorf("broker %q: auth.mode is required (must be one of %v)", alias, validAuthModes))
		return errs
	}

	if !slices.Contains(validAuthModes, broker.Auth.Mode) {
		errs = append(errs, fmt.Errorf("broker %q: unsupported auth mode %q (must be one of %v)", alias, broker.Auth.Mode, validAuthModes))
		return errs
	}

	// Validate credentials are present based on auth mode.
	switch broker.Auth.Mode {
	case AuthModeBasic:
		if broker.Auth.Username == "" {
			errs = append(errs, fmt.Errorf("broker %q: username is required for basic auth", alias))
		}
		if broker.Auth.Password == "" {
			errs = append(errs, fmt.Errorf("broker %q: password is required for basic auth", alias))
		}
	case AuthModeBearer:
		if broker.Auth.Token == "" {
			errs = append(errs, fmt.Errorf("broker %q: token is required for bearer auth", alias))
		}
	}

	return errs
}

// ValidatePort checks that a port number is within the valid TCP range (1-65535).
// Used by both validate() and applyEnvOverrides() to avoid duplicating the range check.
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", port)
	}
	return nil
}

// validateBrokerURL checks that s is a well-formed URL with an http or https
// scheme and a host component. This is structural validation only — it does
// NOT verify that the host resolves, is reachable, or actually runs a SEMP
// endpoint. Those are deliberately runtime concerns surfaced on the first
// tool call. When productionMode is true, http:// is rejected — credentials
// would otherwise be transmitted in plaintext.
func validateBrokerURL(s string, productionMode bool) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if productionMode && u.Scheme == "http" {
		return fmt.Errorf("url scheme must be https to protect credentials in transit (got %q)", s)
	}
	if u.Host == "" {
		return fmt.Errorf("url must include a host, got %q", s)
	}
	return nil
}

// applyEnvOverrides checks for environment variable overrides and applies them
// to the config. This runs before validate() so overridden values are still
// range-checked. The overridden value is validated via ValidatePort internally.
func applyEnvOverrides(cfg *ServerConfig) error {
	if envPort := os.Getenv("MCP_SERVER_PORT"); envPort != "" {
		port, err := strconv.Atoi(envPort)
		if err != nil {
			return fmt.Errorf("invalid MCP_SERVER_PORT %q: must be a number", envPort)
		}
		if err := ValidatePort(port); err != nil {
			return fmt.Errorf("MCP_SERVER_PORT: %w", err)
		}
		cfg.Port = port
	}
	return nil
}
