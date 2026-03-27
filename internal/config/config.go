// Package config loads and validates the Solace Broker MCP Server configuration
// from a YAML file. It supports multiple brokers with independent credentials
// loaded from environment variables using per-broker env_prefix, and applies
// defaults from the defaults package for optional fields.
package config

import (
	"fmt"
	"log"
	"os"
	"regexp"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"gopkg.in/yaml.v3"
)

// envPrefixPattern validates that env_prefix values contain only uppercase
// letters, numbers, and underscores.
var envPrefixPattern = regexp.MustCompile(`^[A-Z0-9_]+$`)

// ServerConfig holds the complete MCP server configuration, including all
// configured brokers and SEMP client settings.
type ServerConfig struct {
	Brokers map[string]*BrokerConfig // broker alias → config
	SEMP    SEMPConfig               // SEMP client settings
	Port    string                   // HTTP port the MCP server listens on
}

// BrokerConfig holds the connection and authentication configuration for a
// single Solace broker.
type BrokerConfig struct {
	URL           string     `yaml:"url"`             // SEMP API base URL (e.g., "https://broker:1943")
	TLSSkipVerify bool       `yaml:"tls_skip_verify"` // skip TLS cert verification (dev only)
	EnvPrefix     string     `yaml:"env_prefix"`      // prefix for credential env vars ({EnvPrefix}_USERNAME, {EnvPrefix}_PASSWORD)
	Auth          AuthConfig `yaml:"auth"`             // authentication config
}

// AuthConfig holds the authentication credentials for a broker connection.
type AuthConfig struct {
	Method   string `yaml:"method"` // "basic" for this release
	Username string // populated from {EnvPrefix}_USERNAME env var
	Password string // populated from {EnvPrefix}_PASSWORD env var
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
	Brokers map[string]*BrokerConfig `yaml:"brokers"`
	SEMP    SEMPConfig               `yaml:"semp"`
	Port    string                   `yaml:"port"`
}

// LoadConfig reads a YAML configuration file from path, applies defaults for
// missing optional fields, validates the structure, resolves credentials from
// environment variables, and returns a ServerConfig ready for use.
func LoadConfig(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var raw yamlConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	cfg := &ServerConfig{
		Brokers: raw.Brokers,
		SEMP:    raw.SEMP,
		Port:    raw.Port,
	}

	applyDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	if err := resolveCredentials(cfg); err != nil {
		return nil, fmt.Errorf("resolving credentials: %w", err)
	}

	log.Printf("WARNING: env_prefix naming convention is provisional — only uppercase letters, numbers, and underscores allowed. This convention may change.")

	return cfg, nil
}

// resolveCredentials reads credentials from environment variables for each
// broker using the broker's env_prefix. For a broker with env_prefix "PROD_US",
// it reads PROD_US_USERNAME and PROD_US_PASSWORD.
func resolveCredentials(cfg *ServerConfig) error {
	for alias, broker := range cfg.Brokers {
		if broker.EnvPrefix == "" {
			return fmt.Errorf("broker %q: env_prefix is required", alias)
		}

		if !envPrefixPattern.MatchString(broker.EnvPrefix) {
			return fmt.Errorf("broker %q: env_prefix must contain only uppercase letters, numbers, and underscores, got %q", alias, broker.EnvPrefix)
		}

		usernameVar := broker.EnvPrefix + "_USERNAME"
		username, exists := os.LookupEnv(usernameVar)
		if !exists {
			return fmt.Errorf("broker %q: environment variable %s is not set (required by env_prefix %q)", alias, usernameVar, broker.EnvPrefix)
		}

		passwordVar := broker.EnvPrefix + "_PASSWORD"
		password, exists := os.LookupEnv(passwordVar)
		if !exists {
			return fmt.Errorf("broker %q: environment variable %s is not set (required by env_prefix %q)", alias, passwordVar, broker.EnvPrefix)
		}

		broker.Auth.Username = username
		broker.Auth.Password = password
	}
	return nil
}

// applyDefaults fills in missing optional fields from the defaults package.
func applyDefaults(cfg *ServerConfig) {
	if cfg.Port == "" {
		cfg.Port = defaults.DefaultPort
	}
	if cfg.SEMP.MaxConcurrentPerBroker == 0 {
		cfg.SEMP.MaxConcurrentPerBroker = defaults.DefaultMaxConcurrentPerBroker
	}
	if cfg.SEMP.RequestTimeoutSeconds == 0 {
		cfg.SEMP.RequestTimeoutSeconds = defaults.DefaultSEMPRequestTimeoutSeconds
	}
	for _, broker := range cfg.Brokers {
		if !broker.TLSSkipVerify {
			broker.TLSSkipVerify = defaults.DefaultTLSSkipVerify
		}
	}
}

// validate checks that the config has all required fields and that values are
// within acceptable ranges. Credential validation happens in resolveCredentials,
// not here — this function validates structure only.
func validate(cfg *ServerConfig) error {
	if len(cfg.Brokers) == 0 {
		return fmt.Errorf("at least one broker must be configured")
	}

	for alias, broker := range cfg.Brokers {
		if broker.URL == "" {
			return fmt.Errorf("broker %q: url is required", alias)
		}

		if broker.EnvPrefix == "" {
			return fmt.Errorf("broker %q: env_prefix is required", alias)
		}

		if !envPrefixPattern.MatchString(broker.EnvPrefix) {
			return fmt.Errorf("broker %q: env_prefix must contain only uppercase letters, numbers, and underscores, got %q", alias, broker.EnvPrefix)
		}

		// ASSUMPTION: defaulting to basic auth when method is not specified.
		// Only basic auth is supported in this PR. When OAuth is added, this
		// default may need to change or become an error.
		if broker.Auth.Method == "" {
			broker.Auth.Method = "basic"
		}

		if broker.Auth.Method != "basic" {
			return fmt.Errorf("broker %q: unsupported auth method %q (only \"basic\" is supported)", alias, broker.Auth.Method)
		}
	}

	if cfg.SEMP.MaxConcurrentPerBroker < 0 {
		return fmt.Errorf("semp.max_concurrent_per_broker must be > 0, got %d", cfg.SEMP.MaxConcurrentPerBroker)
	}

	if cfg.SEMP.RequestTimeoutSeconds < 0 {
		return fmt.Errorf("semp.request_timeout_seconds must be > 0, got %d", cfg.SEMP.RequestTimeoutSeconds)
	}

	return nil
}

