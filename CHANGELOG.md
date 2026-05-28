# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Apache 2.0 LICENSE file for open source compliance
- CONTRIBUTING.md with comprehensive contribution guidelines including DCO requirements
- CODE_OF_CONDUCT.md with Contributor Covenant 2.1
- SECURITY.md with vulnerability disclosure policy and security best practices
- GitHub issue templates for bug reports and feature requests
- GitHub pull request template with comprehensive checklist
- Copyright headers to all Go source files
- Status badges in README.md (build, license, Go version, code of conduct)
- Contributing and License sections in README.md

### Changed

- **BREAKING**: Client auth config consolidated into single required `client_auth.mode` enum (`disabled` | `static` | `oauth`). The legacy `development_mode` flag is deprecated and ignored — its presence in YAML logs a deprecation warning at startup. The previous "development_mode + empty dev_token = silent no-auth" path (SOL-149921) is replaced by the explicit `mode: disabled`. Migration:

  | Old config | New config |
  |---|---|
  | `development_mode: true` + `dev_token: "abc"` | `client_auth: { mode: static, dev_token: "abc" }` |
  | `development_mode: true` + missing/empty `dev_token` | `client_auth: { mode: disabled }` |
  | `development_mode: false` + OIDC fields | `client_auth: { mode: oauth, issuer, audience, resource_url }` |

  `mode: oauth` is the only legal production mode and enforces `https://` on broker URLs, issuer, and resource_url. `mode: disabled` and `mode: static` are development-only and allow `http://`. A prominent WARN-level boot banner fires for `disabled` and `static` modes. Tracked under SOL-149989.

- **BREAKING**: Broker aliases must now satisfy a contract: 1–63 characters, only letters/digits/hyphens, must start and end alphanumeric, compared case-insensitively. Configs that previously loaded silently will now be rejected at startup if they contain: empty aliases, whitespace, underscores, dots, embedded special characters, leading or trailing hyphens, aliases longer than 63 characters, or case-only collisions (e.g. `Prod` and `prod` in the same config). Original casing is preserved in all user-facing output (logs, `list-brokers`, error messages); tool calls resolve case-insensitively so any casing of a configured alias works. Migration:

  | Old alias | New alias |
  |---|---|
  | `prod_east` | `prod-east` |
  | `Prod` + `prod` (collision) | rename one of them |

  Tracked under SOL-149789.

- Config loader now rejects unknown YAML fields at startup instead of silently ignoring them. Previously, a typo like `developmnet_mode` or `insecure_skip_verfy` was accepted and the operator's intended override became a no-op. The loader now fails fast with an error naming the offending field. Existing configs with stale or misspelled keys will fail to start until the typo is corrected; configs with only valid keys are unaffected. Tracked under SOL-149927.

## [0.1.0] - 2026-04-24

### Added
- Initial release of Solace Broker MCP Server
- **Tool Manager Foundation**
  - Generic tool registration and routing infrastructure
  - Parameter and output validation against JSON Schema
  - Broker resolution and connection pooling
  - Structured logging for all tool invocations
- **Composite Tool Engine**
  - YAML-driven multi-step tool definitions
  - Go template-based argument resolution
  - Parallel and sequential step execution
  - Configurable result strategies (collect, merge, unwrap)
- **SEMP Client Layer**
  - HTTP client with Basic Auth and Bearer token support
  - OpenAPI spec parser (799 operations from Monitor, Config, Action APIs)
  - Lazy broker connection pooling with thread-safe double-checked locking
  - Per-broker HTTP transport and connection pooling
- **Configuration Management**
  - YAML config file with environment variable substitution (`${VAR_NAME}`)
  - `.env` file loading for local development
  - Validation for broker URLs, auth modes, ports, TLS pairing
  - Multiple broker support with independent credentials
- **Authentication & Security**
  - OAuth/JWT token validation with OIDC provider integration
  - Development mode with optional static dev token
  - Automatic JWKS key rotation
  - Scope-based access control (optional)
  - OAuth 2.0 Protected Resource Metadata endpoint (RFC 9728)
- **Secure Logging**
  - Structured JSON logging with `log/slog`
  - Credential redaction via `slog.LogValuer` pattern
  - `ReplaceAttr` safety net for defense-in-depth
  - Configurable log levels (debug, info, warn, error)
  - Never logs passwords, tokens, or authorization headers
- **Testing Infrastructure**
  - Unit tests across all packages with `-race` detector
  - Integration tests for tool manager and handlers
  - E2E test suite with two-broker Docker Compose setup
  - OAuth integration tests with Keycloak
  - Comprehensive test coverage (config, semp, composite, tools, auth)
- **CI/CD Pipeline**
  - GitHub Actions workflow for lint, build, test, E2E
  - golangci-lint with security checks (gosec, bodyclose, noctx)
  - E2E tests run automatically on all PRs
  - OAuth E2E tests with Terraform-managed Keycloak
- **Production Deployment**
  - Dockerfile with multi-stage build and non-root user
  - Kubernetes manifests (Deployment, Service, ConfigMap, Secret)
  - GitHub Actions release workflow with multi-platform binaries
  - Health check endpoint (`/health`)
  - Graceful shutdown with 120-second timeout
- **Documentation**
  - Comprehensive README with quickstart guide
  - Architecture documentation with component diagrams and request flow
  - Secure logging rules with examples
  - E2E testing guide
  - Packaging and release documentation

### Changed
- Upgraded MCP Go SDK to v1.5.0

### Security
- All credentials redacted from logs by default
- Constant-time comparison for dev token validation (prevents timing attacks)
- TLS certificate verification enabled by default (`insecure_skip_verify: false`)
- HTTP server ReadHeaderTimeout set to prevent Slowloris attacks

## [0.0.1] - 2026-02-15

### Added
- Initial proof-of-concept implementation
- Basic SEMP client
- Simple config loading

---

## Versioning

This project uses [Semantic Versioning](https://semver.org/):
- **MAJOR** version for incompatible API changes
- **MINOR** version for new functionality in a backward-compatible manner
- **PATCH** version for backward-compatible bug fixes

## Release Process

1. Update this CHANGELOG with all changes in `[Unreleased]`
2. Move unreleased changes to a new version section with date
3. Create git tag: `git tag -a v0.2.0 -m "Release v0.2.0"`
4. Push tag: `git push origin v0.2.0`
5. GitHub Actions automatically builds binaries and creates GitHub Release

## Links

- [Unreleased]: https://github.com/SolaceDev/solace-broker-mcp/compare/v0.1.0...HEAD
- [0.1.0]: https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.1.0
- [0.0.1]: https://github.com/SolaceDev/solace-broker-mcp/releases/tag/v0.0.1
