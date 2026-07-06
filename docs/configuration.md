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
| `listen_address` | — | see below | Host the server binds to. Empty binds all interfaces; the default depends on the client auth mode. |
| `allow_remote_unauthenticated` | — | `false` | Opt-in to a non-loopback `listen_address` while `mcp_client_auth.mode: disabled`. Acknowledges that the listener has no client authentication. |
| `tls_cert_file` | — | none | Path to TLS certificate (PEM). |
| `tls_key_file` | — | none | Path to TLS private key (PEM). |
| `log_level` | — | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |
| `enable_write_tools` | — | `false` | When `true`, register every tool that is not read-only (13 in total): the four action-API tools (`delete-queue-messages`, `clear-queue-stats`, `disconnect-client`, `clear-client-stats`) plus the nine Config-API management tools (`create`/`update`/`delete` for `message-vpn`, `queue`, and `topic-endpoint`). This includes the non-destructive stats-reset tools (which still mutate broker state) and provisioning tools such as `delete-message-vpn`. When `false`, those tools are skipped at registration and never appear in `tools/list`. Secure-by-default for trial / dev deployments. |

**TLS:** Provide both `tls_cert_file` and `tls_key_file` together — providing only one is a startup error. When both are set, the server starts with HTTPS; when neither is set, plain HTTP.

```yaml
port: 9090
tls_cert_file: "/etc/certs/server.pem"
tls_key_file: "/etc/certs/server-key.pem"
```

**Bind address:** When `listen_address` is unset, the effective bind depends on `mcp_client_auth.mode` so the dev modes are safe by default — they are not reachable from the network unless an operator opts in:

| `mcp_client_auth.mode` | `listen_address` unset | `listen_address` set |
|---|---|---|
| `oauth` | all interfaces (`:port`) | used verbatim |
| `disabled` / `static` | `127.0.0.1` only | used verbatim |

An explicit `listen_address` must be an IP address or `localhost`. Under `mode: disabled` (no client authentication), binding a non-loopback address is **refused at startup** — it would expose unauthenticated MCP access backed by the broker admin credential to the network. To proceed, bind `127.0.0.1`, switch to `mode: oauth`, or set `allow_remote_unauthenticated: true` to accept the risk. The effective bind address is logged at startup.

**Logging:** The server writes structured JSON logs to stderr. The server automatically redacts credentials in all log output. Every tool invocation is logged with the tool name, target broker, status, and duration.

## Event Broker Settings

Configured under the `brokers` map. Each key defines an event broker alias for the `broker` parameter in MCP tools.

Aliases must be 1–63 characters, contain only letters, digits, and hyphens, and start and end with an alphanumeric character. Comparison is case-insensitive — `Prod` and `prod` collide and the server will refuse to start. Original casing is preserved in all user-facing output.

| YAML field | Default | Description |
|---|---|---|
| `url` | — | SEMP management API base URL (for example, `https://broker:1943`). |
| `auth.mode` | — | `basic` or `bearer`. |
| `auth.username` | — | Basic auth username. |
| `auth.password` | — | Basic auth password. |
| `auth.token` | — | Bearer token (used when `auth.mode: bearer`). |
| `insecure_skip_verify` | `false` | Skip TLS certificate verification. Development only — do not use in production. |

Solace recommends using `https://` event broker URLs in production environments.

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

Configured under the `mcp_client_auth` key. The `mode` field is required and selects the auth backend; required peer fields follow from the mode. The previous `development_mode` flag is deprecated — its presence is parsed but ignored, with a deprecation warning logged at startup. See [Authentication](authentication.md) for full setup instructions.

| YAML field | Description |
|---|---|
| `mcp_client_auth.mode` | **Required.** One of `disabled`, `static`, or `oauth`. Selects the client auth backend and the operational profile (dev vs. production). |
| `mcp_client_auth.dev_token` | Static bearer token. Required when `mcp_client_auth.mode` is `static`. |
| `mcp_client_auth.issuer` | IdP issuer URL. Required when `mcp_client_auth.mode` is `oauth`. |
| `mcp_client_auth.audience` | Expected `aud` claim value. Required when `mcp_client_auth.mode` is `oauth`. |
| `mcp_client_auth.resource_url` | OAuth resource URL (for example, `https://mcp.example.com/mcp`). Required when `mcp_client_auth.mode` is `oauth`. |

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
