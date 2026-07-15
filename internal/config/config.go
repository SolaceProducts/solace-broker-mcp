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
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/banner"
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
	ListenAddress string              // host the HTTP server binds to. Empty => all interfaces. Defaulted to 127.0.0.1 unless mcp_client_auth.mode is oauth (see applyDefaults).
	MCPClientAuth MCPClientAuthConfig // Hop 1: authentication config for mcp client to server interactions
	BrokerOAuth   *BrokerOAuthConfig  // Hop 2: global OAuth IdP coordinates for broker token exchange. Required when any broker uses auth.mode: oauth; otherwise optional (provided-but-unused configs log a WARN at startup so operators can stage broker_oauth ahead of switching brokers to oauth mode).
	TLSCertFile   string              // path to TLS certificate file (optional, enables HTTPS)
	TLSKeyFile    string              // path to TLS private key file (optional, requires TLSCertFile)

	// TLSTerminatedUpstream acknowledges that TLS is terminated by an upstream
	// proxy or ingress, so the server may serve a plaintext listener under
	// mcp_client_auth.mode: oauth. Without it, OAuth mode with no cert/key is a
	// fatal config error (the listener would transmit client bearer tokens and
	// tool results in cleartext while validating as production). Honored only in
	// OAuth mode; ignored in the dev modes. See OAuthPlaintextListenerAcknowledged.
	TLSTerminatedUpstream bool

	// EnableWriteTools gates registration of every write/action tool (any tool
	// that is not read-only — e.g. delete-queue-messages, clear-queue-stats,
	// disconnect-client, clear-client-stats). Default false — write tools do
	// not appear in tools/list unless explicitly enabled. Secure-by-default for
	// trial / dev deployments.
	EnableWriteTools bool

	// Observability holds the feature flags and tunables for the observability
	// capabilities (correlation IDs, panic recovery, metrics, audit log,
	// tracing, saturation events). Capability flags load from OBS_* env vars in
	// applyEnvOverrides; numeric tunables parse from the YAML observability:
	// block and are defaulted in applyDefaults. See ObservabilityConfig.
	Observability ObservabilityConfig

	// AllowRemoteUnauthenticated opts in to binding a non-loopback address while
	// mcp_client_auth.mode is disabled. disabled mode enforces no client auth, so
	// a routable bind exposes unauthenticated MCP access backed by the broker
	// admin credential — validate() refuses it unless this is explicitly true.
	AllowRemoteUnauthenticated bool

	// AllowInsecureBrokerTLS opts in to a broker with insecure_skip_verify: true
	// while in production (oauth) mode. Disabling certificate verification lets a
	// MITM present any cert and capture the broker admin credential the server
	// attaches to every SEMP request — validate() refuses it in production unless
	// this is explicitly true. Ignored in dev modes, where self-signed brokers
	// are expected.
	AllowInsecureBrokerTLS bool
}

// BrokerOAuthConfig holds the global OAuth IdP coordinates the MCP server uses
// to obtain broker tokens via RFC 8693 token exchange. One IdP per deployment.
//
// ClientAuth is a discriminated union: the populated sub-block names the
// authentication method the MCP server uses with the IdP. See BrokerClientAuth
// and its sub-blocks (ClientSecretAuth, future PrivateKeyJWTAuth, etc.). Exactly
// one sub-block must be populated; the validator enforces this structurally.
//
// GrantType and AudienceParam are required string fields validated against
// their respective allowlists (validGrantTypes, validAudienceParams). No
// defaults — operators acknowledge each protocol choice explicitly.
type BrokerOAuthConfig struct {
	TokenURL      string           `yaml:"idp_token_endpoint"`      // IdP token endpoint (token-exchange POST target). YAML key uses "endpoint" to match the OAuth spec and OIDC Discovery JSON (`token_endpoint`); Go field keeps `URL` to match the language convention (golang.org/x/oauth2 also names its field TokenURL).
	ClientID      string           `yaml:"mcp_server_client_id"`    // MCP server's client_id registered at the IdP
	ClientAuth    BrokerClientAuth `yaml:"mcp_server_client_auth"`  // discriminated union; exactly one sub-block populated
	GrantType     string           `yaml:"grant_type"`              // required; must be in validGrantTypes
	AudienceParam string           `yaml:"audience_parameter_name"` // required; one of {audience, scope, resource}
}

// BrokerClientAuth is a discriminated union of OAuth client authentication
// methods the MCP server uses with the IdP at the token endpoint. Exactly
// one sub-block must be populated. The populated sub-block's name is the
// method (matching the IANA "OAuth Token Endpoint Authentication Methods"
// registry values). Each sub-block contains only the fields its method
// needs — the schema structurally enforces the method/credential coupling.
//
// V1 ships ClientSecretBasic and ClientSecretPost. Future methods land as
// new sibling sub-blocks added by follow-up tickets, purely additive:
//
//	PrivateKeyJWT *PrivateKeyJWTAuth `yaml:"private_key_jwt,omitempty"`
//	TLSClientAuth *TLSClientAuthConfig `yaml:"tls_client_auth,omitempty"`
//
// Existing operator configs using ClientSecretBasic continue to parse
// unchanged when new sub-blocks are added — additive evolution preserves
// the V1 schema contract forever.
type BrokerClientAuth struct {
	ClientSecretBasic *ClientSecretAuth `yaml:"client_secret_basic,omitempty"` // RFC 6749 §2.3 — Basic header
	ClientSecretPost  *ClientSecretAuth `yaml:"client_secret_post,omitempty"`  // RFC 6749 §2.3 — form body
}

// ClientSecretAuth holds the shared secret for the client_secret_basic and
// client_secret_post methods. Both methods use the same configuration shape
// (one secret string); they differ only on the wire (which OAuth parameter
// carries the secret), which the runtime selects from the populated
// sub-block's name.
type ClientSecretAuth struct {
	Secret string `yaml:"secret"` // MCP server's client_secret (use ${VAR_NAME} for env var)
}

// Client authentication method identifiers. These are the standard IANA "OAuth
// Token Endpoint Authentication Methods" strings, used both as the YAML keys
// for the BrokerClientAuth sub-blocks and as the return value of Method().
const (
	ClientAuthMethodSecretBasic = "client_secret_basic" // RFC 6749 §2.3 — Basic header
	ClientAuthMethodSecretPost  = "client_secret_post"  // RFC 6749 §2.3 — form body
)

