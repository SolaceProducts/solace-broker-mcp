# solace-broker-mcp

An MCP (Model Context Protocol) server for Solace broker, built with Go using the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Prerequisites

- [Go](https://go.dev/dl/) (latest stable version)
- Git
- Access to one or more Solace brokers with SEMP management enabled

## Development Setup

### 1. Clone and install dependencies

```bash
git clone https://github.com/SolaceDev/solace-broker-mcp.git
cd solace-broker-mcp
go mod download
```

### 2. Create broker config

Create a `broker-config.yaml` file in the repo root (this file is gitignored):

```yaml
brokers:
  my-broker:
    url: "http://my-broker.example.com:8080"
    env_prefix: "MY_BROKER"
    auth:
      method: basic
```

Each broker needs:
- `url` — the SEMP management API base URL
- `env_prefix` — prefix for credential environment variables (uppercase letters, numbers, underscores only)
- `auth.method` — `basic` (only supported method currently)

You can configure multiple brokers:

```yaml
brokers:
  dev:
    url: "http://dev-broker.example.com:8080"
    env_prefix: "DEV"
    auth:
      method: basic
  staging:
    url: "http://staging-broker.example.com:8080"
    env_prefix: "STAGING"
    auth:
      method: basic
```

### 3. Set up credentials

Create a `.env` file next to `broker-config.yaml` (this file is gitignored):

```env
MY_BROKER_USERNAME=admin
MY_BROKER_PASSWORD=admin
```

The naming convention is `{ENV_PREFIX}_USERNAME` and `{ENV_PREFIX}_PASSWORD`. For multiple brokers, add credentials for each using their `env_prefix`.

The server automatically loads the `.env` file on startup. You can also set environment variables directly (e.g., in CI/CD) — they take precedence over `.env` values.

### 4. Run the server

```bash
go run ./cmd/server
```

The server listens on port `9090` by default.

### 5. Connect from Claude Code

Add the MCP server to Claude Code:

```bash
claude mcp add solace-broker --transport http http://localhost:9090/
```

Then ask Claude to interact with your brokers:

```
List queues in the default VPN on the dev broker
```

## Project Structure

```
solace-broker-mcp/
├── cmd/server/          # Entry point — starts the MCP server
├── internal/
│   ├── defaults/        # Default values with assumption annotations
│   ├── config/          # YAML config loading, env_prefix credentials
│   └── semp/            # SEMP client layer (broker pool, HTTP client, spec parser)
├── docs/                # Architecture documentation
├── specs/               # Design specs, task breakdowns, test plans
├── .github/workflows/   # GitHub Actions CI
├── broker-config.yaml   # Local broker config (gitignored)
├── .env                 # Local credentials (gitignored)
├── go.mod
├── go.sum
└── README.md
```

## CI

GitHub Actions CI runs automatically on pull requests targeting `main` and on pushes to `main`. The workflow builds the project, runs `go vet`, and runs tests.

## Architecture

See [docs/architecture.md](docs/architecture.md) for component diagrams, request flow, and design decisions.
