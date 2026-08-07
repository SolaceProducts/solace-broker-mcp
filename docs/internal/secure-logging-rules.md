# Secure Logging Rules

Rules for writing safe, secure, and compliant log output in the Solace Broker MCP Server. These rules apply identically in dev and prod — the only differences between environments are level, format, and source info.

---

## Never-Log List

These must never appear in log output, in any environment:

| Item | Where it lives in our code |
|---|---|
| SEMP passwords | `AuthConfig.Password`, `auth.BasicAuthenticator.password` |
| SEMP usernames | `AuthConfig.Username`, `auth.BasicAuthenticator.username` |
| SEMP bearer tokens | `AuthConfig.Token`, `auth.BearerAuthenticator.token` |
| Raw `Authorization` header values | Built in `HTTPClient.Execute()` |
| TLS private keys | If we ever load them |
| URLs with embedded credentials | `http://user:pass@host` patterns |
| Full broker config maps | `map[string]*BrokerConfig` contains credentials via `AuthConfig` |

**What to log instead:** auth method (`basic`/`bearer`), broker alias, broker URL (without credentials).

---

## Always-Log List (INFO level)

| Event | Required fields |
|---|---|
| Server startup | port, broker_count, broker alias list (`[]string`), auth_mode, log_level |
| Server shutdown | reason (signal, error) |
| Tool invocation | tool name, broker alias, status, duration_ms |
| Tool error | tool name, broker alias, error_type, http_status |
| Broker connection created | broker alias, URL |
| Config loaded | broker_count, port |

**Important:** At startup, log the broker alias list (`[]string`) — never the broker config map. The config map contains `AuthConfig` which holds credentials.

---

## Implementation Rules

### Rule 1: Always use structured fields

Never `fmt.Sprintf` into the message string with external data. Always `slog.String("key", value)` for variable data. This prevents log injection attacks.

### Rule 2: Credential-carrying types must implement `slog.LogValuer`

- `AuthConfig` — expose only `Mode`
- `BrokerConfig` — expose `URL`, `InsecureSkipVerify`, `Auth.Mode`
- `HTTPClient` — lower risk (unexported fields) but should still get `LogValuer` for defense in depth
- OAuth config types (`BrokerOAuthConfig`, `BrokerClientAuth`, `ClientSecretAuth`) also implement `LogValuer`, excluding secret material
- Any new credential-carrying type follows the same pattern

### Rule 3: `ReplaceAttr` safety net on the handler

Keys matching `password`, `token`, `secret`, `authorization`, `credential`, `api_key`, `private_key` get replaced with `[REDACTED]`. This is defense in depth — catches anything that slips past Rule 2.

### Rule 4: Never log raw structs

Always log explicit fields, even if `LogValuer` is implemented.

- Avoid: `slog.Any("config", brokerConfig)`
- Prefer: `slog.String("url", config.URL)`

### Rule 5: Review errors from external systems before logging

SEMP errors and HTTP responses may contain credential context. Log structured error context (operation, broker, http_status) instead of raw `err.Error()` when the error source is external.

### Rule 6: Log to stderr, never stdout

MCP protocol reserves stdout for JSON-RPC messages. Go's `log/slog` defaults to stderr — keep it that way.

### Rule 7: Same security rules in dev and prod

Credential redaction is identical in both modes. `LogValuer` and `ReplaceAttr` are active in both modes. Only differences are level, format, and source info (see table below).

---

## Dev vs Prod Configuration

| Setting | Dev | Prod |
|---|---|---|
| Level | DEBUG (when added later) | INFO |
| Format | `slog.TextHandler` (human-readable) | `slog.JSONHandler` (machine-parseable) |
| Output | stderr | stderr |
| AddSource | true (includes file:line for debugging) | false (skip for performance) |
| Credential redaction | Yes | Yes |
| `ReplaceAttr` safety net | Yes | Yes |
| `LogValuer` on credential types | Yes | Yes |