// selectedMethod inspects which client_auth sub-block is populated and
// returns the method name when exactly one is set. If zero or multiple
// sub-blocks are populated, it returns "" along with operator-facing
// errors describing the structural violation.
//
// Used by:
//   - validateBrokerClientAuth — propagates errors to the validator chain.
//   - BrokerOAuthConfig.LogValue / BrokerClientAuth.LogValue — discards
//     errors; logs "" when the union is in a non-canonical state (validation
//     surfaces the real problem separately).
//
// Adding a new method requires three coordinated edits: a new field on
// BrokerClientAuth, a new if-block here adding it to `populated`, and a
// new entry in allowedClientAuthMethods.
func (b BrokerClientAuth) selectedMethod() (string, []error) {
	var populated []string
	if b.ClientSecretBasic != nil {
		populated = append(populated, ClientAuthMethodSecretBasic)
	}
	if b.ClientSecretPost != nil {
		populated = append(populated, ClientAuthMethodSecretPost)
	}
	switch len(populated) {
	case 0:
		return "", []error{fmt.Errorf(
			"broker_oauth.mcp_server_client_auth: at least one method sub-block is required (one of %v)",
			allowedClientAuthMethods())}
	case 1:
		return populated[0], nil
	default:
		return "", []error{fmt.Errorf(
			"broker_oauth.mcp_server_client_auth: only one method sub-block may be configured at a time; got %v",
			populated)}
	}
}

// allowedClientAuthMethods returns every method name the schema knows about,
// in stable order. Used in error messages to tell operators which sub-blocks
// the schema accepts. The list grows when follow-up tickets add new methods.
func allowedClientAuthMethods() []string {
	return []string{
		ClientAuthMethodSecretBasic,
		ClientAuthMethodSecretPost,
	}
}

// OAuth grant-type strings sent to the IdP token endpoint (Hop 2). V1 supports
// only RFC 8693 token exchange; Entra OBO (jwt-bearer) is tracked as follow-up.
const (
	// #nosec G101 -- public RFC 8693 grant-type URN, not a credential.
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" // RFC 8693
)

// validGrantTypes is the allowlist of grant types this version implements.
// Add an entry here when runtime support for the new grant type lands.
var validGrantTypes = []string{
	GrantTypeTokenExchange,
}

// audience_param values: which OAuth request parameter carries the per-broker
// audience value on the wire. The runtime uses this to drive its request-body
// composition per IdP family (RFC 8693 / Entra OBO / RFC 8707). The schema
// allowlist is open to all three even though V1 runtime support is limited;
// see the decisions doc for why "schema-flexible, validator-strict" applies.
const (
	AudienceParamAudience = "audience" // RFC 8693 default
	AudienceParamScope    = "scope"    // Entra OBO style (audience prefixed onto each scope)
	AudienceParamResource = "resource" // RFC 8707 resource indicator style
)

// validAudienceParams is the allowlist of audience-carrying parameter names
// accepted by the schema. Membership here does not imply V1 runtime support
// for that wire format — see the decisions doc.
var validAudienceParams = []string{
	AudienceParamAudience,
	AudienceParamScope,
	AudienceParamResource,
}

// LogValue implements slog.LogValuer for BrokerOAuthConfig. It exposes the
// non-secret fields and the resolved authentication method but deliberately
// excludes the secret material in nested ClientAuth sub-blocks. See
// docs/internal/secure-logging-rules.md Rule 2.
//
// LogValue is best-effort: any structural error from selectedMethod is
// discarded here because the validator surfaces it separately. If the union
// is in a non-canonical state at log time, the method field is logged as "".
func (b BrokerOAuthConfig) LogValue() slog.Value {
	method, _ := b.ClientAuth.selectedMethod()
	return slog.GroupValue(
		slog.String("idp_token_endpoint", sanitizeURLString(b.TokenURL)),
		slog.String("mcp_server_client_id", b.ClientID),
		slog.String("mcp_server_client_auth_method", method),
		slog.String("grant_type", b.GrantType),
		slog.String("audience_parameter_name", b.AudienceParam),
	)
}

// LogValue for BrokerClientAuth — exposes only the resolved method name, never
// the credential bytes inside the populated sub-block. Defense in depth.
func (b BrokerClientAuth) LogValue() slog.Value {
	method, _ := b.selectedMethod()
	return slog.GroupValue(
		slog.String("method", method),
	)
}

