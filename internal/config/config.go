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
	"bytes"
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
	// brokers is keyed by canonical (lowercase) alias after validate() runs.
	// Access from outside this package via Broker(alias) and BrokerAliases();
	// within the package, prefer the accessors so canonicalization is not
	// bypassed by accident.
	brokers       map[string]*BrokerConfig
	SEMP          SEMPConfig          // SEMP client settings
	Port          int                 // HTTP port the MCP server listens on
	LogLevel      string              // slog level name: "debug", "info", "warn", "error"
	MCPClientAuth MCPClientAuthConfig // authentication config for mcp client to server interactions
	TLSCertFile   string              // path to TLS certificate file (optional, enables HTTPS)
	TLSKeyFile    string              // path to TLS private key file (optional, requires TLSCertFile)
}

// Broker returns the broker config for alias (compared case-insensitively),
// and a bool indicating whether the alias is known.
func (c *ServerConfig) Broker(alias string) (*BrokerConfig, bool) {
	b, ok := c.brokers[strings.ToLower(alias)]
	return b, ok
}

// BrokerAliases returns the configured broker aliases in their original
// (display) casing, sorted alphabetically.
func (c *ServerConfig) BrokerAliases() []string {
	out := make([]string, 0, len(c.brokers))
	for _, b := range c.brokers {
		out = append(out, b.displayName)
	}
	slices.Sort(out)
	return out
}

type MCPClientAuthConfig struct {
	Issuer      string `yaml:"issuer"`       // IdP issuer URL — required when mode == "oauth"
	Audience    string `yaml:"audience"`     // Expected 'aud' claim value — required when mode == "oauth"
	DevToken    string `yaml:"dev_token"`    // Static token for dev — required when mode == "static"
	ResourceURL string `yaml:"resource_url"` // OAuth resource URL (e.g., "https://mcp.example.com/mcp") — required when mode == "oauth"
	// Mode selects the client authentication backend. One of AuthModeDisabled,
	// AuthModeStatic, or AuthModeOAuth. Required — no default. The validator
	// rejects configs that omit it. See docs/superpowers/specs/2026-05-20-client-auth-mode-design.md
	// for the design rationale.
	Mode string `yaml:"mode"`
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
	displayName        string     `yaml:"-"`                    // original alias casing from YAML, set by validate()
}

// DisplayName returns the broker alias in its original casing as written in
// the YAML config. Use this in logs, error messages, and any user-facing
// output so operators see the identifier they configured.
func (b *BrokerConfig) DisplayName() string {
	return b.displayName
}

// Auth mode constants for broker authentication (Hop 2: MCP Server → Broker).
const (
	AuthModeBasic  = "basic"
	AuthModeBearer = "bearer"
)

// validAuthModes is the allowlist of supported auth modes for broker connections.
// Add new modes (e.g., "oauth") here — validate() and error messages derive from this slice.
var validAuthModes = []string{AuthModeBasic, AuthModeBearer}

// Client authentication modes (Hop 1: MCP client → MCP server). Choosing one
// of these is mandatory; there is no default. Operational profile (https://
// enforcement, self-signed cert allowance, etc.) is derived from the mode via
// IsProductionMode(). Operational profile is mode-derived; do not add a
// separate dev-mode toggle on ServerConfig.
const (
	AuthModeDisabled = "disabled" // no client auth; every request passes through (dev only)
	AuthModeStatic   = "static"   // shared static dev token; constant-time compare (dev only)
	AuthModeOAuth    = "oauth"    // OAuth/OIDC JWT validation (production)
)

// validAuthClientModes is the allowlist for mcp_client_auth.mode. The validator
// rejects any other value. Add new modes here and extend the validate() switch.
var validAuthClientModes = []string{AuthModeDisabled, AuthModeStatic, AuthModeOAuth}

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
// The URL is routed through sanitizeURLString so any userinfo is stripped
// before reaching the log — defense in depth against logging a BrokerConfig
// before validateBrokerURL has had a chance to reject credentialed URLs.
// See docs/secure-logging-rules.md Rule 2.
func (b BrokerConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", sanitizeURLString(b.URL)),
		slog.Bool("insecure_skip_verify", b.InsecureSkipVerify),
		slog.String("auth_mode", b.Auth.Mode),
	)
}

