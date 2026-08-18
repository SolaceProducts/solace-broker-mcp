# solace-broker-mcp

[![Build Status](https://github.com/SolaceProducts/solace-broker-mcp/workflows/Build%20and%20Test/badge.svg)](https://github.com/SolaceProducts/solace-broker-mcp/actions)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/SolaceProducts/solace-broker-mcp)](go.mod)
[![Contributor Covenant](https://img.shields.io/badge/Contributor%20Covenant-2.1-4baaaa.svg)](.github/CODE_OF_CONDUCT.md)
[![Latest Release](https://img.shields.io/github/v/release/SolaceProducts/solace-broker-mcp)](https://github.com/SolaceProducts/solace-broker-mcp/releases)

An MCP (Model Context Protocol) server for Solace event brokers, built with Go using the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Table of Contents

- [Overview](#overview)
- [Features](#features)
- [Architecture](#architecture)
- [Tools](#tools)
- [Guides](#guides)
- [Prerequisites](#prerequisites)
- [Quickstart](#quickstart)
  - [Configuration](#configuration)
  - [Binary Deployment](#binary-deployment)
  - [Install with go install](#install-with-go-install)
  - [Docker Deployment](#docker-deployment)
  - [Connect from Claude Code](#connect-from-claude-code)
  - [Connect from Solace Agent Mesh](#connect-from-solace-agent-mesh)
- [Development Setup](#development-setup)
  - [Configuration Options](#configuration-options)
- [Project Structure](#project-structure)
- [CI](#ci)
- [Contributing](#contributing)
- [Support](#support)
- [Security](#security)
- [Disclaimer](#disclaimer)
- [License](#license)

## Overview

An HTTP service that exposes Solace event broker management and monitoring to AI assistants through the Model Context Protocol (MCP). The server provides 40 tools: 24 read-only tools that query event broker status, inspect queues, diagnose client issues, and monitor message traffic, plus 16 optional write and action tools (off by default) for operational actions and configuration. It uses the Solace Element Management Protocol (SEMP) v1 and v2 APIs.

MCP-compatible clients, for example, Claude Code, invoke these tools using natural language. The AI assistant translates requests into tool calls. The server handles authentication, rate limiting, retries, and response formatting.

## Features

- **24 read-only monitoring tools** — Event broker status, Message VPNs, queues, clients, REST delivery points, bridges, Kafka receivers/senders, and SEMPv2 schema introspection
- **16 optional write and action tools** — Disconnect clients, delete queued messages, reset statistics, and create, update, or delete Message VPNs, queues, topic endpoints, and REST delivery points; gated behind `enable_write_tools` (off by default)
- **Client authentication** — Development mode (no auth), static bearer tokens, or OAuth 2.1/OIDC with JWT validation
- **Claim-based tool authorization** — Under OAuth mode, gate individual MCP tools by a configurable OIDC claim carrying the caller's group or role memberships (`groups` by default); `list-brokers` stays exempt so callers can always discover configured brokers
- **Multi-broker configuration** — Connect to multiple brokers and address them by configured alias
- **Retry and rate limiting** — Configurable backoff intervals and concurrent request limits per broker
- **Deployment options** — Standalone binary, Docker container, or Go source
- **TLS/HTTPS support** — Optional certificate-based transport encryption
- **Structured logging** — JSON output with automatic credential redaction

## Architecture

The server implements the MCP HTTP transport specification and exposes event broker operations as MCP tools. When an AI assistant invokes a tool, the server executes the corresponding SEMP API request against the target event broker and returns structured data to the client.

**Component diagram:**

```
                                           ┌───────────────────┐
                                           │  OAuth IdP        │
                                           │  (Keycloak etc.)  │ ◀── JWT validation
                                           │                   │     (production only)
                                           └─────────┬─────────┘
                                                     │
                                                     ▼
┌──────────────────┐   MCP over HTTP    ┌──────────────────────────┐   SEMPv1 + SEMPv2    ┌──────────────────┐
│                  │                    │   Broker MCP Server      │                      │                  │
│   AI Agent       │ ────────────────▶ │                          │  ──────────────────▶ │  Solace          │
│  (Claude Code,   │   JSON-RPC         │  • Auth (OAuth / token)  │   HTTP(S) /SEMP      │  Event           │
│  Claude Desktop) │   + Bearer JWT     │  • 24 read + 16 write    │                      │  Broker(s)       │
│                  │                    │  • Rate-limit + retry    │                      │                  │
│                  │ ◀──────────────── │  • SEMP client pool      │ ◀──────────────────  │                  │
└──────────────────┘                    └──────────────────────────┘  basic/bearer/oauth  └──────────────────┘
```

## Tools

The server exposes read-only tools grouped by what they inspect, plus write tools for operational actions and configuration management. Every tool except `list-brokers` and `describe-semp-schema` takes a `broker` parameter naming a configured broker alias. See the [Tools Reference](docs/tools-reference.md) for full per-tool parameters, output shape, and example invocations; the [user guide](docs/user-guide.md#tools-reference) has the narrative overview.

> **Note:** Results are interpreted and acted on by an AI assistant. Treat tool output as input to a human decision, not as verified fact, and confirm any write or destructive action before allowing it. See the [Disclaimer](#disclaimer).

| Category | Tools | Description |
|---|---|---|
| Discovery | `list-brokers`, `describe-semp-schema` | List configured broker aliases for use as the `broker` parameter; look up a SEMPv2 operation's request-body schema before calling a write tool |
| Broker status | `get-broker-status`, `get-redundancy-status` | Snapshot of version, uptime, resources, spool, and HA and mate-link state |
| Replication | `get-replication-status` | Replication role, sync eligibility, bridge status, transaction mode, and queued-message counts |
| Message VPN | `list-vpns`, `get-vpn-status`, `get-message-rates` | List VPNs, check per-VPN service status, read message and byte rates |
| Queues | `list-queues`, `get-queue-metrics` | List queues with cumulative spooled count and throughput; drill into a single queue for authoritative current depth, spool, and rates |
| Clients | `list-clients`, `get-client-details`, `list-client-subscriptions`, `list-slow-subscribers` | List connections, inspect per-client rates and discards, list subscriptions, filter for slow-subscriber-flagged clients |
| REST Delivery Points | `list-rdps`, `get-rdp-status` | List RDPs; inspect bindings, REST consumers, and last failure reason |
| Bridges | `list-bridges`, `get-bridge-status` | List bridges with inbound/outbound connection state; inspect a single bridge's connection establisher and failure category |
| Kafka | `list-kafka-receivers`, `get-kafka-receiver-status`, `list-kafka-senders`, `get-kafka-sender-status` | List Kafka Receivers/Senders with up/down status; inspect a single receiver's or sender's topic/queue-binding status |
| Discards | `get-discard-stats`, `list-queue-discards` | Broker-wide and per-VPN discard aggregates; per-queue discard counters |
| Actions | `delete-queue-messages`, `clear-queue-stats`, `disconnect-client`, `clear-client-stats` | One tool per operational action. Destructive tools (`delete-queue-messages`, `disconnect-client`) are annotated `destructiveHint` so clients can prompt before invocation, and their descriptions ask the model to confirm; the `clear-*-stats` tools are non-destructive. |
| Management | `create-message-vpn`, `update-message-vpn`, `delete-message-vpn`, `create-queue`, `update-queue`, `delete-queue`, `create-topic-endpoint`, `update-topic-endpoint`, `delete-topic-endpoint`, `create-rdp`, `update-rdp`, `delete-rdp` | Create, update, and delete Config-API objects (Message VPNs, queues, topic endpoints, REST delivery points). `delete-*` and the service-affecting `update-*` tools are annotated `destructiveHint` so clients can prompt before invocation, and their descriptions ask the model to confirm; `create-*` is additive and not annotated. |

**The action and management tools are write tools, gated behind `enable_write_tools: true` in the config — default off; not registered in `tools/list` when disabled.** That's 16 write tools in total (four action, 12 management), on top of the 24 read-only tools.

> **Confirmation is not enforced.** `enable_write_tools` is the only enforced control. `destructiveHint` and the confirmation text in tool descriptions are hints, not enforced by the MCP protocol — whether the user is actually prompted depends on the client and the model.

## Guides

- [User Guide](docs/user-guide.md) — overview, tools reference, deployment, and troubleshooting
- [Tools Reference](docs/tools-reference.md) — per-tool parameters, output schema, and example invocations for all 40 tools
- [Examples](docs/examples.md) — Claude Desktop config, natural-language queries, and multi-broker setup
- [Configuration](docs/configuration.md) — server settings, event broker config, client auth, and rate-limit/retry settings
- [Authentication](docs/authentication.md) — OAuth/OIDC and static token setup for MCP clients
- [Agent Mesh Integration](docs/sam-integration.md) — wire this MCP server into an Agent Mesh project as an agent

## Prerequisites

- Access to one or more Solace event brokers with SEMP management enabled
- [Docker](https://docs.docker.com/get-docker/) (for Docker deployment) or a supported OS/arch for the binary (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64)
- **For Development / Building from Source:**
  - [Go 1.25+](https://go.dev/dl/) — required to build and run the MCP server from source
  - Not needed if using pre-built binaries or Docker images

## Quickstart

### Configuration

All deployment methods use the same YAML config file and `.env` credentials file.

**1. Create a config file** (`broker-config.yaml`):

```yaml
mcp_client_auth:
  mode: disabled        # no client auth — local development only

brokers:
  my-broker:
    url: "http://my-broker.example.com:8080"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

`mcp_client_auth.mode: disabled` skips client authentication entirely — only use this for local development. For production, set `mcp_client_auth.mode: oauth` and provide `issuer`, `audience`, and `resource_url`. A third mode, `static`, accepts a fixed bearer token for local development with realistic auth flow. See [Authentication](docs/authentication.md) for full setup instructions.

**Audit-log identity.** In `oauth` and `static` modes, every tool-invocation log line carries the caller's `sub`, `iss`, `client_id`, and `jti` claims (the latter three appear as `<absent>` when the IdP does not issue them). A separate sentinel `<verifier-bug>` is reserved for an internal coding error — it should never appear in production, and its presence indicates a bug in the server's claim-extraction code, not in the caller's token; alert on it. The request still completes and the audit line is still written. In `disabled` mode no client auth runs, so log lines carry no identity fields at all. **`disabled` and `static` modes are not real audit trails**: `disabled` lines have no attribution, and `static` lines attribute every invocation to the hardcoded `dev-user`. Use `oauth` mode for any deployment whose audit logs need to answer "who ran what tool against which broker?"

Under `oauth` mode with a `tool_authorization` policy configured, each gated tool call also emits a `"tool authorization"` audit line at the same `correlation_id` — logged at `INFO` on allow and `WARN` on deny, with a `decision_reason` code operators can filter and alert on. See [Tool authorization](docs/configuration.md#tool-authorization) for the full schema.

The same policy can also narrow `tools/list` to the tools each caller may invoke, so an agent is not handed tools it will be denied. Off by default; opt in with `filter_tools_list: true` and see [Filtering `tools/list`](docs/configuration.md#filtering-toolslist). This is discovery hygiene rather than access control — `tools/call` remains the enforcement point either way.

Each event broker needs:
- `url` — the SEMP management API base URL
- `auth.mode` — `basic`, `bearer`, or `oauth` (examples below use basic auth; for bearer token authentication, set `auth.mode: bearer` and provide `auth.token` instead; for OAuth token exchange, see [Step 2b: Configure broker OAuth (Hop 2)](docs/authentication.md#step-2b-configure-broker-oauth-hop-2))
- `auth.username` / `auth.password` — credentials (use `${VAR_NAME}` to reference environment variables)

**Broker alias contract.** The map key under `brokers:` (for example, `my-broker`) is the alias that appears in tool inputs (`broker="my-broker"`), logs, and `list-brokers` output. Aliases must be 1–63 characters, contain only letters, digits, and hyphens, and start and end with an alphanumeric character. Comparison is case-insensitive — `Prod` and `prod` collide and the server refuses to start. Original casing is preserved in all user-facing output.

**2. Create a `.env` file** next to the config file:

```env
BROKER_USERNAME=admin
BROKER_PASSWORD=admin
```

The `.env` file is loaded automatically. Environment variables set directly (for example, in CI/CD) take precedence over `.env` values. See [Configuration Options](#configuration-options) for all settings, including port, TLS, and file path overrides.

---

Select a deployment method:
- **[Binary](#binary-deployment)** - Single executable with no dependencies; suitable for local development and VM deployment
- **[go install](#install-with-go-install)** - Build and install from source with the Go toolchain; suitable when you already have Go and want the latest tagged release on your `PATH`
- **[Docker](#docker-deployment)** - Containerized deployment; suitable for production and Kubernetes environments

For contributors running from source, see [Development Setup](#development-setup).

### Binary Deployment

Download the archive for your platform from the [latest release](https://github.com/SolaceProducts/solace-broker-mcp/releases/latest). Available platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64.

Download the checksums file, verify the checksum, and extract:

```bash
# Verify checksum
shasum -a 256 -c checksums-sha256.txt --ignore-missing

# Extract
tar xzf solace-broker-mcp-v*.tar.gz
```

Every release archive also carries a build provenance attestation. Verifying it proves the archive was produced by this repository's `release.yml` workflow, not rebuilt or replaced by someone else — a stronger guarantee than the checksum, which only proves the file matches the checksums list published beside it. Requires the [GitHub CLI](https://cli.github.com/), authenticated with `gh auth login` (the attestation is fetched from the GitHub API, which needs a token even for a public repository):

```bash
gh attestation verify solace-broker-mcp-v1.2.0-linux-amd64.tar.gz \
  --repo SolaceProducts/solace-broker-mcp \
  --signer-workflow SolaceProducts/solace-broker-mcp/.github/workflows/release.yml
```

Pass the exact archive filename — the command takes a single file, so a glob such as `solace-broker-mcp-v*.tar.gz` fails once you have more than one archive in the directory. `--signer-workflow` is what pins the attestation to the release workflow; `--repo` alone would accept an attestation minted by any workflow in this repository.

The archive contains the binary, an example config (`broker-config.example.yaml`), and the license. Copy the example config to `broker-config.yaml` and modify as needed.

Run the MCP server with the config file:

```bash
CONFIG_FILE=./broker-config.yaml ./solace-broker-mcp
```

If the config file is named `broker-config.yaml` in the current directory, the server does not require `CONFIG_FILE`.

Verify:

```bash
curl http://localhost:9090/livez
# {"status":"alive"}
```

The binary is statically linked with no external dependencies. It handles `SIGTERM` and `SIGINT` for graceful shutdown.

### Install with go install

If you have the Go toolchain installed ([Go 1.25+](https://go.dev/dl/)), install the server directly from source:

```bash
go install github.com/SolaceProducts/solace-broker-mcp/cmd/server@latest
```

This builds the latest tagged release and places a `server` binary in `$(go env GOBIN)` (or `$(go env GOPATH)/bin`). Ensure that directory is on your `PATH`. Pin a specific version by replacing `@latest` with a tag, for example, `@v1.2.0`.

Run it the same way as the downloaded binary, pointing `CONFIG_FILE` at your config:

```bash
CONFIG_FILE=./broker-config.yaml server
```

> **Note:** The installed binary is named `server` (the command's package directory), not `solace-broker-mcp`. Rename it or create a symlink if you prefer the longer name. Unlike release archives, `go install` does not include the example config or license — copy `broker-config.example.yaml` from the repository.

Verify:

```bash
curl http://localhost:9090/livez
# {"status":"alive"}
```

### Docker Deployment

```bash
docker run -d \
  --name solace-broker-mcp \
  -p 9090:9090 \
  -v /path/to/config.yaml:/etc/mcp-server/config.yaml:ro \
  --env-file /path/to/.env \
  ghcr.io/solaceproducts/solace-broker-mcp:latest
```

> **Note:** If the repository is private, authenticate with GHCR before pulling:
> ```bash
> gh auth token | docker login ghcr.io -u $(gh api user --jq .login) --password-stdin
> ```

The image carries a build provenance attestation, published to the registry alongside it. Verifying it proves the image was built by this repository's `release.yml` workflow:

```bash
gh attestation verify oci://ghcr.io/solaceproducts/solace-broker-mcp:latest \
  --repo SolaceProducts/solace-broker-mcp \
  --signer-workflow SolaceProducts/solace-broker-mcp/.github/workflows/release.yml
```

As above, `--signer-workflow` is what pins the attestation to the release workflow rather than to the repository at large. By default the attestation is fetched from the GitHub API; add `--bundle-from-oci` to read the copy stored beside the image on `ghcr.io` instead, which is also the copy `cosign` verifies.

The container reads config from `/etc/mcp-server/config.yaml` by default. Pass the credentials via `--env-file` or individual `-e` flags.

Verify:

```bash
curl http://localhost:9090/livez
# {"status":"alive"}
```

The image includes a built-in Docker health check using the binary's `--health` flag (no shell or curl needed in the container). Check status with `docker inspect --format '{{.State.Health.Status}}' solace-broker-mcp`, and the reason for a failure with `docker inspect --format '{{json .State.Health.Log}}' solace-broker-mcp`.

With TLS configured, the probe verifies the server's certificate against the file at `tls_cert_file`, which must therefore be readable inside the container and must carry at least one DNS or IP SAN — a certificate identified only by Common Name reports the container unhealthy even though the server serves HTTPS fine. See [Health Check Fails](docs/user-guide.md#troubleshooting) for the diagnostics.

**Docker Compose:**

```yaml
services:
  solace-broker-mcp:
    image: ghcr.io/solaceproducts/solace-broker-mcp:latest
    ports:
      - "9090:9090"
    volumes:
      - ./config.yaml:/etc/mcp-server/config.yaml:ro
    env_file:
      - .env
```

### Connect from Claude Code

After the MCP server is running (via binary, Docker, or `go run`), add it as an MCP server:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```

Example query:

```
List queues in the default VPN on the dev broker
```

### Connect from Solace Agent Mesh

After the MCP server is running, configure it to accept a static dev token (local development only). For production, use `mcp_client_auth.mode: oauth` — see [Authentication](docs/authentication.md).

```yaml
# broker-config.yaml
mcp_client_auth:
  mode: static
  dev_token: "sam-mcp-dev-token-local-only"
```

Then in your Agent Mesh project, add an MCP-tooled agent that points at this server with the matching bearer token. For complete setup instructions, see: [Agent Mesh Integration](docs/sam-integration.md).

Example queries through the Agent Mesh web UI:

```
What event brokers are configured?
List the queues on event-broker-one's default VPN.
```

## Development Setup

### 1. Clone and install dependencies

```bash
git clone https://github.com/SolaceProducts/solace-broker-mcp.git
cd solace-broker-mcp
go mod download
```

### 2. Install the git hooks

```bash
make hooks
```

Once per clone. This installs `.githooks/prepare-commit-msg` into `.git/hooks/` — read out of `origin/main` rather than out of your working tree, so `git fetch` first on a fresh clone — where it adds the DCO `Signed-off-by` trailer that the required `DCO` check looks for, so you do not have to remember `git commit -s`. Use `HOOKS_REF=HEAD make hooks` when you are editing the hook itself. See [Sign off automatically](.github/CONTRIBUTING.md#sign-off-automatically), and the header of `.githooks/prepare-commit-msg` for why the hook is copied from a trusted ref rather than activated with `core.hooksPath`.

### 3. Create event broker config and credentials

Create `broker-config.yaml` and `.env` in the repo root (both are gitignored). See [Configuration](#configuration) for the file format and examples.

### 4. Run the MCP server

```bash
go run ./cmd/server
```

The MCP server listens on port `9090` by default and serves the MCP endpoint at `/mcp`. The canonical liveness endpoint is `/livez`, which returns `{"status":"alive"}`. `/health` is retained for backward compatibility and preserves its original `{"status":"healthy"}` body — it is not a body-identical alias of `/livez`.

### Configuration Options

The server requires a YAML config file (event broker definitions) and credentials (via `.env` file or environment variables). Environment variables override file paths and port settings.


**Config and credential file locations:**

| Setting | Env var | Default |
|---|---|---|
| Config file path | `CONFIG_FILE` | `broker-config.yaml` in current directory |
| Credentials file path | `ENV_FILE` | `.env` next to config file |

**Server settings:**

| Setting | Env var | YAML field | Default |
|---|---|---|---|
| Port | `MCP_SERVER_PORT` | `port` | `9090` |
| TLS certificate | — | `tls_cert_file` | none (plain HTTP) |
| TLS private key | — | `tls_key_file` | none (plain HTTP) |

Priority: env var > YAML config > default.

**TLS (HTTPS):**

To enable HTTPS, add both `tls_cert_file` and `tls_key_file` to the YAML config:

```yaml
port: 9090
mcp_client_auth:
  mode: disabled
tls_cert_file: "/etc/certs/server.pem"
tls_key_file: "/etc/certs/server-key.pem"

brokers:
  my-broker:
    url: "http://broker:8080"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

When both are configured, the server starts with HTTPS. When neither is configured, plain HTTP. Providing only one is a startup error. The certificate must carry at least one DNS or IP SAN — see [Configuration](docs/configuration.md) for why, and for the container health-check consequence.

### 5. Connect from Claude Code

See [Connect from Claude Code](#connect-from-claude-code) in the Quickstart section.

## Project Structure

```
solace-broker-mcp/
├── cmd/server/          # Entry point — starts the MCP server
├── internal/
│   ├── auth/            # OAuth/JWT authentication middleware
│   ├── config/          # YAML config loading, env var substitution, validation
│   ├── composite/       # YAML-driven composite tool engine (loader, executor)
│   ├── defaults/        # Default values with assumption annotations
│   ├── semp/            # SEMP client layer (event broker pool, HTTP client, spec parser)
│   ├── tools/           # MCP tool registration, event broker resolution, tool call logging
│   └── version/         # Build-time version injection
├── docs/                # Architecture and secure logging rules
├── .claude/skills/      # Claude Code skills (add-logs, check-logs)
├── .github/workflows/   # GitHub Actions CI
├── broker-config.yaml   # Local event broker config (gitignored)
├── .env                 # Local credentials (gitignored)
├── go.mod
├── go.sum
└── README.md
```

## CI

GitHub Actions CI runs automatically on pull requests targeting `main` and on pushes to `main`. The workflow runs lint (golangci-lint), build, `go vet`, unit tests, E2E tests against real Solace brokers, and OAuth integration tests.

## Contributing

We welcome contributions! Please see our [Contributing Guidelines](.github/CONTRIBUTING.md) for details on:

- Reporting bugs and requesting features
- Development setup and workflow
- Coding standards and testing requirements
- Pull request process

Please read our [Code of Conduct](.github/CODE_OF_CONDUCT.md) before participating.

## Support

- **Solace support contract:** Contact support@solace.com.
- **Everyone else:** Ask in the [Solace Community](https://solace.community/).

See [SUPPORT.md](.github/SUPPORT.md) for details.

## Security

For security vulnerability reporting, see [SECURITY.md](.github/SECURITY.md).

## Disclaimer

This software is provided under the Apache License 2.0 on an "AS IS" basis, without warranties or conditions of any kind. See the [LICENSE](LICENSE) file for the full terms. Use it at your own risk.

If you have a Solace support contract, this project is covered through the usual [support@solace.com](mailto:support@solace.com) channel. Everyone else gets community support on a best-effort basis with no service-level commitment. See [Support](#support) for details.

You are responsible for ensuring your use of this server complies with the terms governing your brokers and any laws, regulations, or policies that apply to you.

**Production brokers.** This server issues real SEMP calls against the brokers you configure, including production brokers. Read operations add monitoring load; write operations (see [Tools](#tools)) change broker state. Test against a non-production broker before using it against production.

**AI-generated output.** This server is driven by AI assistants that interpret broker data and decide which tools to call. AI models can misread data, draw wrong conclusions, and select the wrong tool or arguments. Review the server's responses before acting on them, and review every proposed write or destructive action before you allow it.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

Attribution and third-party components are listed in [NOTICE](NOTICE) and [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).

Copyright 2024-2026 Solace Corporation. All rights reserved.