// LogValue for ClientSecretAuth — the entire purpose of this type is to hold a
// secret; expose nothing. Defense in depth.
func (c ClientSecretAuth) LogValue() slog.Value {
	return slog.GroupValue()
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

// Hop2OAuthActive reports whether the Hop-2 (MCP server ↔ broker) OAuth
// runtime should be constructed for this process. It is true only when
// ALL THREE preconditions hold:
//
//  1. ENABLE_UNRELEASED_BROKER_OAUTH is set truthy (the operator has
//     explicitly opted into the unreleased runtime for testing).
//  2. The global broker_oauth: block is populated (IdP coordinates
//     exist for token exchange).
//  3. At least one broker has auth.mode: oauth (there is actually
//     something for the runtime to do).
//
// If any of the three is missing, cmd/server/main.go builds no Hop-2
// resources — no IdP HTTP client, no token exchanger, no in-memory copy
// of the client secret. This is what turns the flag's meaning from "the
// runtime is compiled in" into "the runtime is live in this process."
//
// LIFECYCLE: at ship time, delete only the first precondition (the flag
// check). The other two remain — they are the correct final gate for the
// post-ship runtime, since building an exchanger for a config that
// declares broker_oauth: but does not consume it would be wasteful even
// then. See the LIFECYCLE comment on unreleasedBrokerOAuthEnabled.
func (c *ServerConfig) Hop2OAuthActive() bool {
	if !unreleasedBrokerOAuthEnabled() {
		return false
	}
	if c.BrokerOAuth == nil {
		return false
	}
	for _, b := range c.brokers {
		if b.Auth.Mode == AuthModeOAuth {
			return true
		}
	}
	return false
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

	// Pointer so absence-in-YAML is distinguishable from present-with-enabled:false;
	// drives I1 by shape and I3 via Enabled *bool.
	ToolAuthorization *ToolAuthorizationConfig `yaml:"tool_authorization"`
}

// ToolAuthorizationConfig holds the parsed tool_authorization YAML block.
// Field names mirror the Solace broker's OAuth profile
// (accessLevelGroupsClaimName, accessLevelGroups) for cross-product consistency.
type ToolAuthorizationConfig struct {
	// *bool so presence-in-YAML is distinguishable from default; enforces I3.
	Enabled           *bool               `yaml:"enabled"`
	GroupsClaimName   *string             `yaml:"groups_claim_name"`
	AccessLevelGroups map[string][]string `yaml:"access_level_groups"`
}

// LogValue implements slog.LogValuer for ToolAuthorizationConfig.
func (t ToolAuthorizationConfig) LogValue() slog.Value {
	enabled := "unset"
	if t.Enabled != nil {
		if *t.Enabled {
			enabled = "true"
		} else {
			enabled = "false"
		}
	}
	groupsClaimName := ""
	if t.GroupsClaimName != nil {
		groupsClaimName = *t.GroupsClaimName
	}
	return slog.GroupValue(
		slog.String("enabled", enabled),
		slog.String("groups_claim_name", groupsClaimName),
	)
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
var validAuthModes = []string{AuthModeBasic, AuthModeBearer, AuthModeOAuth}

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
	Mode     string `yaml:"mode"`     // "basic", "bearer", or "oauth"
	Username string `yaml:"username"` // basic auth username (use ${VAR_NAME} for env var)
	Password string `yaml:"password"` // basic auth password (use ${VAR_NAME} for env var)
	Token    string `yaml:"token"`    // bearer token (use ${VAR_NAME} for env var)

	// OAuth-mode field. Used when Mode == "oauth"; ignored otherwise.
	// Optional in V1 — the broker's OAuth profile may have audience
	// validation disabled. The runtime omits the field from the
	// token-exchange request when empty.
	Audience string `yaml:"audience,omitempty"`
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
	Brokers               map[string]*BrokerConfig `yaml:"brokers"`
	SEMP                  SEMPConfig               `yaml:"semp"`
	Port                  int                      `yaml:"port"`
	ListenAddress         string                   `yaml:"listen_address"`
	LogLevel              string                   `yaml:"log_level"`
	DevelopmentMode       *bool                    `yaml:"development_mode"` // *bool so we can detect presence-in-YAML (deprecation warning); the value is ignored
	MCPClientAuth         MCPClientAuthConfig      `yaml:"mcp_client_auth"`
	BrokerOAuth           *BrokerOAuthConfig       `yaml:"broker_oauth"`
	TLSCertFile           string                   `yaml:"tls_cert_file"`
	TLSKeyFile            string                   `yaml:"tls_key_file"`
	TLSTerminatedUpstream bool                     `yaml:"tls_terminated_upstream"` // acknowledges upstream TLS termination; allows a plaintext listener under mcp_client_auth.mode: oauth
	EnableWriteTools      bool                     `yaml:"enable_write_tools"`      // default false; gates registration of write/action tools
	Observability         ObservabilityConfig      `yaml:"observability"`           // numeric tunables parse here; capability flags come from OBS_* env vars (see ObservabilityConfig)

	AllowRemoteUnauthenticated bool `yaml:"allow_remote_unauthenticated"` // opt-in to a non-loopback bind under mcp_client_auth.mode: disabled
	AllowInsecureBrokerTLS     bool `yaml:"allow_insecure_broker_tls"`    // opt-in to a broker insecure_skip_verify: true under mcp_client_auth.mode: oauth
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
		brokers:               raw.Brokers,
		SEMP:                  raw.SEMP,
		Port:                  raw.Port,
		ListenAddress:         raw.ListenAddress,
		LogLevel:              raw.LogLevel,
		MCPClientAuth:         raw.MCPClientAuth,
		BrokerOAuth:           raw.BrokerOAuth,
		TLSCertFile:           raw.TLSCertFile,
		TLSKeyFile:            raw.TLSKeyFile,
		TLSTerminatedUpstream: raw.TLSTerminatedUpstream,
		EnableWriteTools:      raw.EnableWriteTools,
		Observability:         raw.Observability,

		AllowRemoteUnauthenticated: raw.AllowRemoteUnauthenticated,
		AllowInsecureBrokerTLS:     raw.AllowInsecureBrokerTLS,
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
	// Trim before the empty-check so a whitespace-only value resolves to the
	// secure default rather than an unbindable address — matches the file-wide
	// rule applied to operator-supplied strings (see the dev_token note in
	// validate()).
	cfg.ListenAddress = strings.TrimSpace(cfg.ListenAddress)
	// Bind loopback-only by default unless the operator is running production
	// OAuth, which is the only mode meant to be network-reachable out of the
	// box. The dev modes (disabled, static) must opt in to a routable bind by
	// setting listen_address explicitly. Mode is not normalized until validate(),
	// so compare case-insensitively here.
	if cfg.ListenAddress == "" && strings.ToLower(cfg.MCPClientAuth.Mode) != AuthModeOAuth {
		cfg.ListenAddress = defaults.DefaultLoopbackListenAddress
	}
	// Trim TLS cert/key paths for the same reason as listen_address above: a
	// whitespace-only value (an unresolved ${VAR}, or a secret mounted with a
	// trailing newline) must read as "no path". Otherwise it stays non-empty,
	// silently satisfies the oauth plaintext-listener guard in validate() with no
	// acknowledgment, and then fails deep inside ListenAndServeTLS instead of
	// clearly at LoadConfig. Normalizing here means every downstream reader — the
	// cert/key pairing check, OAuthPlaintextListenerAcknowledged, and startServer —
	// agrees on the "TLS is off" signal.
	cfg.TLSCertFile = strings.TrimSpace(cfg.TLSCertFile)
	cfg.TLSKeyFile = strings.TrimSpace(cfg.TLSKeyFile)
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

	if toolAuthorizationFeatureEnabled() {
		applyToolAuthorizationDefaults(cfg)
	}

	// Observability numeric tunables (saturation_threshold_ms, etc.). The
	// capability flags are env-driven and applied in applyEnvOverrides instead.
	applyObservabilityDefaults(cfg)
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

	// Normalize mcp_client_auth.mode up front, before ANY consumer runs. mode
	// is the single source of truth for the production-vs-dev profile via
	// IsProductionMode(), which compares against the lowercase constants. The
	// broker-TLS refusal below (and other IsProductionMode() checks) must see
	// the normalized value; otherwise a config with "OAuth"/"OAUTH" would not
	// register as production and would silently bypass the insecure-TLS refusal.
	// Keep this ahead of the broker loop — the mode-specific required-field
	// switch further down relies on it too.
	cfg.MCPClientAuth.Mode = strings.ToLower(cfg.MCPClientAuth.Mode)

	if len(cfg.brokers) == 0 {
		errs = append(errs, fmt.Errorf("at least one broker must be configured"))
	}

	canonical, aliasErrs := validateAndCanonicalizeBrokers(cfg.brokers)
	errs = append(errs, aliasErrs...)
	cfg.brokers = canonical

	// oauthBrokerCount tracks how many brokers were configured with the not-
	// yet-supported oauth mode. Used at end of validate() to emit the loud
	// operator-facing banner separately from the joined error.
	oauthBrokerCount := 0

	for _, lower := range slices.Sorted(maps.Keys(cfg.brokers)) {
		broker := cfg.brokers[lower]
		errs = append(errs, validateBroker(broker, cfg.IsProductionMode())...)
		if broker.Auth.Mode == AuthModeOAuth {
			oauthBrokerCount++
		}
		// In production (oauth) mode, https:// is enforced but a disabled cert
		// check still exposes the broker admin credential to a MITM. Refuse it
		// unless the operator explicitly accepts the risk (mirrors the
		// allow_remote_unauthenticated guard). When accepted, keep the startup
		// WARN so the insecure setting stays visible in triage logs.
		if cfg.IsProductionMode() && broker.InsecureSkipVerify {
			if !cfg.AllowInsecureBrokerTLS {
				errs = append(errs, fmt.Errorf(
					"broker %q sets insecure_skip_verify: true while mcp_client_auth.mode is %q: TLS is enforced but certificate verification is disabled, exposing the broker admin credential to a man-in-the-middle. Use a trusted certificate, or set allow_insecure_broker_tls: true to accept the risk",
					broker.DisplayName(), AuthModeOAuth))
			} else {
				slog.Warn("INSECURE: TLS verification disabled for broker",
					slog.String("broker", broker.DisplayName()))
			}
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
	// mode was normalized to lowercase at the top of validate() so every check
	// above (broker TLS, IsProductionMode) and the switch below agree on it.
	switch cfg.MCPClientAuth.Mode {
	case "":
		errs = append(errs, fmt.Errorf("mcp_client_auth.mode is required (must be one of %v)", validAuthClientModes))
	case AuthModeDisabled:
		// no further required fields
	case AuthModeStatic:
		// Trim before comparing: a credential resolved from ${VAR} to
		// whitespace-only would otherwise pass startup validation and fail
		// every request at runtime with a 401. Apply this rule to every
		// operator-supplied required string in this file — see also the
		// basic/bearer credential checks in validateBroker.
		if strings.TrimSpace(cfg.MCPClientAuth.DevToken) == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.dev_token is required when mcp_client_auth.mode is %q", AuthModeStatic))
		}
	case AuthModeOAuth:
		if strings.TrimSpace(cfg.MCPClientAuth.Issuer) == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.issuer is required when mcp_client_auth.mode is %q", AuthModeOAuth))
		} else if err := validateBrokerURL(cfg.MCPClientAuth.Issuer, cfg.IsProductionMode()); err != nil {
			errs = append(errs, fmt.Errorf("mcp_client_auth.issuer: %w", err))
		}
		if strings.TrimSpace(cfg.MCPClientAuth.Audience) == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.audience is required when mcp_client_auth.mode is %q", AuthModeOAuth))
		}
		if strings.TrimSpace(cfg.MCPClientAuth.ResourceURL) == "" {
			errs = append(errs, fmt.Errorf("mcp_client_auth.resource_url is required when mcp_client_auth.mode is %q", AuthModeOAuth))
		} else if err := validateBrokerURL(cfg.MCPClientAuth.ResourceURL, cfg.IsProductionMode()); err != nil {
			errs = append(errs, fmt.Errorf("mcp_client_auth.resource_url: %w", err))
		}
		// Listener transport: OAuth mode is production, so a plaintext listener
		// must be an explicit choice. With no server-side TLS and no upstream-
		// termination acknowledgment the listener would carry client bearer
		// tokens and tool results in cleartext while validating as production.
		// resource_url being https:// does not imply the listener is encrypted.
		// TLSCertFile == "" is the "TLS is off" signal (same convention as
		// StaticTokenExposedCleartext); paths are trimmed in applyDefaults so a
		// whitespace-only value counts as absent here. A half-configured cert/key
		// pair is caught independently by the pairing check below.
		if cfg.TLSCertFile == "" && !cfg.TLSTerminatedUpstream {
			errs = append(errs, fmt.Errorf(
				"mcp_client_auth.mode %q serves a plaintext listener with no TLS: "+
					"provide tls_cert_file and tls_key_file to terminate TLS at the server, "+
					"or set tls_terminated_upstream: true to acknowledge TLS is terminated "+
					"by an upstream proxy/ingress", AuthModeOAuth))
		}
	default:
		errs = append(errs, fmt.Errorf("mcp_client_auth.mode %q is invalid (must be one of %v)", cfg.MCPClientAuth.Mode, validAuthClientModes))
	}

	if toolAuthorizationFeatureEnabled() {
		errs = append(errs, validateToolAuthorization(cfg)...)
	}

	// listen_address: validate form, then guard the disabled-mode exposure.
	// An explicit value must be an IP or "localhost" so an unbindable host fails
	// at startup rather than deep inside ListenAndServe. Empty is allowed (it
	// means all interfaces) but only ever reaches here under oauth, since
	// applyDefaults fills loopback for the dev modes.
	if cfg.ListenAddress != "" && !isLoopbackHost(cfg.ListenAddress) && net.ParseIP(cfg.ListenAddress) == nil {
		errs = append(errs, fmt.Errorf("listen_address %q is invalid (must be an IP address or %q)", cfg.ListenAddress, "localhost"))
	} else if cfg.MCPClientAuth.Mode == AuthModeDisabled && !isLoopbackHost(cfg.ListenAddress) && !cfg.AllowRemoteUnauthenticated {
		// Only disabled is guarded here. static can bind a non-loopback address
		// without the override BY DESIGN: it still authenticates every request
		// against the shared dev token, so a routable bind is a deliberate
		// (token-gated) choice rather than wide-open exposure. disabled has no
		// such gate — a non-loopback bind would expose unauthenticated MCP access,
		// backed by the broker admin credential the server holds, to the network.
		// Refuse unless the operator explicitly accepts the risk.
		errs = append(errs, fmt.Errorf(
			"listen_address %q binds a non-loopback interface while mcp_client_auth.mode is %q (no client authentication): this exposes unauthenticated MCP access backed by the broker admin credential to the network. Bind 127.0.0.1, switch to mcp_client_auth.mode: oauth, or set allow_remote_unauthenticated: true to accept the risk",
			cfg.ListenAddress, AuthModeDisabled))
	}

	// TLS: both cert and key must be provided together, or neither.
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		errs = append(errs, fmt.Errorf("both tls_cert_file and tls_key_file must be provided together; got cert=%q, key=%q", cfg.TLSCertFile, cfg.TLSKeyFile))
	}

	// Hop 2 OAuth IdP block. Runs after per-broker validation so any oauth-mode
	// broker has already been examined; this validator inspects the global
	// block and its relationship with the brokers' auth modes.
	errs = append(errs, validateBrokerOAuthConfig(cfg)...)

	// OAuth-related operator-facing surface. Two invariants compete for the
	// operator's attention when at least one broker uses auth.mode: oauth:
	//
	//   1) The OAuth-not-supported guard (temporary): while the OAuth
	//      runtime is not yet wired, every oauth-mode broker is rejected at
	//      startup. This is the ONLY message operators should see for that
	//      config shape — clear, one direction of remediation ("use basic
	//      or bearer until OAuth-on-brokers ships").
	//
	//   2) The Hop 1 / Hop 2 alignment invariant (permanent): when Hop 2
	//      OAuth is in use, mcp_client_auth.mode must also be oauth so the
	//      MCP server has an agent token to exchange for a broker token.
	//
	// Both error AND banner for the alignment invariant are gated behind
	// "the OAuth-not-supported guard did NOT fire." While the guard is
	// active, layering the alignment remediation on top would point
	// operators at a second remediation path for a feature that does not
	// yet run — noise. The validator function validateHop1Hop2Alignment
	// stays callable and is exercised by TestValidateHop1Hop2Alignment_Direct
	// so the invariant does not rot while it is sleeping in validate().
	//
	// FLAG CONTRACT: ENABLE_UNRELEASED_BROKER_OAUTH does not change
	// schema/shape validation — malformed OAuth YAML is rejected either
	// way. It only gates the temporary not-yet-supported guard and the
	// banner branch below (see the if/else on unreleasedBrokerOAuthEnabled).
	// Runtime construction is a separate concern; see ServerConfig.Hop2OAuthActive.
	//
	// LIFECYCLE: when the OAuth runtime ships (SOL-150070 follow-up sub-
	// tickets), delete the whole `if oauthBrokerCount > 0` arm — the
	// `else if` then runs unconditionally and the alignment check
	// becomes the operator-facing surface for Hop 1 / Hop 2 mismatches.
	// See banner.LogOAuthNotSupported doc-comment for the full removal
	// checklist.
	if oauthBrokerCount > 0 && !unreleasedBrokerOAuthEnabled() {
		banner.LogOAuthNotSupported(oauthBrokerCount)
	} else if err := validateHop1Hop2Alignment(cfg); err != nil {
		errs = append(errs, err)
		banner.LogHop2WithoutHop1(countHop2Brokers(cfg), cfg.MCPClientAuth.Mode)
	}

	return errors.Join(errs...)
}

