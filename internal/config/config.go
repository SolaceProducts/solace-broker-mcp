// Package config loads and validates the Solace Broker MCP Server configuration
// from a YAML file. It supports ${VAR_NAME} env var substitution in any YAML
// field, multiple brokers with independent credentials, and applies defaults
// from the defaults package for optional fields.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"gopkg.in/yaml.v3"
)

// ServerConfig holds the complete MCP server configuration, including all
// configured brokers and SEMP client settings.
type ServerConfig struct {
	Brokers     map[string]*BrokerConfig // broker alias → config
	SEMP        SEMPConfig               // SEMP client settings
	Port        int                      // HTTP port the MCP server listens on (1-65535)
	TLSCertFile string                   // path to TLS certificate file (optional, enables HTTPS)
	TLSKeyFile  string                   // path to TLS private key file (optional, requires TLSCertFile)
}

// BrokerConfig holds the connection and authentication configuration for a
// single Solace broker.
type BrokerConfig struct {
	URL           string     `yaml:"url"`             // SEMP API base URL (e.g., "https://broker:1943")
	TLSSkipVerify bool       `yaml:"tls_skip_verify"` // skip TLS cert verification (dev only)
	Auth          AuthConfig `yaml:"auth"`            // authentication config
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
		slog.Bool("tls_skip_verify", b.TLSSkipVerify),
		slog.String("auth_mode", b.Auth.Mode),
	)
}

// SEMPConfig holds settings that control how the MCP server communicates with
// brokers over the SEMP API.
type SEMPConfig struct {
	MaxConcurrentPerBroker int `yaml:"max_concurrent_per_broker"` // semaphore size per broker
	RequestTimeoutSeconds  int `yaml:"request_timeout_seconds"`   // HTTP request timeout for SEMP calls
}

// yamlConfig is the intermediate representation used for YAML unmarshalling.
// It mirrors the YAML file structure before being transformed into ServerConfig.
type yamlConfig struct {
	Brokers     map[string]*BrokerConfig `yaml:"brokers"`
	SEMP        SEMPConfig               `yaml:"semp"`
	Port        int                      `yaml:"port"`
	TLSCertFile string                   `yaml:"tls_cert_file"`
	TLSKeyFile  string                   `yaml:"tls_key_file"`
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
	data, err := os.ReadFile(path)
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
		Brokers:     raw.Brokers,
		SEMP:        raw.SEMP,
		Port:        raw.Port,
		TLSCertFile: raw.TLSCertFile,
		TLSKeyFile:  raw.TLSKeyFile,
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
	if cfg.SEMP.MaxConcurrentPerBroker == 0 {
		cfg.SEMP.MaxConcurrentPerBroker = defaults.DefaultMaxConcurrentPerBroker
	}
	if cfg.SEMP.RequestTimeoutSeconds == 0 {
		cfg.SEMP.RequestTimeoutSeconds = defaults.DefaultSEMPRequestTimeoutSeconds
	}
	// TLSSkipVerify is not defaulted here — Go's zero value for bool (false)
	// already matches the intended default (verify TLS certificates).
}

// loadEnvFile loads a .env file into the process environment. It checks two
// locations in order: the ENV_FILE environment variable (explicit path), then
// a .env file in the same directory as the config file (convention). If neither
// exists, it silently skips — .env files are optional. Variables already set in
// the environment are not overwritten, so real env vars (e.g., from CI/CD)
// take precedence over .env values.
func loadEnvFile(configPath string) {
	envPath := os.Getenv("ENV_FILE")
	if envPath == "" {
		envPath = filepath.Join(filepath.Dir(configPath), ".env")
	}
	data, err := os.ReadFile(envPath) //nolint:gosec // G703 — path from trusted config/env, not external user input
	if err != nil {
		return // .env file is optional — if it doesn't exist, silently skip
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
		value = strings.TrimSpace(value)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value) //nolint:gosec // G104 — os.Setenv only fails on Plan 9; safe to ignore
		}
	}

	slog.Info("loaded .env file",
		slog.String("path", envPath))
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

	for alias, broker := range cfg.Brokers {
		errs = append(errs, validateBroker(alias, broker)...)
	}

	if err := ValidatePort(cfg.Port); err != nil {
		errs = append(errs, err)
	}

	if cfg.SEMP.MaxConcurrentPerBroker < 0 {
		errs = append(errs, fmt.Errorf("semp.max_concurrent_per_broker must be > 0, got %d", cfg.SEMP.MaxConcurrentPerBroker))
	}

	if cfg.SEMP.RequestTimeoutSeconds < 0 {
		errs = append(errs, fmt.Errorf("semp.request_timeout_seconds must be > 0, got %d", cfg.SEMP.RequestTimeoutSeconds))
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
func validateBroker(alias string, broker *BrokerConfig) []error {
	var errs []error

	if broker.URL == "" {
		errs = append(errs, fmt.Errorf("broker %q: url is required", alias))
	} else if err := validateBrokerURL(broker.URL); err != nil {
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

// validateBrokerURL checks that s is a well-formed http or https URL with a
// host component. This is structural validation only — it does NOT verify
// that the host resolves, is reachable, or actually runs a SEMP endpoint.
// Those are deliberately runtime concerns surfaced on the first tool call.
// Both http and https are permitted; production-mode https-only enforcement
// will be added with the dev/prod flag from Story 1B.
func validateBrokerURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("url must include a host, got %q", s)
	}
	return nil
}

// applyEnvOverrides checks for environment variable overrides and applies them
// to the config. This runs after validation and credential resolution. The
// overridden value is validated via ValidatePort internally.
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
