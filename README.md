# solace-broker-mcp

[![Build Status](https://github.com/SolaceDev/solace-broker-mcp/workflows/Build%20and%20Test/badge.svg)](https://github.com/SolaceDev/solace-broker-mcp/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/SolaceDev/solace-broker-mcp)](https://goreportcard.com/report/github.com/SolaceDev/solace-broker-mcp)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/SolaceDev/solace-broker-mcp)](go.mod)
[![Contributor Covenant](https://img.shields.io/badge/Contributor%20Covenant-2.1-4baaaa.svg)](.github/CODE_OF_CONDUCT.md)

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
  - [Docker Deployment](#docker-deployment)
  - [Connect from Claude Code](#connect-from-claude-code)
  - [Connect from Solace Agent Mesh (SAM)](#connect-from-solace-agent-mesh-sam)
- [Development Setup](#development-setup)
  - [Configuration Options](#configuration-options)
- [Project Structure](#project-structure)
- [CI](#ci)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Overview

An HTTP service that exposes Solace event broker management and monitoring to AI assistants through the Model Context Protocol (MCP). The server provides 17 read-only tools that query event broker status, inspect queues, diagnose client issues, and monitor message traffic using SEMP v1 and v2 API calls.

MCP-compatible clients, for example Claude Code, invoke these tools using natural language. The AI assistant translates requests into tool calls. The server handles authentication, rate limiting, retries, and response formatting.

## Features

- **17 read-only monitoring tools** — Event broker status, message VPNs, queues, clients, and REST delivery points
- **Client authentication** — Development mode (no auth), static bearer tokens, or OAuth 2.1/OIDC with JWT validation
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
│  Claude Desktop) │   + Bearer JWT     │  • 13 read-only tools    │                      │  Broker(s)       │
│                  │                    │  • Rate-limit + retry    │                      │                  │
│                  │ ◀──────────────── │  • SEMP client pool      │ ◀──────────────────  │                  │
└──────────────────┘                    └──────────────────────────┘   basic / bearer     └──────────────────┘
```

## Tools

The server exposes read-only tools grouped by what they inspect, plus a small set of action tools for operational workflows. Every tool except `list-brokers` takes a `broker` parameter naming a configured broker alias. See the [user guide](docs/user-guide.md#tools) for per-tool descriptions, parameters, and pagination defaults.

| Category | Tools | Description |
|---|---|---|
| Discovery | `list-brokers` | List configured broker aliases for use as the `broker` parameter |
| Broker status | `get-broker-status`, `get-redundancy-status` | Snapshot of version, uptime, resources, spool, and HA and mate-link state |
| Replication | `get-replication-status` | Replication role, sync eligibility, bridge status, transaction mode, and queued-message counts |
| Message VPN | `list-vpns`, `get-vpn-health`, `get-message-rates` | List VPNs, check per-VPN service health, read message and byte rates |
| Queues | `list-queues`, `get-queue-metrics` | List queues with depth and throughput; drill into spool, bindings, consumers |
| Clients | `list-clients`, `get-client-details`, `list-client-subscriptions`, `list-slow-subscribers` | List connections, inspect per-client rates and discards, list subscriptions, filter for slow-subscriber-flagged clients |
| REST Delivery Points | `list-rdps`, `get-rdp-status` | List RDPs; inspect bindings, REST consumers, and last failure reason |
| Discards | `get-discard-stats`, `list-queue-discards` | Broker-wide and per-VPN discard aggregates; per-queue discard counters |
| Actions | `execute-queue-action`, `execute-client-action` | Execute queue actions (`deleteMsgs`, `clearStats`) and client actions (`disconnect`, `clearStats`); destructive variants instruct the LLM to obtain user confirmation before invocation. **Gated behind `enable_write_tools: true` in the config — default off; not registered in `tools/list` when disabled.** |

## Guides

- [User Guide](docs/user-guide.md) — overview, tools reference, deployment, and troubleshooting
- [Configuration](docs/configuration.md) — server settings, event broker config, client auth, and rate-limit/retry settings
- [Authentication](docs/authentication.md) — OAuth/OIDC and static token setup for MCP clients
- [SAM Integration](docs/sam-integration.md) — wire this MCP server into a Solace Agent Mesh project as an agent

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

Each event broker needs:
- `url` — the SEMP management API base URL
- `auth.mode` — `basic` or `bearer` (examples below use basic auth; for bearer token authentication, set `auth.mode: bearer` and provide `auth.token` instead)
- `auth.username` / `auth.password` — credentials (use `${VAR_NAME}` to reference environment variables)

**Broker alias contract.** The map key under `brokers:` (e.g. `my-broker`) is the alias that appears in tool inputs (`broker="my-broker"`), logs, and `list-brokers` output. Aliases must be 1–63 characters, contain only letters, digits, and hyphens, and start and end with an alphanumeric character. Comparison is case-insensitive — `Prod` and `prod` collide and the server will refuse to start. Original casing is preserved in all user-facing output.

**2. Create a `.env` file** next to the config file:

```env
BROKER_USERNAME=admin
BROKER_PASSWORD=admin
```

The `.env` file is loaded automatically. Environment variables set directly (for example, in CI/CD) take precedence over `.env` values. See [Configuration Options](#configuration-options) for all settings, including port, TLS, and file path overrides.

---

Select a deployment method:
- **[Binary](#binary-deployment)** - Single executable with no dependencies; suitable for local development and VM deployment
- **[Docker](#docker-deployment)** - Containerized deployment; suitable for production and Kubernetes environments

For contributors running from source, see [Development Setup](#development-setup).

### Binary Deployment

Download the archive for your platform from the [latest release](https://github.com/SolaceDev/solace-broker-mcp/releases/latest). Available platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64.

Download the checksums file, verify the checksum, and extract:

```bash
# Verify checksum
shasum -a 256 -c checksums-sha256.txt --ignore-missing

# Extract
tar xzf solace-broker-mcp-v*.tar.gz
```

The archive contains the binary, an example config (`broker-config.example.yaml`), and the license. Copy the example config to `broker-config.yaml` and modify as needed.

Run the MCP server with the config file:

```bash
CONFIG_FILE=./broker-config.yaml ./solace-broker-mcp
```

If the config file is named `broker-config.yaml` in the current directory, the server does not require `CONFIG_FILE`.

Verify:

```bash
curl http://localhost:9090/health
# {"status": "ok"}
```

The binary is statically linked with no external dependencies. It handles `SIGTERM` and `SIGINT` for graceful shutdown.

### Docker Deployment

```bash
docker run -d \
  --name solace-broker-mcp \
  -p 9090:9090 \
  -v /path/to/config.yaml:/etc/mcp-server/config.yaml:ro \
  --env-file /path/to/.env \
  ghcr.io/solacedev/solace-broker-mcp:latest
```

> **Note:** If the repository is private, authenticate with GHCR before pulling:
> ```bash
> gh auth token | docker login ghcr.io -u $(gh api user --jq .login) --password-stdin
> ```

The container reads config from `/etc/mcp-server/config.yaml` by default. Pass the credentials via `--env-file` or individual `-e` flags.

Verify:

```bash
curl http://localhost:9090/health
# {"status": "ok"}
```

The image includes a built-in Docker health check using the binary's `--health` flag (no shell or curl needed in the container). Check status with `docker inspect --format '{{.State.Health.Status}}' solace-broker-mcp`.

**Docker Compose:**

```yaml
services:
  solace-broker-mcp:
    image: ghcr.io/solacedev/solace-broker-mcp:latest
    ports:
      - "9090:9090"
    volumes:
      - ./config.yaml:/etc/mcp-server/config.yaml:ro
    env_file:
      - .env
```

### Connect from Claude Code

Once the MCP server is running (via binary, Docker, or `go run`), add it as an MCP server:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```

Example query:

```
List queues in the default VPN on the dev broker
```

### Connect from Solace Agent Mesh (SAM)

Once the MCP server is running, configure it to accept a static dev token (local development only). For production, use `mcp_client_auth.mode: oauth` — see [Authentication](docs/authentication.md).

```yaml
# broker-config.yaml
mcp_client_auth:
  mode: static
  dev_token: "sam-mcp-dev-token-local-only"
```

Then in your SAM project, add an MCP-tooled agent that points at this server with the matching bearer token. For complete setup instructions, see: [SAM Integration](docs/sam-integration.md).

Example queries through the SAM web UI:

```
What event brokers are configured?
List the queues on event-broker-one's default VPN.
```

## Development Setup

### 1. Clone and install dependencies

```bash
git clone https://github.com/SolaceDev/solace-broker-mcp.git
cd solace-broker-mcp
go mod download
```

### 2. Create event broker config and credentials

Create `broker-config.yaml` and `.env` in the repo root (both are gitignored). See [Configuration](#configuration) for the file format and examples.

### 3. Run the MCP server

```bash
go run ./cmd/server
```

The MCP server listens on port `9090` by default and serves the MCP endpoint at `/mcp`. A health check endpoint is available at `/health`.

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

When both are configured, the server starts with HTTPS. When neither is configured, plain HTTP. Providing only one is a startup error.

### 4. Connect from Claude Code

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

## Security

For security vulnerability reporting, please see [SECURITY.md](.github/SECURITY.md).

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

Copyright 2024-2026 Solace Corporation. All rights reserved.