// countHop2Brokers returns the number of brokers configured with
// auth.mode: oauth. Identical, while the OAuth-not-supported guard is
// active, to the oauthBrokerCount computed in validate(); kept as a
// separate helper because the two counters diverge once the guard is
// removed (oauthBrokerCount goes away; the Hop 2 count remains).
func countHop2Brokers(cfg *ServerConfig) int {
	n := 0
	for _, b := range cfg.brokers {
		if b.Auth.Mode == AuthModeOAuth {
			n++
		}
	}
	return n
}

// validateHop1Hop2Alignment enforces the structural invariant that Hop 2
// applyToolAuthorizationDefaults applies Q1 and Q3 defaults for the
// tool_authorization block. Called only when ENABLE_TOOL_AUTHORIZATION is set.
func applyToolAuthorizationDefaults(cfg *ServerConfig) {
	// Q1: synthesize an empty ToolAuthorizationConfig when the block is omitted
	// in oauth mode, so the I3 validator arm always sees a non-nil pointer.
	if strings.ToLower(cfg.MCPClientAuth.Mode) == AuthModeOAuth && cfg.MCPClientAuth.ToolAuthorization == nil {
		cfg.MCPClientAuth.ToolAuthorization = &ToolAuthorizationConfig{Enabled: nil}
	}
	// Q3: default GroupsClaimName to "groups" when the block is present but
	// the field is omitted (nil), matching the Solace broker's own default.
	if cfg.MCPClientAuth.ToolAuthorization != nil && cfg.MCPClientAuth.ToolAuthorization.GroupsClaimName == nil {
		def := "groups"
		cfg.MCPClientAuth.ToolAuthorization.GroupsClaimName = &def
	}
}

