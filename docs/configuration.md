# Configuration

The server is configured via a YAML config file plus a `.env` file for credentials. See the [Quickstart](../README.md#quickstart) in the README for a minimal working example.

## Config File Location

The server searches for the config file in this order:

| Priority | Source | Value |
|---|---|---|
| 1 | `CONFIG_FILE` env var | Custom path |
| 2 | System path | `/etc/mcp-server/config.yaml` |
| 3 | Local path | `./broker-config.yaml` |

Set `CONFIG_FILE` explicitly if the file is in a non-standard location.

The server loads a separate credentials file automatically (default: `.env` in the same directory as the config file). Override the path with the `ENV_FILE` env var.

## Environment Variable Substitution

Use `${VAR_NAME}` anywhere in the YAML config to reference an environment variable:

```yaml
brokers:
  my-broker:
    auth:
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

The server resolves variables at startup. The `.env` file loads automatically before substitution. Precedence: environment variable > `.env` file > YAML literal value.

## Server Settings

| YAML field | Env var | Default | Description |
|---|---|---|---|
| `port` | `MCP_SERVER_PORT` | `9090` | Port the MCP server listens on. |
| `tls_cert_file` | — | none | Path to TLS certificate (PEM). |
| `tls_key_file` | — | none | Path to TLS private key (PEM). |
| `log_level` | — | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |

**TLS:** Provide both `tls_cert_file` and `tls_key_file` together — providing only one is a startup error. When both are set, the server starts with HTTPS; when neither is set, plain HTTP.

```yaml
port: 9090
tls_cert_file: "/etc/certs/server.pem"
tls_key_file: "/etc/certs/server-key.pem"
```

**Logging:** The server writes structured JSON logs to stderr. The server automatically redacts credentials in all log output. Every tool invocation is logged with the tool name, target broker, status, and duration.

## Event Broker Settings

Configured under the `brokers` map. Each key defines an event broker alias for the `broker` parameter in MCP tools.

| YAML field | Default | Description |
|---|---|---|
| `url` | — | SEMP management API base URL (for example, `https://broker:1943`). |
| `auth.mode` | — | `basic` or `bearer`. |
| `auth.username` | — | Basic auth username. |
| `auth.password` | — | Basic auth password. |
| `auth.token` | — | Bearer token (used when `auth.mode: bearer`). |
| `insecure_skip_verify` | `false` | Skip TLS certificate verification. Development only — do not use in production. |

**Production recommendation:** Solace recommends using `https://` event broker URLs in production environments.

```yaml
brokers:
  my-broker:
    url: "https://broker:1943"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

## Client Authentication Settings

Configured under the `client_auth` key. The `mode` field is required and selects the auth backend; required peer fields follow from the mode. The previous `development_mode` flag is deprecated — its presence is parsed but ignored, with a deprecation warning logged at startup. See [Authentication](authentication.md) for full setup instructions.

| YAML field | Description |
|---|---|
| `client_auth.mode` | **Required.** One of `disabled`, `static`, or `oauth`. Selects the client auth backend and the operational profile (dev vs. production). |
| `client_auth.dev_token` | Static bearer token. Required when `client_auth.mode` is `static`. |
| `client_auth.issuer` | IdP issuer URL. Required when `client_auth.mode` is `oauth`. |
| `client_auth.audience` | Expected `aud` claim value. Required when `client_auth.mode` is `oauth`. |
| `client_auth.resource_url` | OAuth resource URL (for example, `https://mcp.example.com/mcp`). Required when `client_auth.mode` is `oauth`. |

## Rate Limiting and Retry

Configured under the `semp` key. Controls how the server throttles and retries requests to event broker SEMP APIs.

| YAML field | Default | Description |
|---|---|---|
| `semp.request_min_interval` | `100ms` | Minimum spacing between successive SEMP requests per event broker. Set to `0` to disable throttling. |
| `semp.request_timeout_duration` | `1m` | HTTP request timeout for individual SEMP calls. |
| `semp.retries` | `10` | Maximum retry attempts for a failed SEMP call. Set to `0` to disable retries. |
| `semp.retry_min_interval` | `3s` | Starting backoff before the first retry. |
| `semp.retry_max_interval` | `30s` | Maximum backoff cap regardless of retry count. |
| `semp.max_concurrent_per_broker` | `10` | Maximum concurrent SEMP requests per event broker. |
