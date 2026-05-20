# Client Auth Mode — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Consolidate `development_mode` + `dev_token` interaction into a single required `client_auth.mode` enum (`disabled` | `static` | `oauth`), with a refactor-robust boot banner for insecure-mode signaling.

**Architecture:** Auth selection becomes one switch on `cfg.ClientAuth.Mode`. Operational profile (https:// requirement) derives from mode via a new `IsProductionMode()` helper. The legacy `development_mode` YAML key is parsed (so old configs don't fail with "unknown field") but logs a deprecation warning and is otherwise ignored. A new `internal/auth/banner.go` emits a multi-line WARN banner from `cmd/server/main.go` for `disabled`/`static` modes — replaces the scattered `slog.Warn` calls inside `NewAuthMiddleware` / `NewTokenVerifier`.

**Tech Stack:** Go 1.25, `gopkg.in/yaml.v3`, `log/slog`, `github.com/coreos/go-oidc/v3/oidc`, `github.com/modelcontextprotocol/go-sdk`.

**Spec:** `docs/superpowers/specs/2026-05-20-client-auth-mode-design.md`
**Jira:** [SOL-149989](https://sol-jira.atlassian.net/browse/SOL-149989)
**Branch:** `SOL-149989/consolidate-client-auth-mode` (already created)

---

## File map (created or modified)

| File | Responsibility | New / Modified |
|---|---|---|
| `internal/config/config.go` | `ClientAuthConfig.Mode` field, mode constants, `IsProductionMode()`, switch-on-mode validator, deprecation detection on legacy `development_mode` | Modified |
| `internal/config/config_test.go` | Updated fixtures (~27), new mode-validation tests, deprecation-warning test | Modified |
| `internal/auth/middleware.go` | `NewAuthMiddleware` switches on `cfg.ClientAuth.Mode`; `NewProtectedResourceMetadataHandler` gates on `mode == "oauth"`; `NewTokenVerifier` switches on mode; remove inline `slog.Warn` calls | Modified |
| `internal/auth/middleware_test.go` | New per-mode middleware tests; new PRM handler per-mode tests; remove `Test_AuthDisabled` (asserts behavior we no longer have) | Modified |
| `internal/auth/banner.go` | `LogStartupBanner(cfg)` — multi-line WARN banner for `disabled`/`static`; INFO line for `oauth` | **New** |
| `internal/auth/banner_test.go` | Banner content tests per mode | **New** |
| `cmd/server/main.go` | Call `auth.LogStartupBanner(cfg)` between config load and slog-level reconfigure | Modified |
| `broker-config.example.yaml` | Show all three modes (commented); default example uses `mode: oauth` | Modified |
| `test/e2e/broker-config.yaml` | `mode: static` + `dev_token` | Modified |
| `test/e2e/helpers.sh` | `MCP_DEV_TOKEN` variable + `Authorization: Bearer` headers in mcp_* functions | Modified |
| `test/e2e/agent/main.go` | Bearer round tripper reading `MCP_DEV_TOKEN` from env | Modified |
| `test/e2e/oauth/test-config.yaml` | Mark broken with TODO referencing follow-up | Modified |
| `CHANGELOG.md` | Breaking-change entry under `[Unreleased]` with migration map | Modified |

---

## Task 1: Add `Mode` field, constants, and `IsProductionMode()` helper

Pure scaffolding — no validator changes yet. Defines the type machinery that later tasks build on.

**Files:**
- Modify: `internal/config/config.go` (add field + constants + method)
- Test: `internal/config/config_test.go` (new `TestIsProductionMode`)

- [ ] **Step 1: Write the failing test**

Add at the end of `internal/config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run test, verify compile failure**

Run: `go test ./internal/config/... -run TestIsProductionMode`
Expected: compile fails — `AuthModeDisabled` undefined, `IsProductionMode` undefined.

- [ ] **Step 3: Add the field, constants, and method**

In `internal/config/config.go`:

Add to the `ClientAuthConfig` struct (after `ResourceURL`, before the closing brace):

```go
// Mode selects the client authentication backend. One of AuthModeDisabled,
// AuthModeStatic, or AuthModeOAuth. Required — no default. The validator
// rejects configs that omit it. See docs/superpowers/specs/2026-05-20-client-auth-mode-design.md
// for the design rationale.
Mode string `yaml:"mode"`
```

Add (after the existing `AuthModeBasic`/`AuthModeBearer` block, around line 77):

```go
// Client authentication modes (Hop 1: MCP client → MCP server). Choosing one
// of these is mandatory; there is no default. Operational profile (https://
// enforcement, self-signed cert allowance, etc.) is derived from the mode via
// IsProductionMode() — DO NOT reintroduce cfg.DevelopmentMode checks.
const (
	AuthModeDisabled = "disabled" // no client auth; every request passes through (dev only)
	AuthModeStatic   = "static"   // shared static dev token; constant-time compare (dev only)
	AuthModeOAuth    = "oauth"    // OAuth/OIDC JWT validation (production)
)

// validAuthClientModes is the allowlist for client_auth.mode. The validator
// rejects any other value. Add new modes here and extend the validate() switch.
var validAuthClientModes = []string{AuthModeDisabled, AuthModeStatic, AuthModeOAuth}
```

Add (after the existing `ValidatePort` function, around line 565):

```go
// IsProductionMode reports whether the server is configured for production
// (OAuth client auth). This is the single source of truth for production-vs-dev
// operational behavior — https:// enforcement on broker/issuer/resource URLs,
// self-signed cert allowance, etc. DO NOT reintroduce cfg.DevelopmentMode
// checks; that field is deprecated and ignored.
func (c *ServerConfig) IsProductionMode() bool {
	return c.ClientAuth.Mode == AuthModeOAuth
}
```

- [ ] **Step 4: Run test, verify pass**

Run: `go test ./internal/config/... -run TestIsProductionMode -v`
Expected: PASS for all four cases.

- [ ] **Step 5: Confirm no regressions**

Run: `go build ./... && go vet ./... && go test ./internal/config/...`
Expected: build clean, vet clean, all existing config tests still pass (no behavior change yet).

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Add ClientAuthConfig.Mode field and IsProductionMode helper

Pure scaffolding for the auth-mode refactor. Adds the Mode string field
plus AuthModeDisabled/AuthModeStatic/AuthModeOAuth constants and an
IsProductionMode() method that will replace !cfg.DevelopmentMode checks
in later tasks. No validator or runtime behavior change yet.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Update existing test fixtures to include `mode: static`

Mechanical fixture update. Every YAML test fixture that uses `development_mode: true` gets a `client_auth: { mode: static, dev_token: test }` block added. This is a no-behavior-change task — the validator doesn't read `mode` yet — but it sets up the fixtures for the validator rules added in subsequent tasks. The legacy `development_mode: true` line is kept in fixtures for now; Task 7 (deprecation warning) will remove it from most of them.

**Files:**
- Modify: `internal/config/config_test.go` (~27 fixture YAML strings)

- [ ] **Step 1: Identify all affected fixtures**

Run: `grep -n "development_mode: true" internal/config/config_test.go`
Expected: ~27 matches.

- [ ] **Step 2: Add `client_auth: { mode: static, dev_token: test }` to each fixture**

For every fixture that has `development_mode: true` but no `client_auth:` block, insert:

```yaml
client_auth:
  mode: static
  dev_token: test
```

Immediately after the `development_mode: true` line.

For fixtures that already have a `client_auth:` block (e.g., the OAuth-validation tests), add `mode: oauth` if they have `issuer`/`audience`/`resource_url`, or `mode: static` + `dev_token: test` otherwise. Inspect each carefully — there are only a few of these.

Example before:
```yaml
development_mode: true
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
```

Example after:
```yaml
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
```

- [ ] **Step 3: Run all config tests, verify still green**

Run: `go test ./internal/config/... -v`
Expected: every test passes. Validator doesn't enforce mode yet, so adding the field is harmless extra YAML.

- [ ] **Step 4: Commit**

```bash
git add internal/config/config_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Add client_auth.mode to existing test fixtures

Mechanical no-behavior-change update. Every fixture using
development_mode: true now also declares client_auth.mode (static for
non-OAuth fixtures, oauth where OIDC fields are present). The validator
doesn't enforce mode yet — this prepares the fixtures so the upcoming
validator changes don't break unrelated tests.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Validator — `client_auth.mode` is required; reject unknown values

Add the first two validation rules: missing mode and invalid mode value.

**Files:**
- Modify: `internal/config/config.go:482-510` (the `// Validate client authentication configuration` block)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Add at the end of `internal/config/config_test.go`:

```go
func TestLoadConfig_AuthMode_Missing(t *testing.T) {
	// client_auth.mode is required — omitting it must fail validation with a
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
		t.Fatal("expected error when client_auth.mode is missing")
	}
	if !strings.Contains(err.Error(), "client_auth.mode is required") {
		t.Errorf("error should state client_auth.mode is required, got: %v", err)
	}
	for _, m := range []string{"disabled", "static", "oauth"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error should list valid mode %q, got: %v", m, err)
		}
	}
}

func TestLoadConfig_AuthMode_Invalid(t *testing.T) {
	yaml := `
client_auth:
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
		t.Fatal("expected error for unknown client_auth.mode value")
	}
	if !strings.Contains(err.Error(), `client_auth.mode "production" is invalid`) {
		t.Errorf("error should quote the bad value, got: %v", err)
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/config/... -run 'TestLoadConfig_AuthMode_(Missing|Invalid)' -v`
Expected: both FAIL — current validator doesn't enforce mode at all.

- [ ] **Step 3: Rewrite the validator's auth block as switch-on-mode**

In `internal/config/config.go`, replace the existing block (lines ~482–510, the `// Validate client authentication configuration based on development mode.` block through the `client_auth.resource_url` validation) with:

```go
	// Validate client authentication configuration. mode is the single source
	// of truth for auth backend selection AND production-vs-dev operational
	// profile (via IsProductionMode). Required fields follow from the mode.
	// See docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
	//
	// Modes are tiered, not interleaved:
	//   - disabled / static: dev-only, http:// broker URLs allowed
	//   - oauth: production, https:// required everywhere
	cfg.ClientAuth.Mode = strings.ToLower(cfg.ClientAuth.Mode)
	switch cfg.ClientAuth.Mode {
	case "":
		errs = append(errs, fmt.Errorf("client_auth.mode is required (must be one of %v)", validAuthClientModes))
	case AuthModeDisabled:
		// no further required fields
	case AuthModeStatic:
		if cfg.ClientAuth.DevToken == "" {
			errs = append(errs, fmt.Errorf("client_auth.dev_token is required when client_auth.mode is %q", AuthModeStatic))
		}
	case AuthModeOAuth:
		if cfg.ClientAuth.Issuer == "" {
			errs = append(errs, fmt.Errorf("client_auth.issuer is required when client_auth.mode is %q", AuthModeOAuth))
		}
		if cfg.ClientAuth.Audience == "" {
			errs = append(errs, fmt.Errorf("client_auth.audience is required when client_auth.mode is %q", AuthModeOAuth))
		}
		if cfg.ClientAuth.ResourceURL == "" {
			errs = append(errs, fmt.Errorf("client_auth.resource_url is required when client_auth.mode is %q", AuthModeOAuth))
		}
	default:
		errs = append(errs, fmt.Errorf("client_auth.mode %q is invalid (must be one of %v)", cfg.ClientAuth.Mode, validAuthClientModes))
	}

	// Validate issuer structure if set (under mode: oauth — required; under
	// other modes — ignored if present, as documented in the spec).
	if cfg.ClientAuth.Issuer != "" {
		if err := validateBrokerURL(cfg.ClientAuth.Issuer, cfg.IsProductionMode()); err != nil {
			errs = append(errs, fmt.Errorf("client_auth.issuer: %w", err))
		}
	}

	// Validate resource_url structure if set
	if cfg.ClientAuth.ResourceURL != "" {
		if err := validateBrokerURL(cfg.ClientAuth.ResourceURL, cfg.IsProductionMode()); err != nil {
			errs = append(errs, fmt.Errorf("client_auth.resource_url: %w", err))
		}
	}
```

Also replace the broker-URL validation call (around line 440):

```go
	for _, alias := range slices.Sorted(maps.Keys(cfg.Brokers)) {
		errs = append(errs, validateBroker(alias, cfg.Brokers[alias], cfg.IsProductionMode())...)
	}
```

(The change: `!cfg.DevelopmentMode` → `cfg.IsProductionMode()`.)

- [ ] **Step 4: Run new tests, verify pass**

Run: `go test ./internal/config/... -run 'TestLoadConfig_AuthMode_(Missing|Invalid)' -v`
Expected: both PASS.

- [ ] **Step 5: Confirm full config-package test suite is green**

Run: `go test ./internal/config/...`
Expected: every test passes. Task 2 fixtures now satisfy the new validator.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Enforce client_auth.mode required, validate enum values

Validator now requires client_auth.mode and rejects unknown values.
Replaces the old !cfg.DevelopmentMode branch with a switch on mode plus
cfg.IsProductionMode() for operational profile gating. Required peer
fields (dev_token for static; issuer/audience/resource_url for oauth)
are enforced inline. Mode value is normalized to lowercase before the
switch.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Validator — mode value is case-insensitive

Verify the lowercase normalization added in Task 3 actually works end-to-end.

**Files:**
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test (probably passes already — confirms behavior)**

Add at the end of `internal/config/config_test.go`:

```go
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
  resource_url: "https://mcp.example.com/mcp"`
			}
			yaml := `
client_auth:
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
			if cfg.ClientAuth.Mode != strings.ToLower(mode) {
				t.Errorf("expected normalized mode %q, got %q", strings.ToLower(mode), cfg.ClientAuth.Mode)
			}
		})
	}
}
```

- [ ] **Step 2: Run, verify pass**

Run: `go test ./internal/config/... -run TestLoadConfig_AuthMode_CaseInsensitive -v`
Expected: PASS for all three case variants. (Confirms Task 3's normalization is wired correctly.)

- [ ] **Step 3: Commit**

```bash
git add internal/config/config_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Test case-insensitive client_auth.mode normalization

Verifies the lowercase normalization in validate() works end-to-end for
DISABLED / Static / OAuth value variants.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Validator — `mode: static` requires `dev_token`

Add the negative test that pins the static-mode rule.

**Files:**
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add at the end of `internal/config/config_test.go`:

```go
func TestLoadConfig_AuthMode_Static_NoToken(t *testing.T) {
	// mode: static without a dev_token is exactly the SOL-149921 vulnerability
	// in its new form — must be rejected with a specific error.
	yaml := `
client_auth:
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
		t.Fatal("expected error when client_auth.mode is static and dev_token is empty")
	}
	if !strings.Contains(err.Error(), "client_auth.dev_token is required") {
		t.Errorf("error should name dev_token, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"static"`) {
		t.Errorf("error should quote mode value, got: %v", err)
	}
}
```

- [ ] **Step 2: Run, verify pass (Task 3 already implemented this rule)**

Run: `go test ./internal/config/... -run TestLoadConfig_AuthMode_Static_NoToken -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/config/config_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Test client_auth.mode static requires dev_token

Pins the static-mode validation rule against the SOL-149921 silent-bypass
shape (mode set, token empty).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Validator — `mode: oauth` requires issuer/audience/resource_url and rejects http://

Three required-field tests plus two https://-enforcement tests.

**Files:**
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Add at the end of `internal/config/config_test.go`:

```go
func TestLoadConfig_AuthMode_OAuth_MissingIssuer(t *testing.T) {
	yaml := `
client_auth:
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
	if err == nil || !strings.Contains(err.Error(), "client_auth.issuer is required") {
		t.Fatalf("expected client_auth.issuer required error, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_MissingAudience(t *testing.T) {
	yaml := `
client_auth:
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
	if err == nil || !strings.Contains(err.Error(), "client_auth.audience is required") {
		t.Fatalf("expected client_auth.audience required error, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_MissingResourceURL(t *testing.T) {
	yaml := `
client_auth:
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
	if err == nil || !strings.Contains(err.Error(), "client_auth.resource_url is required") {
		t.Fatalf("expected client_auth.resource_url required error, got: %v", err)
	}
}

func TestLoadConfig_AuthMode_OAuth_HTTPIssuer(t *testing.T) {
	yaml := `
client_auth:
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
client_auth:
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
client_auth:
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
```

- [ ] **Step 2: Run tests, verify pass (Task 3 already implemented these rules)**

Run: `go test ./internal/config/... -run 'TestLoadConfig_AuthMode_(OAuth|Static_HTTPBroker)' -v`
Expected: all six PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/config/config_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Test oauth required fields and https:// enforcement

Pins the oauth-mode validation rules: issuer/audience/resource_url are
required; http:// URLs are rejected on issuer and broker URLs under
mode: oauth; http:// remains allowed under mode: static (dev profile).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Deprecate `DevelopmentMode` field; log warning when set in YAML

Change `yamlConfig.DevelopmentMode` to `*bool` so the parser can distinguish "present in YAML" from "absent." Emit a one-line WARN when present. Mark `ServerConfig.DevelopmentMode` as `// Deprecated:` for Go-doc clarity.

**Files:**
- Modify: `internal/config/config.go` (yamlConfig + LoadConfig + ServerConfig)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add at the end of `internal/config/config_test.go`:

```go
func TestLoadConfig_DevelopmentModeDeprecationWarning(t *testing.T) {
	// Legacy development_mode YAML field must still parse so operators with
	// old configs reach the helpful client_auth.mode error — not a generic
	// "unknown field" YAML error. But its presence must emit a deprecation
	// warning so operators clean up.
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

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
	if _, err := LoadConfig(writeTemp(t, yaml)); err != nil {
		t.Fatalf("config should parse and validate, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "development_mode is deprecated") {
		t.Errorf("expected deprecation warning in slog output, got: %s", out)
	}
	if !strings.Contains(out, "client_auth.mode") {
		t.Errorf("warning should point operator at the new field, got: %s", out)
	}
}
```

Also at the top of `config_test.go`, add `"bytes"` and `"log/slog"` to the import block if not already present.

- [ ] **Step 2: Run test, verify failure**

Run: `go test ./internal/config/... -run TestLoadConfig_DevelopmentModeDeprecationWarning -v`
Expected: FAIL — no warning currently emitted.

- [ ] **Step 3: Change `yamlConfig.DevelopmentMode` to `*bool` and emit warning**

In `internal/config/config.go`:

Change the field type on `yamlConfig` (around line 160):

```go
type yamlConfig struct {
	Brokers         map[string]*BrokerConfig `yaml:"brokers"`
	SEMP            SEMPConfig               `yaml:"semp"`
	Port            int                      `yaml:"port"`
	LogLevel        string                   `yaml:"log_level"`
	DevelopmentMode *bool                    `yaml:"development_mode"` // *bool so we can detect presence-in-YAML (deprecation warning); the value is ignored
	ClientAuth      ClientAuthConfig         `yaml:"client_auth"`
	TLSCertFile     string                   `yaml:"tls_cert_file"`
	TLSKeyFile      string                   `yaml:"tls_key_file"`
}
```

In `LoadConfig`, after `yaml.Unmarshal` succeeds (around line 239), insert the deprecation check:

```go
	if raw.DevelopmentMode != nil {
		slog.Warn("development_mode is deprecated and ignored; auth profile is now derived from client_auth.mode (one of disabled, static, oauth) — please remove development_mode from your config")
	}
```

Update the ServerConfig assignment (around line 241–250) — drop the `DevelopmentMode` copy from raw, since we no longer use it for control flow:

```go
	cfg := &ServerConfig{
		Brokers:     raw.Brokers,
		SEMP:        raw.SEMP,
		Port:        raw.Port,
		LogLevel:    raw.LogLevel,
		ClientAuth:  raw.ClientAuth,
		TLSCertFile: raw.TLSCertFile,
		TLSKeyFile:  raw.TLSKeyFile,
	}
```

Mark the `ServerConfig.DevelopmentMode` field as deprecated (around line 47):

```go
	// DevelopmentMode is no longer used. Retained only so external Go callers
	// that referenced this field continue to compile; the YAML key is parsed
	// (so old configs don't fail with "unknown field") and a deprecation
	// warning logs at boot if present. Operational profile and auth backend
	// are now derived from ClientAuth.Mode. See IsProductionMode().
	//
	// Deprecated: use ClientAuth.Mode and ServerConfig.IsProductionMode().
	DevelopmentMode bool
```

- [ ] **Step 4: Run test, verify pass**

Run: `go test ./internal/config/... -run TestLoadConfig_DevelopmentModeDeprecationWarning -v`
Expected: PASS.

- [ ] **Step 5: Remove `development_mode: true` from existing fixtures (keep only in the deprecation test)**

Edit `internal/config/config_test.go`: in every test other than `TestLoadConfig_DevelopmentModeDeprecationWarning`, delete the `development_mode: true` line from the fixture (Task 2 added `client_auth.mode: static` next to it; that block stays).

Run: `grep -n "development_mode" internal/config/config_test.go`
Expected: only `TestLoadConfig_DevelopmentModeDeprecationWarning` matches.

- [ ] **Step 6: Run full config suite**

Run: `go test ./internal/config/...`
Expected: green.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Deprecate development_mode; warn when present in YAML

yamlConfig.DevelopmentMode changes from bool to *bool so the parser can
detect presence-in-YAML and emit a deprecation warning that points
operators at the new client_auth.mode field. ServerConfig.DevelopmentMode
field is marked // Deprecated and no longer populated from YAML. Existing
fixtures drop the legacy line; the deprecation test keeps it.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Remove now-stale `cfg.DevelopmentMode` reads from auth middleware

`NewTokenVerifier` still switches on `cfg.DevelopmentMode`. Migrate it to switch on `cfg.ClientAuth.Mode`. The scattered `slog.Warn` calls in `middleware.go:35` and `middleware.go:66` get removed — Task 11 wires the boot banner in their place.

**Files:**
- Modify: `internal/auth/middleware.go`
- Test: `internal/auth/middleware_test.go` (replace `Test_AuthDisabled` with a Mode-based version)

- [ ] **Step 1: Write failing test for `mode: disabled` middleware behavior**

Replace `Test_AuthDisabled` in `internal/auth/middleware_test.go` (lines 40–93) with:

```go
// Test_NewAuthMiddleware_Disabled tests that all requests pass through when
// client_auth.mode is "disabled" — the explicit no-auth dev mode replacing
// the old (silent) development_mode + empty dev_token bypass.
func Test_NewAuthMiddleware_Disabled(t *testing.T) {
	cfg := &config.ServerConfig{
		Port: 9090,
		ClientAuth: config.ClientAuthConfig{
			Mode: config.AuthModeDisabled,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	tests := []struct {
		name         string
		authHeader   string
		expectedCode int
	}{
		{"no auth header", "", http.StatusOK},
		{"with bearer token", "Bearer some-random-token", http.StatusOK},
		{"with garbage", "not-even-bearer", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			middleware.ServeHTTP(rec, req)
			if rec.Code != tt.expectedCode {
				t.Errorf("expected status %d, got %d", tt.expectedCode, rec.Code)
			}
		})
	}
}
```

- [ ] **Step 2: Update existing static-token middleware tests to set `Mode`**

In `internal/auth/middleware_test.go`, find every `config.ClientAuthConfig{...}` literal that uses `DevToken: ...` and add `Mode: config.AuthModeStatic` to it. Same shape: `ClientAuth: config.ClientAuthConfig{Mode: config.AuthModeStatic, DevToken: validToken}`.

Find every `config.ClientAuthConfig{...}` that uses OIDC fields and add `Mode: config.AuthModeOAuth`.

- [ ] **Step 3: Run middleware tests, verify expected state**

Run: `go test ./internal/auth/... -run Test_NewAuthMiddleware_Disabled -v`
Expected: PASS already, because the existing bypass at `middleware.go:34-37` still triggers for the `Mode: AuthModeDisabled` case (since `DevToken == ""`). The test passes for the wrong reason — but the next step fixes that by rewriting the middleware to switch on mode explicitly.

- [ ] **Step 4: Rewrite `NewAuthMiddleware` to switch on mode**

In `internal/auth/middleware.go`, replace the function body (lines 33–58) with:

```go
func NewAuthMiddleware(cfg *config.ServerConfig, next http.Handler) (http.Handler, error) {
	// Auth backend selection mirrors client_auth.mode. Insecure-mode signaling
	// lives in cmd/server/main.go via auth.LogStartupBanner — DO NOT add WARN
	// logs here. See docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
	switch cfg.ClientAuth.Mode {
	case config.AuthModeDisabled:
		return next, nil
	case config.AuthModeStatic, config.AuthModeOAuth:
		// fall through to the verifier construction below
	default:
		return nil, fmt.Errorf("internal: NewAuthMiddleware called with unsupported client_auth.mode %q (validator should have rejected this)", cfg.ClientAuth.Mode)
	}

	verifier, err := NewTokenVerifier(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create token verifier: %w", err)
	}

	// Construct the metadata URL at the server root.
	// Config validation ensures ResourceURL is well-formed if set.
	var metadataURL string
	if cfg.ClientAuth.ResourceURL != "" {
		parsedURL, _ := url.Parse(cfg.ClientAuth.ResourceURL)
		metadataURL = fmt.Sprintf("%s://%s/.well-known/oauth-protected-resource", parsedURL.Scheme, parsedURL.Host)
	}

	middleware := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
	})

	return middleware(next), nil
}
```

- [ ] **Step 5: Rewrite `NewTokenVerifier` to switch on mode**

Replace `NewTokenVerifier` (around lines 60–72) with:

```go
// NewTokenVerifier creates a TokenVerifier based on cfg.ClientAuth.Mode.
//   - AuthModeStatic → constant-time compare against cfg.ClientAuth.DevToken
//   - AuthModeOAuth  → OIDC/JWT verification with automatic key rotation
// cfg has already been validated via config.validate(); other modes are
// programming errors.
func NewTokenVerifier(cfg *config.ServerConfig) (sdkauth.TokenVerifier, error) {
	switch cfg.ClientAuth.Mode {
	case config.AuthModeStatic:
		return createStaticTokenVerifier(cfg.ClientAuth.DevToken), nil
	case config.AuthModeOAuth:
		return createOIDCTokenVerifier(cfg)
	default:
		return nil, fmt.Errorf("internal: NewTokenVerifier called with unsupported client_auth.mode %q (validator should have rejected this)", cfg.ClientAuth.Mode)
	}
}
```

- [ ] **Step 6: Run middleware tests**

Run: `go test ./internal/auth/...`
Expected: PASS. The disabled test still passes (now for the *right* reason — explicit mode match), and the static/OAuth tests pass because they set `Mode` in Step 2.

- [ ] **Step 7: Commit**

```bash
git add internal/auth/middleware.go internal/auth/middleware_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Switch NewAuthMiddleware/NewTokenVerifier on client_auth.mode

Removes the cfg.DevelopmentMode reads from the auth path. Each function
now switches on cfg.ClientAuth.Mode and returns an internal error for
unsupported values (the validator should have caught those). The inline
slog.Warn calls are removed — the boot banner (Task 11) will replace
them.

Test_AuthDisabled is replaced by Test_NewAuthMiddleware_Disabled, which
asserts the same pass-through behavior under the explicit Mode value.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Gate `NewProtectedResourceMetadataHandler` on `mode: oauth`

The PRM handler should advertise OAuth metadata only when OAuth is actually in use. The current implementation has redundant checks against the old DevelopmentMode+DevToken combination.

**Files:**
- Modify: `internal/auth/middleware.go:146-170`
- Test: `internal/auth/middleware_test.go`

- [ ] **Step 1: Write failing tests**

Add at the end of `internal/auth/middleware_test.go`:

```go
func Test_PRMHandler_Disabled(t *testing.T) {
	cfg := &config.ServerConfig{ClientAuth: config.ClientAuthConfig{Mode: config.AuthModeDisabled}}
	if h := NewProtectedResourceMetadataHandler(cfg); h != nil {
		t.Errorf("expected nil PRM handler for mode: disabled, got %T", h)
	}
}

func Test_PRMHandler_Static(t *testing.T) {
	cfg := &config.ServerConfig{ClientAuth: config.ClientAuthConfig{Mode: config.AuthModeStatic, DevToken: "x"}}
	if h := NewProtectedResourceMetadataHandler(cfg); h != nil {
		t.Errorf("expected nil PRM handler for mode: static, got %T", h)
	}
}

func Test_PRMHandler_OAuth(t *testing.T) {
	cfg := &config.ServerConfig{ClientAuth: config.ClientAuthConfig{
		Mode:        config.AuthModeOAuth,
		Issuer:      "https://idp.example.com",
		Audience:    "mcp",
		ResourceURL: "https://mcp.example.com/mcp",
	}}
	if h := NewProtectedResourceMetadataHandler(cfg); h == nil {
		t.Error("expected non-nil PRM handler for mode: oauth")
	}
}
```

- [ ] **Step 2: Run, verify state (Disabled may pass, OAuth depends on current Issuer check)**

Run: `go test ./internal/auth/... -run Test_PRMHandler -v`
Expected: Disabled and Static may PASS already (because `Issuer == ""` so existing nil-return triggers); OAuth PASSES because Issuer is set. The point of the rewrite is to make the gate explicit on mode, not piggy-back on Issuer presence.

- [ ] **Step 3: Replace the gate**

In `internal/auth/middleware.go`, replace the body of `NewProtectedResourceMetadataHandler` (lines 146–170) with:

```go
// NewProtectedResourceMetadataHandler creates an HTTP handler that serves
// OAuth 2.0 Protected Resource Metadata (RFC 9728) for the MCP server.
// This endpoint enables MCP clients to discover the authorization server
// and initiate browser-based OAuth flows (Authorization Code + PKCE).
// Only served under client_auth.mode == "oauth"; returns nil otherwise.
func NewProtectedResourceMetadataHandler(cfg *config.ServerConfig) http.Handler {
	if cfg.ClientAuth.Mode != config.AuthModeOAuth {
		return nil
	}

	metadata := &oauthex.ProtectedResourceMetadata{
		Resource:               cfg.ClientAuth.ResourceURL,
		AuthorizationServers:   []string{cfg.ClientAuth.Issuer},
		ScopesSupported:        []string{"openid"},
		BearerMethodsSupported: []string{"header"},
	}

	return sdkauth.ProtectedResourceMetadataHandler(metadata)
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/auth/... -run Test_PRMHandler -v`
Expected: all three PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/middleware.go internal/auth/middleware_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Gate ProtectedResourceMetadataHandler on mode: oauth

The PRM endpoint advertises the OAuth authorization server, so it should
be served only when OAuth is actually in use. Replaces the old two-field
DevelopmentMode+DevToken check with a single mode check.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Create `internal/auth/banner.go` with `LogStartupBanner`

New file. Pure WARN-banner emission keyed by mode.

**Files:**
- Create: `internal/auth/banner.go`
- Create: `internal/auth/banner_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/auth/banner_test.go`:

```go
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

package auth

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// captureSlog replaces slog.Default with a TextHandler writing to buf.
// Returns a restore func; defer it.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	return buf, func() { slog.SetDefault(prev) }
}

func Test_StartupBanner_Disabled(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()
	cfg := &config.ServerConfig{ClientAuth: config.ClientAuthConfig{Mode: config.AuthModeDisabled}}
	LogStartupBanner(cfg)
	out := buf.String()
	for _, want := range []string{
		"level=WARN",
		"INSECURE MODE",
		"client_auth.mode = disabled",
		"Client authentication is DISABLED",
		"NOT FOR PRODUCTION USE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func Test_StartupBanner_Static(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()
	cfg := &config.ServerConfig{ClientAuth: config.ClientAuthConfig{Mode: config.AuthModeStatic, DevToken: "x"}}
	LogStartupBanner(cfg)
	out := buf.String()
	for _, want := range []string{
		"level=WARN",
		"INSECURE MODE",
		"client_auth.mode = static",
		"static dev token",
		"NOT FOR PRODUCTION USE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func Test_StartupBanner_OAuth(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()
	cfg := &config.ServerConfig{ClientAuth: config.ClientAuthConfig{
		Mode:   config.AuthModeOAuth,
		Issuer: "https://idp.example.com",
	}}
	LogStartupBanner(cfg)
	out := buf.String()
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("expected INFO log for oauth mode, got: %s", out)
	}
	if strings.Contains(out, "INSECURE MODE") {
		t.Errorf("oauth mode should not emit insecure banner, got: %s", out)
	}
	if !strings.Contains(out, "https://idp.example.com") {
		t.Errorf("oauth log should name the issuer, got: %s", out)
	}
}
```

- [ ] **Step 2: Run test, verify compile failure**

Run: `go test ./internal/auth/... -run Test_StartupBanner -v`
Expected: compile fails — `LogStartupBanner` undefined.

- [ ] **Step 3: Implement `LogStartupBanner`**

Create `internal/auth/banner.go`:

```go
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

package auth

import (
	"log/slog"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// LogStartupBanner emits a prominent, refactor-robust signal of the configured
// client authentication mode. It lives in the boot path (called from
// cmd/server/main.go) — NOT in NewAuthMiddleware/NewTokenVerifier — for three
// reasons:
//
//  1. Single emission point — any future transport (stdio, gRPC) goes through
//     main and picks up the signal automatically.
//  2. Refactor-robust — surviving auth-path restructures means the signal
//     cannot vanish silently.
//  3. Visually loud — multi-line banner with INSECURE MODE callouts won't
//     get lost in request-log noise.
//
// For mode: disabled and mode: static the banner emits at WARN. For mode:
// oauth a single INFO log line is emitted; production is the unremarkable case.
//
// Do not move this into middleware. See SOL-149989 design spec at
// docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
func LogStartupBanner(cfg *config.ServerConfig) {
	switch cfg.ClientAuth.Mode {
	case config.AuthModeDisabled:
		slog.Warn(disabledBanner)
	case config.AuthModeStatic:
		slog.Warn(staticBanner)
	case config.AuthModeOAuth:
		slog.Info("client auth: OAuth/OIDC", slog.String("issuer", cfg.ClientAuth.Issuer))
	}
}

const disabledBanner = `
============================================================
  INSECURE MODE: client_auth.mode = disabled
  Client authentication is DISABLED.
  All MCP requests pass through without verification.
  This is development mode — NOT FOR PRODUCTION USE.
============================================================`

const staticBanner = `
============================================================
  INSECURE MODE: client_auth.mode = static
  Authentication uses a shared static dev token.
  This is development mode — NOT FOR PRODUCTION USE.
============================================================`
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/auth/... -run Test_StartupBanner -v`
Expected: all three PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/banner.go internal/auth/banner_test.go
git commit -m "$(cat <<'EOF'
SOL-149989: Add LogStartupBanner for refactor-robust insecure-mode signal

New internal/auth/banner.go emits a multi-line WARN banner for
client_auth.mode disabled/static and a single INFO line for oauth.
Lives in the boot path (not in middleware) so it survives auth-path
refactors and any future transport additions.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Wire `LogStartupBanner` into `cmd/server/main.go`

Call the banner immediately after `config.Load()` succeeds and before slog gets reconfigured to the user log level (so the banner uses the bootstrap INFO-level handler and is always visible at WARN).

**Files:**
- Modify: `cmd/server/main.go:233-235`

- [ ] **Step 1: Insert the banner call**

In `cmd/server/main.go`, immediately after line 233 (the close of the `if err != nil` block following `config.Load()`), insert:

```go
	// Loud, refactor-robust signal of the configured client auth mode.
	// MUST run before the slog handler gets reconfigured to the user log
	// level — at this point the bootstrap handler is at INFO, so WARN
	// banner entries are always visible regardless of cfg.LogLevel.
	// DO NOT move this into middleware; see internal/auth/banner.go.
	auth.LogStartupBanner(cfg)

```

The resulting region (around lines 229–244) should read:

```go
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Loud, refactor-robust signal of the configured client auth mode.
	// MUST run before the slog handler gets reconfigured to the user log
	// level — at this point the bootstrap handler is at INFO, so WARN
	// banner entries are always visible regardless of cfg.LogLevel.
	// DO NOT move this into middleware; see internal/auth/banner.go.
	auth.LogStartupBanner(cfg)

	// Reconfigure slog with the user-configured level. cfg.LogLevel is
	// validated and normalized to one of debug/info/warn/error.
```

- [ ] **Step 2: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 3: Smoke test main_test if present**

Run: `go test ./cmd/server/...`
Expected: PASS (or unchanged from baseline).

- [ ] **Step 4: Commit**

```bash
git add cmd/server/main.go
git commit -m "$(cat <<'EOF'
SOL-149989: Call auth.LogStartupBanner from main after config.Load

Banner runs at the boot path, between config validation and slog
reconfiguration, so WARN banners are always visible regardless of
cfg.LogLevel.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 12: Update `broker-config.example.yaml`

Show all three modes with commented examples. Default to `mode: oauth` (production-shaped). Add the deprecation note about `development_mode`.

**Files:**
- Modify: `broker-config.example.yaml`

- [ ] **Step 1: Rewrite the auth section**

Replace lines 20–29 (`# Set to true only for local development...` through `  # dev_token: "static-token"...`) with:

```yaml
# Client authentication for the MCP server (Hop 1: MCP client → MCP server).
# mode is required. Choose one of:
#   - oauth    — JWT validation via OIDC (production)
#   - static   — shared static dev token (development only)
#   - disabled — no client auth, every request passes through (development only)
#
# Note: the previous `development_mode` flag is deprecated and ignored.
# Operational profile (https:// requirement, etc.) is now derived from
# client_auth.mode: oauth = production, disabled/static = development.
client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp-server"
  resource_url: "https://mcp.example.com/mcp"

# Development examples (uncomment one and comment out the oauth block above):
#
# client_auth:
#   mode: disabled
#
# client_auth:
#   mode: static
#   dev_token: "static-token"
```

- [ ] **Step 2: Verify YAML still parses**

Run a syntax check by loading the example through the config loader. If a unit test exists that loads this file, run it; otherwise just `yamllint` style sanity-check by eye.

Run: `go build ./...`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add broker-config.example.yaml
git commit -m "$(cat <<'EOF'
SOL-149989: Document client_auth.mode in broker-config.example.yaml

Default example uses mode: oauth (production-shaped). Adds commented
disabled / static examples and a note about the development_mode
deprecation.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 13: Update e2e fixtures and wiring for `mode: static`

Reuse the e2e changes PR #50 introduced (which are not on main since we're branching off main). The static-token e2e path needs: config with mode + token, Bearer header in helpers, Bearer round tripper in the Go agent.

**Files:**
- Modify: `test/e2e/broker-config.yaml`
- Modify: `test/e2e/helpers.sh`
- Modify: `test/e2e/agent/main.go`

- [ ] **Step 1: Update `test/e2e/broker-config.yaml`**

Replace the auth-block region (around line 12, where `development_mode: true` and the commented `client_auth:` block live) with:

```yaml
# ── Auth settings ───────────────────────────────────────────────────────────
client_auth:
  mode: static
  dev_token: e2e-static-dev-token
# For production (mode: oauth), set instead:
# client_auth:
#   mode: oauth
#   issuer: "https://idp.example.com"
#   audience: "solace-mcp"
#   resource_url: "https://mcp.example.com/mcp"
```

Remove the existing `development_mode: true` line if present.

- [ ] **Step 2: Update `test/e2e/helpers.sh`**

Near the top of `helpers.sh` (where `MCP_PORT`, `MCP_URL` are defined), add:

```bash
# Static dev token used to authenticate every e2e curl/agent request to the
# broker MCP server. Must match dev_token in write_config() and broker-config.yaml.
# Exported so test/e2e/agent reads it from env (see test/e2e/agent/main.go).
export MCP_DEV_TOKEN="e2e-static-dev-token"
```

In `write_config()` (the heredoc that generates the per-test config), replace the existing auth block with:

```bash
client_auth:
  mode: static
  dev_token: e2e-static-dev-token
```

In `mcp_initialize()`, `mcp_request()`, and any other curl-against-`$MCP_URL/mcp` calls, add the Authorization header:

```bash
        -H "Authorization: Bearer $MCP_DEV_TOKEN" \
```

(One `-H` line per curl invocation.)

- [ ] **Step 3: Update `test/e2e/agent/main.go`**

Add a Bearer round tripper that reads `MCP_DEV_TOKEN` from env and attaches it to every outgoing request. This is the same diff PR #50 introduced.

In the imports block, add `"net/http"`.

After `func main() { ... }`, before `func run(...)`, add:

```go
// bearerRoundTripper wraps http.DefaultTransport to add a Bearer Authorization
// header to every outbound request. Used by the e2e agent so the broker MCP
// server's auth middleware (mode: static) accepts our session-initialize and
// tool calls.
type bearerRoundTripper struct {
	token string
	rt    http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request so we do not mutate the caller's copy.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+b.token)
	return b.rt.RoundTrip(cloned)
}
```

In the `run()` function, before `client.Connect(...)`, add token retrieval and an `HTTPClient` on the transport:

```go
	token := os.Getenv("MCP_DEV_TOKEN")
	if token == "" {
		return fmt.Errorf("MCP_DEV_TOKEN env var is required (set by test harness; matches dev_token in broker config)")
	}
	httpClient := &http.Client{
		Transport: &bearerRoundTripper{token: token, rt: http.DefaultTransport},
	}
```

Update the `client.Connect` call to pass `HTTPClient: httpClient` on the `mcp.StreamableClientTransport`:

```go
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   serverURL + "/mcp",
		HTTPClient: httpClient,
	}, nil)
```

- [ ] **Step 4: Build the e2e agent**

Run: `go build ./test/e2e/agent/`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/broker-config.yaml test/e2e/helpers.sh test/e2e/agent/main.go
git commit -m "$(cat <<'EOF'
SOL-149989: Switch e2e static-token path to client_auth.mode

E2E broker-config.yaml uses mode: static with dev_token. helpers.sh
attaches Authorization: Bearer on every MCP request. test/e2e/agent
reads MCP_DEV_TOKEN from env and attaches it via a custom RoundTripper.
Reuses the wiring originally drafted in PR #50.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 14: Mark e2e OAuth test config as broken with a TODO

The e2e OAuth test uses `http://localhost` for Keycloak; under the new design, `mode: oauth` requires `https://`. The test is already broken on main (per the comment Andrea added in PR #50). We leave it broken with a TODO pointing at the follow-up ticket.

**Files:**
- Modify: `test/e2e/oauth/test-config.yaml`

- [ ] **Step 1: Replace the auth block**

Replace the existing `development_mode: true` line and the `client_auth:` block with:

```yaml
# TODO(SOL-149989 follow-up): this e2e OAuth test path is currently broken.
# Under the new client_auth.mode design, mode: oauth requires https://
# (no localhost http:// exemption), so the test Keycloak needs a TLS cert
# before this test can pass. Tracked as a follow-up ticket under epic
# SOL-149480. The config below is left in its target shape (mode: oauth)
# so the diff against a fixed version is small.
client_auth:
  mode: oauth
  issuer: "http://localhost:8090/realms/solace"
  audience: "solace-mcp-server"
  resource_url: "http://localhost:9091/mcp"
```

Remove any stray `dev_token:` line.

- [ ] **Step 2: Confirm build/test packages unaffected**

Run: `go build ./...`
Expected: clean. (Yaml file isn't compiled, but a stray spec change shouldn't break anything either.)

- [ ] **Step 3: Commit**

```bash
git add test/e2e/oauth/test-config.yaml
git commit -m "$(cat <<'EOF'
SOL-149989: Mark e2e OAuth test config broken with follow-up TODO

The OAuth e2e test uses http://localhost for Keycloak and Keycloak-served
URLs. Under the new client_auth.mode design, mode: oauth requires https://
everywhere — there is no localhost http:// exemption. Test is left in its
target shape with a TODO pointing at the follow-up ticket for TLS Keycloak
wiring.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 15: CHANGELOG entry

Document the breaking change with the migration map operators need.

**Files:**
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Add entry under `[Unreleased]`**

In `CHANGELOG.md`, immediately after the `## [Unreleased]` heading and the existing `### Added` section, add:

```markdown
### Changed

- **BREAKING**: Client auth config consolidated into single required `client_auth.mode` enum (`disabled` | `static` | `oauth`). The legacy `development_mode` flag is deprecated and ignored — its presence in YAML logs a deprecation warning at startup. The previous "development_mode + empty dev_token = silent no-auth" path (SOL-149921) is replaced by the explicit `mode: disabled`. Migration:

  | Old config | New config |
  |---|---|
  | `development_mode: true` + `dev_token: "abc"` | `client_auth: { mode: static, dev_token: "abc" }` |
  | `development_mode: true` + missing/empty `dev_token` | `client_auth: { mode: disabled }` |
  | `development_mode: false` + OIDC fields | `client_auth: { mode: oauth, issuer, audience, resource_url }` |

  `mode: oauth` is the only legal production mode and enforces `https://` on broker URLs, issuer, and resource_url. `mode: disabled` and `mode: static` are development-only and allow `http://`. A prominent WARN-level boot banner fires for `disabled` and `static` modes. Tracked under SOL-149989.
```

- [ ] **Step 2: Commit**

```bash
git add CHANGELOG.md
git commit -m "$(cat <<'EOF'
SOL-149989: CHANGELOG entry for client_auth.mode breaking change

Documents the schema migration with the full operator-facing migration
map.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 16: Final verification — full build, vet, test, log scan

Confirm the whole tree is clean before opening the PR.

**Files:** none modified.

- [ ] **Step 1: Build the entire module**

Run: `go build ./...`
Expected: clean (no errors, no warnings).

- [ ] **Step 2: Vet the entire module**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 3: Run the entire test suite**

Run: `go test ./...`
Expected: every package green. Specifically: 12+ packages, all PASS, no skips except where explicitly intended.

- [ ] **Step 4: Run the logging security check**

Run: `/check-logs` (per repo CLAUDE.md — scan changed files for logging security violations)
Expected: 0 CRITICAL and 0 HIGH issues on the diff. (Medium/low non-blocking but worth flagging.)

- [ ] **Step 5: Sanity-check the diff**

Run: `git log --oneline main..HEAD`
Expected: ~15 commits, all prefixed `SOL-149989:`, one per task.

Run: `git diff main..HEAD --stat`
Expected: changes confined to the files listed in the file map; no surprise edits.

- [ ] **Step 6: Push the branch**

```bash
git push -u origin SOL-149989/consolidate-client-auth-mode
```

- [ ] **Step 7: Open the PR**

```bash
gh pr create --title "SOL-149989: Consolidate client auth config to single client_auth.mode enum" --body "$(cat <<'EOF'
## Summary

Replaces the `development_mode` + `dev_token` interaction with a single required `client_auth.mode` enum. Closes the SOL-149921 silent auth-bypass through structural simplification rather than patching.

- `client_auth.mode` is required (`disabled` | `static` | `oauth`)
- Operational profile (`https://` enforcement, etc.) derives from mode via `IsProductionMode()`
- Boot banner from `cmd/server/main.go` provides refactor-robust insecure-mode signal
- `development_mode` field accepted (so old configs parse), logs deprecation warning, otherwise ignored

Design spec: `docs/superpowers/specs/2026-05-20-client-auth-mode-design.md`
Jira: [SOL-149989](https://sol-jira.atlassian.net/browse/SOL-149989) (supersedes SOL-149921; PR #50 closed)

## Test plan

- [ ] `go test ./...` green
- [ ] `go vet ./...` clean
- [ ] `/check-logs` 0 CRITICAL/HIGH on diff
- [ ] Manual smoke: start server with each of the three modes, observe banner + endpoint behavior
- [ ] FOSSA SCA + vulnerability scans pass
- [ ] CODEOWNERS review

## Follow-ups (separate tickets to be filed)

- Restore e2e OAuth test path (TLS for test Keycloak)
- Investigate lightweight local OAuth server (Mark's vision — long-term replacement for `mode: disabled`/`static`)

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR URL returned. Comment on SOL-149989 with the PR URL.

---

## Self-review

### Spec coverage

- [x] `client_auth.mode` required, no default — Task 3
- [x] Each mode's required fields validated — Task 3, 5, 6
- [x] Specific actionable errors naming field, rule, options — Task 3 (validator), Tasks 5–6 (assertions on message content)
- [x] `mode: oauth` enforces https:// on broker URLs, issuer, resource_url — Task 3 (IsProductionMode wiring), Task 6 (assertions)
- [x] `mode: disabled`/`mode: static` allow http:// — Task 6 (assertion)
- [x] `/.well-known/oauth-protected-resource` served only under mode: oauth — Task 9
- [x] WARN-level boot banner, single emission, refactor-robust — Tasks 10–11
- [x] Mode value case-insensitive — Task 3 (normalization), Task 4 (assertion)
- [x] `development_mode` accepted, deprecation warning, ignored — Task 7
- [x] CHANGELOG entry — Task 15
- [x] Stale fields ignored (spec §"Stale-field handling") — implicit in Task 3 (validator only checks required fields; extras are inert)
- [x] Banner content matches spec §"Banner content" — Task 10 constants

### Placeholder scan

No "TBD" / "TODO" / "implement later" / "fill in details" placeholders in any task step. The one `TODO(...)` reference (Task 14) is a deliberate code comment to be added in the e2e oauth config — concrete content, not a placeholder.

### Type consistency

- Mode constants `AuthModeDisabled` / `AuthModeStatic` / `AuthModeOAuth` used identically in Tasks 1, 3, 8, 9, 10.
- `IsProductionMode()` defined in Task 1, used in Task 3 (validator), Task 7 (no, it's not used there — confirmed not needed).
- YAML values `disabled` / `static` / `oauth` (lowercase) used identically across fixture YAML in Tasks 2, 6, 12, 13, 14.
- Banner content `INSECURE MODE` / `client_auth.mode = <mode>` / `NOT FOR PRODUCTION USE` matches between the spec, the test assertions in Task 10, and the banner constants.

No inconsistencies found.