// validateToolAuthorization checks the tool_authorization config block for
// invariant violations (I1, I3) and structural coherence (Q3a, Q4, Q7).
// Called only when ENABLE_TOOL_AUTHORIZATION is set.
func validateToolAuthorization(cfg *ServerConfig) []error {
	var errs []error

	// I1 (Q14–Q16): tool_authorization is only legal under oauth mode.
	if cfg.MCPClientAuth.Mode != AuthModeOAuth && cfg.MCPClientAuth.ToolAuthorization != nil {
		errs = append(errs, fmt.Errorf(
			`mcp_client_auth.tool_authorization is only supported when mcp_client_auth.mode is "oauth" (currently: %q); either set mode to "oauth" or remove the tool_authorization block`,
			cfg.MCPClientAuth.Mode))
	}

	// I3 (Q17–Q18): in oauth mode, enabled must be set explicitly.
	if cfg.MCPClientAuth.Mode == AuthModeOAuth && cfg.MCPClientAuth.ToolAuthorization != nil && cfg.MCPClientAuth.ToolAuthorization.Enabled == nil {
		errs = append(errs, fmt.Errorf(
			`mcp_client_auth.tool_authorization.enabled must be set explicitly to true or false when mcp_client_auth.mode is "oauth"`))
	}

	// Q3a: groups_claim_name explicitly set to empty or whitespace-only.
	if cfg.MCPClientAuth.ToolAuthorization != nil &&
		cfg.MCPClientAuth.ToolAuthorization.GroupsClaimName != nil &&
		strings.TrimSpace(*cfg.MCPClientAuth.ToolAuthorization.GroupsClaimName) == "" {
		errs = append(errs, fmt.Errorf(
			`mcp_client_auth.tool_authorization.groups_claim_name must be a non-blank string when set; omit the field to accept the default ("groups")`))
	}

	// Structural validation of access_level_groups when the block is present.
	if ta := cfg.MCPClientAuth.ToolAuthorization; ta != nil {
		// Q4: enabled: true with empty access_level_groups is a config error.
		if ta.Enabled != nil && *ta.Enabled && len(ta.AccessLevelGroups) == 0 {
			errs = append(errs, fmt.Errorf(
				"mcp_client_auth.tool_authorization.access_level_groups is required when mcp_client_auth.tool_authorization.enabled is true"))
		}

		// Q7: empty or whitespace-only group name is a config coherence error.
		for groupName := range ta.AccessLevelGroups {
			if strings.TrimSpace(groupName) == "" {
				errs = append(errs, fmt.Errorf(
					"mcp_client_auth.tool_authorization.access_level_groups: group name cannot be empty or whitespace-only"))
				break
			}
		}
	}

	return errs
}