// LogValue implements slog.LogValuer for MCPClientAuthConfig. It exposes the auth
// mode and OAuth configuration (issuer, audience, resource URL) but excludes
// DevToken to prevent credential leaks in log output. Mode is listed first
// because it is the most important operator-facing piece of information —
// operators need to confirm which auth mode the server loaded at startup.
// Issuer and ResourceURL are routed through sanitizeURLString for the same
// defense-in-depth reason as BrokerConfig.LogValue.
// See docs/secure-logging-rules.md Rule 2.
func (c MCPClientAuthConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("mode", c.Mode),
		slog.String("issuer", sanitizeURLString(c.Issuer)),
		slog.String("audience", c.Audience),
		slog.String("resource_url", sanitizeURLString(c.ResourceURL)),
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
	MaxConcurrentPerBroker int            `yaml:"max_concurrent_per_broker"` // transport MaxConnsPerHost per protocol client
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
	DevelopmentMode *bool                    `yaml:"development_mode"` // *bool so we can detect presence-in-YAML (deprecation warning); the value is ignored
	MCPClientAuth   MCPClientAuthConfig      `yaml:"mcp_client_auth"`
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

// yamlNodeValuePattern matches the backtick-quoted node values yaml.v3 embeds
// in type-error messages, e.g. "cannot unmarshal !!int `123456` into string".
var yamlNodeValuePattern = regexp.MustCompile("`[^`]*`")

// sanitizeYAMLError strips raw node values from a YAML decode error before it
// is wrapped and logged. yaml.v3 echoes the offending value when it lands in
// a mismatched target type — most plausibly a credential pulled into a
// non-string field by a wrong ${VAR} reference, since env substitution runs
// before decode. The secret would ride inside the error string where the
// slog ReplaceAttr redaction net cannot reach it. Line numbers and type
// diagnostics are preserved.
func sanitizeYAMLError(err error) error {
	return errors.New(yamlNodeValuePattern.ReplaceAllString(err.Error(), "`[redacted]`"))
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", sanitizeYAMLError(err))
	}

	if raw.DevelopmentMode != nil {
		slog.Warn("development_mode is deprecated and ignored; auth profile is now derived from mcp_client_auth.mode (one of disabled, static, oauth) — please remove development_mode from your config")
	}

	cfg := &ServerConfig{
		brokers:       raw.Brokers,
		SEMP:          raw.SEMP,
		Port:          raw.Port,
		LogLevel:      raw.LogLevel,
		MCPClientAuth: raw.MCPClientAuth,
		TLSCertFile:   raw.TLSCertFile,
		TLSKeyFile:    raw.TLSKeyFile,
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

// brokerAliasPattern enforces the broker alias contract: 1–63 chars, only
// letters/digits/hyphens, must start and end alphanumeric. Applied to the
// original casing as written in YAML.
var brokerAliasPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// isValidAlias reports whether s satisfies the broker alias contract.
func isValidAlias(s string) bool {
	return brokerAliasPattern.MatchString(s)
}

// validateAndCanonicalizeBrokers validates each alias against the contract,
// detects case-only collisions, sets displayName on every BrokerConfig, and
// returns a new map keyed by canonical (lowercase) alias. All errors are
// accumulated so operators see every issue in one run.
//
// What the returned canonical map contains:
//   - Structurally-valid, non-colliding entries (the happy case).
//   - Structurally-invalid entries (regex failures) — kept so that
//     validate()'s per-broker pass (URL, auth, credentials) can attach more
//     errors to them in the same startup attempt.
//
// What the returned canonical map omits:
//   - Nil broker entries (e.g. YAML `brokers: { prod: }`) — reported with a
//     standalone error and skipped to avoid nil-pointer derefs downstream.
//   - Case-collision losers — running per-broker validation on entries the
//     operator is about to rename would be noise; the collision error itself
//     already blocks startup.
//
// See the phase-3 block comment below for the rationale behind the
// kept-vs-omitted asymmetry.
func validateAndCanonicalizeBrokers(brokers map[string]*BrokerConfig) (map[string]*BrokerConfig, []error) {
	var errs []error
	canonical := make(map[string]*BrokerConfig, len(brokers))

	// Phase 1: per-alias structural validation; set displayName on all entries.
	// Nil broker values (e.g. YAML `brokers: { prod: }`) are reported here and
	// excluded from subsequent phases so we never dereference them downstream.
	for alias, broker := range brokers {
		if broker == nil {
			errs = append(errs, fmt.Errorf("broker %q: configuration block is empty (missing url, auth, etc.)", alias))
			continue
		}
		broker.displayName = alias
		if !isValidAlias(alias) {
			errs = append(errs, fmt.Errorf("broker alias %q is invalid: must be 1-63 characters, contain only letters, digits, and hyphens, and start and end with a letter or digit", alias))
		}
	}

	// Phase 2: case-collision detection (group by lowercase form).
	seen := make(map[string][]string, len(brokers))
	for alias := range brokers {
		lower := strings.ToLower(alias)
		seen[lower] = append(seen[lower], alias)
	}
	for _, originals := range seen {
		if len(originals) < 2 {
			continue
		}
		slices.Sort(originals)
		quoted := make([]string, len(originals))
		for i, o := range originals {
			quoted[i] = fmt.Sprintf("%q", o)
		}
		errs = append(errs, fmt.Errorf("broker aliases %s collide: aliases are compared case-insensitively, please rename one", strings.Join(quoted, " and ")))
	}

	// Phase 3: build canonical map. The asymmetry between how collision-losers
	// and structurally-invalid aliases are handled is deliberate.
	//
	// Collision-losers are dropped from the canonical map. The collision error
	// already blocks startup, so running per-broker validation on them would
	// surface downstream errors on entries the operator is about to rename —
	// added noise without value.
	//
	// Structurally-invalid aliases (regex failures) are kept in the canonical
	// map so phase 4 (per-broker validation) can also surface URL/auth/
	// credential errors on them in the same pass. This matches the broader
	// validate() intent of accumulating every issue in a single startup attempt.
	for alias, broker := range brokers {
		if broker == nil {
			continue // already reported in phase 1; never insert nil into canonical
		}
		lower := strings.ToLower(alias)
		if len(seen[lower]) > 1 {
			continue
		}
		canonical[lower] = broker
	}

	return canonical, errs
}

// substituteEnvVars replaces all ${VAR_NAME} occurrences in raw YAML bytes with
// the corresponding environment variable values. YAML comments are skipped —
// a ${VAR} reference inside a # comment has no effect on the parsed config and
// must not cause loading to fail (SOL-149904). Returns an error listing any
// referenced env vars that are not set (comments excluded). This runs before
// YAML parsing so any field can reference env vars.
func substituteEnvVars(data []byte) ([]byte, error) {
	var missing []string
	var result bytes.Buffer
	result.Grow(len(data))

	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		active, comment := splitYAMLComment(line)
		substituted := envVarPattern.ReplaceAllFunc(active, func(match []byte) []byte {
			varName := string(envVarPattern.FindSubmatch(match)[1])
			value, exists := os.LookupEnv(varName)
			if !exists {
				missing = append(missing, varName)
				return match // leave as-is, error reported after
			}
			return []byte(value)
		})
		result.Write(substituted)
		result.Write(comment)
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
	}

	return result.Bytes(), nil
}

// splitYAMLComment returns (active, comment) where active is the portion of
// line before any unquoted YAML comment marker (#) and comment is the rest
// (including the # itself). A # starts a comment when it is at the start of
// the line OR preceded by whitespace, AND not inside a single- or
// double-quoted string on the same line.
//
// Limitations: block scalars (|, >) treat # as literal text — this helper
// does not track block-scalar context. The broker MCP config schema uses only
// scalar values and nested structs, never block scalars, so this is acceptable.
// If a block-scalar field is ever added, extend this helper accordingly.
func splitYAMLComment(line []byte) (active, comment []byte) {
	inSingle := false
	inDouble := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"' && !inSingle:
			// Treat a preceding backslash as escaping the quote (heuristic,
			// not a full YAML lexer — double-backslash isn't unescaped here).
			if !inDouble || i == 0 || line[i-1] != '\\' {
				inDouble = !inDouble
			}
		case c == '\'' && !inDouble:
			// YAML escapes ' inside a single-quoted string as ''. A naive
			// toggle re-enters the string at the second quote, which leaves
			// the in/out state correct at any later #.
			inSingle = !inSingle
		case c == '#' && !inSingle && !inDouble && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i], line[i:]
		}
	}
	return line, nil
}

