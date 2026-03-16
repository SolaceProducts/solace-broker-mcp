# solace-broker-mcp

An MCP (Model Context Protocol) server for Solace broker, built with Go using the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Prerequisites

- [Go](https://go.dev/dl/) (latest stable version)
- Git

## Getting started

Clone the repository:

```bash
git clone https://github.com/SolaceDev/solace-broker-mcp.git
cd solace-broker-mcp
```

Download dependencies:

```bash
go mod download
```

Run the server:

```bash
go run ./cmd/server/
```

The server will start listening on port `8080` by default. To use a custom port:

```bash
PORT=9090 go run ./cmd/server/
```

## Building

```bash
go build -o mcp-server ./cmd/server/
```

Then run the binary:

```bash
./mcp-server
```

## Project structure

```
solace-broker-mcp/
├── cmd/server/        # Entry point — starts the MCP server
├── internal/          # Private application code
├── .github/workflows/ # GitHub Actions CI
├── .gitignore
├── go.mod
├── go.sum
└── README.md
```

## CI

GitHub Actions CI runs automatically on pull requests targeting `main` and on pushes to `main`. The workflow builds the project, runs `go vet`, and runs tests.