// OAuth on any broker requires Hop 1 OAuth on the MCP client auth. Returns
// nil when the invariant holds (no Hop 2 brokers, or Hop 1 mode is oauth)
// and an operator-facing error otherwise.
//
// This validator is a PURE function — it inspects the config and returns
// an error or nil. Whether validate() actually calls it and surfaces the
// error to operators is decided at the call site. While the
// OAuth-not-supported guard is active, validate() does NOT call this
// validator (the alignment branch is gated behind the guard so operators
// get a single remediation path). The invariant is still exercised today
// by TestValidateHop1Hop2Alignment_Direct, which calls this function
// directly with crafted configs, so the alignment logic does not rot
// while it is sleeping in validate().
func validateHop1Hop2Alignment(cfg *ServerConfig) error {
	if cfg.MCPClientAuth.Mode == AuthModeOAuth {
		return nil
	}
	n := countHop2Brokers(cfg)
	if n == 0 {
		return nil
	}
	subject := "1 broker has"
	if n != 1 {
		subject = fmt.Sprintf("%d brokers have", n)
	}
	return fmt.Errorf("mcp_client_auth.mode is %q but %s auth.mode: oauth; the MCP server needs the agent's token (received via mcp_client_auth) to obtain a broker token, so mcp_client_auth.mode must be oauth", cfg.MCPClientAuth.Mode, subject)
}

// validateBroker returns all validation errors for a single broker. Credential
// checks are skipped when auth.mode is missing or invalid because there are no
// meaningful credential rules to apply without a known mode.
func validateBroker(broker *BrokerConfig, productionMode bool) []error {
	var errs []error
	alias := broker.displayName

	if strings.TrimSpace(broker.URL) == "" {
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
		// Trim before comparing: a credential resolved from ${VAR} to
		// whitespace-only would otherwise pass startup validation and fail
		// every request at runtime with a 401.
		if strings.TrimSpace(broker.Auth.Username) == "" {
			errs = append(errs, fmt.Errorf("broker %q: username is required for basic auth", alias))
		}
		if strings.TrimSpace(broker.Auth.Password) == "" {
			errs = append(errs, fmt.Errorf("broker %q: password is required for basic auth", alias))
		}
	case AuthModeBearer:
		// Trim before comparing — see basic-auth note above.
		if strings.TrimSpace(broker.Auth.Token) == "" {
			errs = append(errs, fmt.Errorf("broker %q: token is required for bearer auth", alias))
		}
	case AuthModeOAuth:
		// Per-broker OAuth params. The global broker_oauth block (IdP
		// coordinates) is validated separately in validateBrokerOAuthConfig.
		//
		// Audience is optional in V1: the broker's OAuth profile may have
		// audience validation disabled (resourceServerValidateAudienceEnabled
		// is configurable per the SEMP v2 OauthProfile). The runtime omits the
		// audience parameter from the token-exchange request when empty.
		// When SET, reject whitespace-only — a ${VAR} resolving to "   "
		// would silently land as the audience claim and break every
		// token-exchange request.
		if broker.Auth.Audience != "" && strings.TrimSpace(broker.Auth.Audience) == "" {
			errs = append(errs, fmt.Errorf("broker %q: auth.audience is empty or whitespace-only", alias))
		}

		// RUNTIME GUARD: schema accepts oauth mode (and the OAuth fields
		// above) so configs that target the eventual OAuth runtime can be
		// staged and validated structurally today. The runtime itself is
		// implemented (OAuthAuthenticator + Exchanger + broker wiring) but
		// kept gated until end-to-end validation against real IdPs is done.
		// Reject at startup with an actionable message rather than letting
		// the failure land on the first SEMP request. This block will be
		// removed by the sub-ticket that ships broker OAuth for real.
		// See docs/superpowers/plans/oauth-token-exchange/SOL-150796-T2-config-schema.md
		// for the rationale (no feature flag, removed when runtime lands).
		//
		// Bypass for manual E2E testing: ENABLE_UNRELEASED_BROKER_OAUTH=true
		// skips this error. See unreleasedBrokerOAuthEnabled — the WARN it
		// triggers at startup is the honest counter-signal.
		if !unreleasedBrokerOAuthEnabled() {
			errs = append(errs, fmt.Errorf(
				"broker %q: auth.mode %q is recognized but not yet supported in this version; "+
					"use basic or bearer for now",
				alias, AuthModeOAuth))
		}
	}

	return errs
}

