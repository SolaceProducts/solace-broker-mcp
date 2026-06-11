# Solace Broker MCP Server

Go MCP server exposing Solace broker monitoring/management tools over SEMP.
Architecture and diagrams: `docs/internal/architecture.md`.

## Commands

- `make check` — build, vet, lint, race-enabled tests. Full CI parity; run before pushing.
- `make test` — unit tests. Single test: `go test ./internal/<pkg>/ -run TestName -v`
- `make e2e-all` — full E2E cycle against Dockerized brokers (`docs/internal/e2e-testing.md`)
- CI (`.github/workflows/build-and-test.yml`) is the source of truth; if the Makefile drifts, CI wins.

## Adding a tool

Two mechanisms — prefer the first:

1. **Composite (YAML):** declare in `internal/composite/definitions/tools.yaml`
   (embedded at build time). Multi-step SEMPv2 calls, templates, pagination.
2. **Native Go handler:** `internal/tools/sempv1/`, one package per tool.
   Only for logic YAML can't express (e.g. SEMPv1 XML parsing).

Never declare a `broker` parameter — it is auto-injected into every tool's
schema at registration (`injectBrokerParam` in `internal/tools/register.go`).

## Tool naming

MCP tool names use **kebab-case** — e.g. `get-broker-health`, `list-queues`,
`get-redundancy-status`. This applies to every tool the server exposes,
regardless of whether it's defined in YAML (composite) or implemented in
Go (native SEMPv1/SEMPv2). Avoid `snake_case` and `camelCase`.

Match the in-code constant or `Name:` field exactly to the on-the-wire
tool name; LLMs see and pattern-match against this string.

## Before committing

Run `/check-logs` to scan for logging security violations. Fix all CRITICAL
and HIGH issues before committing. Rules: `docs/internal/secure-logging-rules.md`.
