# Configuration

The server is configured via a YAML config file plus a `.env` file for credentials. See the [Quickstart](../README.md#quickstart) in the README for a minimal working example.

## Config file location

The server searches for the config file in this order:

| Priority | Source | Value |
|---|---|---|
| 1 | `CONFIG_FILE` env var | Any path you set |
| 2 | System path | `/etc/mcp-server/config.yaml` |
| 3 | Local path | `./broker-config.yaml` |

Set `CONFIG_FILE` explicitly if your file is in a non-standard location.

A separate credentials file (default: `.env` next to the config file) is loaded automatically. Override the path with the `ENV_FILE` env var.

## Environment variable substitution

Use `${VAR_NAME}` anywhere in the YAML config to reference an environment variable:

```yaml
brokers:
  my-broker:
    auth:
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

Variables are resolved at startup. The `.env` file is loaded automatically before substitution. Precedence: env var > `.env` file > YAML literal.

## Server settings

| YAML field | Env var | Default | Description |
|---|---|---|---|
| `port` | `MCP_SERVER_PORT` | `9090` | Port the MCP server listens on. |
| `tls_cert_file` | — | none | Path to TLS certificate (PEM). |
| `tls_key_file` | — | none | Path to TLS private key (PEM). |
| `log_level` | — | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |

**TLS:** both `tls_cert_file` and `tls_key_file` must be set together — providing only one is a startup error. When both are set, the server starts with HTTPS; when neither is set, plain HTTP.

```yaml
port: 9090
tls_cert_file: "/etc/certs/server.pem"
tls_key_file: "/etc/certs/server-key.pem"
```

**Logging:** the server writes structured JSON logs to stderr. Credentials are automatically redacted in all log output. Every tool invocation is logged with the tool name, target broker, status, and duration.

## Broker settings

Configured under the `brokers` map. Each key is a broker alias used as the `broker` parameter in MCP tools.

| YAML field | Default | Description |
|---|---|---|
| `url` | — | SEMP management API base URL (e.g., `https://broker:1943`). |
| `auth.mode` | — | `basic` or `bearer`. |
| `auth.username` | — | Basic auth username. |
| `auth.password` | — | Basic auth password. |
| `auth.token` | — | Bearer token (used when `auth.mode: bearer`). |
| `insecure_skip_verify` | `false` | Skip TLS certificate verification. Development only — do not use in production. |

**Production URL enforcement:** when `development_mode: false`, the server rejects `http://` broker URLs at startup. Use `https://` for all production broker connections.

```yaml
brokers:
  my-broker:
    url: "https://broker:1943"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

## Client authentication settings

Configured under the `client_auth` key. Controls how MCP clients authenticate to this server. See the [Authentication](authentication.md) guide for full setup instructions.

| YAML field | Description |
|---|---|
| `development_mode` | `true` disables OAuth for local use. `false` (production) requires a valid JWT on every MCP request. |
| `client_auth.issuer` | IdP issuer URL. Required when `development_mode: false`. |
| `client_auth.audience` | Expected `aud` claim value. Required when `development_mode: false`. |
| `client_auth.resource_url` | OAuth resource URL (e.g., `https://mcp.example.com/mcp`). Defaults to localhost if not set. |
| `client_auth.dev_token` | Static bearer token for development. Only used when `development_mode: true`. |

## Rate limiting and retry

Configured under the `semp` key. Controls how the server throttles and retries requests to broker SEMP APIs.

| YAML field | Default | Description |
|---|---|---|
| `semp.request_min_interval` | `100ms` | Minimum spacing between successive SEMP requests per broker. Set to `0` to disable throttling. |
| `semp.request_timeout_duration` | `1m` | HTTP request timeout for individual SEMP calls. |
| `semp.retries` | `10` | Maximum retry attempts for a failed SEMP call. Set to `0` to disable retries. |
| `semp.retry_min_interval` | `3s` | Starting backoff before the first retry. |
| `semp.retry_max_interval` | `30s` | Maximum backoff cap regardless of retry count. |
| `semp.max_concurrent_per_broker` | `10` | Maximum concurrent SEMP requests per broker. |