// validateBrokerOAuthConfig validates the global broker_oauth block and its
// relationship with per-broker auth modes. Levels 1–3 of the validation tiers
// defined in architecture-plan.md Decision 9: structural (block presence vs.
// usage), required-field (idp_token_endpoint/mcp_server_client_id/client_secret non-empty), and
// semantic (idp_token_endpoint parses as URL, https enforced in production). No live
// IdP probing — that is deliberately deferred per Decision 9.
//
// Three cross-block rules are enforced:
//
//  1. If any broker has auth.mode == AuthModeOAuth and no broker_oauth block
//     is configured, return an error (the runtime path will need IdP
//     coordinates to perform token exchange).
//  2. If the broker_oauth block is configured but no broker uses oauth mode,
//     log a WARN. This is operator-visible noise, not a fatal error — the
//     block may be staged in advance of switching brokers to oauth mode.
//  3. If the broker_oauth block is configured, every field is required and
//     idp_token_endpoint must be a valid URL (https in production mode).
//
// Returns the accumulated errors as a slice for the caller (validate) to
// errors.Join.
func validateBrokerOAuthConfig(cfg *ServerConfig) []error {
	var errs []error

	anyBrokerUsesOAuth := false
	for _, b := range cfg.brokers {
		if b.Auth.Mode == AuthModeOAuth {
			anyBrokerUsesOAuth = true
			break
		}
	}

	if cfg.BrokerOAuth == nil {
		if anyBrokerUsesOAuth {
			errs = append(errs, fmt.Errorf("broker_oauth block is required when any broker uses auth.mode: %q", AuthModeOAuth))
		}
		return errs
	}

	if !anyBrokerUsesOAuth {
		slog.Warn("broker_oauth provided but no broker uses oauth mode",
			slog.Int("broker_count", len(cfg.brokers)))
	}

	// Universal required fields — needed regardless of which mcp_server_client_auth
	// method is configured.
	if strings.TrimSpace(cfg.BrokerOAuth.TokenURL) == "" {
		errs = append(errs, fmt.Errorf("broker_oauth.idp_token_endpoint is required"))
	} else if err := validateBrokerURL(cfg.BrokerOAuth.TokenURL, cfg.IsProductionMode()); err != nil {
		errs = append(errs, fmt.Errorf("broker_oauth.idp_token_endpoint: %w", err))
	}
	if strings.TrimSpace(cfg.BrokerOAuth.ClientID) == "" {
		errs = append(errs, fmt.Errorf("broker_oauth.mcp_server_client_id is required"))
	}

	// grant_type and audience_param: required, no defaults. Operators must
	// explicitly acknowledge each protocol choice — see the decisions doc for
	// the rationale on removing defaults from these discriminator fields.
	if cfg.BrokerOAuth.GrantType == "" {
		errs = append(errs, fmt.Errorf("broker_oauth.grant_type is required (must be one of %v)", validGrantTypes))
	} else if !slices.Contains(validGrantTypes, cfg.BrokerOAuth.GrantType) {
		errs = append(errs, fmt.Errorf(
			"broker_oauth.grant_type %q is not supported in this version (must be one of %v); "+
				"other grant types (e.g. Entra OBO's jwt-bearer) are tracked as follow-up work",
			cfg.BrokerOAuth.GrantType, validGrantTypes))
	}

	if cfg.BrokerOAuth.AudienceParam == "" {
		errs = append(errs, fmt.Errorf("broker_oauth.audience_parameter_name is required (must be one of %v)", validAudienceParams))
	} else if !slices.Contains(validAudienceParams, cfg.BrokerOAuth.AudienceParam) {
		errs = append(errs, fmt.Errorf(
			"broker_oauth.audience_parameter_name %q is invalid (must be one of %v)",
			cfg.BrokerOAuth.AudienceParam, validAudienceParams))
	}

	// mcp_server_client_auth is a discriminated union: exactly one sub-block populated.
	// The populated sub-block names the method (one of the IANA "OAuth Token
	// Endpoint Authentication Methods" strings); the sub-block's required
	// fields are checked below in the per-method validators.
	errs = append(errs, validateBrokerClientAuth(cfg.BrokerOAuth.ClientAuth)...)

	return errs
}

// validateBrokerClientAuth enforces the discriminated-union shape of the
// mcp_server_client_auth block: exactly one sub-block populated, with that sub-block's
// required fields non-empty.
//
// Adding a new client-auth method (e.g., private_key_jwt) to V1's supported
// set MUST come with three coordinated edits in the same PR:
//  1. A new field on BrokerClientAuth pointing at the new method's struct.
//  2. An entry in selectedMethod's if-chain and in allowedClientAuthMethods
//     so the "exactly one populated" rule and error messages recognize the
//     new method.
//  3. A per-method validation case below that enforces the new method's
//     required fields. The default branch catches the case where the
//     schema knows about the method but the validator does not.
func validateBrokerClientAuth(cfg BrokerClientAuth) []error {
	method, errs := cfg.selectedMethod()
	if len(errs) > 0 {
		return errs
	}

	// Per-method required-field validation. Each case enforces only its own
	// method's fields. Methods that share a struct shape (client_secret_basic
	// and client_secret_post both use ClientSecretAuth) share a case.
	switch method {
	case ClientAuthMethodSecretBasic:
		return validateClientSecretAuth(ClientAuthMethodSecretBasic, cfg.ClientSecretBasic)
	case ClientAuthMethodSecretPost:
		return validateClientSecretAuth(ClientAuthMethodSecretPost, cfg.ClientSecretPost)
	default:
		// Reaches here only if a new method was added to BrokerClientAuth's
		// fields (and to selectedMethod's if-chain) but the validator switch
		// above was not extended with a matching case. The schema knows about
		// the method; the validator does not.
		return []error{fmt.Errorf(
			"broker_oauth.mcp_server_client_auth: method %q is declared in the schema "+
				"but has no validator implementation in this build",
			method)}
	}
}

