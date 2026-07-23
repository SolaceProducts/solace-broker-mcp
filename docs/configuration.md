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
| `allow_insecure_broker_tls` | — | `false` | Opt-in to a broker with `insecure_skip_verify: true` while `mcp_client_auth.mode: oauth`. Acknowledges that disabling broker certificate verification exposes the broker admin credential to a man-in-the-middle. |
| `tls_cert_file` | — | none | Path to TLS certificate (PEM). |
| `tls_key_file` | — | none | Path to TLS private key (PEM). |
| `tls_terminated_upstream` | — | `false` | Opt-in to a plaintext listener while `mcp_client_auth.mode: oauth`. Acknowledges that TLS is terminated by an upstream proxy/ingress. Ignored in the dev modes. |
| `log_level` | — | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |
| `enable_write_tools` | — | `false` | When `true`, register every tool that is not read-only (13 in total): the four action-API tools (`delete-queue-messages`, `clear-queue-stats`, `disconnect-client`, `clear-client-stats`) plus the nine Config-API management tools (`create`/`update`/`delete` for `message-vpn`, `queue`, and `topic-endpoint`). This includes the non-destructive stats-reset tools (which still mutate broker state) and provisioning tools such as `delete-message-vpn`. When `false`, those tools are skipped at registration and never appear in `tools/list`. Secure-by-default for trial / dev deployments. |

**TLS:** Provide both `tls_cert_file` and `tls_key_file` together — providing only one is a startup error. When both are set, the server starts with HTTPS; when neither is set, plain HTTP.

```yaml
port: 9090
tls_cert_file: "/etc/certs/server.pem"
tls_key_file: "/etc/certs/server-key.pem"
```

Under `mode: oauth` (production) a plaintext listener would carry client bearer tokens and tool results in cleartext, so it is **refused at startup** unless either TLS certs are set (above) or `tls_terminated_upstream: true` acknowledges that an upstream proxy/ingress terminates TLS. With the acknowledgment the server serves plaintext and logs a startup `WARN`. The dev modes (`static`/`disabled`) are unaffected. See [Authentication](authentication.md) for the two deployment patterns.

**Bind address:** When `listen_address` is unset, the effective bind depends on `mcp_client_auth.mode` so the dev modes are safe by default — they are not reachable from the network unless an operator opts in:

| `mcp_client_auth.mode` | `listen_address` unset | `listen_address` set |
|---|---|---|
| `oauth` | all interfaces (`:port`) | used verbatim |
| `disabled` / `static` | `127.0.0.1` only | used verbatim |

An explicit `listen_address` must be an IP address or `localhost`. Under `mode: disabled` (no client authentication), binding a non-loopback address is **refused at startup** — it would expose unauthenticated MCP access backed by the broker admin credential to the network. To proceed, bind `127.0.0.1`, switch to `mode: oauth`, or set `allow_remote_unauthenticated: true` to accept the risk. The effective bind address is logged at startup.

