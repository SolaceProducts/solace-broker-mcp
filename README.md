# solace-broker-mcp

An MCP (Model Context Protocol) server for Solace broker, built with Go using the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Prerequisites

- Access to one or more Solace brokers with SEMP management enabled
- [Docker](https://docs.docker.com/get-docker/) (for Docker deployment) or a supported OS/arch for the binary (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64)
- [Go](https://go.dev/dl/) (latest stable version) — only needed for development

## Quickstart

### Configuration

Both binary and Docker deployments use the same YAML config file and `.env` credentials file.

**1. Create a config file** (e.g., `config.yaml`):

```yaml
development_mode: true

brokers:
  my-broker:
    url: "http://my-broker.example.com:8080"
    auth:
      mode: basic
      username: "${BROKER_USERNAME}"
      password: "${BROKER_PASSWORD}"
```

`development_mode: true` disables OAuth authentication for local use. For production, set `development_mode: false` and configure the `client_auth` section with your OAuth provider (issuer, audience, resource URL).

Each broker needs:
- `url` — the SEMP management API base URL
- `auth.mode` — `basic` or `bearer`
- `auth.username` / `auth.password` — credentials (use `${VAR_NAME}` to reference environment variables)

**2. Create a `.env` file** next to the config file:

```env
BROKER_USERNAME=admin
BROKER_PASSWORD=admin
```

The `.env` file is loaded automatically. Environment variables set directly (e.g., in CI/CD) take precedence over `.env` values. See [Configuration Options](#configuration-options) for all settings including port, TLS, and file path overrides.

### Binary

Download the archive for your platform from the [latest release](https://github.com/SolaceDev/solace-broker-mcp/releases/latest), verify the checksum, and extract:

```bash
tar xzf solace-broker-mcp-v*.tar.gz
sha256sum -c checksums-sha256.txt --ignore-missing
```

The archive contains the binary, an example config (`broker-config.example.yaml`), and the license.

Run the server with your config file:

```bash
CONFIG_FILE=/path/to/config.yaml ./solace-broker-mcp
```

If the config file is named `broker-config.yaml` in the current directory, `CONFIG_FILE` is not needed.

Verify:

```bash
curl http://localhost:9090/health
# {"status": "ok"}
```

The binary is statically linked with no external dependencies. It handles `SIGTERM` and `SIGINT` for graceful shutdown.

### Docker

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

The container reads config from `/etc/mcp-server/config.yaml` by default. Credentials can be passed via `--env-file` or individual `-e` flags.

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

Once the server is running (via binary, Docker, or `go run`), add it as an MCP server:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/mcp
```

Then ask Claude to interact with your brokers:

```
List queues in the default VPN on the dev broker
```

## Development Setup

### 1. Clone and install dependencies

```bash
git clone https://github.com/SolaceDev/solace-broker-mcp.git
cd solace-broker-mcp
go mod download
```

### 2. Create broker config and credentials

Create `broker-config.yaml` and `.env` in the repo root (both are gitignored). See [Configuration](#configuration) in the Quickstart section for the file format and examples.

### 3. Run the server

```bash
go run ./cmd/server
```

The server listens on port `9090` by default and serves the MCP endpoint at `/mcp`. A health check endpoint is available at `/health`.

### Configuration Options

The server requires a YAML config file (broker definitions) and credentials (via `.env` file or environment variables). Env var overrides are available for file paths and port.

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
development_mode: true
tls_cert_file: "/etc/certs/server.pem"
tls_key_file: "/etc/certs/server-key.pem"

brokers:
  my-broker:
    url: "http://broker:8080"
    auth:
      mode: basic
      username: "${MY_BROKER_USERNAME}"
      password: "${MY_BROKER_PASSWORD}"
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
│   ├── semp/            # SEMP client layer (broker pool, HTTP client, spec parser)
│   ├── tools/           # MCP tool registration, broker resolution, tool call logging
│   └── version/         # Build-time version injection
├── docs/                # Architecture and secure logging rules
├── .claude/skills/      # Claude Code skills (add-logs, check-logs)
├── .github/workflows/   # GitHub Actions CI
├── broker-config.yaml   # Local broker config (gitignored)
├── .env                 # Local credentials (gitignored)
├── go.mod
├── go.sum
└── README.md
```

## CI

GitHub Actions CI runs automatically on pull requests targeting `main` and on pushes to `main`. The workflow runs lint (golangci-lint), build, `go vet`, unit tests, E2E tests against real Solace brokers, and OAuth integration tests.

## Architecture

See [docs/architecture.md](docs/architecture.md) for component diagrams, request flow, and design decisions.