// validateClientSecretAuth enforces the shared "secret must be non-empty" rule
// for both client_secret_basic and client_secret_post. The method name is
// passed so the error message names the operator's chosen sub-block.
func validateClientSecretAuth(method string, cfg *ClientSecretAuth) []error {
	var errs []error
	// cfg is non-nil here because the caller (validateBrokerClientAuth) only
	// dispatches to this validator when the populated-methods check confirms
	// the sub-block was set. Belt-and-suspenders: handle nil cleanly anyway.
	if cfg == nil {
		errs = append(errs, fmt.Errorf("broker_oauth.mcp_server_client_auth.%s: sub-block is empty", method))
		return errs
	}
	// Trim before comparing so a ${VAR} resolving to whitespace fails at
	// startup with a clear error rather than passing validation and then
	// failing every token-exchange request at runtime with a 401.
	if strings.TrimSpace(cfg.Secret) == "" {
		errs = append(errs, fmt.Errorf(
			"broker_oauth.mcp_server_client_auth.%s.secret is required (empty or whitespace-only after ${VAR} substitution if used)",
			method))
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

// BindAddress returns the host:port the HTTP server listens on. An empty
// ListenAddress yields ":port", i.e. all interfaces — preserved for oauth mode.
// The dev modes are defaulted to a loopback host in applyDefaults, so this
// renders e.g. "127.0.0.1:9090" for them. net.JoinHostPort (not a bare
// "%s:%d") so an IPv6 host such as "::1" is bracketed to "[::1]:9090", which is
// what net.Listen requires — validate() accepts IPv6 loopback addresses.
func (c *ServerConfig) BindAddress() string {
	return net.JoinHostPort(c.ListenAddress, strconv.Itoa(c.Port))
}

// StaticTokenExposedCleartext reports whether mode: static will put its shared
// dev token on the wire in cleartext: static auth, a non-loopback bind, and no
// server-side TLS. The static guard in validate() intentionally allows a
// network bind without an override because every request is token-checked — but
// that only protects the token if the transport is encrypted. Without TLS the
// long-lived bearer token travels plaintext on a routable interface, so a
// sniffer gains the same broker-admin-backed access the disabled guard prevents.
// This is a startup WARN (see banner.LogStaticCleartextExposure), not a hard
// error: static is a dev mode and the network bind was a deliberate choice.
// TLS is both-or-neither (validated), so an empty cert file means TLS is off.
func (c *ServerConfig) StaticTokenExposedCleartext() bool {
	return c.MCPClientAuth.Mode == AuthModeStatic &&
		!isLoopbackHost(c.ListenAddress) &&
		c.TLSCertFile == ""
}

// OAuthPlaintextListenerAcknowledged reports whether OAuth (production) mode is
// serving a plaintext listener under an explicit tls_terminated_upstream
// acknowledgment. validate() rejects OAuth-mode-with-no-TLS unless this
// acknowledgment is set, so this predicate is only ever true for a config that
// deliberately relies on an upstream proxy/ingress for TLS. The caller emits a
// startup WARN (see banner.LogOAuthPlaintextListener) so operators can confirm
// the terminating proxy is actually in front of the listener. TLS is
// both-or-neither (validated), so an empty cert file means TLS is off.
func (c *ServerConfig) OAuthPlaintextListenerAcknowledged() bool {
	return c.MCPClientAuth.Mode == AuthModeOAuth &&
		c.TLSCertFile == "" &&
		c.TLSTerminatedUpstream
}

// isLoopbackHost reports whether host binds the loopback interface only.
// "localhost" and any loopback IP (127.0.0.0/8, ::1) qualify; an empty host
// means all interfaces and is NOT loopback. Used to keep the unauthenticated
// dev modes off the network by default.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

	// Observability capability flags are env-driven (OBS_*). Applied here so
	// they load in the same phase as MCP_SERVER_PORT, before validate() runs.
	applyObservabilityEnv(cfg)

	// Unreleased broker-OAuth runtime opt-in — for manual end-to-end testing
	// only. Emit a loud WARN so operators cannot accidentally leave this on
	// in production; the WARN fires once at startup regardless of how many
	// brokers use oauth mode. See unreleasedBrokerOAuthEnabled and the guard
	// site in validateBroker for the full picture.
	if unreleasedBrokerOAuthEnabled() {
		slog.Warn("UNRELEASED FEATURE ENABLED: broker OAuth is bypassed for manual E2E testing; not for production use",
			slog.String("env_var", envEnableUnreleasedBrokerOAuth))
	}

	return nil
}

// envEnableUnreleasedBrokerOAuth is the env var that opts into the unreleased
// broker-OAuth runtime. Undocumented in operator-facing docs (broker-config.example.yaml,
// docs/authentication.md, docs/configuration.md, CHANGELOG.md) by design —
// this is a testing hatch, not a feature toggle. Remove alongside the guard
// in validateBroker when broker OAuth ships.
const envEnableUnreleasedBrokerOAuth = "ENABLE_UNRELEASED_BROKER_OAUTH"

// envEnableToolAuthorization gates the T1 tool-authorization config validation
// (I1, I3, Q3a, Q4, Q7) and the applyDefaults synthesis (Q1, Q3). When unset
// or false, the tool_authorization YAML block is parsed but ignored — existing
// deployments are unaffected. Set truthy to activate validation during
// development and testing. Remove when the full feature (T2–T6) ships.
const envEnableToolAuthorization = "ENABLE_TOOL_AUTHORIZATION"

// toolAuthorizationFeatureEnabled reports whether ENABLE_TOOL_AUTHORIZATION
// is set truthy.
//
// LIFECYCLE: this function, its constant, and the two gate sites
// (applyToolAuthorizationDefaults, validateToolAuthorization) are one linked
// lifecycle. When the full tool-authorization feature ships, delete this
// function and its constant, and make the gated calls unconditional.
func toolAuthorizationFeatureEnabled() bool {
	return envBool(envEnableToolAuthorization, false)
}

// unreleasedBrokerOAuthEnabled reports whether ENABLE_UNRELEASED_BROKER_OAUTH
// is set truthy. Uses the same tolerant-parse behavior as the observability
// flags (unparseable value → WARN + default of false) so a typo cannot
// silently open the door.
//
// External callers (cmd/server/main.go) do not consult this directly — they
// call ServerConfig.Hop2OAuthActive, which combines this flag with the
// structural preconditions for the runtime.
//
// LIFECYCLE: this function, its constant, the WARN in applyEnvOverrides,
// and the three call sites (validateBroker guard, LogOAuthNotSupported
// banner branch, Hop2OAuthActive method) are one linked lifecycle. When
// broker OAuth ships, delete the flag check from Hop2OAuthActive (the
// structural preconditions remain), delete the validateBroker guard and
// the banner branch, and then delete this function and its constant.
func unreleasedBrokerOAuthEnabled() bool {
	return envBool(envEnableUnreleasedBrokerOAuth, false)
}