**Broker TLS in production:** Under `mode: oauth`, a broker with `insecure_skip_verify: true` is **refused at startup** — disabling certificate verification would expose the broker admin credential the server sends on every SEMP request to a man-in-the-middle. To proceed, use a trusted certificate, or set `allow_insecure_broker_tls: true` to accept the risk. `allow_insecure_broker_tls` is a single server-wide opt-in, not per-broker: setting it to onboard one self-signed broker also lifts the check for every other broker in the config, including ones added later. Dev modes (`static`/`disabled`) allow self-signed brokers without the opt-in.

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
| `insecure_skip_verify` | `false` | Skip TLS certificate verification. Development only. Under `mcp_client_auth.mode: oauth` (production) it is **refused at startup** unless `allow_insecure_broker_tls: true` is also set (see below). |

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
| `mcp_client_auth.tool_authorization` | Optional claim-based tool authorization block. Only legal under `mcp_client_auth.mode: oauth` — the validator refuses to start if it is set under `static` or `disabled`. See [Tool authorization](#tool-authorization) below. |

## Tool Authorization

Under `mcp_client_auth.mode: oauth`, the server can gate individual MCP tools by the caller's group or role memberships. The caller's identity provider must include a claim in the issued access token that lists these memberships — the name of the claim is configurable via `groups_claim_name` (defaulting to `"groups"`). The server compares the values in that claim against a policy the operator defines under `mcp_client_auth.tool_authorization`. Under `mode: static` or `mode: disabled` the feature is off and a `tool_authorization` block is a startup error.

**Opting in or out is required under `mode: oauth`.** You must set `enabled` explicitly to `true` or `false` — there is no implicit default. Omitting the block, or omitting `enabled` from it, is a startup error. This forces every oauth deployment to make a deliberate choice on tool-level access control.

Opt out — every authenticated caller can invoke any tool:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp-server"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: false
```

Opt in — gate tools by group:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp-server"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: true
    groups_claim_name: "groups"
    access_level_groups:
      Ops:
        - list-vpns
        - list-queues
        - get-queue-metrics
      Admin:
        - list-vpns
        - list-queues
        - get-queue-metrics
        - delete-queue-messages
```

| YAML field | Description |
|---|---|
| `enabled` | Required. `true` turns tool authorization on; `false` turns it off. There is no default — the field must be present under `mode: oauth`. |
| `groups_claim_name` | Name of the OIDC claim in the caller's JWT that carries their group or role memberships. Optional; defaults to `"groups"`. Must match the claim your IdP emits (see [Authentication](authentication.md) for setting this up on the IdP side). Only meaningful when `enabled: true`. **Top-level lookup only** — the value is read from the top of the JWT claims object; nested paths (e.g. `authorization.roles`) are not supported. If your IdP emits memberships inside a nested object, flatten them into a top-level claim with an IdP mapper before the token is issued. |
| `access_level_groups` | Map from group name — as it appears in the caller's token — to the list of MCP tool names that group grants. Required when `enabled: true`. Union semantics: a caller is allowed to invoke a tool when at least one of their groups grants it. A tool that no group grants is unreachable by every caller. **No wildcard** — a group that should grant every tool must list every tool name explicitly. This is deliberate: an "all tools" glob would silently include every newly-added tool at upgrade time, without the operator noticing the surface expanded. |

**`list-brokers` is structurally exempt.** Every authenticated caller can invoke `list-brokers` regardless of their groups; the tool is not composed with the authorization wrapper at all. A caller needs it to discover which broker aliases exist before invoking any other tool, so gating it would deadlock every session. Listing `list-brokers` in an `access_level_groups` entry is inert — the server emits a startup `WARN` naming the group but the grant has no effect.

**Group soft cap.** If a caller's claim carries more than 250 memberships, the server uses the first 250 in JWT-array order and emits a WARN naming the caller. The cap sits above the ceilings of the major IdPs (Entra 200, Okta 100), so legitimate deployments never hit it — the WARN indicates either a claim mapper misconfiguration on the IdP or a caller belonging to unusually many groups.

**Audit logging.** Every gated tool invocation emits a single structured log line with `msg: "tool authorization"` before dispatching the underlying tool call. The line carries the caller identity fields the server already logs (`sub`, `iss`, `client_id`, `jti`, `correlation_id`) plus authorization-specific fields:

| Field | On allow | On deny |
|---|---|---|
| `decision` | `"allowed"` | `"denied"` |
| `decision_reason` | (absent) | `"missing_claim"` when the token has no claim under the configured `groups_claim_name`, or `"not_permitted"` when the claim is present but no matched group grants the tool |
| `expected_claim` | (absent) | the configured `groups_claim_name` — present on `missing_claim` so operators can distinguish an IdP mapper misconfiguration (token missing the claim entirely) from a server-side typo (`groups_claim_name` set to a value the IdP does not emit) |
| `matched_groups` | sanitized names of the caller groups that grant the tool | `[]` on `not_permitted`; absent on `missing_claim` |
| `matched_groups_total` | the true number of matched groups before capping | `0` on `not_permitted`; absent on `missing_claim` |
| `matched_groups_truncated` | `true` when `matched_groups` was capped at 32 entries | `false` on `not_permitted`; absent on `missing_claim` |

Allow decisions log at `INFO`; denials log at `WARN`. The caller sees only a generic `"You are not authorized to use this tool."` error — the distinction between `missing_claim` and `not_permitted` stays server-side. `list-brokers` calls never emit this line because the tool is not gated; a `"tool invoked"` line stands alone for them.

Denied caller groups are never logged on the deny path — only the fact of denial and the reason code. That separation is deliberate: it keeps a caller's group membership off the operator's audit stream while still letting alerting fire on `decision: "denied"`.

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