// validate checks that the config has all required fields and that values are
// within acceptable ranges. All validation errors are collected and returned as
// a single joined error so operators see every issue in one run instead of
// fixing them one-by-one across multiple reloads.
func validate(cfg *ServerConfig) error {
	var errs []error

	if len(cfg.brokers) == 0 {
		errs = append(errs, fmt.Errorf("at least one broker must be configured"))
	}

	canonical, aliasErrs := validateAndCanonicalizeBrokers(cfg.brokers)
	errs = append(errs, aliasErrs...)
	cfg.brokers = canonical

	for _, lower := range slices.Sorted(maps.Keys(cfg.brokers)) {
		broker := cfg.brokers[lower]
		errs = append(errs, validateBroker(broker, cfg.IsProductionMode())...)
		// Surface insecure_skip_verify=true at startup so operators see it
		// in triage logs without scraping per-request SEMP-client warns.
		if cfg.IsProductionMode() && broker.InsecureSkipVerify {
			slog.Warn("INSECURE: TLS verification disabled for broker",
				slog.String("broker", broker.DisplayName()))
		}
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

	// Validate client authentication configuration. mode is the single source
	// of truth for auth backend selection AND production-vs-dev operational
	// profile (via IsProductionMode). Required fields follow from the mode.
	// See docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
	//
	// Modes are tiered, not interleaved:
	//   - disabled / static: dev-only, http:// broker URLs allowed
	//   - oauth: production, https:// required everywhere
	cfg.MCPClientAuth.Mode = strings.ToLower(cfg.MCPClientAuth.Mode)
	switch cfg.MCPClientAuth.Mode {
	case "":
		errs = append(errs, fmt.Errorf("mcp_client_auth.mode is required (must be one of %v)", validAuthClientModes))
	case AuthModeDisabled:
		// no further required fields
	case AuthModeStatic:
		if cfg.MCPClientAuth.DevToken == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.dev_token is required when mcp_client_auth.mode is %q", AuthModeStatic))
		}
	case AuthModeOAuth:
		if cfg.MCPClientAuth.Issuer == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.issuer is required when mcp_client_auth.mode is %q", AuthModeOAuth))
		} else if err := validateBrokerURL(cfg.MCPClientAuth.Issuer, cfg.IsProductionMode()); err != nil {
			errs = append(errs, fmt.Errorf("mcp_client_auth.issuer: %w", err))
		}
		if cfg.MCPClientAuth.Audience == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.audience is required when mcp_client_auth.mode is %q", AuthModeOAuth))
		}
		if cfg.MCPClientAuth.ResourceURL == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.resource_url is required when mcp_client_auth.mode is %q", AuthModeOAuth))
		} else if err := validateBrokerURL(cfg.MCPClientAuth.ResourceURL, cfg.IsProductionMode()); err != nil {
			errs = append(errs, fmt.Errorf("mcp_client_auth.resource_url: %w", err))
		}
	default:
		errs = append(errs, fmt.Errorf("mcp_client_auth.mode %q is invalid (must be one of %v)", cfg.MCPClientAuth.Mode, validAuthClientModes))
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
func validateBroker(broker *BrokerConfig, productionMode bool) []error {
	var errs []error
	alias := broker.displayName

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

// IsProductionMode reports whether the server is configured for production
// (OAuth client auth). This is the single source of truth for production-vs-dev
// operational behavior — https:// enforcement on broker/issuer/resource URLs,
// self-signed cert allowance, etc. Operational profile is mode-derived;
// do not add a separate dev-mode toggle on ServerConfig.
func (c *ServerConfig) IsProductionMode() bool {
	return c.MCPClientAuth.Mode == AuthModeOAuth
}

// validateBrokerURL checks that s is a well-formed URL with an http or https
// scheme and a host component. This is structural validation only — it does
// NOT verify that the host resolves, is reachable, or actually runs a SEMP
// endpoint. Those are deliberately runtime concerns surfaced on the first
// tool call. When productionMode is true, http:// is rejected — credentials
// would otherwise be transmitted in plaintext.
//
// URLs with embedded userinfo (e.g. "https://user:pass@host") are rejected:
// credentials belong in the auth block, not the URL, and accepting them
// invites disclosure via logs, error messages, and any future field that
// echoes the URL back to operators.
func validateBrokerURL(s string, productionMode bool) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("url is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.User != nil {
		// Surface a clear error WITHOUT echoing the userinfo. Operators get
		// directed to the auth block; logs do not capture the credential.
		return fmt.Errorf("url must not include credentials (use the auth block instead), got %q", sanitizeURL(u))
	}
	if productionMode && u.Scheme == "http" {
		return fmt.Errorf("url scheme must be https to protect credentials in transit (got %q)", sanitizeURL(u))
	}
	if u.Host == "" {
		return fmt.Errorf("url must include a host, got %q", sanitizeURL(u))
	}
	return nil
}

// sanitizeURL returns u's string form with any userinfo stripped, so the
// result is safe to embed in error messages and log lines. The previous
// validateBrokerURL implementation formatted the raw URL via %q, which
// would have leaked any user:pass@ portion through the config-load error
// to slog.Error and onward to any log aggregator the operator forgot was
// retained. Defense in depth alongside the userinfo rejection above.
func sanitizeURL(u *url.URL) string {
	if u.User == nil {
		return u.String()
	}
	cp := *u
	cp.User = nil
	return cp.String()
}

// sanitizeURLString parses s and returns the URL with any userinfo stripped.
// Empty input passes through unchanged. Unparseable input is replaced with
// "<unparseable url>" rather than echoed back: we cannot prove the original
// string is credential-free, so the safe default is to drop it. Used by the
// LogValuer implementations on BrokerConfig and MCPClientAuthConfig.
func sanitizeURLString(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return "<unparseable url>"
	}
	return sanitizeURL(u)
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
