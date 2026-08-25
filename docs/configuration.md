# Configuration

The server is configured via a YAML configuration file plus a `.env` file for credentials. See the [Quickstart](../README.md#quickstart) in the README for a minimal working example.

## Configuration File Location

The server searches for the configuration file in this order:

| Priority | Source | Value |
|---|---|---|
| 1 | `CONFIG_FILE` environment variable | Custom path |
| 2 | System path | `/etc/mcp-server/config.yaml` |
| 3 | Local path | `./broker-config.yaml` |

Set `CONFIG_FILE` explicitly if the file is in a non-standard location.

The server loads a separate credentials file automatically (default: `.env` in the same directory as the configuration file). Override the path with the `ENV_FILE` environment variable.

## Environment Variable Substitution

Use `${VAR_NAME}` anywhere in the YAML configuration to reference an environment variable:

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
| `listen_address` | — | see Bind address | Host the server binds to. Empty binds all interfaces; the default depends on the client auth mode. |
| `allow_remote_unauthenticated` | — | `false` | Opt in to a non-loopback `listen_address` while `mcp_client_auth.mode: disabled`. Acknowledges that the listener has no client authentication. |
| `allow_insecure_broker_tls` | — | `false` | Opt in to an event broker with `insecure_skip_verify: true` while `mcp_client_auth.mode: oauth`. Acknowledges that disabling event broker certificate verification exposes the event broker admin credential to a man-in-the-middle. |
| `tls_cert_file` | — | none | Path to TLS certificate (PEM). |
| `tls_key_file` | — | none | Path to TLS private key (PEM). |
| `tls_terminated_upstream` | — | `false` | Opt in to a plaintext listener while `mcp_client_auth.mode: oauth`. Acknowledges that TLS is terminated by an upstream proxy/ingress. Ignored in the dev modes. |
| `log_level` | — | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |
| `enable_write_tools` | — | `false` | When `true`, register every tool that is not read-only (16 in total): the four action-API tools (`delete-queue-messages`, `clear-queue-stats`, `disconnect-client`, `clear-client-stats`) plus the 12 Config-API management tools (`create`/`update`/`delete` for `message-vpn`, `queue`, `topic-endpoint`, and `rdp`). This includes the non-destructive stats-reset tools (which still mutate event broker state) and provisioning tools such as `delete-message-vpn`. When `false`, those tools are skipped at registration and never appear in `tools/list`. Secure-by-default for trial / dev deployments. |

**TLS:** Provide both `tls_cert_file` and `tls_key_file` together — providing only one is a startup error. When both are set, the server starts with HTTPS; when neither is set, plain HTTP.

The certificate must carry at least one DNS or IP SAN. A Common Name alone is not enough for any TLS client (Go has ignored the Common Name since 1.15), and in the container image it is specifically enforced by the built-in health check: the `--health` probe verifies the server's certificate against this file, so a SAN-less certificate makes the container report unhealthy even though the server itself starts and serves HTTPS. The server loads the keypair once at startup, so replacing either file in place takes effect only after a restart.

```yaml
port: 9090
tls_cert_file: "/etc/certs/server.pem"
tls_key_file: "/etc/certs/server-key.pem"
```

Under `mode: oauth` (production) a plaintext listener would carry client bearer tokens and tool results in cleartext, so it is **refused at startup** unless either TLS certs are set (see TLS) or `tls_terminated_upstream: true` acknowledges that an upstream proxy/ingress terminates TLS. With the acknowledgment the server serves plaintext and logs a startup `WARN`. The dev modes (`static`/`disabled`) are unaffected. See [Authentication](authentication.md) for the two deployment patterns.

**Bind address:** When `listen_address` is unset, the effective bind depends on `mcp_client_auth.mode` so the dev modes are safe by default — they are not reachable from the network unless an operator opts in:

| `mcp_client_auth.mode` | `listen_address` unset | `listen_address` set |
|---|---|---|
| `oauth` | all interfaces (`:port`) | used verbatim |
| `disabled` / `static` | `127.0.0.1` only | used verbatim |

An explicit `listen_address` must be an IP address or `localhost`. Under `mode: disabled` (no client authentication), binding a non-loopback address is **refused at startup** — it would expose unauthenticated MCP access backed by the event broker admin credential to the network. To proceed, bind `127.0.0.1`, switch to `mode: oauth`, or set `allow_remote_unauthenticated: true` to accept the risk. The effective bind address is logged at startup.

**Event Broker TLS in production:** Under `mode: oauth`, an event broker with `insecure_skip_verify: true` is **refused at startup** — disabling certificate verification would expose the event broker admin credential the server sends on every SEMP request to a man-in-the-middle. To proceed, use a trusted certificate, or set `allow_insecure_broker_tls: true` to accept the risk. `allow_insecure_broker_tls` is a single server-wide opt-in, not per event broker: setting it to onboard one self-signed event broker also lifts the check for every other event broker in the configuration, including ones added later. Dev modes (`static`/`disabled`) allow self-signed event brokers without the opt-in.

**Logging:** The server writes structured JSON logs to stderr. The server automatically redacts credentials in all log output. Every tool invocation is logged with the tool name, target event broker, status, and duration.

## Event Broker Settings

Configured under the `brokers` map. Each key defines an event broker alias for the `broker` parameter in MCP tools.

Aliases must be 1-63 characters, contain only letters, digits, and hyphens, and start and end with an alphanumeric character. Comparison is case-insensitive — `Prod` and `prod` collide and the server refuses to start. Original casing is preserved in all user-facing output.

| YAML field | Default | Description |
|---|---|---|
| `url` | — | SEMP management API base URL (for example, `https://broker:1943`). |
| `auth.mode` | — | `basic`, `bearer`, or `oauth`. |
| `auth.username` | — | Basic auth username. |
| `auth.password` | — | Basic auth password. |
| `auth.token` | — | Bearer token (used when `auth.mode: bearer`). |
| `auth.audience` | — | Optional, even under `auth.mode: oauth` — omitting it does not fail configuration load or startup. RFC 8693 audience value for this event broker, forwarded to the IdP during token exchange (requires the top-level `broker_oauth:` block — see [Broker OAuth (Hop 2)](#broker-oauth-hop-2)). When omitted, the runtime sends the token-exchange request without an audience parameter at all. Omit when the event broker's OAuth profile does not validate audience; set it only if the event broker's OAuth profile does. If set, it must not be whitespace-only — a `${VAR}` that resolves to blank does fail configuration load. |
| `insecure_skip_verify` | `false` | Skip TLS certificate verification. Development only. Under `mcp_client_auth.mode: oauth` (production) it is **refused at startup** unless `allow_insecure_broker_tls: true` is also set (see Event Broker TLS in production). |

Under `mcp_client_auth.mode: oauth`, every configured event broker's `url` must be `https://` — the server refuses to start otherwise. Outside `oauth` mode, Solace still recommends `https://` event broker URLs, but it isn't enforced.

```yaml
brokers:
  my-broker:
    url: "https://broker:1943"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

## Broker OAuth (Hop 2)

Configured under the top-level `broker_oauth` key. Required when any event broker uses `auth.mode: oauth` — obtains the event-broker-bound token by exchanging the calling agent's Hop 1 token (RFC 8693 token exchange) against an identity provider (IdP).

`mcp_client_auth.mode: oauth` (Hop 1) is required first: token exchange consumes the agent's Hop 1 JSON Web Token (JWT) as its `subject_token`, so an event broker with `auth.mode: oauth` while Hop 1 is `static`/`disabled` is **refused at configuration load** with an `mcp_client_auth.mode must be oauth` error naming the affected event broker(s). The `broker_oauth:` block itself is likewise required once any event broker uses `auth.mode: oauth` — omitting it fails configuration load with `broker_oauth block is required when any broker uses auth.mode: "oauth"`. Every field in the following table (other than `circuit_breaker`/`retry_after`) is required; an empty or unsupported value fails configuration load naming that field.

| YAML field | Default | Description |
|---|---|---|
| `idp_token_endpoint` | — | **Required.** The IdP's token endpoint URL (the token-exchange POST target). Must be `https://` in production. |
| `mcp_server_client_id` | — | **Required.** The MCP server's own `client_id`, registered at the IdP. |
| `mcp_server_client_auth` | — | **Required.** Discriminated union — exactly one of the following sub-blocks must be populated. |
| `mcp_server_client_auth.client_secret_basic.secret` | — | Client secret sent via HTTP Basic auth (RFC 6749 §2.3). |
| `mcp_server_client_auth.client_secret_post.secret` | — | Client secret sent in the token-request form body (RFC 6749 §2.3). |
| `grant_type` | — | **Required.** Selects the OAuth grant type for the Hop 2 exchange. Must be `"urn:ietf:params:oauth:grant-type:token-exchange"` (RFC 8693) — the only grant type this version implements; any other value is rejected at configuration load. |
| `audience_parameter_name` | — | **Required.** Which request parameter carries each event broker's `auth.audience` value. Must be `audience` (RFC 8693 default) — the only value implemented in this version; any other value, including `scope` (Entra On-Behalf-Of style) or `resource` (RFC 8707), is rejected at configuration load. |
| `circuit_breaker` | omitted (all defaults, enabled) | Optional. See [Circuit Breaker](#circuit-breaker). |
| `retry_after` | omitted (default cap) | Optional. See [Retry-After Gate](#retry-after-gate). |

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://your-idp.example.com/realms/your-realm"
  audience: "solace-mcp-server"
  resource_url: "https://your-mcp-server.example.com/mcp"

broker_oauth:
  idp_token_endpoint: "https://your-idp.example.com/realms/your-realm/protocol/openid-connect/token"
  mcp_server_client_id: "mcp-server"
  mcp_server_client_auth:
    client_secret_basic:
      secret: "${MCP_SERVER_CLIENT_SECRET}"
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: "audience"

brokers:
  prod:
    url: "https://broker.example.com:943"
    auth:
      mode: oauth
      audience: "solace-broker-prod"
```

### Circuit Breaker

Nested under `broker_oauth.circuit_breaker`. Protects the shared IdP from a sustained outage by failing exchanges fast instead of driving every event broker's requests into the full retry budget. Every field is optional; an omitted field falls back to its shipped default. Setting `circuit_breaker: {}` is equivalent to omitting the block entirely.

| YAML field | Default | Description |
|---|---|---|
| `enabled` | `true` | Escape hatch — `false` disables the breaker while retries still run, and logs a startup WARN. Not recommended in production. |
| `failure_rate_window` | `30s` | Rolling window over which the failure rate is measured. |
| `minimum_requests` | `10` | Minimum classified exchanges in the window before the failure-rate rule can trip. |
| `failure_rate_threshold_percent` | `50` | Percentage of counted (non-excluded) exchanges failing that trips the breaker. |
| `consecutive_failure_threshold` | `5` | Consecutive failures that trip the breaker immediately, without waiting for the rate rule's sample. `0` disables this rule. |
| `open_state_duration` | `30s` | How long the breaker stays open (rejecting exchanges immediately) before probing recovery. |
| `half_open_probe_requests` | `2` | Consecutive successful probes required to close the breaker again. |

### Retry-After Gate

Nested under `broker_oauth.retry_after`. Shares a process-wide backoff across every event broker when an exhausted 429 retry chain to the IdP returns a `Retry-After` header, so one throttled event broker doesn't let every other event broker keep hammering the same IdP.

| YAML field | Default | Description |
|---|---|---|
| `max_honored_duration` | `60s` | Ceiling on how long a `Retry-After` value is honored. An IdP-requested duration longer than this is clamped to the cap rather than honored uncapped. Must be a positive duration. |

## Client Authentication Settings

Configured under the `mcp_client_auth` key. The `mode` field is required and selects the auth backend; required peer fields follow from the mode. The previous `development_mode` flag is deprecated — its presence is parsed but ignored, with a deprecation warning logged at startup. See [Authentication](authentication.md) for full setup instructions.

| YAML field | Description |
|---|---|
| `mcp_client_auth.mode` | **Required.** One of `disabled`, `static`, or `oauth`. Selects the client auth backend and the operational profile (dev versus production). |
| `mcp_client_auth.dev_token` | Static bearer token. Required when `mcp_client_auth.mode` is `static`. |
| `mcp_client_auth.issuer` | IdP issuer URL. Required when `mcp_client_auth.mode` is `oauth`. |
| `mcp_client_auth.audience` | Expected `aud` claim value. Required when `mcp_client_auth.mode` is `oauth`. |
| `mcp_client_auth.resource_url` | OAuth resource URL (for example, `https://mcp.example.com/mcp`). Required when `mcp_client_auth.mode` is `oauth`. |
| `mcp_client_auth.tool_authorization` | Claim-based tool authorization block. Required under `mcp_client_auth.mode: oauth` — the `enabled` field must be set explicitly to `true` or `false`; omitting the block, or omitting `enabled` from it, is a startup error. Not legal under `static` or `disabled`. See [Tool authorization](#tool-authorization). |

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
| `filter_tools_list` | Optional, defaults to `false`. When `true`, `tools/list` returns only the tools the caller's groups grant, instead of every registered tool. Only meaningful when tool authorization is on (`enabled: true` in this same block) — setting it while `enabled: false` logs a startup `WARN` and leaves filtering off, because there is no policy to filter against. See [Filtering `tools/list`](#filtering-toolslist). |
| `groups_claim_name` | Name of the OIDC claim in the caller's JWT that carries their group or role memberships. Optional; defaults to `"groups"`. Must match the claim your IdP emits (see [Authentication](authentication.md) for setting this up on the IdP side). Only meaningful when `enabled: true`. **Top-level lookup only** — the value is read from the top of the JWT claims object; nested paths (for example, `authorization.roles`) are not supported. If your IdP emits memberships inside a nested object, flatten them into a top-level claim with an IdP mapper before the token is issued. |
| `access_level_groups` | Map from group name — as it appears in the caller's token — to the list of MCP tool names that group grants. Required when `enabled: true`. Union semantics: a caller is allowed to invoke a tool when at least one of their groups grants it. A tool that no group grants is unreachable by every caller. **No wildcard** — a group intended to grant every tool must list every tool name explicitly. This is deliberate: an "all tools" glob would silently include every newly-added tool at upgrade time, without the operator noticing the surface expanded. |

**`list-brokers` and `describe-semp-schema` are structurally exempt.** Every authenticated caller can invoke these tools regardless of their groups; neither is composed with the authorization wrapper. `list-brokers` lets callers discover which event broker aliases exist; `describe-semp-schema` lets callers inspect the SEMPv2 schema for any operation (spec content only, no event broker state). Listing either tool in an `access_level_groups` entry is inert — the server emits a startup `WARN` naming the group but the grant has no effect.

**Interaction with `enable_write_tools`.** The two controls are orthogonal — they answer different questions, and a caller reaches a tool only when both answers permit it. `enable_write_tools` decides which tools the server registers at all; `access_level_groups` decides which callers may invoke a registered tool. Granting a write/action tool (for example `delete-queue-messages`, `disconnect-client`, `clear-queue-stats`, `clear-client-stats`, or any of the Config-API management tools) while `enable_write_tools: false` is therefore inert — the tool never appears in `tools/list`, so the grant cannot take effect. This is a supported way to stage an RBAC policy ahead of enabling write tools, so it is not a startup error; the server emits one startup `WARN` per inert tool naming the tool and the referencing groups. Setting `enable_write_tools: true` activates those grants and silences the WARN. A grant naming a tool the server does not know at all remains a **fatal** startup error.

**Group soft cap.** If a caller's claim carries more than 250 memberships, the server uses the first 250 in JWT-array order and emits a WARN carrying the total count and the cap (no caller identity, so as not to reveal group counts per user on the shared log stream). The cap sits above the ceilings of the major IdPs (Entra 200, Okta 100), so legitimate deployments never hit it — the WARN indicates either a claim mapper misconfiguration on the IdP or a caller belonging to unusually many groups.

### Filtering `tools/list`

By default every authenticated caller receives the full tool list, even for tools their groups do not grant — authorization is enforced when they try to invoke one. Setting `filter_tools_list: true` narrows `tools/list` to the tools the caller may actually invoke:

```yaml
mcp_client_auth:
  mode: oauth
  issuer: "https://idp.example.com"
  audience: "mcp-server"
  resource_url: "https://mcp.example.com/mcp"
  tool_authorization:
    enabled: true
    filter_tools_list: true
    groups_claim_name: "groups"
    access_level_groups:
      Ops:
        - list-vpns
```

The benefit is context, not access control: an AI agent handed tools it will be denied spends context on their descriptions and schemas, and tends to misreport the cause when a call is refused. A caller in a narrow role can drop from approximately 24 tools to two or three.

**This is not an access control.** `tools/call` remains the only authorization boundary, and it is enforced identically whether filtering is on or off. A tool absent from the list is still callable by name — the server resolves tool calls against the full registered set, not against whatever a previous list returned. Filtering changes what a caller *sees*, never what they may *do*.

**If tools are missing or your client misbehaves, set `filter_tools_list: false` and restart.** Authorization is still fully enforced on every tool call, so turning filtering off does not weaken access control. This is the first thing to try when a caller reports missing tools.

The filter reuses the same policy decision as `tools/call`, so a listed tool is always callable and a callable tool is always listed. The two structurally exempt tools (`list-brokers` and `describe-semp-schema`) are never filtered — they are always callable, so hiding them would break that guarantee and remove event broker discovery for every caller.

A caller whose groups grant nothing receives a normal response containing only the exempt tools — never an error and never an empty list. The same applies when the token carries no groups claim at all: the filter fails closed. The two cases are deliberately indistinguishable to the caller but are distinguishable in the server log (see the following section).

**Startup posture.** The server states the filtering posture on every boot, alongside the `tool authorization is enabled/disabled` line:

| `tool_authorization.enabled` | `filter_tools_list` | Log |
|---|---|---|
| `true` | `true` | `INFO` — `tools/list filtering is enabled` |
| `true` | absent or `false` | `INFO` — `tools/list filtering is disabled`, `reason: filter_tools_list not set` |
| `false` | `true` | `WARN` — `tools/list filtering is disabled`, `reason: tool_authorization.enabled is false`. The server starts normally; the flag has no policy to filter against. |
| `false` | absent or `false` | `INFO` — `tools/list filtering is disabled`, `reason: filter_tools_list not set` |

**Filter audit logging.** Because nothing about the filtering reaches the caller, the server log is the only diagnostic. Each filtered `tools/list` emits one line with `msg: "tool list filter"` and `event: "tool_list_filter"`, carrying the same caller identity fields as the call path (`sub`, `iss`, `client_id`, `jti`, `correlation_id`) plus:

| Field | Meaning |
|---|---|
| `decision_reason` | `"filtered"` — groups matched some grants and at least one tool was removed. `"unfiltered"` — the caller's grants cover every registered tool. `"not_permitted"` — the claim was present but matched no grants, so only the exempt tools remain. `"missing_claim"` — the token carried no claim under the configured `groups_claim_name`; the filter failed closed. |
| `groups_present` | Whether the token carried the groups claim at all. |
| `tools_before` / `tools_after` | Tool counts entering and leaving the filter. |
| `expected_claim` | The configured `groups_claim_name`. Present on `missing_claim` only, so a deployment using a non-default claim name can be triaged from the record without cross-referencing the configuration. |

`missing_claim` is logged at **`WARN`**; every other outcome at `INFO`. The distinction matters: `not_permitted` is the policy working as configured, while `missing_claim` means the token could not answer the authorization question at all — typically an IdP claim-mapper misconfiguration, which affects every caller of that deployment rather than one user. The same reasons appear on the `tool authorization` call-path line, so filter both by `event` to tell the two paths apart.

As on the call path, the caller's actual group names are never logged.

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
